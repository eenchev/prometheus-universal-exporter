//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The status page tells the two kinds of target apart: it says what a
// static target and a probe target are, and how many of each the files
// write and the sources found, by name; the members table groups its
// columns under the two kinds, in the bars' colours; a static targets table
// beside the probe jobs table has a row of the written targets and one of
// each discovered_targets entry, spread over the members; and the
// discovery sources table says which kind each source feeds, linking the
// job or the entry. The JSON carries the same.
func TestTheStatusPageTellsStaticAndProbeTargetsApart(t *testing.T) {
	probes := "probe_jobs:\n  - name: websites\n    collector: demo\n    targets: [https://shop.example, https://docs.example]\n    discovery:\n      - name: sites\n        file_sd: {files: [sd/sites.json]}\n"
	p := writeInputs(t, testConfig, discoveredStatic, probes)
	dir := filepath.Dir(p.StaticTargets)
	testutil.WriteIn(t, dir, "nodes.yaml", discoveredNodes)
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "sd/sites.json", `[{"targets": ["https://blog.example", "https://wiki.example"]}]`)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1", "r2")
	t.Cleanup(a.BeginShutdown)
	a.refreshDiscovery(context.Background())
	h := a.Handler("")

	sp := statusOf(t, h, "").Spread
	if sp.StaticWritten != 1 || sp.StaticDiscovered != 3 || sp.ProbeListed != 2 || sp.ProbeDiscovered != 2 ||
		!slices.Equal(sp.StaticSources, []string{"file_sd_0"}) || !slices.Equal(sp.ProbeSources, []string{"sites"}) {
		t.Errorf("the kinds are %d written and %d discovered static targets by %v, %d listed and %d discovered probe targets by %v",
			sp.StaticWritten, sp.StaticDiscovered, sp.StaticSources, sp.ProbeListed, sp.ProbeDiscovered, sp.ProbeSources)
	}
	if len(sp.Static) != 2 || sp.Static[0].Entry != "" || sp.Static[0].Targets != 1 || len(sp.Static[0].Sources) != 0 ||
		sp.Static[1].Entry != "node" || sp.Static[1].Targets != 3 || len(sp.Static[1].Members) != 2 || len(sp.Static[1].Sources) != 1 ||
		sp.Static[1].Members[0].Targets+sp.Static[1].Members[1].Targets != 3 {
		t.Errorf("the static targets by origin are %+v", sp.Static)
	}

	page := ask(h, "/", "").Body.String()
	requireIn(t, page,
		`<b class="swatch seg-static"></b><strong>Static targets</strong> are scraped by the member that owns them`,
		`<b class="swatch seg-probe"></b><strong>Probe targets</strong> are what Prometheus asks this allocator for`,
		`<p id="kinds-summary">4 static targets: 1 written in the static target file and 3 discovered by file_sd_0. 4 probe targets: 2 listed in the probe jobs and 2 discovered by sites.</p>`,
		`<th class="group" colspan="2"><b class="swatch seg-static"></b>Static targets</th><th class="group"><b class="swatch seg-probe"></b>Probe targets</th>`,
		`<tr><th class="num">Owned</th><th class="num">Draining</th><th class="num">Routed</th></tr>`,
		`<span><b class="seg-static"></b>static targets, owned</span>`, `<span><b class="seg-probe"></b>probe targets, routed</span>`,
		`<h2 id="static-targets"><b class="swatch seg-static"></b>Static targets</h2>`,
		`<td class="id"><strong>written</strong><br><span class="muted kind">in the static target file</span></td>`,
		`<tr id="static-entry-node">`, `<td class="id"><strong>node</strong><br><span class="muted kind">discovered_targets entry</span></td>`,
		`<h2 id="probe-jobs-section"><b class="swatch seg-probe"></b>Probe jobs</h2>`,
		`<thead><tr><th>Source</th><th>Feeds</th><th>Result</th>`,
		`<b class="swatch seg-probe"></b>probe targets<br><span class="muted kind">of the job</span> <a href="?job=websites"`,
		`<b class="swatch seg-static"></b>static targets<br><span class="muted kind">of the entry</span> <a href="#static-entry-node"`,
	)
	// The static targets table comes before the probe jobs table, as the
	// members table's columns do.
	if strings.Index(page, `id="static-targets"`) > strings.Index(page, `id="probe-jobs-section"`) {
		t.Error("the static targets table is after the probe jobs table")
	}
	requireIn(t, ask(h, "/?member=r1", "").Body.String(), `<h2><b class="swatch seg-static"></b>Static targets owned`, `<h2><b class="swatch seg-probe"></b>Probe targets routed`)
	requireIn(t, ask(h, "/?job=websites", "").Body.String(), `<h2><b class="swatch seg-probe"></b>Probe targets <span class="count">4</span></h2>`)
}
