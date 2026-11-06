//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"path/filepath"
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

// An interval configured is written as it was given, its fraction kept:
// a watch interval of 500ms, a heartbeat interval of 2.5s, the member TTL
// that follows from it and a settle time of 1500ms, and a source's
// refresh_interval of 1500ms in the sources table and on its job's view,
// where age, which writes how long ago something was in whole seconds,
// would cut them to 0s, 2s and 1s. An age is still whole seconds.
func TestTheAllocatorPagesKeepAConfiguredIntervalsFraction(t *testing.T) {
	clock := newFakeClock()
	a := New(mustLoad(t, writeInputs(t, testConfig, staticTargets(3), "")), Options{Settle: 1500 * time.Millisecond, HeartbeatInterval: 2500 * time.Millisecond,
		WatchInterval: 500 * time.Millisecond, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	clock.Advance(1700 * time.Millisecond)
	page := ask(a.Handler(""), "/", "").Body.String()
	requireIn(t, page, "read again every 500ms for changes", "(up 1s)",
		"The replicas heartbeat every 2.5s; a member not heard from for 7.5s stops being live. A changed member list holds for 1.5s before targets move.")

	p := writeInputs(t, testConfig, staticTargets(3), `probe_jobs:
  - name: pods
    collector: demo
    target_template: http://{{address}}/metrics
    discovery:
      - name: pod-files
        file_sd: {files: [sd/pods.json], refresh_interval: 1500ms}
`)
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "sd/pods.json", `[{"targets": ["10.0.0.1:8080"]}]`)
	j, _ := settledAllocator(t, mustLoad(t, p), "r1", "r2")
	t.Cleanup(j.BeginShutdown)
	j.refreshDiscovery(context.Background())
	h := j.Handler("")
	requireIn(t, ask(h, "/", "").Body.String(), `<td class="num">1.5s</td>`)
	requireIn(t, ask(h, "/?job=pods", "").Body.String(), "refreshed every 1.5s")
}
