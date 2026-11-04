package allocator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

// Copies of the allocator that discover run as peers (--peers): they find
// each other by a DNS name or a list, take the live copy of the lowest
// address for their leader, and only the leader asks the discovery sources.
// Every other copy, a follower, takes what the leader found from its
// snapshot (SnapshotPath) in place of asking the sources itself, so every
// copy expands the same input files with the same discovered targets, and
// serves the same bundles, of the same generations, and the same service
// discovery answers. See "Copies with discovery" in docs/ALLOCATOR.md.

// The paths copies ask each other on, under the API's token.
const (
	// PeerPath answers whether the copy is up, and which process it is.
	PeerPath = "/v1/peer"
	// SnapshotPath answers what the copy's discovery sources found.
	SnapshotPath = "/v1/peer/snapshot"
)

// The timings of the peers. Every PeerInterval a copy resolves its peers,
// asks each at PeerPath within PeerTimeout and, as a follower, fetches the
// leader's snapshot within SnapshotTimeout: a snapshot holds what every
// source found, which may be megabytes where a check is a few bytes, and a
// fetch cut short would leave the follower on its last snapshot for as long
// as the snapshot stays that large. A copy that answered stays live until PeerFailures
// checks in a row have failed, so one slow answer does not move the
// leader; one that answers it is shutting down stops being live at once.
// A follower whose snapshot fetches fail PeerFailures times in a row says
// so, once, and keeps serving the snapshot it fetched last.
const (
	PeerInterval    = 2 * time.Second
	PeerTimeout     = time.Second
	SnapshotTimeout = 30 * time.Second
	PeerFailures    = 3
)

// maxSnapshotBytes bounds a snapshot's body: what every source found, a
// source's own answer being bounded at maxDiscoveryAnswerBytes, so that
// bound for each of the copy's sources, and for one when it has none.
func (a *Allocator) maxSnapshotBytes() int {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	return max(len(a.sources), 1) * maxDiscoveryAnswerBytes
}

// PeerOptions are the settings of the peers.
type PeerOptions struct {
	// Peers are the copies, each a host:port: a name resolved to all its
	// addresses every PeerInterval, as the chart's headless Service's, or
	// an IP address. ParsePeers reads --peers into them.
	Peers []string
	// Self is this copy's address as its peers reach it. Left zero, the
	// copy finds itself among the addresses it resolved by asking them:
	// the one answering with its own instance is itself.
	Self netip.AddrPort
	// Token gives the bearer token sent to the peers, the copies sharing
	// the API's; nil sends none.
	Token func() (string, error)
	// Resolve looks a peer's host name up; net.DefaultResolver.LookupHost
	// when nil.
	Resolve func(ctx context.Context, host string) ([]string, error)
	// Client asks the peers; one of no proxy when nil. Each request is
	// bounded by PeerTimeout, or SnapshotTimeout for a snapshot, through
	// its context, so Client needs no timeout of its own, which would cut
	// a snapshot short.
	Client *http.Client
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
// copy is told from its peers by the addresses its peers' names resolve to.
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

// comparePeers orders the copies' addresses, the leader being the live
// copy of the lowest: IPv4 addresses before IPv6, each by its bytes, then
// by port. The order is total and every copy computes it alike from the
// same addresses, so copies that see the same live peers agree on their
// leader.
func comparePeers(a, b netip.AddrPort) int { return a.Compare(b) }

// chooseLeader is the leader among this copy, of the address self, and
// the live peers others, in order: the lowest of them, this copy counting
// as the highest while its address is not known, so that it then leads
// only when no other copy is live. leading says the leader is this copy.
func chooseLeader(self netip.AddrPort, others []netip.AddrPort) (leader netip.AddrPort, leading bool) {
	if len(others) == 0 || (self.IsValid() && comparePeers(self, others[0]) < 0) {
		return self, true
	}
	return others[0], false
}

// peerState is what a copy knows of one of its peers.
type peerState struct {
	// live says the peer answered and has not failed PeerFailures checks in
	// a row since; fails counts the checks it failed in a row.
	live  bool
	fails int
	// self says the address is this copy's own.
	self bool
}

// peers runs a copy's part among its peers.
type peers struct {
	a *Allocator
	o PeerOptions
	// resolved are the addresses each entry of o.Peers last resolved to,
	// and resolveErr the text of its last failure, logged once. Only the
	// round touches them.
	resolved   map[string][]netip.AddrPort
	resolveErr map[string]string

	mu    sync.Mutex
	known map[netip.AddrPort]*peerState
	// self is this copy's address, invalid until it is known.
	self netip.AddrPort
	// decided says a leader has been chosen; leader is it, invalid when it
	// is this copy and its address is not known, and leading says it is
	// this copy.
	decided bool
	leader  netip.AddrPort
	leading bool
	// took says the copy has fetched a snapshot. snapshotAt is when it
	// last held what the leader's snapshot holds, after taking it or
	// finding it the same as its own; zero before the first time.
	// snapshotFails counts fetches that failed in a row, and stale says
	// that was logged; differ counts fetches in a row after which the copy
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

// UsePeers makes the allocator a copy among peers: RunPeers then decides
// whether it asks its discovery sources or takes the leader's snapshot.
// Called before RunPeers and before the API is served; StartDiscovery is
// not called then.
func (a *Allocator) UsePeers(o PeerOptions) {
	if o.Resolve == nil {
		o.Resolve = net.DefaultResolver.LookupHost
	}
	if o.Client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		// The copies reach each other inside their network, never through
		// a proxy the environment names for the discovery sources.
		transport.Proxy = nil
		o.Client = &http.Client{Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Self.IsValid() {
		o.Self = netip.AddrPortFrom(o.Self.Addr().Unmap(), o.Self.Port())
	}
	a.peers = &peers{a: a, o: o, resolved: map[string][]netip.AddrPort{}, resolveErr: map[string]string{},
		known: map[netip.AddrPort]*peerState{}, self: o.Self}
}

// RunPeers runs the peers' rounds, the first at once and then every
// PeerInterval, until ctx ends or the allocator shuts down.
func (a *Allocator) RunPeers(ctx context.Context) {
	for {
		a.peers.round(ctx)
		timer := time.NewTimer(PeerInterval)
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

// newInstance names the allocator's process, so a copy that asks its own
// address knows it is itself.
func newInstance() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolve is the addresses of the peers: every address each entry resolves
// to, an entry whose lookup fails keeping those it resolved to last, and
// this copy's own when it is known.
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
			lookupCtx, cancel := context.WithTimeout(ctx, PeerTimeout)
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
	// Instance names the copy's process.
	Instance string `json:"instance"`
	// Discovering says the copy asks its discovery sources itself: it
	// leads, or runs without peers.
	Discovering bool `json:"discovering"`
}

// errShuttingDown is a peer's answer that it is shutting down.
var errShuttingDown = errors.New("the copy is shutting down")

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

// check asks addr whether it is up, and reports the instance answering.
func (p *peers) check(ctx context.Context, addr netip.AddrPort) (string, error) {
	resp, err := p.get(ctx, addr, PeerPath, "", PeerTimeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return "", errShuttingDown
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%s answered %s: %s", PeerPath, resp.Status, firstLine(body))
	}
	var answer peerAnswer
	if err := json.Unmarshal(body, &answer); err != nil || answer.Instance == "" {
		return "", fmt.Errorf("%s answered no allocator's instance", PeerPath)
	}
	return answer.Instance, nil
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
		instance string
		err      error
	}
	results := make([]result, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		if addr == self {
			continue
		}
		wg.Go(func() {
			instance, err := p.check(ctx, addr)
			results[i] = result{instance, err}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	p.mu.Lock()
	known := make(map[netip.AddrPort]*peerState, len(addrs))
	for i, addr := range addrs {
		st := p.known[addr]
		if st == nil {
			st = &peerState{}
		}
		known[addr] = st
		r := results[i]
		switch {
		case addr == self || r.instance == p.a.instance:
			st.self, st.live, st.fails = true, false, 0
			if !p.self.IsValid() {
				p.self, self = addr, addr
			}
		case r.err == nil:
			if !st.live && st.fails >= PeerFailures {
				p.a.logger.Info("an allocator copy answers again", "peer", addr.String())
			}
			st.live, st.fails = true, 0
		default:
			st.fails++
			p.checkFailures++
			if errors.Is(r.err, errShuttingDown) {
				st.fails = max(st.fails, PeerFailures)
			}
			if st.live && st.fails >= PeerFailures {
				st.live = false
				p.a.logger.Warn("an allocator copy stopped answering; it is no longer among the live copies", "peer", addr.String(), "error", r.err)
			}
		}
	}
	p.known = known

	var others []netip.AddrPort
	for _, addr := range addrs {
		if st := known[addr]; st.live && !st.self {
			others = append(others, addr)
		}
	}
	leader, leading := chooseLeader(self, others)
	// Leading, a copy whose own address became known has not changed
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
			p.a.logger.Info("this allocator copy leads: it asks the discovery sources, and the other copies take what it finds", "self", addrText(self), "live_peers", len(others))
		} else {
			p.a.logger.Info("this allocator copy follows: it takes the discovered targets of the leader in place of asking the discovery sources", "leader", leader.String(), "self", addrText(self))
		}
	}
	if leading {
		if !wasLeading && !hadSnapshot && len(others) > 0 {
			// A copy that leads before it ever followed, as a new copy of
			// the lowest address does, starts from what another copy has,
			// so its discovered targets do not leave the bundles until its
			// sources first answer. A failure leaves it to its sources.
			_, _ = p.fetchSnapshot(ctx, others[0])
		}
		p.a.setDiscovering(true)
		return
	}
	p.a.setDiscovering(false)
	agreed, err := p.fetchSnapshot(ctx, leader)
	if err == nil {
		p.fetched(leader, agreed)
	} else if ctx.Err() == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.snapshotFails++
		p.snapshotFailures++
		if p.snapshotFails >= PeerFailures && !p.stale {
			p.stale = true
			since := "never"
			if !p.snapshotAt.IsZero() {
				since = p.snapshotAt.UTC().Format(time.RFC3339)
			}
			p.a.logger.Warn("the leader's discovered targets cannot be fetched; this copy keeps serving those it took last", "leader", leader.String(), "last_snapshot", since, "error", err)
		}
	}
}

func addrText(addr netip.AddrPort) string {
	if !addr.IsValid() {
		return "unknown"
	}
	return addr.String()
}

// peerSnapshot is the answer at SnapshotPath: every source of the copy's
// input files, by its identity (sourceSpec.identity), with what it found.
// The identity leaves the source's settings out, so copies whose input
// files differ only in a source's settings, as old and new pods during a
// rolling update do, still take each other's targets of it.
type peerSnapshot struct {
	Instance    string                    `json:"instance"`
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
}

// fetchSnapshot takes addr's snapshot and puts what it found in force
// (takeSnapshot). The fetch names in
// If-None-Match the digest of what this copy holds, not of the snapshot it
// took last, so a 304 says the copy holds what the leader does: whatever
// made the two differ since, a reload that came to one first or the
// copy's own sources answering while it led, the next fetch is answered
// with the snapshot and corrects it. agreed says the copy holds what the
// snapshot holds once it is taken.
func (p *peers) fetchSnapshot(ctx context.Context, addr netip.AddrPort) (agreed bool, err error) {
	_, held := p.a.snapshotOf()
	resp, err := p.get(ctx, addr, SnapshotPath, held, SnapshotTimeout)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		// The leader holds what this copy does, so it has every source
		// this copy has: any this copy asked itself, the leader lacking it,
		// is the leader's to ask now.
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
		return false, fmt.Errorf("the snapshot is larger than %d bytes, %d for each of this copy's discovery sources", limit, maxDiscoveryAnswerBytes)
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
// copy holds what it holds when agreed. Only then is the snapshot's age
// reset: a copy whose input files have a source the leader's lack, or
// lack one the leader's have, as while a changed ConfigMap reaches one
// pod before the other, holds other targets, and its age grows, and is
// logged once after PeerFailures fetches in a row, until the two agree.
func (p *peers) fetched(addr netip.AddrPort, agreed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stale {
		p.a.logger.Info("the leader's discovered targets are fetched again", "leader", addr.String())
	}
	p.took, p.snapshotFails, p.stale = true, 0, false
	if agreed {
		if p.differLogged {
			p.a.logger.Info("this copy holds the leader's discovered targets again", "leader", addr.String())
		}
		p.snapshotAt, p.differ, p.differLogged = p.a.clock.Now(), 0, false
		return
	}
	p.differ++
	if p.differ >= PeerFailures && !p.differLogged {
		p.differLogged = true
		since := "never"
		if !p.snapshotAt.IsZero() {
			since = p.snapshotAt.UTC().Format(time.RFC3339)
		}
		p.a.logger.Warn("this copy's discovered targets differ from the leader's snapshot, which it fetches: their input files differ in their discovery sources", "leader", addr.String(), "last_agreed", since)
	}
}

// takeSnapshot puts what the leader's snapshot holds of each source in
// force in place of what the source found here, as an answer of the source
// would be. A source the leader has that has not answered there keeps what
// this copy holds, so a leader that has not yet heard from a source does
// not take its targets away. A source the snapshot does not have at all,
// one the leader's input files lack, as a new source does while a changed
// ConfigMap has not yet reached the leader's pod, is asked by this copy
// itself, from a refresh loop of its own, until the snapshot has it:
// taking nothing of it would leave its targets out of this copy's bundles
// meanwhile.
// A nil snap stands for a snapshot of what this copy holds.
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
				a.logger.Info("the leader's snapshot has no such discovery source; this copy asks it itself until the snapshot has it", attrs...)
				a.startSource(s)
			}
			continue
		}
		if s.cancel != nil && !a.discovering {
			a.logger.Info("the leader's snapshot has the discovery source this copy asked itself; it takes the leader's targets of it again", attrs...)
			s.cancel()
			s.cancel = nil
		}
		if !got.Answered {
			continue
		}
		found, _ := flatten(got.Groups, nil, math.MaxInt)
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
// served with and the If-None-Match a follower names: copies that hold the
// same discovered targets have the same digest.
func (a *Allocator) snapshotOf() (peerSnapshot, string) {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	s := peerSnapshot{Instance: a.instance, Discovering: a.discovering, Sources: map[string]snapshotSource{}}
	digest := map[string]string{}
	for _, src := range a.sources {
		id := src.spec.identity()
		entry := snapshotSource{Answered: src.ok}
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
		// A copy shutting down is no candidate for leader any more: its
		// peers stop counting it at once rather than after PeerFailures
		// checks.
		http.Error(w, "the allocator is shutting down", http.StatusServiceUnavailable)
		return
	}
	a.dmu.Lock()
	discovering := a.discovering
	a.dmu.Unlock()
	writeJSON(w, peerAnswer{Instance: a.instance, Discovering: discovering})
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
	p := a.peers
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	live := 1
	for _, st := range p.known {
		if st.live && !st.self {
			live++
		}
	}
	family("pue_allocator_peers", "gauge", "Allocator copies live among the peers, this one included.")
	fmt.Fprintf(b, "pue_allocator_peers %d\n", live)
	family("pue_allocator_is_leader", "gauge", "1 while this allocator copy leads its peers and asks the discovery sources, 0 while it follows the leader.")
	family("pue_allocator_leader", "gauge", "1, with the address of the copy this one takes for the leader; self when it leads and its own address is not known.")
	isLeader := 0
	if p.decided && p.leading {
		isLeader = 1
	}
	fmt.Fprintf(b, "pue_allocator_is_leader %d\n", isLeader)
	if p.decided {
		leader := "self"
		if p.leader.IsValid() {
			leader = p.leader.String()
		}
		fmt.Fprintf(b, "pue_allocator_leader{peer=\"%s\"} 1\n", escape(leader))
	}
	family("pue_allocator_leader_changes_total", "counter", "Changes of the copy this one takes for the leader, after the first choice.")
	fmt.Fprintf(b, "pue_allocator_leader_changes_total %d\n", p.leaderChanges)
	family("pue_allocator_snapshot_age_seconds", "gauge", "Seconds since this copy last held the discovered targets of the leader's snapshot, having fetched it, or since its start before the first time; 0 while it leads.")
	age := 0.0
	if !p.decided || !p.leading {
		since := p.snapshotAt
		if since.IsZero() {
			since = a.started
		}
		age = max(a.clock.Now().Sub(since).Seconds(), 0)
	}
	fmt.Fprintf(b, "pue_allocator_snapshot_age_seconds %s\n", strconv.FormatFloat(age, 'f', -1, 64))
	family("pue_allocator_peer_failures_total", "counter", "Requests to the peers that failed, by request: check, a peer's liveness, or snapshot, a follower's fetch of the leader's discovered targets.")
	fmt.Fprintf(b, "pue_allocator_peer_failures_total{request=\"check\"} %d\npue_allocator_peer_failures_total{request=\"snapshot\"} %d\n", p.checkFailures, p.snapshotFailures)
}
