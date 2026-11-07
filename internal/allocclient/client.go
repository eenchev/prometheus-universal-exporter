// Package allocclient is the exporter's side of the target allocator
// (internal/allocapi): in allocator mode (--allocator.url) it heartbeats to
// every instance of the allocator, long-polls the allocator for its bundle,
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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// The stages a sync with the allocator can fail at, the stage label of
// http_exporter_allocator_sync_errors_total: a heartbeat to one instance of the
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

// heartbeatTimeout bounds one heartbeat to one instance of the allocator, and a
// lookup of the allocator's addresses, when the heartbeat interval is not
// shorter (heartbeatBound); bundleGrace bounds how much longer than its
// wait a bundle request may take.
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
	// when the reload is refused runs restore and reloads again. generation
	// is that of the files replace puts in place, which the configuration
	// put in force from them carries.
	ReloadReplacing(trigger, generation string, replace func() (restore func() error, err error)) error
}

// Options are what a Client is made with. URL, CacheDir, Member, Advertise
// and Manager are required.
type Options struct {
	// URL is the allocator's base URL, http://host:port.
	URL string
	// CacheDir is where bundles are written (cache.go).
	CacheDir string
	// Member and Advertise are what the heartbeats say of this replica,
	// and Capacity its --allocator.capacity, which they say too; 0 says
	// none, which the allocator takes for allocapi.DefaultCapacity.
	Member, Advertise string
	Capacity          float64
	// StaticTargetsPath is the exporter's --web.static-targets-path, which
	// the heartbeats say, for the allocator's page to link.
	StaticTargetsPath string
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
	// HeartbeatInterval is the interval heartbeated at until an allocator
	// says its own, allocapi.DefaultHeartbeatInterval by default. The
	// exporter has no setting of it: the allocator's is the one in force.
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
	// EventHeartbeat is a heartbeat sent to one instance of the allocator, or a
	// lookup of the instances' addresses that failed; Err is the failure.
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
// self-metrics. Generation is the bundle the client last saw put in force,
// which it learns once the reload has returned: the self-metric that names
// the generation in force reads it from the manager instead
// (config.Manager.Generation), stored with the configuration.
type Status struct {
	Generation string
	// InForceAt is when Generation was put in force: when the reload that
	// put it in force returned, or, for the bundle of the cache directory
	// the exporter started with, when the client was made.
	InForceAt  time.Time
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
	// wake asks the bundle loop to stop waiting for Retry-After or after an
	// answer without a change, and beat the heartbeat loop to heartbeat now.
	wake, beat chan struct{}
	// instance names this process in its heartbeats, and sequence is the
	// last heartbeat's number (allocapi.Heartbeat): a heartbeat given up
	// on may still reach the allocator after the next, which then knows
	// it for an old one.
	instance string
	sequence uint64
	// newTimer starts the wait between two bundle requests; a test makes
	// it a timer of its own.
	newTimer func(time.Duration) (<-chan time.Time, func() bool)
	// now is the time what an instance said is stamped and aged by
	// (takeAnnounced); a test gives a clock of its own.
	now func() time.Time

	mu sync.Mutex
	// inForce is the generation of the bundle in force, seen that of the
	// last bundle fetched, put in force or refused: the bundle request
	// says it, so a refused bundle is not fetched again until the
	// allocator has another. inForce is set once the reload that put the
	// bundle in force has returned, so a heartbeat built while it runs
	// still names the bundle before, for as long as the reload's end
	// takes; the next heartbeat, sent at once, names the new one.
	inForce, seen string
	// inForceAt is when inForce was put in force (Status.InForceAt).
	inForceAt time.Time
	// interval is the heartbeat interval in force: Options' at the start,
	// then the shortest of those announced holds, while one does.
	// intervalChanged is closed, and made anew, when it changes, so each
	// address's schedule takes it at once.
	interval        time.Duration
	intervalChanged chan struct{}
	// reported is the interval the heartbeats say (allocapi.Heartbeat's
	// IntervalSeconds): the longest of those the instances said (says),
	// where interval is the shortest, and 0 until an instance of the
	// allocator has announced one, a heartbeat leaving it out until then.
	// Options' default is no instance's interval, and would tell an
	// instance that has just started nothing of how long the replicas take
	// to reach it. The longest, not the interval in force, is what such an
	// instance needs: while an instance before it still announces a longer
	// interval, the replicas it has not reached yet may still be on that
	// one, and find the new instance only at their next lookup on it.
	// highest is the longest reported has been, and earlyBeat when a change
	// of it last heartbeated every instance at once (takeAnnounced).
	reported, highest time.Duration
	earlyBeat         time.Time
	// says is what each instance of the allocator said in its last answer,
	// keyed by the instance's name (allocapi.HeartbeatReply's Instance), or
	// by the address it answered at when its answer names none: one address
	// may be answered by several instances in turn.
	says map[string]*said
	// sending holds, for each allocator address, the slot of the heartbeat
	// on its way there, one at a time: an instance of the allocator keeps the
	// last heartbeat it hears, so the one ShuttingDown sends must not be
	// overtaken by one built before it. A slot is kept once made, for an
	// address that comes back; there are as many as addresses ever seen.
	sending map[string]chan struct{}
	// known are the addresses the allocator's host resolved to last, which
	// a failed lookup keeps heartbeating.
	known        []string
	refused      *allocapi.Refusal
	shuttingDown bool
	lastSync     time.Time
	syncErrors   map[string]uint64
	refusedTotal uint64
	// lastLogged is the last error logged for each stage and allocator
	// address, so an error repeated at every try is logged once, until it
	// changes or ends: with several addresses failing, each has its own.
	lastLogged map[logKey]string
}

// said is what an instance of the allocator said in its last answer: the
// interval, 0 for none, the address the answer came from, and when. answered
// is false once a heartbeat to that address failed after it: the instance
// then has no say in the interval in force until it answers again, as
// whichever instance the address leads to did not answer. It keeps its say
// in the interval reported, the longest, while its address resolves and
// it is not older than the member TTL of the longest interval said: an
// instance that is slow or restarting still has replicas on the interval it
// said, and one behind an address that several instances answer is not
// heard at every heartbeat.
type said struct {
	interval time.Duration
	address  string
	at       time.Time
	answered bool
}

// logKey is what an error is logged once for: a stage, and for a heartbeat
// the allocator address it went to, empty for the lookup of the addresses.
type logKey struct{ stage, address string }

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
		o.HeartbeatInterval = allocapi.DefaultHeartbeatInterval
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
	c := &Client{
		o: o, base: base, client: &http.Client{Transport: o.Transport},
		wake: make(chan struct{}, 1), beat: make(chan struct{}, 1),
		instance: rand.Text(),
		newTimer: func(d time.Duration) (<-chan time.Time, func() bool) {
			t := time.NewTimer(d)
			return t.C, t.Stop
		},
		now:     time.Now,
		inForce: o.Generation, seen: o.Generation, interval: o.HeartbeatInterval, intervalChanged: make(chan struct{}),
		says: map[string]*said{}, sending: map[string]chan struct{}{},
		syncErrors: map[string]uint64{StageHeartbeat: 0, StageFetch: 0, StageWrite: 0},
		lastLogged: map[logKey]string{},
	}
	if o.Generation != "" {
		c.inForceAt = c.now()
	}
	return c, nil
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
// A heartbeat already on its way, built before and so not saying it, is
// waited for first: the allocator keeps the last heartbeat it hears, and
// that one arriving after would put the replica back. When ctx ends before
// it is back, the heartbeat loop sends the one saying so as soon as it is.
func (c *Client) ShuttingDown(ctx context.Context) {
	c.mu.Lock()
	c.shuttingDown = true
	c.mu.Unlock()
	if !c.heartbeat(ctx) {
		signal(c.beat)
	}
}

// Status returns how the client is keeping up with the allocator.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	errs := make(map[string]uint64, len(c.syncErrors))
	for stage, n := range c.syncErrors {
		errs[stage] = n
	}
	return Status{Generation: c.inForce, InForceAt: c.inForceAt, LastSync: c.lastSync, SyncErrors: errs, Refused: c.refusedTotal}
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
// logged at that stage, for a heartbeat to the same allocator address, said
// the same.
func (c *Client) failed(stage, address string, err error) {
	key := logKey{stage, address}
	c.mu.Lock()
	c.syncErrors[stage]++
	repeated := c.lastLogged[key] == err.Error()
	c.lastLogged[key] = err.Error()
	c.mu.Unlock()
	if !repeated {
		c.o.Logger.Warn("sync with the target allocator failed; trying again", c.logAttrs(key, "error", err.Error())...)
	}
}

// recovered logs that stage, for a heartbeat to address, works again after a
// failure was logged.
func (c *Client) recovered(stage, address string) {
	key := logKey{stage, address}
	c.mu.Lock()
	was := c.lastLogged[key]
	delete(c.lastLogged, key)
	c.mu.Unlock()
	if was != "" {
		c.o.Logger.Info("sync with the target allocator works again", c.logAttrs(key)...)
	}
}

// logAttrs are the attributes of a failure's log line, and more after them.
func (c *Client) logAttrs(key logKey, more ...any) []any {
	attrs := []any{"stage", key.stage, "allocator", c.base.String()}
	if key.address != "" {
		attrs = append(attrs, "address", key.address)
	}
	return append(attrs, more...)
}

// forgetAddresses drops what was logged for heartbeats to addresses the
// allocator's host no longer resolves to, and the intervals they said: a
// instance of the allocator that is gone neither recovers nor fails again, and
// has no say in the interval.
func (c *Client) forgetAddresses(addrs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.lastLogged {
		if key.stage == StageHeartbeat && key.address != "" && !slices.Contains(addrs, key.address) {
			delete(c.lastLogged, key)
		}
	}
	for key, s := range c.says {
		if !slices.Contains(addrs, s.address) {
			delete(c.says, key)
		}
	}
	c.takeAnnounced()
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

// heartbeatLoop looks up the allocator's addresses at once, then every
// heartbeat interval, counted from when the lookup before ended, and at once
// after a bundle was put in force or refused, and runs a heartbeat schedule
// of its own for each address (heartbeatAddress) until ctx ends. An instance
// of the allocator that is slow to answer, or hangs, so delays only the
// heartbeats to itself: every other instance goes on hearing the replica each
// interval, where one round waiting for every instance would let a hung one
// make the replica late to all of them. An address the host no longer
// resolves to has its schedule stopped; a lookup that fails keeps those of
// the addresses it resolved to last.
//
// An interval that changes takes effect at once here too, as it does for
// the heartbeats: the next lookup follows the new interval after the last,
// at once when that is past. A replica told a shorter interval looks up,
// and so finds an instance of the allocator that has just started, within
// it; an instance that has just started waits for the longest interval the
// replicas it hears say (the allocator's firstSettle), which covers those
// still on an older, longer one (reported).
func (c *Client) heartbeatLoop(ctx context.Context) {
	type schedule struct {
		now  chan struct{}
		stop context.CancelFunc
	}
	schedules := map[string]*schedule{}
	var wg sync.WaitGroup
	defer wg.Wait()
	// last is when the lookup before ended, the zero time before the first,
	// so the first is made at once.
	var last time.Time
	for {
		beat := false
		for waiting := true; waiting; {
			c.mu.Lock()
			next, changed := last.Add(c.interval), c.intervalChanged
			c.mu.Unlock()
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				waiting = false
			case <-c.beat:
				timer.Stop()
				beat, waiting = true, false
			case <-changed:
				timer.Stop()
			}
		}
		addrs := c.lookup(ctx)
		last = time.Now()
		for addr, s := range schedules {
			if !slices.Contains(addrs, addr) {
				s.stop()
				delete(schedules, addr)
			}
		}
		for _, addr := range addrs {
			if s := schedules[addr]; s != nil {
				if beat {
					signal(s.now)
				}
				continue
			}
			sctx, stop := context.WithCancel(ctx)
			s := &schedule{now: make(chan struct{}, 1), stop: stop}
			schedules[addr] = s
			wg.Go(func() { c.heartbeatAddress(sctx, addr, s.now) })
		}
	}
}

// heartbeatAddress heartbeats the allocator instance at addr at once, then
// every heartbeat interval, counted from when the heartbeat before was sent,
// and at once on now, until ctx ends. An interval that changes takes effect
// at once: the next heartbeat follows the new interval after the last.
func (c *Client) heartbeatAddress(ctx context.Context, addr string, now <-chan struct{}) {
	for {
		sent := time.Now()
		if !c.heartbeatOne(ctx, addr) {
			return
		}
		for waiting := true; waiting; {
			c.mu.Lock()
			next, changed := sent.Add(c.interval), c.intervalChanged
			c.mu.Unlock()
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				waiting = false
			case <-now:
				timer.Stop()
				waiting = false
			case <-changed:
				timer.Stop()
			}
		}
	}
}

// heartbeatInterval is the heartbeat interval in force.
func (c *Client) heartbeatInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interval
}

// heartbeatBound bounds a heartbeat to one instance, and a lookup of the
// instances' addresses: heartbeatTimeout, or the heartbeat interval when that
// is shorter, so one that hangs is given up on by the time the next is due.
func (c *Client) heartbeatBound() time.Duration {
	return min(heartbeatTimeout, c.heartbeatInterval())
}

// heartbeat sends one heartbeat to every address the allocator URL's host
// resolves to, at once, and waits for them: ShuttingDown's, and a test's.
// sent is false when there was no address to send to, or ctx ended before
// the turn of one of them came (heartbeatOne).
func (c *Client) heartbeat(ctx context.Context) (sent bool) {
	addrs := c.lookup(ctx)
	results := make([]bool, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Go(func() { results[i] = c.heartbeatOne(ctx, addr) })
	}
	wg.Wait()
	return len(addrs) > 0 && !slices.Contains(results, false)
}

// heartbeatOne sends a heartbeat to the allocator instance at addr, built
// once the one before to addr is back, and takes its reply. A reply naming a
// generation other than the last the bundle loop has seen, by the time the
// reply is in, wakes the bundle loop from its wait. The interval the reply
// says, kept within allocapi's bounds, or none, becomes the say of the
// instance that answered (said) in the interval in force, which is the
// shortest any instance whose last heartbeat was answered says
// (takeAnnounced): every instance then hears the replica within its own
// interval, which a longer one would not ensure. The instances last heard at
// an address that fails lose their say, and when no instance says one the
// interval stays as it is, so an instance that cannot be reached for a while
// does not take back what the others said. Their say in the interval the
// heartbeats say, the longest, is kept until the address no longer resolves
// or the member TTL of the longest interval has passed. sent is false when
// ctx ended before this heartbeat's turn came, and nothing was sent.
func (c *Client) heartbeatOne(ctx context.Context, addr string) (sent bool) {
	c.mu.Lock()
	slot := c.sending[addr]
	if slot == nil {
		slot = make(chan struct{}, 1)
		c.sending[addr] = slot
	}
	c.mu.Unlock()
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-slot }()
	c.mu.Lock()
	c.sequence++
	hb := allocapi.Heartbeat{Member: c.o.Member, Advertise: c.o.Advertise, Instance: c.instance, Sequence: c.sequence,
		ShuttingDown: c.shuttingDown, Generation: c.inForce, Refused: c.refused, IntervalSeconds: c.reported.Seconds(), Capacity: c.o.Capacity, StaticTargetsPath: c.o.StaticTargetsPath}
	c.mu.Unlock()
	var reply allocapi.HeartbeatReply
	body, err := json.Marshal(hb)
	if err == nil {
		reply, err = c.heartbeatTo(ctx, addr, body, c.heartbeatBound())
	}
	if err != nil {
		c.failed(StageHeartbeat, addr, err)
	} else {
		c.recovered(StageHeartbeat, addr)
	}
	// What the bundle loop has seen is read now, not when the heartbeat
	// went out: a bundle it fetched while the reply was on its way is no
	// news, and waking it for one would ask again without waiting.
	c.mu.Lock()
	seen, inForce := c.seen, c.inForce
	if err != nil {
		for _, s := range c.says {
			if s.address == addr {
				s.answered = false
			}
		}
	} else {
		key := reply.Instance
		if key == "" {
			key = "address " + addr
		}
		s := &said{address: addr, at: c.now(), answered: true}
		if reply.HeartbeatIntervalSeconds != 0 {
			s.interval = allocapi.ClampHeartbeatInterval(reply.HeartbeatIntervalSeconds)
		}
		c.says[key] = s
	}
	c.takeAnnounced()
	c.mu.Unlock()
	if generation := reply.Generation; err == nil && generation != "" && generation != seen && generation != inForce {
		signal(c.wake)
	}
	c.event(Event{Kind: EventHeartbeat, Err: err})
	return true
}

// takeAnnounced makes the shortest interval announced by the instances
// whose last heartbeat was answered the interval in force, and the longest
// any instance said the one the heartbeats say (reported), when any is,
// first dropping what an instance said that is older than the member TTL of
// the longest said. When the one they say grows, the replica's first being
// told one among those changes, every instance is heartbeated at once
// (beat), not at its next turn on the interval: the heartbeats a new replica
// sent before it heard any instance said none, and an instance that has just
// started would otherwise form its first member list of the replicas that
// came first before it hears from them how long the others may take. That is
// done for a value longer than any said before at once, and for one said
// before, which comes back once an instance that was not heard for a while
// is again, at most once an interval; a value that shrinks waits for the
// next turn, as it makes no instance wait longer. Called under mu.
func (c *Client) takeAnnounced() {
	now := c.now()
	var longest time.Duration
	for _, s := range c.says {
		longest = max(longest, s.interval)
	}
	window := allocapi.MemberTTL(max(longest, c.interval))
	longest = 0
	var shortest time.Duration
	for key, s := range c.says {
		if now.Sub(s.at) >= window {
			delete(c.says, key)
			continue
		}
		longest = max(longest, s.interval)
		if s.answered && s.interval != 0 && (shortest == 0 || s.interval < shortest) {
			shortest = s.interval
		}
	}
	if longest != 0 && longest != c.reported {
		grew := longest > c.reported
		c.reported = longest
		if grew && (longest > c.highest || now.Sub(c.earlyBeat) >= c.interval) {
			c.highest = max(c.highest, longest)
			c.earlyBeat = now
			signal(c.beat)
		}
	}
	if shortest == 0 || shortest == c.interval {
		return
	}
	c.o.Logger.Info("heartbeating at the interval the target allocator says", "interval", shortest.String(), "was", c.interval.String())
	c.interval = shortest
	close(c.intervalChanged)
	c.intervalChanged = make(chan struct{})
}

// lookup is the addresses the allocator URL's host resolves to, or the host
// itself when it is an IP address. A lookup is bounded as a heartbeat is
// (heartbeatBound); one that fails is counted and logged as a heartbeat
// failure, and gives the addresses resolved last, none before the first.
func (c *Client) lookup(ctx context.Context) []string {
	host := c.base.Hostname()
	if net.ParseIP(host) != nil {
		return []string{host}
	}
	ctx, cancel := context.WithTimeout(ctx, c.heartbeatBound())
	defer cancel()
	addrs, err := c.o.Resolve(ctx, host)
	if err == nil && len(addrs) == 0 {
		err = errors.New("no addresses")
	}
	if err != nil {
		err = fmt.Errorf("resolving the allocator's host %s: %w", host, err)
		c.failed(StageHeartbeat, "", err)
		c.event(Event{Kind: EventHeartbeat, Err: err})
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.known
	}
	c.recovered(StageHeartbeat, "")
	c.forgetAddresses(addrs)
	c.mu.Lock()
	c.known = addrs
	c.mu.Unlock()
	return addrs
}

// heartbeatTo sends the heartbeat body to the allocator at addr, with the
// URL's host as the Host header, within timeout, and returns its reply.
func (c *Client) heartbeatTo(ctx context.Context, addr string, body []byte, timeout time.Duration) (allocapi.HeartbeatReply, error) {
	var reply allocapi.HeartbeatReply
	port := c.base.Port()
	if port == "" {
		port = "80"
	}
	u := *c.base.JoinPath(allocapi.HeartbeatPath)
	u.Host = net.JoinHostPort(addr, port)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	req.Host = c.base.Host
	req.Header.Set("Content-Type", "application/json")
	if err := c.authorize(req); err != nil {
		return reply, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return reply, fmt.Errorf("heartbeat to %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return reply, fmt.Errorf("heartbeat to %s: %w", u.Host, err)
	}
	if resp.StatusCode/100 != 2 {
		return reply, fmt.Errorf("heartbeat to %s: the allocator answered %s: %s", u.Host, resp.Status, firstLine(raw))
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &reply); err != nil {
			return reply, fmt.Errorf("heartbeat to %s: the allocator's reply is not a heartbeat reply: %w", u.Host, err)
		}
	}
	return reply, nil
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
// asks as fast as an allocator can answer. A heartbeat reply naming a new
// generation cuts short the wait for Retry-After and the wait after an
// answer without a change, but not a backoff: a failure that persists, such
// as a cache directory that cannot be written, would otherwise fetch the
// whole bundle again at every heartbeat.
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
		wake := c.wake
		switch {
		case retryAfter > 0:
			backoff, wait = 0, min(max(retryAfter, c.o.MinBackoff), c.o.MaxBackoff)
		case !ok:
			backoff = min(max(backoff*2, c.o.MinBackoff), c.o.MaxBackoff)
			wait, wake = backoff, nil
		case unchanged && time.Since(asked) < c.o.MinBackoff:
			backoff, wait = 0, c.o.MinBackoff
		default:
			backoff = 0
			continue
		}
		fired, stop := c.newTimer(wait)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fired:
		case <-wake:
			stop()
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
		c.recovered(StageFetch, "")
		c.event(Event{Kind: EventNotSettled, Err: err})
		return true, true, notSettled.after
	case err != nil:
		c.failed(StageFetch, "", err)
		c.event(Event{Kind: EventError, Err: err})
		return false, false, 0
	}
	c.recovered(StageFetch, "")
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
		c.failed(StageWrite, "", err)
		c.event(Event{Kind: EventError, Generation: b.Generation, Err: err})
		return false, false, 0
	}
	c.recovered(StageWrite, "")
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
// to ask again. An instance of the allocator that has just started answers so
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
