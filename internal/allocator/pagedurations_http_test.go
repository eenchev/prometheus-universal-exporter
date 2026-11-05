//go:build !select_request_types || request_type_http

package allocator

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The allocator's pages write a duration as pagekit.Duration does, every
// unit that is zero left out: the heartbeat interval, the member TTL and
// the settle time, how often the input files are read, the uptime, a
// discovery source's refresh interval on the status page and the job's
// view, and a static target's interval on a member's view. The JSON keeps
// the interval as Go writes it.
func TestTheAllocatorPagesWriteDurationsWithoutZeroUnits(t *testing.T) {
	clock := newFakeClock()
	a := New(mustLoad(t, writeInputs(t, testConfig, staticTargets(3), "")), Options{Settle: 2 * time.Minute, HeartbeatInterval: time.Minute,
		WatchInterval: time.Hour, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	clock.Advance(74 * time.Second)
	page := ask(a.Handler(""), "/", "").Body.String()
	requireIn(t, page, "(up 1m14s)", "read again every 1h for changes",
		"The replicas heartbeat every 1m; a member not heard from for 3m stops being live. A changed member list holds for 2m before targets move.")

	j := jobAllocator(t)
	h := j.Handler("")
	// The file_sd source's refresh_interval is its default, 1m.
	requireIn(t, ask(h, "/", "").Body.String(), `<td class="num">1m</td>`)
	requireIn(t, ask(h, "/?job=pods", "").Body.String(), "refreshed every 1m")
	// t000 has an interval of its own, 1m, and the file's is 30s.
	var member string
	for _, m := range []string{"r1", "r2", "r3"} {
		if strings.Contains(strings.Join(share(t, j, m), " "), "t000") {
			member = m
		}
	}
	view := ask(h, "/?member="+member+"&filter=t000", "").Body.String()
	requireIn(t, view, `<td class="num" data-label="Interval"><span class="v">1m</span></td>`)
	if v := memberViewOf(t, h, "member="+member+"&filter=t000", ""); len(v.Static.List) != 1 || v.Static.List[0].Interval != "1m0s" {
		t.Errorf("the JSON's static targets are %+v", v.Static.List)
	}
	for _, p := range []string{page, ask(h, "/", "").Body.String(), ask(h, "/?job=pods", "").Body.String(), view} {
		if strings.Contains(p, "m0s") || strings.Contains(p, "h0m") {
			t.Errorf("a page writes a zero unit:\n%s", visibleText(p))
		}
	}
}
