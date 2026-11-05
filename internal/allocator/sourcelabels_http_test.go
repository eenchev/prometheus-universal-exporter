//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sourceLabelProbes are a job of listed targets and one of two targets a
// file_sd source finds, each with a label of its own and a source label,
// __meta_zone.
const sourceLabelProbes = `probe_jobs:
  - name: websites
    collector: demo
    targets: [https://a.example/health]
  - name: pods
    collector: demo
    target_template: http://{{address}}/metrics
    discovery:
      - name: pod-files
        file_sd: {files: [sd/pods.json]}
`

// A job's view leaves its targets' source labels, those whose names start
// with __, out of the page unless ?meta=1 asks for them, and its filter,
// Search all and the rows' data-match alike, matches only the labels shown;
// the Show source labels box, on the view of a job with discovery sources
// alone, puts ?meta=1 in the URL or takes it out, and the fragment a
// refresh fetches follows the URL. The JSON keeps every label, and its
// filter matches them all.
func TestAJobViewHidesTheSourceLabelsUnlessAsked(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(3), sourceLabelProbes)
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "sd/pods.json", `[{"targets": ["10.0.0.1:8080"], "labels": {"namespace": "shop", "__meta_zone": "ams"}},
  {"targets": ["10.0.0.2:8080"], "labels": {"namespace": "cart", "__meta_zone": "fra"}}]`)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1", "r2")
	t.Cleanup(a.BeginShutdown)
	a.refreshDiscovery(context.Background())
	h := a.Handler("")

	v := jobViewOf(t, h, "job=pods")
	if v.Targets.Total != 2 || v.Targets.List[0].Labels["__meta_zone"] != "ams" || v.Targets.List[1].Labels["__meta_zone"] != "fra" {
		t.Fatalf("the JSON's targets are %+v, want each with its __meta_zone", v.Targets.List)
	}
	if f := jobViewOf(t, h, "job=pods&filter=__meta_zone=ams"); f.Targets.Matching != 1 {
		t.Errorf("the JSON's filter __meta_zone=ams matches %d targets, want 1", f.Targets.Matching)
	}

	rows := func(page string) int { return strings.Count(page, `<tr data-match=`) }
	hidden := ask(h, "/?job=pods", "").Body.String()
	requireIn(t, hidden, `<label class="check" for="source-labels"><input id="source-labels" name="meta" value="1" type="checkbox">Show source labels`,
		`<span class="label">namespace=shop</span>`, `url.searchParams.set("meta", "1")`, `url.searchParams.delete("meta")`,
		"Both match a target, the member it is routed to and the labels shown.")
	if strings.Contains(hidden, "__meta_zone") || rows(hidden) != 2 {
		t.Errorf("without ?meta=1 the page shows a source label, or not both targets:\n%s", visibleText(hidden))
	}
	for filter, want := range map[string]int{"ams": 0, "__meta": 0, "namespace=shop": 1} {
		page := ask(h, "/?job=pods&filter="+filter, "").Body.String()
		if rows(page) != want {
			t.Errorf("without ?meta=1 Search all of %q lists %d targets, want %d", filter, rows(page), want)
		}
	}

	shown := ask(h, "/?job=pods&meta=1", "").Body.String()
	requireIn(t, shown, `<input id="source-labels" name="meta" value="1" type="checkbox" checked>`,
		`<span class="label">__meta_zone=ams</span>`, `<span class="label">__meta_zone=fra</span>`, "__meta_zone=ams\nnamespace=shop")
	if page := ask(h, "/?job=pods&meta=1&filter=ams", "").Body.String(); rows(page) != 1 {
		t.Errorf("with ?meta=1 Search all of ams lists %d targets, want 1", rows(page))
	}
	// A refresh fetches the URL's fragment, ?meta=1 with it.
	if f := ask(h, "/?job=pods&meta=1&format=fragment", "").Body.String(); !strings.Contains(f, "__meta_zone=ams") {
		t.Errorf("the fragment with ?meta=1 has no source label:\n%.600s", f)
	}
	if f := ask(h, "/?job=pods&format=fragment", "").Body.String(); strings.Contains(f, "__meta_zone") {
		t.Errorf("the fragment without ?meta=1 has a source label:\n%.600s", f)
	}

	if page := ask(h, "/?job=websites", "").Body.String(); strings.Contains(page, `id="source-labels"`) {
		t.Error("the view of a job whose file lists every target has a Show source labels box")
	}
	if page := ask(h, "/?member=r1", "").Body.String(); strings.Contains(page, `id="source-labels"`) {
		t.Error("a member's view has a Show source labels box")
	}
}
