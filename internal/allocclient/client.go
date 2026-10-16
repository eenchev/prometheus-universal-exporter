// Package allocclient is the exporter's side of the target allocator
// (internal/allocapi): in allocator mode (--allocator.url) it heartbeats to
// every copy of the allocator, long-polls the allocator for its bundle,
// writes a new bundle into the cache directory where the configuration
// manager loads it, and has the manager reload it.
//
// A bundle is written beside the one in force and put in place by swapping
// one symbolic link, so whatever reads the cache directory finds one bundle
// or the other, never part of each (cache.go). It is checked before, as the
// reload would check it: a bundle that does not load is refused, the files
// in force stay, and the refusal is reported in the next heartbeat.
package allocclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// The stages a sync with the allocator can fail at, the stage label of
// http_exporter_allocator_sync_errors_total: a heartbeat to one copy of the
// allocator, the fetch of the bundle, or writing it into the cache directory.
const (
	StageHeartbeat = "heartbeat"
	StageFetch     = "fetch"
	StageWrite     = "write"
)

// ReloadTrigger is the trigger the reload of a new bundle is logged with.
const ReloadTrigger = "allocator"

// DefaultWait is how long the allocator holds a bundle request that already
// has the generation it would answer, the wait parameter of BundlePath.
const DefaultWait = 30 * time.Second

// The bounds of the wait between two tries after an error: the first try
// again after DefaultMinBackoff, each next after twice as long, up to
// DefaultMaxBackoff. A bundle request answered at once without a change
// waits DefaultMinBackoff too, so an allocator that does not hold requests
// is not asked as fast as it answers.
const (
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = 30 * time.Second
)

// heartbeatTimeout bounds one heartbeat to one copy of the allocator, and
// bundleGrace how much longer than its wait a bundle request may take.
const (
	heartbeatTimeout = 5 * time.Second
	bundleGrace      = 15 * time.Second
)

// maxBundleBytes and maxReplyBytes bound what is read of an answer.
const (
	maxBundleBytes = 64 << 20
	maxReplyBytes  = 1 << 20
)

// Reloader is the configuration manager as the client uses it
// (config.Manager).
type Reloader interface {
	// CheckReplacement checks the files at the two paths as a reload
	// would, without putting them in force.
	CheckReplacement(configPath, targetsPath string) error
	// ReloadReplacing runs replace and reloads under the reload lock, and
	// when the reload is refused runs restore and reloads again.
	ReloadReplacing(trigger string, replace func() (restore func() error, err error)) error
}

// Options are what a Client is made with. URL, CacheDir, Member, Advertise
// and Manager are required.
type Options struct {
	// URL is the allocator's base URL, http://host:port.
	URL string
	// CacheDir is where bundles are written (cache.go).
	CacheDir string
	// Member and Advertise are what the heartbeats say of this replica.
	Member, Advertise string
	// TokenFile, when set, holds the bearer token sent with every request,
	// read again for each, so a rotated Secret is used from the next.
	TokenFile string
	Manager   Reloader
	Logger    *slog.Logger
	// Generation is the generation of the bundle in force at the start,
	// found in the cache directory (CachedGeneration), empty for none.
	Generation string

	// The rest are for tests; left zero they are the defaults.

	// Resolve gives the addresses the allocator URL's host name resolves
	// to; net.DefaultResolver.LookupHost by default.
	Resolve func(ctx context.Context, host string) ([]string, error)
	// HeartbeatInterval is allocapi.HeartbeatInterval by default.
	HeartbeatInterval time.Duration
	// Wait is DefaultWait by default.
	Wait time.Duration
	// MinBackoff and MaxBackoff are DefaultMinBackoff and DefaultMaxBackoff
	// by default.
	MinBackoff, MaxBackoff time.Duration
	// Transport is http.DefaultTransport's clone by default.
	Transport http.RoundTripper
	// Event, when set, is told of what the client did, once it is done.
	Event func(Event)
	// Step, when set, is called after each change made to the cache
	// directory, naming it, while the change after it has not been made.
	Step func(change string)
}

// Event is something the client did, for Options.Event.
type Event struct {
	// Kind is one of the Event... kinds.
	Kind string
	// Generation is the bundle's, for a bundle event.
	Generation string
	// Err is the error or refusal, when there was one.
	Err error
}

// The kinds of Event.
const (
	// EventHeartbeat is a heartbeat sent to every copy of the allocator;
	// Err joins the failures.
	EventHeartbeat = "heartbeat"
	// EventInstalled is a new bundle put in force.
	EventInstalled = "installed"
	// EventRefused is a new bundle refused.
	EventRefused = "refused"
	// EventUnchanged is a bundle request answered without a new bundle.
	EventUnchanged = "unchanged"
	// EventNotSettled is a bundle request the allocator answered 503 with
	// Retry-After, as it does until its member list has formed; it is
	// asked again after Retry-After, and is no failure.
	EventNotSettled = "not_settled"
	// EventError is a bundle that could not be fetched or written; it is
	// tried again after a backoff.
	EventError = "error"
)

// Status is how the client is keeping up with the allocator, for the
// self-metrics (exporter.AllocatorStatus has the same fields).
type Status struct {
	Generation string
	LastSync   time.Time
	SyncErrors map[string]uint64
	Refused    uint64
}

// Client heartbeats to the allocator and keeps the bundle in the cache
// directory up to date. Make it with New.
type Client struct {
	o      Options
	base   *url.URL
	client *http.Client
	// wake asks the bundle loop to stop waiting out a backoff, and beat the
	// heartbeat loop to heartbeat now.
	wake, beat chan struct{}

	mu sync.Mutex
	// inForce is the generation of the bundle in force, seen that of the
	// last bundle fetched, put in force or refused: the bundle request
	// says it, so a refused bundle is not fetched again until the
	// allocator has another.
	inForce, seen string
	refused       *allocapi.Refusal
	shuttingDown  bool
	lastSync      time.Time
	syncErrors    map[string]uint64
	refusedTotal  uint64
	// lastLogged is the last error logged for each stage, so an error
	// repeated at every try is logged once, until it changes or ends.
	lastLogged map[string]string
}

// New checks o and returns a client for it.
func New(o Options) (*Client, error) {
	base, err := ParseURL(o.URL)
	if err != nil {
		return nil, err
	}
	if o.CacheDir == "" || o.Member == "" || o.Advertise == "" || o.Manager == nil {
		return nil, errors.New("the allocator client needs a cache directory, a member, an address to advertise and a configuration manager")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Resolve == nil {
		o.Resolve = net.DefaultResolver.LookupHost
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = allocapi.HeartbeatInterval
	}
	if o.Wait <= 0 {
		o.Wait = DefaultWait
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = max(DefaultMaxBackoff, o.MinBackoff)
	}
	if o.Transport == nil {
		o.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &Client{
		o: o, base: base, client: &http.Client{Transport: o.Transport},
		wake: make(chan struct{}, 1), beat: make(chan struct{}, 1),
		inForce: o.Generation, seen: o.Generation,
		syncErrors: map[string]uint64{StageHeartbeat: 0, StageFetch: 0, StageWrite: 0},
		lastLogged: map[string]string{},
	}, nil
}

// ParseURL checks --allocator.url: an http URL with a host, and nothing
// after its path. The allocator is served over plain HTTP.
func ParseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("--allocator.url %q must be the allocator's http:// URL, such as http://pue-allocator:9116", raw)
	}
	return u, nil
}

// Run heartbeats and keeps the bundle up to date until ctx ends.
func (c *Client) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { c.heartbeatLoop(ctx) })
	wg.Go(func() { c.bundleLoop(ctx) })
	wg.Wait()
}

// ShuttingDown makes every heartbeat from now on say the replica is shutting
// down, and sends one at once, within ctx, so the allocator takes the
// replica out of the member list now rather than when its heartbeats stop.
func (c *Client) ShuttingDown(ctx context.Context) {
	c.mu.Lock()
	c.shuttingDown = true
	c.mu.Unlock()
	c.heartbeat(ctx)
}

// Status returns how the client is keeping up with the allocator.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	errs := make(map[string]uint64, len(c.syncErrors))
	for stage, n := range c.syncErrors {
		errs[stage] = n
	}
	return Status{Generation: c.inForce, LastSync: c.lastSync, SyncErrors: errs, Refused: c.refusedTotal}
}

// event tells Options.Event of e.
func (c *Client) event(e Event) {
	if c.o.Event != nil {
		c.o.Event(e)
	}
}

// signal sends on a channel of one without waiting: a signal already
// pending is the same signal.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// failed counts a failure at stage and logs it, unless the last failure
// logged at that stage said the same.
func (c *Client) failed(stage string, err error) {
	c.mu.Lock()
	c.syncErrors[stage]++
	repeated := c.lastLogged[stage] == err.Error()
	c.lastLogged[stage] = err.Error()
	c.mu.Unlock()
	if !repeated {
		c.o.Logger.Warn("sync with the target allocator failed; trying again", "stage", stage, "allocator", c.base.String(), "error", err.Error())
	}
}

// recovered logs that stage works again after a failure was logged.
func (c *Client) recovered(stage string) {
	c.mu.Lock()
	was := c.lastLogged[stage]
	delete(c.lastLogged, stage)
	c.mu.Unlock()
	if was != "" {
		c.o.Logger.Info("sync with the target allocator works again", "stage", stage, "allocator", c.base.String())
	}
}

// authorize sets the bearer token from the token file on req.
func (c *Client) authorize(req *http.Request) error {
	if c.o.TokenFile == "" {
		return nil
	}
	b, err := os.ReadFile(c.o.TokenFile)
	if err != nil {
		return fmt.Errorf("reading --allocator.token-file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return fmt.Errorf("--allocator.token-file %s is empty", c.o.TokenFile)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// heartbeatLoop heartbeats at once, then every HeartbeatInterval, and at
// once again after a bundle was put in force or refused, until ctx ends.
func (c *Client) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(c.o.HeartbeatInterval)
	defer ticker.Stop()
	for {
		c.heartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.beat:
		}
	}
}

// heartbeat sends one heartbeat to every address the allocator URL's host
// resolves to, at once, and waits for them: two copies of the allocator
// behind one name both hear every replica. A reply naming a generation
// other than the last seen wakes the bundle loop from a backoff.
func (c *Client) heartbeat(ctx context.Context) {
	c.mu.Lock()
	hb := allocapi.Heartbeat{Member: c.o.Member, Advertise: c.o.Advertise, ShuttingDown: c.shuttingDown, Generation: c.inForce, Refused: c.refused}
	seen := c.seen
	c.mu.Unlock()
	body, err := json.Marshal(hb)
	if err != nil {
		c.failed(StageHeartbeat, err)
		c.event(Event{Kind: EventHeartbeat, Err: err})
		return
	}
	addrs, err := c.addresses(ctx)
	if err != nil {
		err = fmt.Errorf("resolving the allocator's host %s: %w", c.base.Hostname(), err)
		c.failed(StageHeartbeat, err)
		c.event(Event{Kind: EventHeartbeat, Err: err})
		return
	}
	errs := make([]error, len(addrs))
	replies := make([]string, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Go(func() { replies[i], errs[i] = c.heartbeatTo(ctx, addr, body) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			c.failed(StageHeartbeat, err)
		}
	}
	all := errors.Join(errs...)
	if all == nil {
		c.recovered(StageHeartbeat)
	}
	for _, generation := range replies {
		if generation != "" && generation != seen {
			signal(c.wake)
			break
		}
	}
	c.event(Event{Kind: EventHeartbeat, Err: all})
}

// addresses are the addresses the allocator URL's host resolves to, or the
// host itself when it is an IP address.
func (c *Client) addresses(ctx context.Context) ([]string, error) {
	host := c.base.Hostname()
	if net.ParseIP(host) != nil {
		return []string{host}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	addrs, err := c.o.Resolve(ctx, host)
	if err == nil && len(addrs) == 0 {
		err = errors.New("no addresses")
	}
	return addrs, err
}

// heartbeatTo sends the heartbeat body to the allocator at addr, with the
// URL's host as the Host header, and returns the generation its reply names.
func (c *Client) heartbeatTo(ctx context.Context, addr string, body []byte) (string, error) {
	port := c.base.Port()
	if port == "" {
		port = "80"
	}
	u := *c.base.JoinPath(allocapi.HeartbeatPath)
	u.Host = net.JoinHostPort(addr, port)
	ctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Host = c.base.Host
	req.Header.Set("Content-Type", "application/json")
	if err := c.authorize(req); err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("heartbeat to %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return "", fmt.Errorf("heartbeat to %s: %w", u.Host, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("heartbeat to %s: the allocator answered %s: %s", u.Host, resp.Status, firstLine(raw))
	}
	var reply allocapi.HeartbeatReply
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &reply); err != nil {
			return "", fmt.Errorf("heartbeat to %s: the allocator's reply is not a heartbeat reply: %w", u.Host, err)
		}
	}
	return reply.Generation, nil
}

// retryAfter is a Retry-After header of seconds as a duration, 0 when it is
// none or not a number of seconds.
func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(min(seconds, 3600)) * time.Second
}

// firstLine is the first line of an error answer's body, for a log line.
func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// bundleLoop asks the allocator for this replica's bundle, and puts each new
// one in force, until ctx ends. Each request names the last generation seen,
// which the allocator holds for up to Wait while it has no other. A failure
// is tried again after a backoff that doubles up to MaxBackoff; an answer
// without a change that came at once waits MinBackoff, so the loop never
// asks as fast as an allocator can answer.
func (c *Client) bundleLoop(ctx context.Context) {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		asked := time.Now()
		unchanged, ok, retryAfter := c.syncOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		// A heartbeat reply that woke the loop before this answer is older
		// than it.
		select {
		case <-c.wake:
		default:
		}
		var wait time.Duration
		switch {
		case retryAfter > 0:
			backoff, wait = 0, min(max(retryAfter, c.o.MinBackoff), c.o.MaxBackoff)
		case !ok:
			backoff = min(max(backoff*2, c.o.MinBackoff), c.o.MaxBackoff)
			wait = backoff
		case unchanged && time.Since(asked) < c.o.MinBackoff:
			backoff, wait = 0, c.o.MinBackoff
		default:
			backoff = 0
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-c.wake:
			timer.Stop()
		}
	}
}

// syncOnce makes one bundle request and acts on its answer. ok is false
// when the bundle could not be fetched or written, and unchanged says the
// answer brought no new bundle. retryAfter is set when the allocator has no
// member list yet: it is the allocator's Retry-After, and no failure.
func (c *Client) syncOnce(ctx context.Context) (unchanged, ok bool, retryAfter time.Duration) {
	b, err := c.fetch(ctx)
	var notSettled *notSettledError
	switch {
	case err != nil && ctx.Err() != nil:
		return false, false, 0
	case errors.As(err, &notSettled):
		c.recovered(StageFetch)
		c.event(Event{Kind: EventNotSettled, Err: err})
		return true, true, notSettled.after
	case err != nil:
		c.failed(StageFetch, err)
		c.event(Event{Kind: EventError, Err: err})
		return false, false, 0
	}
	c.recovered(StageFetch)
	c.mu.Lock()
	c.lastSync = time.Now()
	same := b == nil || b.Generation == c.seen || b.Generation == c.inForce
	c.mu.Unlock()
	if same {
		c.event(Event{Kind: EventUnchanged})
		return true, true, 0
	}
	refusal, err := c.apply(b)
	if err != nil {
		c.failed(StageWrite, err)
		c.event(Event{Kind: EventError, Generation: b.Generation, Err: err})
		return false, false, 0
	}
	c.recovered(StageWrite)
	c.mu.Lock()
	c.seen = b.Generation
	if refusal != nil {
		c.refused = &allocapi.Refusal{Generation: b.Generation, Error: refusal.Error()}
		c.refusedTotal++
	} else {
		c.inForce, c.refused = b.Generation, nil
	}
	c.mu.Unlock()
	if refusal != nil {
		c.o.Logger.Error("bundle from the target allocator refused; the configuration in force stays", "generation", b.Generation, "error", refusal.Error())
		c.event(Event{Kind: EventRefused, Generation: b.Generation, Err: refusal})
	} else {
		c.o.Logger.Info("bundle from the target allocator put in force", "generation", b.Generation, "files", len(b.Files))
		c.event(Event{Kind: EventInstalled, Generation: b.Generation})
	}
	// The allocator hears of it now rather than at the next interval.
	signal(c.beat)
	return false, true, 0
}

// notSettledError is the allocator's 503 answer with Retry-After: it has
// no member list yet, so it cannot tell this replica's share, and says when
// to ask again. A copy of the allocator that has just started answers so
// for its settle time, while the replica keeps the bundle it has.
type notSettledError struct {
	after time.Duration
	text  string
}

func (e *notSettledError) Error() string {
	return "the allocator has no member list yet: " + e.text
}

// fetch asks the allocator for this replica's bundle, naming the last
// generation seen in If-None-Match, and returns it, or nil when the answer
// was 304 Not Modified.
func (c *Client) fetch(ctx context.Context) (*allocapi.Bundle, error) {
	c.mu.Lock()
	seen := c.seen
	c.mu.Unlock()
	u := c.base.JoinPath(allocapi.BundlePath)
	u.RawQuery = url.Values{"member": {c.o.Member}, "wait": {c.o.Wait.String()}}.Encode()
	ctx, cancel := context.WithTimeout(ctx, c.o.Wait+bundleGrace)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if seen != "" {
		// An entity tag is quoted (RFC 9110); the allocator's ETag is.
		req.Header.Set("If-None-Match", `"`+seen+`"`)
	}
	if err := c.authorize(req); err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bundle request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bundle request: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, nil
	case resp.StatusCode == http.StatusServiceUnavailable && retryAfter(resp.Header.Get("Retry-After")) > 0:
		return nil, &notSettledError{after: retryAfter(resp.Header.Get("Retry-After")), text: firstLine(raw)}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("bundle request: the allocator answered %s: %s", resp.Status, firstLine(raw))
	case len(raw) > maxBundleBytes:
		return nil, fmt.Errorf("bundle request: the bundle is larger than %d bytes", maxBundleBytes)
	}
	var b allocapi.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("bundle request: the answer is not a bundle: %w", err)
	}
	return &b, nil
}
