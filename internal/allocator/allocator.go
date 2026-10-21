// Package allocator is the target allocator, cmd/pue-allocator: it serves
// every exporter replica in allocator mode its configuration and its share
// of the static targets as one bundle, and Prometheus the probe targets
// already pointed at a replica, through HTTP service discovery.
//
// Replicas make themselves known with heartbeats (allocapi.Heartbeat). The
// members whose heartbeats keep coming form the member list, which takes
// effect once it has held for the settle time, and every target is
// allocated to one member of it by rendezvous hashing (allocapi.Owner). The
// allocator keeps nothing on disk: the same input files and the same
// heartbeats give the same bundles and the same answers, on a restarted
// allocator or on a second one beside it. See docs/ALLOCATOR.md.
package allocator

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// DefaultSettle is how long a changed member list has to hold before it
// takes effect: a replica restarting inside it comes back before anything
// moves, and keeps its share.
const DefaultSettle = 15 * time.Second

// DefaultMaxMembers is how many live members the allocator takes by default:
// a heartbeat may name any member, so without a bound each new name would
// add one, and with it a bundle to build and keep.
const DefaultMaxMembers = 1000

// Options are an Allocator's settings.
type Options struct {
	// Settle is how long a changed member list must hold before it takes
	// effect; 0 takes it at once.
	Settle time.Duration
	// MaxMembers is how many live members the allocator takes: a heartbeat
	// of a member it does not know is refused (ErrTooManyMembers) while
	// that many are live. 0 takes DefaultMaxMembers.
	MaxMembers int
	// Logger is where the allocator logs; slog's default when nil.
	Logger *slog.Logger
	// Clock is the allocator's time; the wall clock when nil. Tests give
	// one of their own.
	Clock Clock
	// MaxDiscoveredTargets bounds the targets of one discovery source's
	// answer, for a source that sets no max_targets; 0 takes
	// DefaultMaxDiscoveredTargets.
	MaxDiscoveredTargets int
}

// Allocator holds the inputs in force, the members, the member list that
// allocation uses and the bundles made from them.
type Allocator struct {
	mu     sync.Mutex
	clock  Clock
	logger *slog.Logger
	settle time.Duration
	// maxMembers bounds the live members; full says a heartbeat was
	// refused for it since the last new member was taken, so the refusals
	// are logged once while the allocator stays full.
	maxMembers int
	full       bool
	// started is when the allocator began to listen for heartbeats: the
	// first member list takes effect no sooner than a heartbeat interval
	// after it, so every replica has been heard from by then.
	started time.Time
	inputs  *Inputs
	members map[string]*member
	// proposed are the live members, sorted, and proposedAt when they last
	// changed; effective is the member list allocation uses, which becomes
	// proposed once proposed has held for the settle time. settled says it
	// has been formed at least once: before that the allocator knows
	// nothing to answer with.
	proposed   []string
	proposedAt time.Time
	effective  []string
	settled    bool
	// draining are the static targets a member has lost and keeps for one
	// of the target's intervals, until the time they map to.
	draining map[drainKey]time.Time
	// bundles are the bundles of the effective members, and joining the
	// bundle of any other member: the configuration with no static
	// targets.
	bundles map[string]*allocapi.Bundle
	joining *allocapi.Bundle
	// collectorsOf are the collectors of the configuration each recent
	// bundle generation carries, for service discovery to route a probe
	// only to a member whose bundle has its collector. Kept are the
	// generations of the bundles held and those live members report.
	collectorsOf map[string]map[string]bool
	// changed is closed, and replaced, whenever a bundle changes, which
	// wakes the long polls waiting for one.
	changed chan struct{}
	// timer is the next wake (wake), and stopping is closed by
	// BeginShutdown.
	timer    Timer
	stopping chan struct{}
	stopped  bool
	// refusals are the bundle generations each member last reported it
	// refused, so each refusal is logged once.
	refusals map[string]string
	// What /metrics counts.
	reloadsOK, reloadsFailed, memberChanges, membersRefused uint64
	// reloadMu serializes reloads; lastRefusal is the text the last
	// refused reload is recognised by (model.SameFailureText), so the
	// same refusal at every check is logged and counted once.
	reloadMu    sync.Mutex
	lastRefusal string
	// base are the input files in force, as read; inputs are base with
	// the discovered targets added (Inputs.expand). checked are the
	// discovered static targets last checked, and lastSkipped what the
	// last expansion left out, logged once while it stays the same. All
	// three change under reloadMu.
	base        *Inputs
	checked     map[string]checkedTarget
	lastSkipped string
	// dmu guards the discovery sources and what they found; it is taken
	// after mu or reloadMu, never before.
	dmu              sync.Mutex
	sources          map[string]*sourceState
	maxDiscovered    int
	discovering      bool
	discoveryStopped bool
}

type member struct {
	advertise    string
	lastSeen     time.Time
	generation   string
	shuttingDown bool
	// instance and sequence are those of the last heartbeat applied, and
	// retired the instance before it, so a late heartbeat is known.
	instance, retired string
	sequence          uint64
}

type drainKey struct{ target, member string }

// New returns an allocator serving in.
func New(in *Inputs, opts Options) *Allocator {
	a := &Allocator{
		clock:         opts.Clock,
		logger:        opts.Logger,
		settle:        opts.Settle,
		maxMembers:    opts.MaxMembers,
		inputs:        in,
		members:       map[string]*member{},
		draining:      map[drainKey]time.Time{},
		bundles:       map[string]*allocapi.Bundle{},
		collectorsOf:  map[string]map[string]bool{},
		changed:       make(chan struct{}),
		stopping:      make(chan struct{}),
		refusals:      map[string]string{},
		sources:       map[string]*sourceState{},
		base:          in,
		maxDiscovered: opts.MaxDiscoveredTargets,
	}
	if a.clock == nil {
		a.clock = realClock{}
	}
	if a.logger == nil {
		a.logger = slog.Default()
	}
	if a.maxMembers <= 0 {
		a.maxMembers = DefaultMaxMembers
	}
	if a.maxDiscovered <= 0 {
		a.maxDiscovered = DefaultMaxDiscoveredTargets
	}
	a.started = a.clock.Now()
	a.proposedAt = a.started
	if in != nil {
		a.syncSources(in)
		a.inputs, _, a.checked = in.expand(nil, nil)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rebuild()
	a.schedule()
	return a
}

// ErrBadHeartbeat is the answer to a heartbeat that names no member, no
// address to probe it at, no instance or no sequence.
var ErrBadHeartbeat = errors.New("a heartbeat names its member, the host:port Prometheus sends its probes to, its instance and its sequence")

// ErrTooManyMembers is the answer to a heartbeat of a new member while the
// allocator has as many live members as it takes (Options.MaxMembers).
var ErrTooManyMembers = errors.New("the allocator has as many live members as --members.max allows")

// Heartbeat records hb and answers with the generation of the bundle the
// member is to have, "" while the member list has not been formed. A
// heartbeat older than the last applied of its member (allocapi.Heartbeat's
// Instance and Sequence), or of a member shutting down that the allocator
// does not hold, is answered the same but changes nothing.
func (a *Allocator) Heartbeat(hb allocapi.Heartbeat) (allocapi.HeartbeatReply, error) {
	if err := checkHeartbeat(hb); err != nil {
		return allocapi.HeartbeatReply{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.members[hb.Member]
	if (m != nil && m.stale(hb)) || (m == nil && hb.ShuttingDown) {
		return a.replyLocked(hb.Member), nil
	}
	if m == nil {
		if n := a.liveCount(); n >= a.maxMembers {
			a.membersRefused++
			if !a.full {
				a.full = true
				a.logger.Warn("refusing the heartbeats of new members: the allocator has as many live members as --members.max allows; raise it, or find what sends heartbeats of new names", "member", hb.Member, "live_members", n, "max_members", a.maxMembers)
			}
			return allocapi.HeartbeatReply{}, fmt.Errorf("%w (%d); member %q is not taken until one of them stops heartbeating", ErrTooManyMembers, a.maxMembers, hb.Member)
		}
		a.full = false
		m = &member{}
		a.members[hb.Member] = m
	}
	if m.instance != hb.Instance {
		m.retired, m.instance = m.instance, hb.Instance
	}
	m.sequence = hb.Sequence
	m.advertise, m.lastSeen, m.generation, m.shuttingDown = hb.Advertise, a.clock.Now(), hb.Generation, hb.ShuttingDown
	switch {
	case hb.Refused == nil:
		// The replica keeps reporting a refusal until it puts a later
		// bundle in force, so one without is a refusal over: the next
		// one is news, even of the same generation.
		delete(a.refusals, hb.Member)
	case a.refusals[hb.Member] != hb.Refused.Generation:
		a.refusals[hb.Member] = hb.Refused.Generation
		a.logger.Warn("a replica refused its bundle and keeps its last good one; fix the configuration the allocator serves", "member", hb.Member, "generation", hb.Refused.Generation, "error", hb.Refused.Error)
	}
	a.update()
	return a.replyLocked(hb.Member), nil
}

// stale reports whether hb is older than the last heartbeat applied to m:
// of the same process and not counted past it, or of the process the
// current one replaced, which has ended.
func (m *member) stale(hb allocapi.Heartbeat) bool {
	if hb.Instance == m.instance {
		return hb.Sequence <= m.sequence
	}
	return hb.Instance == m.retired
}

// replyLocked is the answer to a heartbeat of member.
func (a *Allocator) replyLocked(member string) allocapi.HeartbeatReply {
	reply := allocapi.HeartbeatReply{}
	if b := a.bundleLocked(member); b != nil {
		reply.Generation = b.Generation
	}
	return reply
}

// Bundle is the bundle member is to have, and nil while the member list
// has not been formed: until then the allocator cannot tell a replica's
// share from nothing.
func (a *Allocator) Bundle(member string) *allocapi.Bundle {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bundleLocked(member)
}

func (a *Allocator) bundleLocked(member string) *allocapi.Bundle {
	if !a.settled {
		return nil
	}
	if b := a.bundles[member]; b != nil {
		return b
	}
	return a.joining
}

// changes is closed at the next change of any bundle.
func (a *Allocator) changes() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.changed
}

// Ready says the allocator serves: it has inputs, which it always has once
// made, and is not shutting down.
func (a *Allocator) Ready() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inputs != nil && !a.stopped
}

// BeginShutdown makes /ready answer 503 and ends every long poll, so the
// HTTP server's graceful shutdown waits for none of them.
func (a *Allocator) BeginShutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}
	a.stopped = true
	a.stopDiscovery()
	close(a.stopping)
	if a.timer != nil {
		a.timer.Stop()
	}
}

// liveCount is how many members are live now. A member whose heartbeats
// stopped no longer counts once its TTL has run out, so it frees its place
// for a new one.
func (a *Allocator) liveCount() int {
	now := a.clock.Now()
	n := 0
	for _, m := range a.members {
		if live(m, now) {
			n++
		}
	}
	return n
}

// live reports whether m counts as a member at now: heard from within the
// member TTL, and not shutting down.
func live(m *member, now time.Time) bool {
	return !m.shuttingDown && now.Sub(m.lastSeen) < allocapi.MemberTTL
}

// update brings the member list, the draining targets and the bundles up to
// the clock, and arranges the next wake. It is called under mu after
// every event: a heartbeat, a reload, a wake.
func (a *Allocator) update() {
	now := a.clock.Now()
	var liveMembers []string
	for name, m := range a.members {
		if live(m, now) {
			liveMembers = append(liveMembers, name)
		} else if !slices.Contains(a.effective, name) && now.Sub(m.lastSeen) >= allocapi.MemberTTL {
			// A member shutting down is kept for the member TTL, though
			// not live: a heartbeat it sent before, arriving late, is then
			// known for what it is rather than taken as a new member's.
			delete(a.members, name)
			delete(a.refusals, name)
		}
	}
	sort.Strings(liveMembers)
	if !slices.Equal(liveMembers, a.proposed) {
		a.proposed, a.proposedAt = liveMembers, now
	}
	rebuild := false
	if a.settleDue(now) {
		a.takeEffect(now)
		rebuild = true
	}
	for key, until := range a.draining {
		if !now.Before(until) {
			delete(a.draining, key)
			rebuild = true
		}
	}
	if rebuild {
		a.rebuild()
	}
	a.schedule()
}

// settleDue reports whether the proposed member list takes effect at now:
// it differs from the effective one, has held for the settle time, and, for
// the first one, is not empty and comes a heartbeat interval or more after
// the start.
func (a *Allocator) settleDue(now time.Time) bool {
	if a.settled && slices.Equal(a.proposed, a.effective) {
		return false
	}
	if !a.settled && (len(a.proposed) == 0 || now.Before(a.firstSettle())) {
		return false
	}
	return !now.Before(a.proposedAt.Add(a.settle))
}

// firstSettle is the earliest the first member list takes effect.
func (a *Allocator) firstSettle() time.Time {
	return a.started.Add(max(a.settle, allocapi.HeartbeatInterval))
}

// takeEffect makes the proposed member list the effective one. A static
// target that moves from a member still in the list keeps being that
// member's for one of its intervals, while its new owner already scrapes
// it, so its series has no gap.
func (a *Allocator) takeEffect(now time.Time) {
	before, after := a.effective, slices.Clone(a.proposed)
	a.effective = after
	if a.settled {
		a.memberChanges++
	}
	first := !a.settled
	a.settled = true
	if in := a.inputs; in != nil && in.Targets != nil && len(before) > 0 {
		for i := range in.Targets.Targets {
			t := &in.Targets.Targets[i]
			was, _ := allocapi.Owner(before, in.shardKeys[i])
			is, _ := allocapi.Owner(after, in.shardKeys[i])
			if was != is && slices.Contains(after, was) {
				a.draining[drainKey{t.Name, was}] = now.Add(time.Duration(t.Interval))
			}
		}
	}
	for key := range a.draining {
		if !slices.Contains(after, key.member) || a.ownerOf(key.target) == key.member {
			delete(a.draining, key)
		}
	}
	added, removed := difference(after, before), difference(before, after)
	if first {
		a.logger.Info("member list formed", "members", after)
		return
	}
	a.logger.Info("member list changed", "members", after, "added", added, "removed", removed, "draining", len(a.draining))
}

// handOver keeps, for one of its intervals, a static target the reload
// from old to the inputs in force moves to another member, in the share of
// the member that had it, as takeEffect does for a change of the member
// list: a changed shard_key moves a target as surely as a new member does.
// A target the new inputs no longer have drains nowhere, nor does one that
// is back with the member draining it.
func (a *Allocator) handOver(old *Inputs, now time.Time) {
	in := a.inputs
	if a.settled && old != nil && old.Targets != nil && in.Targets != nil && len(a.effective) > 0 {
		for i := range in.Targets.Targets {
			t := &in.Targets.Targets[i]
			j, ok := old.targetIndex[t.Name]
			if !ok {
				continue
			}
			was, _ := allocapi.Owner(a.effective, old.shardKeys[j])
			is, _ := allocapi.Owner(a.effective, in.shardKeys[i])
			if was != is {
				a.draining[drainKey{t.Name, was}] = now.Add(time.Duration(t.Interval))
			}
		}
	}
	for key := range a.draining {
		if owner := a.ownerOf(key.target); owner == "" || owner == key.member {
			delete(a.draining, key)
		}
	}
}

// ownerOf is the effective member that owns the static target named name,
// "" when there is none.
func (a *Allocator) ownerOf(name string) string {
	in := a.inputs
	if in == nil || in.Targets == nil {
		return ""
	}
	i, ok := in.targetIndex[name]
	if !ok {
		return ""
	}
	owner, _ := allocapi.Owner(a.effective, in.shardKeys[i])
	return owner
}

// difference is what of a is not in b.
func difference(a, b []string) []string {
	out := []string{}
	for _, name := range a {
		if !slices.Contains(b, name) {
			out = append(out, name)
		}
	}
	return out
}

// schedule arranges the next wake: when a live member's TTL runs out, the
// proposed member list's settle time does, or a draining target's interval.
func (a *Allocator) schedule() {
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	if a.stopped {
		return
	}
	now := a.clock.Now()
	var next time.Time
	consider := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, m := range a.members {
		if live(m, now) {
			consider(m.lastSeen.Add(allocapi.MemberTTL))
		}
	}
	if !a.settled || !slices.Equal(a.proposed, a.effective) {
		if at := a.proposedAt.Add(a.settle); a.settled || len(a.proposed) > 0 {
			if !a.settled && at.Before(a.firstSettle()) {
				at = a.firstSettle()
			}
			consider(at)
		}
	}
	for _, until := range a.draining {
		consider(until)
	}
	if next.IsZero() {
		return
	}
	a.timer = a.clock.AfterFunc(max(next.Sub(now), 0), a.wake)
}

// wake is the timer's call.
func (a *Allocator) wake() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.stopped {
		a.update()
	}
}

// rebuild makes every bundle again from the inputs, the effective member
// list and the draining targets, and wakes the long polls when one of them
// changed.
func (a *Allocator) rebuild() {
	in := a.inputs
	if in == nil {
		return
	}
	shares := map[string][]int{}
	if in.Targets != nil && len(a.effective) > 0 {
		for i := range in.Targets.Targets {
			owner, _ := allocapi.Owner(a.effective, in.shardKeys[i])
			shares[owner] = append(shares[owner], i)
		}
		for key := range a.draining {
			if i, ok := in.targetIndex[key.target]; ok {
				shares[key.member] = append(shares[key.member], i)
			}
		}
	}
	bundles := map[string]*allocapi.Bundle{}
	for _, name := range a.effective {
		share := shares[name]
		sort.Ints(share)
		share = slices.Compact(share)
		bundles[name] = a.bundleOf(share)
	}
	joining := a.bundleOf(nil)
	changed := a.joining == nil || a.joining.Generation != joining.Generation || len(bundles) != len(a.bundles)
	for name, b := range bundles {
		if old := a.bundles[name]; old == nil || old.Generation != b.Generation {
			changed = true
		}
	}
	a.bundles, a.joining = bundles, joining
	a.remember()
	if changed {
		close(a.changed)
		a.changed = make(chan struct{})
	}
}

// bundleOf is the bundle of the configuration and the static targets at
// share: without static-targets.yaml when share is empty, since a static
// target file must hold a target.
func (a *Allocator) bundleOf(share []int) *allocapi.Bundle {
	in := a.inputs
	files := make(map[string]string, len(in.configFiles)+1)
	for name, body := range in.configFiles {
		files[name] = body
	}
	if len(share) > 0 {
		text, err := in.static.render(share)
		if err != nil {
			// The nodes were read from YAML a moment ago; writing them
			// back cannot fail but for a bug, which leaves the share out
			// rather than serving half of it.
			a.logger.Error("writing a replica's share of the static targets failed", "error", err)
		} else {
			files[allocapi.StaticTargetsFile] = text
		}
	}
	b := &allocapi.Bundle{Files: files}
	b.Seal()
	return b
}

// remember notes the collectors of the bundles held, and forgets the
// generations neither held nor reported by a live member.
func (a *Allocator) remember() {
	keep := map[string]bool{a.joining.Generation: true}
	for _, b := range a.bundles {
		keep[b.Generation] = true
	}
	now := a.clock.Now()
	for _, m := range a.members {
		if live(m, now) && m.generation != "" {
			keep[m.generation] = true
		}
	}
	for generation := range keep {
		if _, known := a.collectorsOf[generation]; !known && a.isHeld(generation) {
			a.collectorsOf[generation] = a.inputs.collectors
		}
	}
	for generation := range a.collectorsOf {
		if !keep[generation] {
			delete(a.collectorsOf, generation)
		}
	}
}

func (a *Allocator) isHeld(generation string) bool {
	if a.joining.Generation == generation {
		return true
	}
	for _, b := range a.bundles {
		if b.Generation == generation {
			return true
		}
	}
	return false
}

// Reload reads the input files again and puts them in force when they
// changed and are accepted. Refused files leave the last good ones in
// force; a refusal is logged and counted once for as long as it stays the
// same. It reports whether the inputs in force changed.
func (a *Allocator) Reload(p Paths) bool {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if s, err := takeSnapshot(p); err == nil && a.lastRefusal == "" && a.base != nil && a.base.digest == s.digest {
		return false
	}
	in, err := LoadInputs(p)
	if errors.Is(err, ErrChanged) {
		a.logger.Debug(err.Error() + "; they are read again at the next check")
		return false
	}
	if err != nil {
		if text := model.SameFailureText(err); text != a.lastRefusal {
			a.lastRefusal = text
			a.mu.Lock()
			a.reloadsFailed++
			a.mu.Unlock()
			a.logger.Error("the allocator's input files were refused; the last good ones stay in force", "error", err)
		}
		return false
	}
	a.lastRefusal = ""
	if a.base != nil && a.base.digest == in.digest {
		return false
	}
	a.base = in
	// What was checked was checked against the files in force before.
	a.checked = nil
	a.syncSources(in)
	a.putInForce(in)
	a.mu.Lock()
	a.reloadsOK++
	a.mu.Unlock()
	a.logger.Info("input files reloaded", a.currentInputs().logAttrs()...)
	return true
}

// logAttrs describe the inputs in a log line.
func (in *Inputs) logAttrs() []any {
	static, probes := 0, 0
	if in.Targets != nil {
		static = len(in.Targets.Targets)
	}
	if in.Probes != nil {
		for i := range in.Probes.Jobs {
			probes += len(in.Probes.Jobs[i].Targets)
		}
	}
	return []any{"collectors", len(in.Config.Collectors), "collector_files", len(in.configFiles) - 1, "static_targets", static, "probe_targets", probes}
}

// LogAttrs describe the inputs in a log line, for the start's.
func (in *Inputs) LogAttrs() []any { return in.logAttrs() }

// WatchLoop reloads the inputs at every tick until done is closed.
func (a *Allocator) WatchLoop(done <-chan struct{}, ticks <-chan time.Time, p Paths) {
	for {
		select {
		case <-done:
			return
		case <-ticks:
			a.Reload(p)
		}
	}
}
