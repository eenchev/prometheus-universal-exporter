package allocator

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// membershipAllocator is an allocator without inputs on a clock of the
// test's own, for the tests of the member list alone.
func membershipAllocator(t *testing.T, settle time.Duration) (*Allocator, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	return New(nil, Options{Settle: settle, Clock: clock, Logger: testutil.QuietLogger(t)}), clock
}

// lastSequence is the last sequence stamp gave.
var lastSequence atomic.Uint64

// stamp gives hb the instance "test", when it names none, and the next
// sequence, when it has none: what a replica's client sets.
func stamp(hb allocapi.Heartbeat) allocapi.Heartbeat {
	if hb.Instance == "" {
		hb.Instance = "test"
	}
	if hb.Sequence == 0 {
		hb.Sequence = lastSequence.Add(1)
	}
	return hb
}

// beat sends a heartbeat of each member.
func beat(t *testing.T, a *Allocator, members ...string) {
	t.Helper()
	for _, name := range members {
		if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: name, Advertise: name + ":9115"})); err != nil {
			t.Fatal(err)
		}
	}
}

// keepBeating moves the clock on by d in steps of a heartbeat interval, the
// members heartbeating at each.
func keepBeating(t *testing.T, a *Allocator, clock *fakeClock, d time.Duration, members ...string) {
	t.Helper()
	for step := time.Duration(0); step < d; step += allocapi.HeartbeatInterval {
		clock.Advance(min(allocapi.HeartbeatInterval, d-step))
		beat(t, a, members...)
	}
}

func effective(a *Allocator) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.settled {
		return nil
	}
	return slices.Clone(a.effective)
}

func memberChanges(a *Allocator) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.memberChanges
}

// The first member list forms once the members have held for the settle
// time, and not before: a heartbeat a moment before is too soon.
func TestTheFirstMemberListFormsAfterTheSettleTime(t *testing.T) {
	a, clock := membershipAllocator(t, 15*time.Second)
	beat(t, a, "r1", "r2")
	clock.Advance(15*time.Second - time.Millisecond)
	if got := effective(a); got != nil {
		t.Fatalf("the member list formed before the settle time: %v", got)
	}
	clock.Advance(time.Millisecond)
	if got := effective(a); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("got the member list %v, want [r1 r2]", got)
	}
	if memberChanges(a) != 0 {
		t.Errorf("forming the first member list counted %d changes", memberChanges(a))
	}
}

// Without a settle time, the first member list still waits a heartbeat
// interval from the start, so every replica has been heard from: an
// allocator that restarts does not hand out shares from the first replica
// it hears.
func TestTheFirstMemberListWaitsAHeartbeatInterval(t *testing.T) {
	a, clock := membershipAllocator(t, 0)
	beat(t, a, "r1")
	clock.Advance(allocapi.HeartbeatInterval / 2)
	beat(t, a, "r2")
	if got := effective(a); got != nil {
		t.Fatalf("the member list formed before a heartbeat interval: %v", got)
	}
	clock.Advance(allocapi.HeartbeatInterval / 2)
	if got := effective(a); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("got the member list %v, want [r1 r2]", got)
	}
}

// A member that joins changes the member list once its heartbeats have held
// for the settle time.
func TestAJoiningMemberTakesEffectAfterTheSettleTime(t *testing.T) {
	a, clock := membershipAllocator(t, 15*time.Second)
	beat(t, a, "r1")
	keepBeating(t, a, clock, 20*time.Second, "r1")
	beat(t, a, "r2")
	keepBeating(t, a, clock, 10*time.Second, "r1", "r2")
	if got := effective(a); !slices.Equal(got, []string{"r1"}) {
		t.Fatalf("the joining member took effect within the settle time: %v", got)
	}
	keepBeating(t, a, clock, 10*time.Second, "r1", "r2")
	if got := effective(a); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("got the member list %v, want [r1 r2]", got)
	}
	if memberChanges(a) != 1 {
		t.Errorf("counted %d member list changes, want 1", memberChanges(a))
	}
}

// A member whose heartbeats stop leaves the live members after the member
// TTL, and the member list the settle time after that.
func TestASilentMemberLeavesAfterTheTTLAndTheSettleTime(t *testing.T) {
	a, clock := membershipAllocator(t, 15*time.Second)
	beat(t, a, "r1", "r2")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2")
	keepBeating(t, a, clock, allocapi.MemberTTL+10*time.Second, "r1")
	if got := effective(a); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("the silent member left before its TTL and the settle time: %v", got)
	}
	keepBeating(t, a, clock, 10*time.Second, "r1")
	if got := effective(a); !slices.Equal(got, []string{"r1"}) {
		t.Fatalf("got the member list %v, want [r1]", got)
	}
}

// A member shutting down leaves the live members at once, without waiting
// for its TTL, and the member list after the settle time.
func TestAMemberShuttingDownLeavesAtOnce(t *testing.T) {
	a, clock := membershipAllocator(t, 15*time.Second)
	beat(t, a, "r1", "r2")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2")
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r2", Advertise: "r2:9115", ShuttingDown: true})); err != nil {
		t.Fatal(err)
	}
	keepBeating(t, a, clock, 15*time.Second, "r1")
	if got := effective(a); !slices.Equal(got, []string{"r1"}) {
		t.Fatalf("got the member list %v the settle time after r2 shut down, want [r1]", got)
	}
}

// A member that restarts and is back inside the settle time keeps its
// place: nothing moves.
func TestAMemberBackInsideTheSettleTimeKeepsItsPlace(t *testing.T) {
	a, clock := membershipAllocator(t, 15*time.Second)
	beat(t, a, "r1", "r2")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2")
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r2", Advertise: "r2:9115", ShuttingDown: true})); err != nil {
		t.Fatal(err)
	}
	keepBeating(t, a, clock, 10*time.Second, "r1")
	beat(t, a, "r2")
	keepBeating(t, a, clock, time.Minute, "r1", "r2")
	if got := effective(a); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("got the member list %v, want [r1 r2]", got)
	}
	if memberChanges(a) != 0 {
		t.Errorf("a restart inside the settle time counted %d member list changes", memberChanges(a))
	}
}

// The heartbeat of a member shutting down that the allocator never heard
// from adds nothing.
func TestAShuttingDownHeartbeatOfAStrangerAddsNothing(t *testing.T) {
	a, clock := membershipAllocator(t, 0)
	beat(t, a, "r1")
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r2", Advertise: "r2:9115", ShuttingDown: true})); err != nil {
		t.Fatal(err)
	}
	clock.Advance(allocapi.HeartbeatInterval)
	if got := effective(a); !slices.Equal(got, []string{"r1"}) {
		t.Fatalf("got the member list %v, want [r1]", got)
	}
}

// A heartbeat without a member, a host:port to probe it at, an instance or
// a sequence is refused.
func TestAHeartbeatWithoutMemberOrAddressIsRefused(t *testing.T) {
	a, _ := membershipAllocator(t, 0)
	for _, hb := range []allocapi.Heartbeat{
		{Member: "", Advertise: "a:1"},
		{Member: "  ", Advertise: "a:1"},
		{Member: "a\nb", Advertise: "a:1"},
		{Member: "a", Advertise: ""},
		{Member: "a", Advertise: "a"},
		{Member: "a", Advertise: ":9115"},
		{Member: "a", Advertise: "a:"},
		{Member: "a", Advertise: "a:http"},
		{Member: "a", Advertise: "a:0"},
		{Member: "a", Advertise: "a:65536"},
		{Member: "a", Advertise: "a:99999"},
		{Member: "a", Advertise: "a:+80"},
		{Member: "a", Advertise: "a:-1"},
		{Member: "a", Advertise: "a:0x50"},
		{Member: "a", Advertise: "a:1", Instance: strings.Repeat("i", 65)},
	} {
		if _, err := a.Heartbeat(stamp(hb)); !errors.Is(err, ErrBadHeartbeat) {
			t.Errorf("%+v: got %v, want it refused", hb, err)
		}
	}
	for _, hb := range []allocapi.Heartbeat{
		{Member: "a", Advertise: "a:1", Sequence: 1},
		{Member: "a", Advertise: "a:1", Instance: "i"},
	} {
		if _, err := a.Heartbeat(hb); !errors.Is(err, ErrBadHeartbeat) {
			t.Errorf("%+v: got %v, want it refused", hb, err)
		}
	}
	for _, advertise := range []string{"[::1]:9115", "a:1", "a:65535"} {
		if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "a", Advertise: advertise})); err != nil {
			t.Errorf("a heartbeat advertising %s was refused: %v", advertise, err)
		}
	}
}

// The allocator takes as many live members as its bound: the heartbeat of a
// new member beyond it is refused, logged once and counted, while the
// members it has keep heartbeating and a member shutting down is no new
// one. A member whose heartbeats stopped frees its place once its TTL has
// run out, though it stays in the member list for the settle time.
func TestNewMembersBeyondTheBoundAreRefused(t *testing.T) {
	clock := newFakeClock()
	logs := testutil.CaptureLogs(t)
	a := New(nil, Options{Settle: 15 * time.Second, MaxMembers: 2, Clock: clock, Logger: slog.Default()})
	beat(t, a, "r1", "r2")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2")
	for range 3 {
		if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r3", Advertise: "r3:9115"})); !errors.Is(err, ErrTooManyMembers) {
			t.Fatalf("a third member of two allowed: got %v, want ErrTooManyMembers", err)
		}
	}
	if got := strings.Count(logs.String(), "refusing the heartbeats of new members"); got != 1 {
		t.Errorf("the refusals were logged %d times, want once:\n%s", got, logs)
	}
	var metrics strings.Builder
	if err := a.WriteMetrics(&metrics); err != nil || !strings.Contains(metrics.String(), "pue_allocator_members_refused_total 3\n") {
		t.Errorf("the refusals are not counted 3 (%v):\n%s", err, metrics.String())
	}
	beat(t, a, "r1", "r2")
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r4", Advertise: "r4:9115", ShuttingDown: true})); err != nil {
		t.Errorf("the shutting-down heartbeat of a stranger was refused: %v", err)
	}
	keepBeating(t, a, clock, allocapi.MemberTTL, "r1")
	if got := effective(a); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("got the member list %v before the settle time, want [r1 r2]", got)
	}
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r3", Advertise: "r3:9115"})); err != nil {
		t.Fatalf("r3 was refused after r2's TTL ran out: %v", err)
	}
	keepBeating(t, a, clock, 20*time.Second, "r1", "r3")
	if got := effective(a); !slices.Equal(got, []string{"r1", "r3"}) {
		t.Errorf("got the member list %v, want [r1 r3]", got)
	}
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r5", Advertise: "r5:9115"})); !errors.Is(err, ErrTooManyMembers) {
		t.Errorf("a new member beyond the bound again: got %v, want ErrTooManyMembers", err)
	}
	if got := strings.Count(logs.String(), "refusing the heartbeats of new members"); got != 2 {
		t.Errorf("a refusal after a new member was taken was not logged again: %d lines", got)
	}
}

// isLive reports whether name is a live member, and the address it
// advertises.
func isLive(a *Allocator, name string) (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.members[name]
	if m == nil {
		return false, ""
	}
	return live(m, a.clock.Now()), m.advertise
}

// A heartbeat that reaches the allocator after a later one of the same
// replica process, as one the replica gave up waiting for may, is answered
// but changes nothing: arriving after the heartbeat saying the replica
// shuts down, it does not make the member live again, whether the member
// is still in the member list or already left it. A restarted replica, a
// new instance counting from 1, is taken, and a late heartbeat of the
// process it replaced is not.
func TestALateHeartbeatDoesNotUndoALaterOne(t *testing.T) {
	for _, settle := range []time.Duration{15 * time.Second, 0} {
		a, clock := membershipAllocator(t, settle)
		send := func(hb allocapi.Heartbeat) {
			t.Helper()
			hb.Member = "r2"
			if hb.Advertise == "" {
				hb.Advertise = "old:9115"
			}
			if _, err := a.Heartbeat(hb); err != nil {
				t.Fatal(err)
			}
		}
		beat(t, a, "r1")
		send(allocapi.Heartbeat{Instance: "old", Sequence: 7})
		keepBeating(t, a, clock, 20*time.Second, "r1")
		send(allocapi.Heartbeat{Instance: "old", Sequence: 8})
		send(allocapi.Heartbeat{Instance: "old", Sequence: 10, ShuttingDown: true})
		if ok, _ := isLive(a, "r2"); ok {
			t.Fatalf("settle %s: r2 is live after it said it shuts down", settle)
		}
		keepBeating(t, a, clock, settle, "r1")
		if got := effective(a); !slices.Equal(got, []string{"r1"}) {
			t.Fatalf("settle %s: got the member list %v the settle time after r2 shut down, want [r1]", settle, got)
		}
		for _, sequence := range []uint64{9, 10, 3} {
			send(allocapi.Heartbeat{Instance: "old", Sequence: sequence})
			if ok, _ := isLive(a, "r2"); ok {
				t.Fatalf("settle %s: a late heartbeat of sequence %d made r2 live again", settle, sequence)
			}
		}
		send(allocapi.Heartbeat{Instance: "new", Sequence: 1, Advertise: "new:9115"})
		if ok, advertise := isLive(a, "r2"); !ok || advertise != "new:9115" {
			t.Fatalf("settle %s: the restarted r2 was not taken: live %v at %s", settle, ok, advertise)
		}
		send(allocapi.Heartbeat{Instance: "old", Sequence: 11, Advertise: "old:9115"})
		if _, advertise := isLive(a, "r2"); advertise != "new:9115" {
			t.Errorf("settle %s: a late heartbeat of the replaced process was applied: r2 at %s", settle, advertise)
		}
		send(allocapi.Heartbeat{Instance: "new", Sequence: 2, Advertise: "new:9115", ShuttingDown: true})
		if ok, _ := isLive(a, "r2"); ok {
			t.Errorf("settle %s: the restarted r2 is live after it said it shuts down", settle)
		}
	}
}

// A refusal is logged once however often it is reported, and again when
// the same bundle is refused after the replica reported none: a heartbeat
// without a refusal says the one before is over.
func TestARefusalIsLoggedAgainAfterOneIsOver(t *testing.T) {
	clock := newFakeClock()
	logs := testutil.CaptureLogs(t)
	a := New(nil, Options{Clock: clock, Logger: slog.Default()})
	refused := &allocapi.Refusal{Generation: "g2", Error: "bad"}
	for _, hb := range []allocapi.Heartbeat{
		{Generation: "g1", Refused: refused},
		{Generation: "g1", Refused: refused},
		{Generation: "g3"},
		{Generation: "g3", Refused: refused},
		{Generation: "g3", Refused: refused},
	} {
		hb.Member, hb.Advertise = "r1", "r1:9115"
		if _, err := a.Heartbeat(stamp(hb)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(logs.String(), "a replica refused its bundle"); got != 2 {
		t.Errorf("the refusals were logged %d times, want twice:\n%s", got, logs)
	}
}
