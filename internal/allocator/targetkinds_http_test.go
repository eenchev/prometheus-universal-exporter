//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"encoding/json"
	"net/http"
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
		`<tr id="static-entry-node">`, `<td class="id"><a href="?entry=node" title="the static targets of the discovered_targets entry node"><strong>node</strong></a><br><span class="muted kind">discovered_targets entry</span></td>`,
		`<h2 id="probe-jobs-section"><b class="swatch seg-probe"></b>Probe jobs</h2>`,
		`<thead><tr><th>Source</th><th>Feeds</th><th>Result</th>`,
		`<b class="swatch seg-probe"></b>probe targets<br><span class="muted kind">of the job</span> <a href="?job=websites"`,
		`<b class="swatch seg-static"></b>static targets<br><span class="muted kind">of the entry</span> <a href="?entry=node"`,
	)
	// The static targets table comes before the probe jobs table, as the
	// members table's columns do.
	if strings.Index(page, `id="static-targets"`) > strings.Index(page, `id="probe-jobs-section"`) {
		t.Error("the static targets table is after the probe jobs table")
	}
	requireIn(t, ask(h, "/?member=r1", "").Body.String(), `<h2><b class="swatch seg-static"></b>Static targets owned`, `<h2><b class="swatch seg-probe"></b>Probe targets routed`)
	requireIn(t, ask(h, "/?job=websites", "").Body.String(), `<h2><b class="swatch seg-probe"></b>Probe targets <span class="count">4</span></h2>`)
}

// entryViewOf is the JSON of a discovered_targets entry's view.
func entryViewOf(t *testing.T, h http.Handler, query string) EntryView {
	t.Helper()
	rec := ask(h, StatusPath+"?"+query, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%s?%s answered %d: %s", StatusPath, query, rec.Code, rec.Body)
	}
	var v EntryView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A discovered_targets entry has a view of its own, as a probe job has:
// its labels, shard key and sources, the members its targets are spread
// over with the count of each, the spread's, and each target with the
// member that owns it, filtered; an entry the file does not have is a page
// that says so, and a 404 in JSON.
func TestAnEntrysViewListsItsTargets(t *testing.T) {
	p := writeInputs(t, testConfig, discoveredStatic, "")
	testutil.WriteIn(t, filepath.Dir(p.StaticTargets), "nodes.yaml", discoveredNodes)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1", "r2")
	t.Cleanup(a.BeginShutdown)
	a.refreshDiscovery(context.Background())
	h := a.Handler("")
	sp := statusOf(t, h, "").Spread

	v := entryViewOf(t, h, "entry=node")
	if v.Entry != "node" || v.Partial || v.Limit != MemberViewLimit || v.ShardKey != ShardKeyTarget || v.Labels["team"] != "infra" ||
		len(v.Sources) != 1 || v.Sources[0].Name != "file_sd_0" || v.Spread == nil || v.Spread.Targets != 3 || len(v.Spread.Sources) != 1 {
		t.Fatalf("the view is %+v", v)
	}
	counts := map[string]int{}
	for _, m := range sp.Static[1].Members {
		counts[m.Member] = m.Targets
	}
	for i, m := range v.Spread.Members {
		if m.Targets != counts[m.Member] || (i > 0 && v.Spread.Members[i-1].Targets < m.Targets) {
			t.Errorf("the members are %+v, the spread's %v", v.Spread.Members, counts)
		}
	}
	if v.Targets.Total != 3 || v.Targets.Matching != 3 || len(v.Targets.List) != 3 {
		t.Fatalf("the targets are %+v", v.Targets)
	}
	owned := map[string]string{}
	for _, m := range []string{"r1", "r2"} {
		for _, st := range memberViewOf(t, h, "member="+m, "").Static.List {
			owned[st.Name] = m
		}
	}
	for i, target := range v.Targets.List {
		if !strings.HasPrefix(target.Name, "node") || target.Member == "" || target.Member != owned[target.Name] || target.Collector != "demo" ||
			!strings.HasPrefix(target.Target, "http://") || target.Labels["team"] != "infra" || (i > 0 && v.Targets.List[i-1].Name > target.Name) {
			t.Errorf("the targets are %+v; the members own %v", v.Targets.List, owned)
		}
	}
	if f := entryViewOf(t, h, "entry=node&filter="+v.Targets.List[0].Member); f.Targets.Total != 3 || f.Targets.Matching != counts[v.Targets.List[0].Member] {
		t.Errorf("filtered by its member, the list is %+v", f.Targets)
	}

	page := ask(h, "/?entry=node", "").Body.String()
	requireIn(t, page, `<title>Entry node · pue-allocator</title>`, `<p class="back"><a href="./">← Allocator status</a></p>`,
		`<h1>Entry <span class="member-id">node</span> <span class="muted kind">discovered_targets</span></h1>`,
		`<input type="hidden" name="entry" value="node">`, `<label for="target-filter">Filter: name, target, collector, label or member</label>`,
		`<tr><th>Shard key</th><td>target</td></tr>`, `<strong>file_sd_0</strong>`,
		`<span class="seg-static" style="width: `, `static targets of the entry owned`,
		`<h2><b class="swatch seg-static"></b>Static targets <span class="count">3</span></h2>`,
		`<table class="targets" id="entry-targets">`, `<td class="moved" data-label="Member"><span class="v"><a href="?member=`,
		`<a href="v1/status?entry=node">JSON</a>`)
	if r := ask(h, StatusPath+"?entry=gone", ""); r.Code != http.StatusNotFound {
		t.Errorf("the JSON of an entry the file does not have answered %d", r.Code)
	}
	if r := ask(h, "/?entry=gone", ""); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `id="unknown-entry"`) {
		t.Errorf("the page of an entry the file does not have answered %d:\n%s", r.Code, r.Body)
	}
	for _, path := range []string{StatusPath + "?entry=", "/?entry="} {
		if r := ask(h, path, ""); r.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d", path, r.Code)
		}
	}
}
