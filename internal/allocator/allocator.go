// Package allocator is the target allocator, cmd/pue-allocator: it serves
// every exporter replica in allocator mode its configuration and its share
// of the static targets as one bundle, and Prometheus the probe targets
// already pointed at a replica, through HTTP service discovery.
//
// Replicas make themselves known with heartbeats (allocapi.Heartbeat). The
// members whose heartbeats keep coming form the member list, which takes
// effect once it has held for the settle time, and every target is placed
// on one member of it by its weight and the members' capacities, by
// rendezvous hashing weighted by capacity with bounded loads (placement.go),
// which with every weight and capacity alike is allocapi.Owner's. The
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
	// HeartbeatInterval is how often the replicas are to heartbeat, which
	// every heartbeat reply says; a member not heard from for
	// allocapi.MemberTTL of it is dropped. 0 takes
	// allocapi.DefaultHeartbeatInterval; the command line keeps it within
	// allocapi's bounds.
	HeartbeatInterval time.Duration
	// MaxDiscoveredTargets bounds the targets of one discovery source's
	// answer, for a source that sets no max_targets; 0 takes
	// DefaultMaxDiscoveredTargets.
	MaxDiscoveredTargets int
	// Address is --web.listen-address, Version and Revision the build's,
	// and WatchInterval --config.watch-interval, 0 for files read once,
	// which the status shows (Status): the watch itself is the command's
	// (WatchLoop).
	Address, Version, Revision string
	WatchInterval              time.Duration
	// BalanceFactor is --allocation.balance-factor, how far above its fair
	// share a member's load may go (placement.go); 0 takes
	// DefaultBalanceFactor. The command line keeps it within
	// MinBalanceFactor and MaxBalanceFactor.
	BalanceFactor float64
}

// Allocator holds the inputs in force, the members, the member list that
// allocation uses and the bundles made from them.
type Allocator struct {
	mu     sync.Mutex
	clock  Clock
	logger *slog.Logger
	settle time.Duration
	// heartbeatInterval is Options.HeartbeatInterval, and memberTTL the
	// member TTL of it.
	heartbeatInterval, memberTTL time.Duration
	// longestHeard is the longest interval a heartbeat said
	// (allocapi.Heartbeat.IntervalSeconds), the longest the instances its
	// replica finds told it, before the first member list formed, kept within
	// allocapi's bounds; a heartbeat that says none is no say (firstSettle).
	longestHeard time.Duration
	// firstHeard is when the first heartbeat came, the zero time before:
	// the instance had been found by then (firstSettle).
	firstHeard time.Time
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
	// proposedCaps and effectiveCaps are the capacities of the members of
	// proposed and of effective, in their order, in thousandths: a
	// capacity is part of the member list, and a change of one takes
	// effect, as a member's coming or going does, once it has held for the
	// settle time.
	proposedCaps, effectiveCaps []int64
	// balanceFactor is Options.BalanceFactor in thousandths, and plan the
	// placement made last (planFor). heavy are the units heavier than a
	// fair share the last placement found, by key, logged once while they
	// stay so.
	balanceFactor int64
	plan          *placement
	heavy         map[string]bool
	// fallbacksTotal counts the discovered targets whose weight_label gave
	// no weight, as they begin to fall back; fallingBack are those of the
	// inputs in force, and fallbackLogged the sources and reasons logged,
	// once while any target of them falls back. The last two change under
	// reloadMu, the counts under mu.
	fallbacksTotal map[fallbackKey]uint64
	fallingBack    map[weightFallback]bool
	fallbackLogged map[string]bool
	// draining are the static targets a member has lost and keeps for one
	// of the target's intervals, until the time they map to.
	draining map[drainKey]drain
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
	// id names the allocator's process, for an instance to know itself
	// among its peers; peers are those, nil without --peers.
	id    string
	peers *peers
	// address, version, revision and watchInterval are Options'.
	address, version, revision string
	watchInterval              time.Duration
}

type member struct {
	// capacity is what its last heartbeat said, within allocapi's bounds,
	// in thousandths.
	capacity  int64
	advertise string
	// staticTargetsPath is where the replica serves its static targets,
	// as its last heartbeat said (allocapi.Heartbeat).
	staticTargetsPath string
	lastSeen          time.Time
	generation        string
	shuttingDown      bool
	// instance and sequence are those of the last heartbeat applied, and
	// retired the instance before it, so a late heartbeat is known.
	instance, retired string
	sequence          uint64
	// retiredAt is when the process before the current one was replaced,
	// and twiceAt when a heartbeat of it last came long enough after that
	// to be no late one (twice): two processes then heartbeat under the
	// one name. twiceLogged says so was logged, once while it lasts.
	retiredAt, twiceAt time.Time
	twiceLogged        bool
}

// twice reports whether two processes heartbeat as m: one of the process the
// current one replaced came within the last member TTL, and more than a
// member TTL after it was replaced, which no heartbeat that was merely late
// is. Each of the two has the member's bundle and scrapes its share.
func (a *Allocator) twice(m *member, now time.Time) bool {
	return !m.twiceAt.IsZero() && now.Sub(m.twiceAt) <= a.memberTTL
}

type drainKey struct{ target, member string }

// drain is how long a member keeps a static target it has lost, and
// whether a change of the discovered targets moved it. Those are the drains
// a follower takes from the leader's snapshot (takeDrains) rather than
// works out: the leader puts the answers of its sources in force one at a
// time, as they come, and a target one answer moves it drains, where a
// follower that takes several answers at once would not see the target
// move at all, or see it move from elsewhere.
type drain struct {
	until      time.Time
	discovered bool
	// own, on a drain of the leader's snapshot that this instance drained
	// for a reason of its own too, is when its own would end: the drain is
	// the leader's, which the snapshot and its digest list as the leader's
	// do, and its own takes over when the leader's leaves the snapshot.
	own time.Time
}

// New returns an allocator serving in.
func New(in *Inputs, opts Options) *Allocator {
	a := &Allocator{
		clock:             opts.Clock,
		logger:            opts.Logger,
		settle:            opts.Settle,
		heartbeatInterval: opts.HeartbeatInterval,
		maxMembers:        opts.MaxMembers,
		inputs:            in,
		members:           map[string]*member{},
		draining:          map[drainKey]drain{},
		bundles:           map[string]*allocapi.Bundle{},
		collectorsOf:      map[string]map[string]bool{},
		changed:           make(chan struct{}),
		stopping:          make(chan struct{}),
		refusals:          map[string]string{},
		sources:           map[string]*sourceState{},
		base:              in,
		maxDiscovered:     opts.MaxDiscoveredTargets,
		id:                newInstanceID(),
		address:           opts.Address,
		version:           opts.Version,
		revision:          opts.Revision,
		watchInterval:     opts.WatchInterval,
		balanceFactor:     factorMilli(opts.BalanceFactor),
		heavy:             map[string]bool{},
		fallbacksTotal:    map[fallbackKey]uint64{},
	}
	if a.clock == nil {
		a.clock = realClock{}
	}
	if a.logger == nil {
		a.logger = slog.Default()
	}
	if a.heartbeatInterval <= 0 {
		a.heartbeatInterval = allocapi.DefaultHeartbeatInterval
	}
	a.memberTTL = allocapi.MemberTTL(a.heartbeatInterval)
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
	if !a.settled && a.firstHeard.IsZero() {
		a.firstHeard = a.clock.Now()
	}
	if !a.settled && hb.IntervalSeconds != 0 {
		a.longestHeard = max(a.longestHeard, allocapi.ClampHeartbeatInterval(hb.IntervalSeconds))
	}
	m := a.members[hb.Member]
	if (m != nil && m.stale(hb)) || (m == nil && hb.ShuttingDown) {
		reply := a.replyLocked(hb.Member)
		// A heartbeat of the process the member's current one replaced is
		// a late one for as long as a heartbeat may be on its way, and the
		// member TTL is longer than that. After it the process is alive:
		// two exporters run under one name, as two hosts of one host name
		// or a copied --allocator.member do, each with the member's bundle
		// and each scraping its share. Both are told, and it is logged
		// once while it lasts.
		if now := a.clock.Now(); m != nil && hb.Instance == m.retired && !hb.ShuttingDown && now.Sub(m.retiredAt) > a.memberTTL {
			if !a.twice(m, now) {
				m.twiceLogged = false
			}
			m.twiceAt = now
			reply.SameName = true
			if !m.twiceLogged {
				m.twiceLogged = true
				a.logger.Warn("two exporters heartbeat under one member name: each has the member's bundle and scrapes its share of the static targets, and probes go to the one that started last; give each exporter a name of its own with --allocator.member", "member", hb.Member, "address", m.advertise, "other_address", hb.Advertise)
			}
		}
		return reply, nil
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
		m.retiredAt, m.twiceAt, m.twiceLogged = a.clock.Now(), time.Time{}, false
	}
	m.sequence = hb.Sequence
	m.advertise, m.lastSeen, m.generation, m.shuttingDown = hb.Advertise, a.clock.Now(), hb.Generation, hb.ShuttingDown
	m.capacity = allocapi.CapacityMilli(hb.Capacity)
	m.staticTargetsPath = linkablePath(hb.StaticTargetsPath)
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
	reply := a.replyLocked(hb.Member)
	reply.SameName = a.twice(m, m.lastSeen)
	return reply, nil
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

// replyLocked is the answer to a heartbeat of member, which also tells the
// replica the heartbeat interval and the instance that answered.
func (a *Allocator) replyLocked(member string) allocapi.HeartbeatReply {
	reply := allocapi.HeartbeatReply{HeartbeatIntervalSeconds: a.heartbeatInterval.Seconds(), Instance: a.id}
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
		if a.live(m, now) {
			n++
		}
	}
	return n
}

// live reports whether m counts as a member at now: heard from within the
// member TTL, and not shutting down.
func (a *Allocator) live(m *member, now time.Time) bool {
	return !m.shuttingDown && now.Sub(m.lastSeen) < a.memberTTL
}

// update brings the member list, the draining targets and the bundles up to
// the clock, and arranges the next wake. It is called under mu after
// every event: a heartbeat, a reload, a wake.
func (a *Allocator) update() {
	now := a.clock.Now()
	var liveMembers []string
	for name, m := range a.members {
		if a.live(m, now) {
			liveMembers = append(liveMembers, name)
		} else if !slices.Contains(a.effective, name) && now.Sub(m.lastSeen) >= a.memberTTL {
			// A member shutting down is kept for the member TTL, though
			// not live: a heartbeat it sent before, arriving late, is then
			// known for what it is rather than taken as a new member's.
			delete(a.members, name)
			delete(a.refusals, name)
		}
	}
	sort.Strings(liveMembers)
	caps := make([]int64, len(liveMembers))
	for i, name := range liveMembers {
		caps[i] = a.members[name].capacity
	}
	if !slices.Equal(liveMembers, a.proposed) || !slices.Equal(caps, a.proposedCaps) {
		a.proposed, a.proposedCaps, a.proposedAt = liveMembers, caps, now
	}
	rebuild := false
	if a.settleDue(now) {
		a.takeEffect(now)
		rebuild = true
	}
	for key, d := range a.draining {
		if !now.Before(d.until) {
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
// the first one, is not empty and comes no sooner than firstSettle.
func (a *Allocator) settleDue(now time.Time) bool {
	if a.settled && a.proposedInEffect() {
		return false
	}
	if !a.settled && (len(a.proposed) == 0 || now.Before(a.firstSettle())) {
		return false
	}
	return !now.Before(a.proposedAt.Add(a.settle))
}

// firstSettle is the earliest the first member list takes effect: the settle
// time or the instance's own heartbeat interval after the start, whichever is
// longer, and no sooner than the longest interval a replica heard from said
// after the first heartbeat came. An instance started with an interval
// shorter than the instances before it is heard by each replica only at the
// replica's next lookup of the instances' addresses on the old interval after
// the instance could be found, which is later than its start: a headless
// Service lists it once it is ready, and the lookups see it later still. The
// first heartbeat comes once a replica found it, so by the old interval after
// that every replica on the old interval has found it, and heartbeated it at
// once. A replica says the longest interval the instances it finds said, not
// the shorter one it heartbeats at, so while an instance from before is
// found every replica says the old interval, one that started after this
// instance too, and the first list waits for the others rather than forming
// of those that happened to come first and changing as each of the rest
// arrives. A replica not told an interval yet, one that has just started,
// says none: the default it heartbeats at is no instance's, and says nothing
// of how long the others take, so it does not count for the interval, though
// its heartbeat, as any, says the instance can be found; it heartbeats again
// at once when the instances' answers tell it one. Replicas that started once
// no instance from before is found have none to learn the old interval from:
// heard alone before this instance's own interval and the settle time have
// passed, they form the first list alone, and it changes as the others
// arrive.
func (a *Allocator) firstSettle() time.Time {
	at := a.started.Add(max(a.settle, a.heartbeatInterval))
	if !a.firstHeard.IsZero() && a.firstHeard.Add(a.longestHeard).After(at) {
		at = a.firstHeard.Add(a.longestHeard)
	}
	return at
}

// proposedInEffect reports whether the proposed member list, with its
// capacities, is the effective one.
func (a *Allocator) proposedInEffect() bool {
	return slices.Equal(a.proposed, a.effective) && slices.Equal(a.proposedCaps, a.effectiveCaps)
}

// takeEffect makes the proposed member list the effective one. A static
// target that moves from a member still in the list keeps being that
// member's for one of its intervals, while its new owner already scrapes
// it, so its series has no gap.
func (a *Allocator) takeEffect(now time.Time) {
	before, was := a.effective, a.currentPlan()
	a.effective, a.effectiveCaps = slices.Clone(a.proposed), slices.Clone(a.proposedCaps)
	after, is := a.effective, a.currentPlan()
	if a.settled {
		a.memberChanges++
	}
	first := !a.settled
	a.settled = true
	if in := a.inputs; in != nil && in.Targets != nil && len(before) > 0 {
		for i := range in.Targets.Targets {
			t := &in.Targets.Targets[i]
			from, to := was.owner[in.shardKeys[i]], is.owner[in.shardKeys[i]]
			if from != to && slices.Contains(after, from) {
				a.draining[drainKey{t.Name, from}] = drain{until: now.Add(time.Duration(t.Interval))}
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
// is back with the member draining it. discovered says the change is one of
// the discovered targets.
func (a *Allocator) handOver(old *Inputs, now time.Time, discovered bool) {
	in := a.inputs
	if a.settled && old != nil && old.Targets != nil && in.Targets != nil && len(a.effective) > 0 {
		before := a.planFor(old, a.effective, a.effectiveCaps)
		after := a.currentPlan()
		for i := range in.Targets.Targets {
			t := &in.Targets.Targets[i]
			j, ok := old.targetIndex[t.Name]
			if !ok {
				continue
			}
			was, is := before.owner[old.shardKeys[j]], after.owner[in.shardKeys[i]]
			if was != is {
				a.draining[drainKey{t.Name, was}] = drain{until: now.Add(time.Duration(t.Interval)), discovered: discovered}
			}
		}
	}
	a.dropStaleDrains()
}

// dropStaleDrains drops the draining targets the inputs in force no longer
// have, and those back with the member draining them.
func (a *Allocator) dropStaleDrains() {
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
	return a.currentPlan().owner[in.shardKeys[i]]
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
		if a.live(m, now) {
			consider(m.lastSeen.Add(a.memberTTL))
		}
	}
	if !a.settled || !a.proposedInEffect() {
		if at := a.proposedAt.Add(a.settle); a.settled || len(a.proposed) > 0 {
			if !a.settled && at.Before(a.firstSettle()) {
				at = a.firstSettle()
			}
			consider(at)
		}
	}
	for _, d := range a.draining {
		consider(d.until)
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
		plan := a.currentPlan()
		for i := range in.Targets.Targets {
			owner := plan.owner[in.shardKeys[i]]
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
	if a.balanceFactor != factorMilli(DefaultBalanceFactor) {
		files[allocapi.AllocationFile] = allocationFile(a.balanceFactor)
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
		if a.live(m, now) && m.generation != "" {
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
	a.putInForce(in, false, nil)
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
