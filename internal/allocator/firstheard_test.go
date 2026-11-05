package allocator

import (
	"slices"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An instance started with a 5s interval during a rollout from 40s is found
// only once it is ready and the lookups see it, here 30s after its start:
// the replicas on the old interval find it at their next lookup after that,
// r1 at once and r2 35s later. The first member list waits 40s, the old
// interval, after the first heartbeat, and so has r2 in it, where 40s after
// the start it formed of r1 alone and changed when r2 came.
func TestTheFirstMemberListWaitsTheOldIntervalAfterTheFirstHeartbeat(t *testing.T) {
	clock := newFakeClock()
	a := New(nil, Options{Settle: time.Second, HeartbeatInterval: 5 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	at := func(interval time.Duration, members ...string) {
		t.Helper()
		for _, name := range members {
			if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: name, Advertise: name + ":9115", IntervalSeconds: interval.Seconds()})); err != nil {
				t.Fatal(err)
			}
		}
	}
	clock.Advance(30 * time.Second)
	at(40*time.Second, "r1")
	for range 7 {
		clock.Advance(5 * time.Second)
		at(40*time.Second, "r1")
	}
	// 65s after the start, 35s after the first heartbeat: r2's lookup on
	// the old interval finds the instance.
	at(40*time.Second, "r2")
	if got := effective(a); got != nil {
		t.Fatalf("the first member list formed of %v before r2 was heard", got)
	}
	clock.Advance(4999 * time.Millisecond)
	if got := effective(a); got != nil {
		t.Fatalf("the first member list formed of %v 1ms before 40s after the first heartbeat", got)
	}
	clock.Advance(time.Millisecond)
	at(40*time.Second, "r1", "r2")
	if got := effective(a); !slices.Equal(got, []string{"r1", "r2"}) || memberChanges(a) != 0 {
		t.Errorf("40s after the first heartbeat got the member list %v after %d changes, want [r1 r2] as the first", got, memberChanges(a))
	}
}
