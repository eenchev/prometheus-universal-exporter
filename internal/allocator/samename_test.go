package allocator

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Two exporters given one member name, as two hosts of one host name or a
// copied --allocator.member are, both take the member's bundle and both
// scrape its share, and nothing said so: the allocator took the one that
// started last and passed over the other's heartbeats as late ones. A
// heartbeat of the process a member's current one replaced that comes more
// than a member TTL after the replacement is no late one: both exporters are
// then told, it is logged once while it lasts, the status and
// pue_allocator_members_same_name show it, and it is over a member TTL after
// the other's last heartbeat. A restart, whose old process's heartbeats end,
// and a heartbeat that is merely late, say nothing.
func TestTwoExportersUnderOneMemberNameAreSaid(t *testing.T) {
	clock := newFakeClock()
	logs := testutil.CaptureLogs(t)
	a := New(nil, Options{Clock: clock, Logger: slog.Default()})
	sequence := map[string]uint64{}
	send := func(process string) allocapi.HeartbeatReply {
		t.Helper()
		sequence[process]++
		reply, err := a.Heartbeat(allocapi.Heartbeat{Member: "exporter", Advertise: process + ":9115", Instance: process, Sequence: sequence[process]})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	warned := func() int { return strings.Count(logs.String(), "two exporters heartbeat under one member name") }
	sameName := func() bool {
		for _, m := range a.Status().Members.List {
			if m.ID == "exporter" {
				return m.SameName
			}
		}
		t.Fatal("the member is not in the status")
		return false
	}
	metric := func() bool {
		var b strings.Builder
		if err := a.WriteMetrics(&b); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(b.String(), "\npue_allocator_members_same_name 1\n")
	}

	// A restart: the new process takes over, and a heartbeat of the old
	// one still on its way is late, no more.
	send("first")
	clock.Advance(10 * time.Second)
	send("second")
	clock.Advance(5 * time.Second)
	if reply := send("first"); reply.SameName || warned() != 0 || sameName() || metric() {
		t.Fatalf("a late heartbeat of the replaced process was taken for a second exporter: %+v\n%s", reply, logs)
	}

	// The old process is still there a member TTL later: it is another
	// exporter.
	for range 4 {
		clock.Advance(10 * time.Second)
		send("second")
	}
	if reply := send("first"); !reply.SameName || warned() != 1 || !sameName() || !metric() {
		t.Fatalf("a second exporter under the name: reply %+v, warned %d times, status %v, metric %v\n%s", reply, warned(), sameName(), metric(), logs)
	}
	if !strings.Contains(logs.String(), `"member":"exporter"`) || !strings.Contains(logs.String(), `"address":"second:9115"`) || !strings.Contains(logs.String(), `"other_address":"first:9115"`) {
		t.Errorf("the warning does not name the member and both addresses:\n%s", logs)
	}
	// Both are told, and it is logged once while both heartbeat.
	for range 5 {
		clock.Advance(10 * time.Second)
		if reply := send("second"); !reply.SameName {
			t.Fatal("the exporter that started last is not told")
		}
		if reply := send("first"); !reply.SameName {
			t.Fatal("the other exporter is not told")
		}
	}
	if warned() != 1 {
		t.Errorf("warned %d times while it lasted, want once", warned())
	}
	if _, advertise := isLive(a, "exporter"); advertise != "second:9115" {
		t.Errorf("the member is at %s, want the exporter that started last", advertise)
	}

	// The other stops: a member TTL later it is over, and one that comes
	// back is said again.
	for range 4 {
		clock.Advance(10 * time.Second)
		send("second")
	}
	if reply := send("second"); reply.SameName || sameName() || metric() {
		t.Fatalf("it is still said a member TTL after the other's last heartbeat: %+v", reply)
	}
	if reply := send("first"); !reply.SameName || warned() != 2 {
		t.Errorf("the other came back: reply %+v, warned %d times, want twice", reply, warned())
	}
}
