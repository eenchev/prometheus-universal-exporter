package allocator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Instances of the allocator that discover run as peers (--peers): they find
// each other by a DNS name or a list, take the live instance of the lowest
// address for their leader, and only the leader asks the discovery sources.
// Every other instance, a follower, takes what the leader found from its
// snapshot (SnapshotPath) in place of asking the sources itself, so every
// instance expands the same input files with the same discovered targets, and
// serves the same bundles, of the same generations, and the same service
// discovery answers. See "Instances with discovery" in docs/ALLOCATOR.md.

// The paths instances ask each other on, under the API's token.
const (
	// PeerPath answers whether the instance is up, and which process it is.
	PeerPath = "/v1/peer"
	// SnapshotPath answers what the instance's discovery sources found.
	SnapshotPath = "/v1/peer/snapshot"
)

// The timings of the peers, PeerOptions' CheckInterval, CheckTimeout,
// SnapshotTimeout and FailureThreshold, from --peers.check-interval,
// --peers.check-timeout, --peers.snapshot-timeout and
// --peers.failure-threshold. Every check interval an instance resolves its
// peers, asks each at PeerPath within the check timeout and, as a follower,
// fetches the leader's snapshot within the snapshot timeout: a snapshot
// holds what every source found, which may be megabytes where a check is a
// few bytes, and a fetch cut short would leave the follower on its last
// snapshot for as long as the snapshot stays that large. The fetch runs
// beside the rounds, one at a time (snapshotFetch), so a leader that hangs
// while it writes its snapshot delays no check: the follower goes on
// checking every check interval, and leads, or follows another instance, as
// soon as the checks say so, which cancels the fetch. An instance that
// answered stays live until the failure threshold's checks in a row have
// failed, so one slow answer does not move the leader; one that answers it
// is shutting down stops being live at once. A follower whose snapshot
// fetches fail as many times in a row says so, once, and keeps serving the
// snapshot it fetched last.
const (
	DefaultPeerCheckInterval    = 2 * time.Second
	DefaultPeerCheckTimeout     = time.Second
	DefaultPeerSnapshotTimeout  = 30 * time.Second
	DefaultPeerFailureThreshold = 3
)

// The bounds of the peers' timings, which the command line holds them to.
// A check interval of at least 500ms keeps the rounds, each a lookup of
// every --peers name and a request to every peer, and a follower's
// snapshot requests from loading the instances and their DNS; one of at
// most 1m keeps a failover, the failure threshold's intervals, within
// minutes, and the shutdown delay, an interval and a check timeout, under
// 2m, which the chart's allocator pods' grace period covers. A check
// timeout of at least 100ms leaves a check across a cluster's network
// room to answer, and one below the interval keeps a round, which waits
// for its checks, shorter than the wait between two rounds. The failure
// threshold is from 1, where one failed check moves the leader, to 20,
// where a leader that stopped answering leads on for 20 intervals. The
// snapshot timeout is at least the check timeout, a snapshot being at
// least as large as a check's answer, and at most 5m, how long one fetch
// from a leader that hangs keeps a follower from fetching again.
const (
	MinPeerCheckInterval    = 500 * time.Millisecond
	MaxPeerCheckInterval    = time.Minute
	MinPeerCheckTimeout     = 100 * time.Millisecond
	MaxPeerFailureThreshold = 20
	MaxPeerSnapshotTimeout  = 5 * time.Minute
)

// ShutdownDelay is how long an instance among peers keeps serving after
// BeginShutdown, its PeerPath answering 503, before its server stops: a
// round of every peer and its check, the check interval and the check
// timeout, so each peer sees it is shutting down and the next instance
// leads at once.
func (o PeerOptions) ShutdownDelay() time.Duration {
	o = o.withDefaults()
	return o.CheckInterval + o.CheckTimeout
}

// withDefaults is o with each timing left zero at its default.
func (o PeerOptions) withDefaults() PeerOptions {
	if o.CheckInterval == 0 {
		o.CheckInterval = DefaultPeerCheckInterval
	}
	if o.CheckTimeout == 0 {
		o.CheckTimeout = DefaultPeerCheckTimeout
	}
	if o.SnapshotTimeout == 0 {
		o.SnapshotTimeout = DefaultPeerSnapshotTimeout
	}
	if o.FailureThreshold == 0 {
		o.FailureThreshold = DefaultPeerFailureThreshold
	}
	return o
}

// maxSnapshotBytes bounds a snapshot's body: what every source found, a
// source's own answer being bounded at maxDiscoveryAnswerBytes, so that
// bound for each of the instance's sources, and for one when it has none.
func (a *Allocator) maxSnapshotBytes() int {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	return max(len(a.sources), 1) * maxDiscoveryAnswerBytes
}

// PeerOptions are the settings of the peers.
type PeerOptions struct {
	// Peers are the instances, each a host:port: a name resolved to all its
	// addresses every CheckInterval, as the chart's headless Service's, or
	// an IP address. ParsePeers reads --peers into them.
	Peers []string
	// Self is this instance's address as its peers reach it. Left zero, the
	// instance finds itself among the addresses it resolved by asking them:
	// the one answering with its own instance is itself.
	Self netip.AddrPort
	// Token gives the bearer token sent to the peers, the instances sharing
	// the API's; nil sends none.
	Token func() (string, error)
	// Resolve looks a peer's host name up; net.DefaultResolver.LookupHost
	// when nil.
	Resolve func(ctx context.Context, host string) ([]string, error)
	// LeaderOnly is --peers.leader-only: only the leader serves the whole of
	// /metrics and of the status; a follower serves the peers' part of each
	// alone, so the instances' allocation is in one instance's metrics, not
	// in every instance's.
	LeaderOnly bool
	// Client asks the peers; one of no proxy when nil. Each request is
	// bounded by CheckTimeout, or SnapshotTimeout for a snapshot, through
	// its context, so Client needs no timeout of its own, which would cut
	// a snapshot short.
	Client *http.Client
	// CheckInterval, CheckTimeout, SnapshotTimeout and FailureThreshold are
	// the peers' timings, --peers.check-interval, --peers.check-timeout,
	// --peers.snapshot-timeout and --peers.failure-threshold; each left
	// zero is its default (DefaultPeerCheckInterval, ...). The command line
	// holds them to their bounds (MinPeerCheckInterval, ...).
	CheckInterval    time.Duration
	CheckTimeout     time.Duration
	SnapshotTimeout  time.Duration
	FailureThreshold int
}

// ParsePeers reads --peers: host:port entries, comma-separated, each with
// a host and a decimal port from 1 to 65535.
func ParsePeers(raw string) ([]string, error) {
	var out []string
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if !validAddress(entry) {
			return nil, fmt.Errorf("--peers entry %q is not a host:port with a port from 1 to 65535, such as pue-allocator.monitoring.svc:9116", entry)
		}
		out = append(out, entry)
	}
	return out, nil
}

// ParseSelf reads --peers.self: an IP address, taking the port of
// listenAddress, or an IP address and a port. A name is refused, since a
// instance is told from its peers by the addresses its peers' names resolve to.
func ParseSelf(raw, listenAddress string) (netip.AddrPort, error) {
	if addr, err := netip.ParseAddr(raw); err == nil {
		_, port, err := net.SplitHostPort(listenAddress)
		n, perr := strconv.ParseUint(port, 10, 16)
		if err != nil || perr != nil || n == 0 {
			return netip.AddrPort{}, fmt.Errorf("--peers.self %q has no port, and --web.listen-address %q has none to take; write the address as ip:port", raw, listenAddress)
		}
		return netip.AddrPortFrom(addr.Unmap(), uint16(n)), nil
	}
	ap, err := netip.ParseAddrPort(raw)
	if err != nil || ap.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("--peers.self %q is not an IP address, or an IP address and a port, such as 10.0.3.7 or 10.0.3.7:9116", raw)
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
}

// comparePeers orders the instances' addresses, the leader being the live
// instance of the lowest: IPv4 addresses before IPv6, each by its bytes, then
// by port. The order is total and every instance computes it alike from the
// same addresses, so instances that see the same live peers agree on their
// leader.
func comparePeers(a, b netip.AddrPort) int { return a.Compare(b) }

// livePeer is a live peer and its last answer at PeerPath.
type livePeer struct {
	addr   netip.AddrPort
	answer peerAnswer
}

// chooseLeader is the leader among this instance, of the address self, its
// process id, discovering while it does, and the live peers others, in
// order. leading says the leader is this instance.
//
// An instance that knows its address takes the lowest of its own and the
// others'. One that does not cannot place itself among them, as when
// --peers leaves its own address out or it cannot reach itself; were it
// to follow the lowest other, two such instances would follow each other
// and neither would discover. It follows only an instance that discovers,
// the lowest that knows its own address, which leads by the order alone,
// and else the one of the lowest id among those that do not, keeping on
// leading itself when its own id is lower still; with no instance
// discovering it leads. Two instances of unknown addresses so settle on
// one leader, the one of the lower id, even when both begin to lead in the
// same round.
func chooseLeader(self netip.AddrPort, id string, discovering bool, others []livePeer) (leader netip.AddrPort, leading bool) {
	if self.IsValid() {
		if len(others) == 0 || comparePeers(self, others[0].addr) < 0 {
			return self, true
		}
		return others[0].addr, false
	}
	var unranked *livePeer
	for i := range others {
		o := &others[i]
		switch {
		case !o.answer.Discovering:
		case o.answer.AddressKnown:
			return o.addr, false
		case unranked == nil || o.answer.ID < unranked.answer.ID:
			unranked = o
		}
	}
	if unranked == nil || (discovering && id < unranked.answer.ID) {
		return netip.AddrPort{}, true
	}
	return unranked.addr, false
}

// peerState is what an instance knows of one of its peers.
type peerState struct {
	// live says the peer answered and has not failed FailureThreshold
	// checks in a row since; fails counts the checks it failed in a row.
	live  bool
	fails int
	// self says the address is this instance's own.
	self bool
	// answer is the peer's last answer, and checked when it was last
	// checked, whatever the check's outcome; zero for this instance.
	answer  peerAnswer
	checked time.Time
}

// peers runs an instance's part among its peers.
type peers struct {
	a *Allocator
	o PeerOptions
	// resolved are the addresses each entry of o.Peers last resolved to,
	// and resolveErr the text of its last failure, logged once. Only the
	// round touches them.
	resolved   map[string][]netip.AddrPort
	resolveErr map[string]string
	// fetch is the snapshot fetch running beside the rounds, or the last
	// one, which has ended; nil before the first. Only the round touches
	// it.
	fetch *snapshotFetch

	mu    sync.Mutex
	known map[netip.AddrPort]*peerState
	// self is this instance's address, invalid until it is known. rounds
	// counts the rounds, and selfMissing says the address was still not
	// known after FailureThreshold of them, which is logged once.
	self        netip.AddrPort
	rounds      int
	selfMissing bool
	// decided says a leader has been chosen; leader is it, invalid when it
	// is this instance and its address is not known, and leading says it is
	// this instance.
	decided bool
	leader  netip.AddrPort
	leading bool
	// took says the instance has fetched a snapshot. snapshotAt is when it
	// last held what the leader's snapshot holds, after taking it or
	// finding it the same as its own; zero before the first time.
	// snapshotFails counts fetches that failed in a row, and stale says
	// that was logged; differ counts fetches in a row after which the instance
	// still held something else, and differLogged says that was logged.
	took          bool
	snapshotAt    time.Time
	snapshotFails int
	stale         bool
	differ        int
	differLogged  bool
	// What /metrics counts.
	checkFailures, snapshotFailures, leaderChanges uint64
}

// UsePeers makes the allocator an instance among peers: RunPeers then decides
// whether it asks its discovery sources or takes the leader's snapshot.
// Called before RunPeers and before the API is served; StartDiscovery is
// not called then.
func (a *Allocator) UsePeers(o PeerOptions) {
	if o.Resolve == nil {
		o.Resolve = net.DefaultResolver.LookupHost
	}
	if o.Client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		// The instances reach each other inside their network, never through
		// a proxy the environment names for the discovery sources.
		transport.Proxy = nil
		o.Client = &http.Client{Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Self.IsValid() {
		o.Self = netip.AddrPortFrom(o.Self.Addr().Unmap(), o.Self.Port())
	}
	o = o.withDefaults()
	a.peers = &peers{a: a, o: o, resolved: map[string][]netip.AddrPort{}, resolveErr: map[string]string{},
		known: map[netip.AddrPort]*peerState{}, self: o.Self}
}

// RunPeers runs the peers' rounds, the first at once and then every
// CheckInterval, until ctx ends or the allocator shuts down, which ends a
// snapshot fetch still running too.
func (a *Allocator) RunPeers(ctx context.Context) {
	defer a.peers.stopFetch()
	for {
		a.peers.round(ctx)
		timer := time.NewTimer(a.peers.o.CheckInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.stopping:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// newInstanceID names the allocator's process, so an instance that asks
// its own address knows it is itself.
func newInstanceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolve is the addresses of the peers: every address each entry resolves
// to, an entry whose lookup fails keeping those it resolved to last, and
// this instance's own when it is known.
func (p *peers) resolve(ctx context.Context) []netip.AddrPort {
	seen := map[netip.AddrPort]bool{}
	if p.o.Self.IsValid() {
		seen[p.o.Self] = true
	}
	for _, entry := range p.o.Peers {
		host, portText, _ := net.SplitHostPort(entry)
		port, _ := strconv.ParseUint(portText, 10, 16)
		var addrs []netip.AddrPort
		if ip, err := netip.ParseAddr(host); err == nil {
			addrs = []netip.AddrPort{netip.AddrPortFrom(ip.Unmap(), uint16(port))}
		} else {
			lookupCtx, cancel := context.WithTimeout(ctx, p.o.CheckTimeout)
			hosts, err := p.o.Resolve(lookupCtx, host)
			cancel()
			if err != nil {
				if text := model.SameFailureText(err); text != p.resolveErr[entry] {
					p.resolveErr[entry] = text
					p.a.logger.Warn("the peers' name does not resolve; the addresses it resolved to last stay the peers", "peer", entry, "error", err)
				}
				addrs = p.resolved[entry]
			} else {
				if p.resolveErr[entry] != "" {
					p.a.logger.Info("the peers' name resolves again", "peer", entry)
					delete(p.resolveErr, entry)
				}
				for _, h := range hosts {
					if ip, err := netip.ParseAddr(h); err == nil {
						addrs = append(addrs, netip.AddrPortFrom(ip.Unmap(), uint16(port)))
					}
				}
				p.resolved[entry] = addrs
			}
		}
		for _, addr := range addrs {
			seen[addr] = true
		}
	}
	out := make([]netip.AddrPort, 0, len(seen))
	for addr := range seen {
		out = append(out, addr)
	}
	sort.Slice(out, func(i, j int) bool { return comparePeers(out[i], out[j]) < 0 })
	return out
}

// peerAnswer is the answer at PeerPath.
type peerAnswer struct {
	// ID is the instance's process's id (newInstanceID).
	ID string `json:"id"`
	// Discovering says the instance asks its discovery sources itself: it
	// leads, or runs without peers.
	Discovering bool `json:"discovering"`
	// AddressKnown says the instance knows its own address, and so leads by
	// the order of the addresses alone, or runs without peers and never
	// follows: an instance that does not know its own follows such an
	// instance first (chooseLeader).
	AddressKnown bool `json:"address_known"`
}

// errShuttingDown is a peer's answer that it is shutting down.
var errShuttingDown = errors.New("the instance is shutting down")

// get asks addr for path, with the token, If-None-Match etag when not "",
// within timeout, reading the body included.
func (p *peers) get(ctx context.Context, addr netip.AddrPort, path, etag string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr.String()+path, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if p.o.Token != nil {
		token, err := p.o.Token()
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", `"`+etag+`"`)
	}
	resp, err := p.o.Client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelBody ends a request's context once its body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// check asks addr whether it is up, and reports its answer.
func (p *peers) check(ctx context.Context, addr netip.AddrPort) (peerAnswer, error) {
	resp, err := p.get(ctx, addr, PeerPath, "", p.o.CheckTimeout)
	if err != nil {
		return peerAnswer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return peerAnswer{}, err
	}
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return peerAnswer{}, errShuttingDown
	case resp.StatusCode != http.StatusOK:
		return peerAnswer{}, fmt.Errorf("%s answered %s: %s", PeerPath, resp.Status, firstLine(body))
	}
	var answer peerAnswer
	if err := json.Unmarshal(body, &answer); err != nil || answer.ID == "" {
		return peerAnswer{}, fmt.Errorf("%s answered no instance ID", PeerPath)
	}
	return answer, nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}

// round resolves the peers, checks each, chooses the leader and, as a
// follower, takes the leader's snapshot; as the leader it asks its
// discovery sources.
func (p *peers) round(ctx context.Context) {
	addrs := p.resolve(ctx)
	p.mu.Lock()
	self := p.self
	p.mu.Unlock()

	type result struct {
		answer peerAnswer
		err    error
	}
	results := make([]result, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		if addr == self {
			continue
		}
		wg.Go(func() {
			answer, err := p.check(ctx, addr)
			results[i] = result{answer, err}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	p.a.dmu.Lock()
	discovering := p.a.discovering
	p.a.dmu.Unlock()

	now := p.a.clock.Now()
	p.mu.Lock()
	known := make(map[netip.AddrPort]*peerState, len(addrs))
	for i, addr := range addrs {
		st := p.known[addr]
		if st == nil {
			st = &peerState{}
		}
		known[addr] = st
		r := results[i]
		if addr != self {
			st.checked = now
		}
		switch {
		case addr == self || r.answer.ID == p.a.id:
			st.self, st.live, st.fails = true, false, 0
			if !p.self.IsValid() {
				p.self, self = addr, addr
			}
		case r.err == nil:
			if !st.live && st.fails >= p.o.FailureThreshold {
				p.a.logger.Info("an allocator instance answers again", "peer", addr.String())
			}
			st.live, st.fails, st.answer = true, 0, r.answer
		default:
			st.fails++
			p.checkFailures++
			if errors.Is(r.err, errShuttingDown) {
				st.fails = max(st.fails, p.o.FailureThreshold)
			}
			if st.live && st.fails >= p.o.FailureThreshold {
				st.live = false
				p.a.logger.Warn("an allocator instance stopped answering; it is no longer among the live instances", "peer", addr.String(), "error", r.err)
			}
		}
	}
	p.known = known
	p.rounds++
	if !self.IsValid() && p.rounds >= p.o.FailureThreshold && !p.selfMissing {
		p.selfMissing = true
		p.a.logger.Error("no address of --peers answers as this instance, so it cannot place itself among its peers: list every instance in --peers, this one included, or give --peers.self; meanwhile it follows only an instance that discovers", "peers", strings.Join(p.o.Peers, ","))
	}

	var others []livePeer
	for _, addr := range addrs {
		if st := known[addr]; st.live && !st.self {
			others = append(others, livePeer{addr, st.answer})
		}
	}
	leader, leading := chooseLeader(self, p.a.id, discovering, others)
	// Leading, an instance whose own address became known has not changed
	// leader.
	changed := !p.decided || leading != p.leading || (!leading && leader != p.leader)
	if changed && p.decided {
		p.leaderChanges++
	}
	wasLeading, hadSnapshot := p.decided && p.leading, p.took
	p.decided, p.leader, p.leading = true, leader, leading
	p.mu.Unlock()

	if changed {
		if leading {
			p.a.logger.Info("this allocator instance leads: it asks the discovery sources, and the other instances take what it finds", "self", addrText(self), "live_peers", len(others))
		} else {
			p.a.logger.Info("this allocator instance follows: it takes the discovered targets of the leader in place of asking the discovery sources", "leader", leader.String(), "self", addrText(self))
		}
	}
	if leading {
		if !wasLeading && !hadSnapshot && len(others) > 0 {
			// An instance that leads before it ever followed, as a new
			// instance of the lowest address does, starts from what another
			// instance has, so its discovered targets do not leave the
			// bundles until its sources first answer; it asks them once the
			// fetch ends, whether it took the snapshot or not.
			p.stopFetch()
			p.startFetch(ctx, others[0].addr, true)
			return
		}
		if f := p.fetch; f != nil && f.seed && f.running() {
			return
		}
		p.stopFetch()
		p.a.setDiscovering(true)
		return
	}
	if f := p.fetch; f != nil && !f.seed && f.addr == leader && f.running() {
		// One fetch at a time: the leader's snapshot is still on its way.
		p.a.setDiscovering(false)
		return
	}
	p.stopFetch()
	p.a.setDiscovering(false)
	p.startFetch(ctx, leader, false)
}

// snapshotFetch is a fetch of a peer's snapshot, which runs beside the
// rounds so that a peer slow to write it, or hanging, delays no check.
// seed says it is the fetch of an instance coming to lead (round), which asks
// its sources once it ends.
type snapshotFetch struct {
	addr   netip.AddrPort
	seed   bool
	cancel context.CancelFunc
	done   chan struct{}
}

func (f *snapshotFetch) running() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}

// stopFetch cancels the fetch running, if one is, and waits for it to end,
// so what it fetched from an instance that no longer leads is never put in
// force after the round moves on. A fetch cancelled ends at once.
func (p *peers) stopFetch() {
	if f := p.fetch; f != nil {
		f.cancel()
		<-f.done
	}
}

// startFetch fetches addr's snapshot beside the rounds.
func (p *peers) startFetch(ctx context.Context, addr netip.AddrPort, seed bool) {
	ctx, cancel := context.WithCancel(ctx)
	f := &snapshotFetch{addr: addr, seed: seed, cancel: cancel, done: make(chan struct{})}
	p.fetch = f
	go func() {
		defer close(f.done)
		defer cancel()
		agreed, err := p.fetchSnapshot(ctx, addr)
		switch {
		case ctx.Err() != nil:
			// Cancelled: the instance no longer follows addr, or shuts down.
		case seed:
			// A failure leaves the new leader to its sources.
			p.mu.Lock()
			leading := p.leading
			p.mu.Unlock()
			if leading {
				p.a.setDiscovering(true)
			}
		case err == nil:
			p.fetched(addr, agreed)
		default:
			p.fetchFailed(addr, err)
		}
	}()
}

// fetchFailed notes a failed fetch of the leader addr's snapshot.
func (p *peers) fetchFailed(addr netip.AddrPort, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshotFails++
	p.snapshotFailures++
	if p.snapshotFails >= p.o.FailureThreshold && !p.stale {
		p.stale = true
		since := "never"
		if !p.snapshotAt.IsZero() {
			since = p.snapshotAt.UTC().Format(time.RFC3339)
		}
		p.a.logger.Warn("the leader's discovered targets cannot be fetched; this instance keeps serving those it took last", "leader", addr.String(), "last_snapshot", since, "error", err)
	}
}

func addrText(addr netip.AddrPort) string {
	if !addr.IsValid() {
		return "unknown"
	}
	return addr.String()
}

// peerSnapshot is the answer at SnapshotPath: every source of the instance's
// input files, by its identity (sourceSpec.identity), with what it found.
// The identity leaves the source's settings out, so instances whose input
// files differ only in a source's settings, as old and new pods during a
// rolling update do, still take each other's targets of it.
type peerSnapshot struct {
	ID          string                    `json:"id"`
	Discovering bool                      `json:"discovering"`
	Sources     map[string]snapshotSource `json:"sources"`
}

// snapshotSource is one source in a snapshot: whether it has answered,
// and what its last good answer found, as target groups (groupsOf): a
// group's labels are written once, as the source's answer has them, not
// once for each of its targets, which for a file_sd file of one group of
// thousands of targets, each with kilobytes of labels, would make a
// snapshot of a few hundred kilobytes one of tens of megabytes.
type snapshotSource struct {
	Answered bool          `json:"answered"`
	Groups   []TargetGroup `json:"groups,omitempty"`
	// RefreshIntervalSeconds is how often the source is asked
	// (sourceState.interval), which a follower shows for what it took of
	// it. It is part of the digest: a follower names the leader's interval
	// it took, so it is told of a change of it, as of the targets.
	RefreshIntervalSeconds float64 `json:"refresh_interval_seconds,omitempty"`
}

// fetchSnapshot takes addr's snapshot and puts what it found in force
// (takeSnapshot). The fetch names in
// If-None-Match the digest of what this instance holds, not of the snapshot it
// took last, so a 304 says the instance holds what the leader does: whatever
// made the two differ since, a reload that came to one first or the
// instance's own sources answering while it led, the next fetch is answered
// with the snapshot and corrects it. agreed says the instance holds what the
// snapshot holds once it is taken.
func (p *peers) fetchSnapshot(ctx context.Context, addr netip.AddrPort) (agreed bool, err error) {
	_, held := p.a.snapshotOf()
	resp, err := p.get(ctx, addr, SnapshotPath, held, p.o.SnapshotTimeout)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		// The leader holds what this instance does, so it has every source
		// this instance has: any this instance asked itself, the leader
		// lacking it, is the leader's to ask now.
		p.a.takeSnapshot(nil)
		return true, nil
	case http.StatusOK:
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("%s answered %s: %s", SnapshotPath, resp.Status, firstLine(body))
	}
	limit := p.a.maxSnapshotBytes()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return false, err
	}
	if len(raw) > limit {
		return false, fmt.Errorf("the snapshot is larger than %d bytes, %d for each of this instance's discovery sources", limit, maxDiscoveryAnswerBytes)
	}
	var s peerSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, fmt.Errorf("the snapshot is not one an allocator serves: %w", err)
	}
	p.a.takeSnapshot(s.Sources)
	_, held = p.a.snapshotOf()
	return held == etagValue(resp.Header.Get("ETag")), nil
}

// fetched notes a snapshot fetched from the leader addr, after which the
// instance holds what it holds when agreed. Only then is the snapshot's age
// reset: an instance whose input files have a source the leader's lack, or
// lack one the leader's have, as while a changed ConfigMap reaches one
// pod before the other, holds other targets, and its age grows, and is
// logged once after FailureThreshold fetches in a row, until the two agree.
func (p *peers) fetched(addr netip.AddrPort, agreed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stale {
		p.a.logger.Info("the leader's discovered targets are fetched again", "leader", addr.String())
	}
	p.took, p.snapshotFails, p.stale = true, 0, false
	if agreed {
		if p.differLogged {
			p.a.logger.Info("this instance holds the leader's discovered targets again", "leader", addr.String())
		}
		p.snapshotAt, p.differ, p.differLogged = p.a.clock.Now(), 0, false
		return
	}
	p.differ++
	if p.differ >= p.o.FailureThreshold && !p.differLogged {
		p.differLogged = true
		since := "never"
		if !p.snapshotAt.IsZero() {
			since = p.snapshotAt.UTC().Format(time.RFC3339)
		}
		p.a.logger.Warn("this instance's discovered targets differ from the leader's snapshot, which it fetches: their input files differ in their discovery sources", "leader", addr.String(), "last_agreed", since)
	}
}

// takeSnapshot puts what the leader's snapshot holds of each source in
// force in place of what the source found here, as an answer of the source
// would be. A source the leader has that has not answered there keeps what
// this instance holds, so a leader that has not yet heard from a source does
// not take its targets away. A source the snapshot does not have at all,
// one the leader's input files lack, as a new source does while a changed
// ConfigMap has not yet reached the leader's pod, is asked by this instance
// itself, from a refresh loop of its own, until the snapshot has it:
// taking nothing of it would leave its targets out of this instance's bundles
// meanwhile.
// A nil snap stands for a snapshot of what this instance holds.
func (a *Allocator) takeSnapshot(snap map[string]snapshotSource) {
	changed := false
	a.dmu.Lock()
	for _, s := range a.sources {
		attrs := []any{"kind", s.spec.kind, "owner", s.spec.owner, "source", s.spec.cfg.Name}
		got, has := snap[s.spec.identity()]
		if snap == nil {
			got, has = snapshotSource{}, true
		}
		if !has {
			if s.cancel == nil && !a.discoveryStopped {
				a.logger.Info("the leader's snapshot has no such discovery source; this instance asks it itself until the snapshot has it", attrs...)
				a.startSource(s)
			}
			continue
		}
		if s.cancel != nil && !a.discovering {
			a.logger.Info("the leader's snapshot has the discovery source this instance asked itself; it takes the leader's targets of it again", attrs...)
			s.cancel()
			s.cancel = nil
		}
		if !got.Answered {
			continue
		}
		// Checked and bounded as the source's own answer is, by this
		// instance's max_targets or --discovery.max-targets: a leader of other
		// settings, or no allocator at all, does not put more in force.
		found, err := flatten(got.Groups, nil, s.limit)
		if err == nil {
			err = checkGroups(got.Groups)
		}
		s.refreshed = a.clock.Now()
		if err != nil {
			a.sourceFailed(s, fmt.Errorf("the leader's snapshot of it is refused: %w", err))
			continue
		}
		a.sourceAnswered(s)
		s.fromSnapshot = true
		// One beyond what a duration holds, which no allocator says, is
		// none.
		s.leaderRefresh = 0
		if x := got.RefreshIntervalSeconds; x > 0 && x < 1e9 {
			s.leaderRefresh = time.Duration(x * float64(time.Second))
		}
		if s.ok && sameDiscovered(s.found, found) {
			continue
		}
		s.found, s.ok = found, true
		changed = true
	}
	a.dmu.Unlock()
	if changed {
		a.applyDiscovery()
	}
}

// snapshotOf is the allocator's snapshot and its digest, the ETag it is
// served with and the If-None-Match a follower names: instances that hold the
// same discovered targets have the same digest.
func (a *Allocator) snapshotOf() (peerSnapshot, string) {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	s := peerSnapshot{ID: a.id, Discovering: a.discovering, Sources: map[string]snapshotSource{}}
	digest := map[string]string{}
	for _, src := range a.sources {
		id := src.spec.identity()
		entry := snapshotSource{Answered: src.ok, RefreshIntervalSeconds: src.interval().Seconds()}
		if src.ok {
			entry.Groups = groupsOf(src.found)
		}
		s.Sources[id] = entry
		text, _ := json.Marshal(entry)
		digest[id] = string(text)
	}
	return s, allocapi.Generation(digest)
}

func (a *Allocator) servePeer(w http.ResponseWriter, _ *http.Request) {
	if !a.Ready() {
		// An instance shutting down is no candidate for leader any more: its
		// peers stop counting it at once rather than after FailureThreshold
		// checks.
		http.Error(w, "the allocator is shutting down", http.StatusServiceUnavailable)
		return
	}
	a.dmu.Lock()
	discovering := a.discovering
	a.dmu.Unlock()
	known := true
	if p := a.peers; p != nil {
		p.mu.Lock()
		known = p.self.IsValid()
		p.mu.Unlock()
	}
	writeJSON(w, peerAnswer{ID: a.id, Discovering: discovering, AddressKnown: known})
}

func (a *Allocator) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	s, etag := a.snapshotOf()
	w.Header().Set("ETag", `"`+etag+`"`)
	if etagValue(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, s)
}

// writePeerMetrics writes the metrics of the peers, when there are.
// Called under a.mu.
func (a *Allocator) writePeerMetrics(b *strings.Builder, family func(name, kind, help string)) {
	if st := a.peersStatusLocked(a.clock.Now()); st != nil {
		writePeerFamilies(b, family, st)
	}
}
