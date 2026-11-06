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

// refreshProbes is a probe job of three sources: a file_sd and a dns_sd
// source that set no refresh_interval, and a file_sd source that sets its
// own.
const refreshProbes = `probe_jobs:
  - name: web
    collector: demo
    discovery:
      - name: files
        file_sd: {files: [sd/a.json]}
      - name: own
        file_sd: {files: [sd/a.json], refresh_interval: 2m}
      - name: names
        dns_sd: {names: [_http._tcp.example.com]}
`

// A discovery source that sets no refresh_interval is asked every
// --discovery.refresh-interval (Paths.DiscoveryRefresh), whatever its kind,
// and without it every its kind's own default, 1m for file_sd and 30s for
// dns_sd; one that sets its own keeps it. The status says each source's
// interval, the page in the discovery sources table, and a timeout is
// held to the interval the source is asked at, the default one included.
func TestASourceWithoutARefreshIntervalTakesTheDefault(t *testing.T) {
	p := writeInputs(t, testConfig, "", refreshProbes)
	testutil.WriteIn(t, p.ProbeTargets[:strings.LastIndex(p.ProbeTargets, "/")], "sd/a.json", `[{"targets": ["10.0.0.1:80"]}]`)
	intervals := func(p Paths) map[string]float64 {
		t.Helper()
		a, _ := settledAllocator(t, mustLoad(t, p), "r1")
		t.Cleanup(a.BeginShutdown)
		out := map[string]float64{}
		for _, s := range a.Status().Sources {
			out[s.Name] = s.RefreshIntervalSeconds
		}
		return out
	}
	if got := intervals(p); len(got) != 3 || got["files"] != 60 || got["own"] != 120 || got["names"] != 30 {
		t.Errorf("without a default the sources are asked every %v seconds", got)
	}
	p.DiscoveryRefresh = 7 * time.Second
	if got := intervals(p); len(got) != 3 || got["files"] != 7 || got["own"] != 120 || got["names"] != 7 {
		t.Errorf("with a default of 7s the sources are asked every %v seconds", got)
	}
	in := mustLoad(t, p)
	for _, s := range in.sources {
		if want := map[string]time.Duration{"files": 7 * time.Second, "own": 2 * time.Minute, "names": 7 * time.Second}[s.cfg.Name]; s.cfg.refresh() != want {
			t.Errorf("the loop of %s waits %s, want %s", s.cfg.Name, s.cfg.refresh(), want)
		}
		if s.cfg.Name == "names" && s.cfg.timeout() != 7*time.Second {
			t.Errorf("the dns_sd source's timeout is %s, want the interval, 7s", s.cfg.timeout())
		}
	}
	a, _ := settledAllocator(t, in, "r1")
	t.Cleanup(a.BeginShutdown)
	requireIn(t, ask(a.Handler(""), "/", "").Body.String(), `<th class="num">Refreshed every</th>`, "<td class=\"num\">7s</td>\n<td class=\"num\">")

	timed := writeInputs(t, testConfig, "", "probe_jobs:\n  - name: web\n    collector: demo\n    discovery:\n      - dns_sd: {names: [_http._tcp.example.com], timeout: 10s}\n")
	if _, err := LoadInputs(timed); err != nil {
		t.Fatalf("a timeout of 10s under dns_sd's 30s is refused: %v", err)
	}
	timed.DiscoveryRefresh = 5 * time.Second
	if _, err := LoadInputs(timed); err == nil || !strings.Contains(err.Error(), "dns_sd.timeout 10s is longer than the default refresh interval 5s (--discovery.refresh-interval)") {
		t.Errorf("a timeout over the default interval of 5s is not refused: %v", err)
	}
}

// The status says how often the input files are read again
// (Options.WatchInterval), or that they are read once, and how long a
// changed member list holds before targets move.
func TestTheStatusSaysHowOftenTheInputsAreRead(t *testing.T) {
	in := mustLoad(t, writeInputs(t, testConfig, staticTargets(2), ""))
	for _, c := range []struct {
		watch time.Duration
		want  string
	}{
		{45 * time.Second, "<tr><th>Input files</th><td>read again every 45s for changes</td></tr>"},
		{0, `<tr><th>Input files</th><td><span class="muted">read once, at the start</span> (<code class="flag">--config.watch-interval=0</code>)</td></tr>`},
	} {
		a := New(in, Options{Settle: 15 * time.Second, Clock: newFakeClock(), Logger: testutil.QuietLogger(t), WatchInterval: c.watch})
		t.Cleanup(a.BeginShutdown)
		if s := a.Status(); s.Instance.WatchIntervalSeconds != c.watch.Seconds() || s.Members.SettleSeconds != 15 {
			t.Errorf("the instance is %+v and the members %+v", s.Instance, s.Members)
		}
		requireIn(t, ask(a.Handler(""), "/", "").Body.String(), c.want, "A changed member list holds for 15s before targets move.")
	}
}

// The discovery sources table keeps a source's kind, its owner's and where
// its targets came from on one line each, and lets the names of a source and
// its owner, as a probe job's of 85 characters, wrap, so a long name does
// not push the table past the page's edge, and a long error in the result
// cell, a path in it broken where it must be, takes the width that is left.
func TestTheSourcesTableWrapsLongNamesAndKeepsShortCellsOnOneLine(t *testing.T) {
	job := strings.Repeat("pods-with-a-long-name-", 4)[:85]
	p := writeInputs(t, testConfig, staticTargets(2), "probe_jobs:\n  - name: "+job+"\n    collector: demo\n    discovery:\n      - name: pod-files-with-a-very-long-source-name-too\n        file_sd: {files: [sd/pods.json]}\n")
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "sd/pods.json", `[{"targets": ["10.0.0.1:8080"]}]`)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1", "r2")
	t.Cleanup(a.BeginShutdown)
	a.refreshDiscovery(context.Background())
	page := ask(a.Handler(""), "/", "").Body.String()
	requireIn(t, page, "table.sources span.kind, table.sources td:last-child { white-space: nowrap; }",
		"table.sources td.owner { overflow-wrap: anywhere; min-width: 9em; }", "table.sources td.result { overflow-wrap: anywhere; min-width: 12em; }",
		"td.id { overflow-wrap: anywhere; min-width: 9em; }",
		`<td class="id"><strong>pod-files-with-a-very-long-source-name-too</strong><br><span class="muted kind">file_sd</span></td>`,
		`<td class="owner">`+job+`<br><span class="muted kind">probe job</span></td>`,
		"td.sources span.source { overflow-wrap: anywhere; }")
	for _, nowrap := range []string{"table.sources td:first-child", "table.sources td:nth-child(2)", "td.sources span.source { white-space: nowrap; }"} {
		if strings.Contains(page, nowrap) {
			t.Errorf("the style still keeps a name on one line: %s", nowrap)
		}
	}
}
