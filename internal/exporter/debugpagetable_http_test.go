//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The debug page's table of a run's series (debugSamples) and the rename of
// its Probe a target radio.

// sampleLine is s written as the text format writes a sample line, its
// labels in name order.
func sampleLine(s debugSample) string {
	names := make([]string, 0, len(s.Labels))
	for name := range s.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(s.Name)
	if len(names) > 0 {
		b.WriteByte('{')
		for i, name := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(name + `="` + s.Labels[name] + `"`)
		}
		b.WriteByte('}')
	}
	return b.String() + " " + s.Value
}

// A run answers its series one sample at a time beside the text: each with
// its name, the metric it is of, the metric's type, its labels and its value
// as the text writes it, a histogram's buckets, +Inf, _sum and _count and a
// summary's quantiles, _sum and _count each a sample, in the text's order.
func TestTheDebugPageAnswersEachSampleOfARun(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	written := "name: passthrough\nrequest: {type: http}\ntransform: {type: prometheus}\n"
	body := `# TYPE temp gauge
temp{room="kitchen"} 21.5
temp{room="attic"} NaN
# TYPE jobs_total counter
jobs_total 3
# TYPE latency_seconds histogram
latency_seconds_bucket{path="/",le="0.5"} 4
latency_seconds_bucket{path="/",le="+Inf"} 6
latency_seconds_sum{path="/"} 2.25
latency_seconds_count{path="/"} 6
# TYPE rpc_seconds summary
rpc_seconds{quantile="0.9"} 0.3
rpc_seconds{quantile="0.5"} 0.1
rpc_seconds_sum 7
rpc_seconds_count 40
# TYPE big untyped
big +Inf
`
	answer := postRun(t, server, map[string]any{"collector": written, "body": body, "content_type": "text/plain; version=0.0.4"}, http.StatusOK)
	if !answer.OK {
		t.Fatalf("the run failed: %+v", answer)
	}
	type row struct{ line, metric, typ string }
	var got []row
	for _, s := range answer.Samples {
		got = append(got, row{sampleLine(s), s.Metric, s.Type})
	}
	want := []row{
		{`temp{room="kitchen"} 21.5`, "temp", "gauge"},
		{`temp{room="attic"} NaN`, "temp", "gauge"},
		{`jobs_total 3`, "jobs_total", "counter"},
		{`latency_seconds_bucket{le="0.5",path="/"} 4`, "latency_seconds", "histogram"},
		{`latency_seconds_bucket{le="+Inf",path="/"} 6`, "latency_seconds", "histogram"},
		{`latency_seconds_sum{path="/"} 2.25`, "latency_seconds", "histogram"},
		{`latency_seconds_count{path="/"} 6`, "latency_seconds", "histogram"},
		{`rpc_seconds{quantile="0.5"} 0.1`, "rpc_seconds", "summary"},
		{`rpc_seconds{quantile="0.9"} 0.3`, "rpc_seconds", "summary"},
		{`rpc_seconds_sum 7`, "rpc_seconds", "summary"},
		{`rpc_seconds_count 40`, "rpc_seconds", "summary"},
		{`big +Inf`, "big", "untyped"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("samples:\n%v\nwant\n%v", got, want)
	}
	// The samples are the text's lines, in its order, to the character.
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(answer.Series), "\n") {
		if !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	var fromSamples []string
	for _, r := range got {
		fromSamples = append(fromSamples, r.line)
	}
	if !slices.Equal(lines, fromSamples) {
		t.Errorf("the samples are not the text's lines:\n%q\n%q", fromSamples, lines)
	}
	// A refused run has none.
	if refused := postRun(t, server, pasted("name: x\n", "{}"), http.StatusUnprocessableEntity); refused.Samples != nil {
		t.Errorf("a refused run has samples %+v", refused.Samples)
	}
}

// A lint finding names the metric it is about as the samples of that metric
// do, so the page puts it on their rows; a finding about a rule that gave no
// series names a metric no sample is of, and the page lists it above the
// table.
func TestTheDebugPagesLintFindingsNameTheMetricOfTheirSamples(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	written := `name: app
request: {type: http}
decoder: {type: json}
transform: {type: jq}
metrics:
  - {name: jobs, type: counter, expression: .jobs}
  - {name: idle, type: counter, items: '(.idle // [])[]', expression: .}
  - {name: queue_length, description: Jobs waiting, type: gauge, expression: .queue}
`
	answer := postRun(t, server, pasted(written, `{"jobs": 3, "queue": 2}`), http.StatusOK)
	of := map[string]bool{}
	for _, s := range answer.Samples {
		of[s.Metric] = true
	}
	if !of["jobs"] || !of["queue_length"] || of["idle"] || len(answer.Samples) != 2 {
		t.Fatalf("samples %+v", answer.Samples)
	}
	byMetric := map[string]string{}
	for _, f := range answer.Lint {
		byMetric[f.Metric] = f.Rule
	}
	if byMetric["jobs"] != model.LintCounterTotal || byMetric["idle"] != model.LintCounterTotal {
		t.Fatalf("lint %+v", answer.Lint)
	}
	if _, found := byMetric["queue_length"]; found {
		t.Errorf("a finding on queue_length: %+v", answer.Lint)
	}
	script := debugPageScript(getPage(t, server, "/debug"))
	requireContains(t, script,
		`if (!metrics.has(f.metric)) { unplaced.push(f); continue; }`,
		`const findings = findingsOf.get(sample.metric);`,
		`", which no series is of: "`,
		`summary.title = findings.map((f) => "[" + f.rule + "] " + f.message).join("\n");`,
		`advice.textContent = "[" + f.rule + "] " + f.message;`)
}

// The page shows the series as a table by default and as the text beside
// it, the text's <pre> as it was; the table sorts by a header, filters by a
// box and by metric, says how many rows are left, draws at most 2000 and
// puts what a run answers in as text alone.
func TestTheDebugPageShowsTheSeriesAsATable(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	page := getPage(t, server, "/debug")
	requireContains(t, page,
		`<button type="button" role="tab" id="view-table" data-view="table" aria-controls="series-table-part" aria-selected="true">Table</button>`,
		`<button type="button" role="tab" id="view-text" data-view="text" aria-controls="series-text-part" aria-selected="false">Text</button>`,
		`<div id="series-text-part" role="tabpanel" aria-labelledby="view-text" hidden>
<pre id="series"></pre>
</div>`,
		`<input id="series-filter" type="search"`,
		`<select id="series-metric"><option value="">Every metric</option></select>`,
		`<div class="table-box" id="series-box" hidden><table class="series" id="series-table">`,
		`.table-box { max-height: 520px; overflow: auto;`)
	script := debugPageScript(page)
	requireContains(t, script,
		`samples = (answer.samples || [])`,
		`labelNames = [...names].sort();`,
		`sortDown = sortBy === column ? !sortDown : false;`,
		`const numberOf = (text) => text === "+Inf" ? Infinity : text === "-Inf" ? -Infinity : Number(text);`,
		`const numericLabels = new Set(["le", "quantile"]);`,
		`if (x === "" || y === "") return (x === "") - (y === "");`,
		`.toLowerCase();`,
		`$("series-filter").addEventListener("input", drawRows);`,
		`$("series-metric").addEventListener("change", drawRows);`,
		`rows.length + " of " + samples.length + " series"`,
		`const tableRows = 2000;`,
		`const drawn = rows.slice(0, tableRows);`,
		`"Showing the first " + tableRows + " of " + rows.length + "; filter to narrow. The text view has them all."`,
		`td.textContent = text;`,
		`store.setItem("debug.view", view)`,
		`const shown = refused ? "text" : view;`)
	if strings.Contains(script, "innerHTML") || strings.Contains(script, "insertAdjacentHTML") || strings.Contains(script, "outerHTML") {
		t.Error("the script writes HTML")
	}
}

// The response radio that probes a target says so: Probe a target, at full
// with a collector chosen and without; the radio's value and id, which the
// script and the URL's target= read, are as they were.
func TestTheDebugPageSaysProbeATarget(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	server.SetDebug(DebugFull)
	for _, path := range []string{"/debug", "/debug?collector=rooms&target=http://x"} {
		page := getPage(t, server, path)
		requireContains(t, page, `value="fetch" id="mode-fetch"`, `> Probe a target</label>`, `to probe a target or scrape a static target.</p>`,
			`if (asked.get("target") !== null && $("target")) { choose("fetch");`)
		if strings.Contains(page, "Fetch a target") || strings.Contains(page, "to fetch a target") {
			t.Errorf("%s still says fetch a target", path)
		}
	}
	server.SetDebug(DebugEdit)
	requireContains(t, getPage(t, server, "/debug"), "Probing a target needs <code>--web.debug=full</code>")
}
