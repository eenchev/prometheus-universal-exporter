//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The debug page's OTLP tab and endpoint test (debugotlp.go,
// debugotlptest.go).

// passthroughYAML is a playground that passes Prometheus text through.
const passthroughYAML = "name: passthrough\nrequest: {type: http}\ntransform: {type: prometheus}\n"

// kindsBody has a series of each kind, and a histogram whose bucket counts
// fall, which is exported as gauges.
const kindsBody = `# TYPE job_duration_seconds gauge
job_duration_seconds{job="a"} 1.5
# TYPE requests_total counter
requests_total{code="200"} 7
# TYPE latency_seconds histogram
latency_seconds_bucket{le="0.1"} 1
latency_seconds_bucket{le="1"} 3
latency_seconds_bucket{le="+Inf"} 3
latency_seconds_sum 1.25
latency_seconds_count 3
# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.1
rpc_seconds{quantile="0.9"} 0.3
rpc_seconds_sum 7
rpc_seconds_count 40
# TYPE broken histogram
broken_bucket{le="1"} 5
broken_bucket{le="2"} 2
broken_bucket{le="+Inf"} 5
broken_sum 1
broken_count 5
`

// otlpDebugServer is a debug page server whose otlp block is change's.
func otlpDebugServer(t *testing.T, endpoint string, change func(*model.OTLPConfig), collectors ...model.Collector) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: append([]model.Collector{roomsCollector("rooms")}, collectors...), OTLP: otlpConfig(endpoint)}
	if change != nil {
		change(&cfg.OTLP)
	}
	return debugPageServer(t, cfg)
}

func pastedText(collector, body string) map[string]any {
	return map[string]any{"collector": collector, "body": body, "content_type": "text/plain; version=0.0.4"}
}

// pointsByName are the tab's points by their OTLP name and attributes.
func pointsByName(otlp *debugOTLP) map[string]debugOTLPPoint {
	out := map[string]debugOTLPPoint{}
	for _, p := range otlp.Points {
		var attributes []string
		for _, a := range p.Attributes {
			attributes = append(attributes, a.Key+"="+a.Value)
		}
		out[p.Name+"{"+strings.Join(attributes, ",")+"}"] = p
	}
	return out
}

// A run of a gauge, a counter, a histogram and a summary shows each point as
// the export would send it: its name, kind, unit and value, and what was
// made of it, under prometheus naming and under otel naming; a histogram
// whose bucket counts fall is gauges under its samples' names.
func TestTheOTLPTabShowsEachKindUnderEitherNaming(t *testing.T) {
	type want struct{ prometheus, kind, unit, value, note string }
	for _, naming := range []struct {
		names string
		want  map[string]want
	}{
		{model.OTLPMetricNamesPrometheus, map[string]want{
			"job_duration_seconds{job=a}": {"job_duration_seconds", "gauge", "", "1.5", ""},
			"requests_total{code=200}":    {"requests_total", "sum (cumulative, monotonic)", "", "7", ""},
			"latency_seconds{}":           {"latency_seconds", "histogram (cumulative)", "", "count 3, sum 1.25; buckets ≤0.1: 1, ≤1: 2, >1: 0", ""},
			"rpc_seconds{}":               {"rpc_seconds", "summary", "", "count 40, sum 7; quantiles 0.5: 0.1, 0.9: 0.3", ""},
			"broken_bucket{le=2}":         {"broken_bucket", "gauge", "", "2", "exported as gauges under the names of its samples, as OpenMetrics' unknown: its bucket counts fall"},
			"broken_count{}":              {"broken_count", "gauge", "", "5", "its bucket counts fall"},
		}},
		{model.OTLPMetricNamesOTel, map[string]want{
			"job_duration{job=a}": {"job_duration_seconds", "gauge", "s", "1.5", "unit s from the suffix _seconds, taken off (otlp.metric_names: otel)"},
			"requests{code=200}":  {"requests_total", "sum (cumulative, monotonic)", "", "7", "_total taken off (otlp.metric_names: otel)"},
			"latency{}":           {"latency_seconds", "histogram (cumulative)", "s", "count 3, sum 1.25; buckets ≤0.1: 1, ≤1: 2, >1: 0", "unit s from the suffix _seconds"},
			"rpc{}":               {"rpc_seconds", "summary", "s", "count 40, sum 7; quantiles 0.5: 0.1, 0.9: 0.3", "unit s from the suffix _seconds"},
			"broken_bucket{le=1}": {"broken_bucket", "gauge", "", "5", "its bucket counts fall"},
		}},
	} {
		t.Run(naming.names, func(t *testing.T) {
			server := otlpDebugServer(t, "http://127.0.0.1:1/v1/metrics", func(c *model.OTLPConfig) { c.MetricNames = naming.names })
			answer := postRun(t, server, pastedText(passthroughYAML, kindsBody), http.StatusOK)
			if answer.OTLP == nil {
				t.Fatalf("no otlp field: %+v", answer)
			}
			if answer.OTLP.MetricNames != naming.names {
				t.Errorf("metric_names %q", answer.OTLP.MetricNames)
			}
			points := pointsByName(answer.OTLP)
			for key, w := range naming.want {
				p, ok := points[key]
				if !ok {
					t.Errorf("no point %s; the tab has %v", key, keysOf(points))
					continue
				}
				if p.Prometheus != w.prometheus || p.Kind != w.kind || p.Unit != w.unit || p.Value != w.value {
					t.Errorf("%s is %+v, want %+v", key, p, w)
				}
				if w.note != "" && !strings.Contains(strings.Join(p.Notes, "\n"), w.note) {
					t.Errorf("%s notes %q, want one with %q", key, p.Notes, w.note)
				}
				if w.note == "" && len(p.Notes) > 0 {
					t.Errorf("%s has notes %q", key, p.Notes)
				}
			}
			notes := strings.Join(answer.OTLP.Notes, "\n")
			if !strings.Contains(notes, "Start times: each cumulative point starts at its own time") {
				t.Errorf("no start time note: %q", notes)
			}
			if !strings.Contains(answer.OTLP.Body, `"resourceMetrics": [`) || !strings.Contains(answer.OTLP.Body, "\n  ") {
				t.Errorf("the body is not the indented request: %s", answer.OTLP.Body)
			}
		})
	}
}

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Without OTLP a run has no otlp field and the page no OTLP tab or test.
func TestTheOTLPTabIsThereWithOTLPAlone(t *testing.T) {
	off := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	recorder := serve(off, runRequest(t, pasted(roomsYAML("rooms"), debugExampleBody)))
	if strings.Contains(recorder.Body.String(), `"otlp"`) {
		t.Errorf("a run without OTLP has an otlp field: %s", recorder.Body)
	}
	for _, gone := range []string{`id="view-otlp"`, `id="series-otlp-part"`, `id="otlp-test-metrics"`} {
		if strings.Contains(getPage(t, off, "/debug"), gone) {
			t.Errorf("the page without OTLP has %s", gone)
		}
	}
	on := otlpDebugServer(t, "http://127.0.0.1:1/v1/metrics", nil)
	page := getPage(t, on, "/debug")
	requireContains(t, page, `<button type="button" role="tab" id="view-otlp" data-view="otlp" aria-controls="series-otlp-part" aria-selected="false">OTLP</button>`,
		`id="otlp-test-metrics" data-signal="metrics" disabled>`, `id="otlp-test-unavailable"`, `showOTLP(answer.otlp, true);`)
	if strings.Contains(page, `id="otlp-logs-part"`) || strings.Contains(page, `id="otlp-test-logs"`) {
		t.Error("the page has the log records without otlp.logs")
	}
	if answer := postRun(t, on, pasted(roomsYAML("rooms"), debugExampleBody), http.StatusOK); answer.OTLP == nil || answer.OTLP.Logs != nil {
		t.Errorf("otlp %+v", answer.OTLP)
	}
	on.SetDebug(DebugFull)
	logs := otlpDebugServer(t, "http://127.0.0.1:1/v1/metrics", func(c *model.OTLPConfig) { c.Logs.Enabled = true })
	logs.SetDebug(DebugFull)
	requireContains(t, getPage(t, logs, "/debug"), `id="otlp-logs-part"`, `id="otlp-test-logs" data-signal="logs">`, `id="otlp-test-metrics" data-signal="metrics">`)
}

// The resource and scope: a probe's run is under the exporter-wide resource
// with otlp.probe_attributes' collector and target; the playground's too,
// said to be a probe of it; a static target's under its own resource, its
// labels_as_resource moved to it and its name its service.instance.id.
func TestTheOTLPTabShowsTheResourceOfTheRun(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}, OTLP: otlpConfig("http://127.0.0.1:1/v1/metrics")}
	cfg.OTLP.ProbeAttributes = true
	cfg.OTLP.Scope = &model.OTLPScopeConfig{Name: "acme.pue", Version: "1.2"}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{
		Name: "berlin", Collector: "rooms", Target: target.URL, ExportViaOTLP: true, Labels: map[string]string{"city": "berlin", "rack": "1"},
		OTLP: model.TargetOTLPConfig{LabelsAsResource: []string{"city"}, InstanceID: true},
	}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	resource := func(o *debugOTLP) string {
		var parts []string
		for _, a := range o.Resource {
			parts = append(parts, a.Key+"="+a.Value)
		}
		return strings.Join(parts, ",")
	}
	probe := postRun(t, server, map[string]any{"configured": "rooms", "target": target.URL}, http.StatusOK).OTLP
	if got := resource(probe); got != "service.name=prometheus-universal-exporter,deployment.environment=test" {
		t.Errorf("a probe's resource is %s", got)
	}
	if probe.Scope != (otlpScope{Name: "acme.pue", Version: "1.2"}) {
		t.Errorf("scope %+v", probe.Scope)
	}
	if p, ok := pointsByName(probe)["room_temperature_celsius{collector=rooms,room=kitchen,target="+target.URL+"}"]; !ok || !reflect.DeepEqual(p.Notes, []string{"attribute collector added (otlp.probe_attributes)", "attribute target added (otlp.probe_attributes)"}) {
		t.Errorf("a probe's point: %+v; points %v", p, keysOf(pointsByName(probe)))
	}
	playground := postRun(t, server, pasted(roomsYAML("rooms"), debugExampleBody), http.StatusOK).OTLP
	if got := resource(playground); got != resource(probe) {
		t.Errorf("the playground's resource is %s", got)
	}
	requireContains(t, strings.Join(playground.ResourceNotes, "\n"), "The playground is no collector of the configuration", "the run has no target, so no target attribute")
	if _, ok := pointsByName(playground)["room_temperature_celsius{collector=rooms,room=attic}"]; !ok {
		t.Errorf("the playground's points: %v", keysOf(pointsByName(playground)))
	}
	static := postRun(t, server, map[string]any{"configured": "rooms", "static_target": "berlin"}, http.StatusOK).OTLP
	if got := resource(static); got != "service.name=prometheus-universal-exporter,city=berlin,deployment.environment=test,service.instance.id=berlin" {
		t.Errorf("the static target's resource is %s", got)
	}
	p, ok := pointsByName(static)["room_temperature_celsius{rack=1,room=kitchen,static_target=berlin}"]
	if !ok || !reflect.DeepEqual(p.Notes, []string{"label city moved to the resource (otlp.labels_as_resource)"}) {
		t.Errorf("the static target's point: %+v; points %v", p, keysOf(pointsByName(static)))
	}
	requireContains(t, strings.Join(static.ResourceNotes, "\n"), "otlp.labels_as_resource moves city", "service.instance.id is the target's name")
	// Nothing of the runs is queued, and no start time is remembered.
	server.otlpMu.Lock()
	pending := len(server.otlpPending)
	server.otlpMu.Unlock()
	server.otlpStarts.mu.Lock()
	starts := len(server.otlpStarts.series)
	server.otlpStarts.mu.Unlock()
	if pending != 0 || starts != 0 {
		t.Errorf("the runs left %d resources waiting and %d start times", pending, starts)
	}
}

// Under otel naming, two counters whose names are one and a gauge and a
// counter of one name are left out as the export leaves them out, with the
// reasons its log gives.
func TestTheOTLPTabListsWhatTheExportLeavesOut(t *testing.T) {
	server := otlpDebugServer(t, "http://127.0.0.1:1/v1/metrics", func(c *model.OTLPConfig) { c.MetricNames = model.OTLPMetricNamesOTel })
	body := "# TYPE x_seconds counter\nx_seconds 1\n# TYPE x_seconds_total counter\nx_seconds_total 2\n# TYPE jobs gauge\njobs 3\n# TYPE jobs_total counter\njobs_total 4\n"
	otlp := postRun(t, server, pastedText(passthroughYAML, body), http.StatusOK).OTLP
	reasons := map[string]string{}
	for _, left := range otlp.LeftOut {
		reasons[left.Prometheus] = left.Reason
	}
	if !strings.HasPrefix(reasons["x_seconds"], otlpNamesMergedMessage+": x is exported for x_seconds_total") {
		t.Errorf("x_seconds left out for %q; all %v", reasons["x_seconds"], reasons)
	}
	if !strings.HasPrefix(reasons["jobs"], otlpNameClashMessage+": jobs is exported as sum") {
		t.Errorf("jobs left out for %q; all %v", reasons["jobs"], reasons)
	}
	if len(otlp.LeftOut) != 2 {
		t.Errorf("left out %+v", otlp.LeftOut)
	}
	clashes := strings.Join(otlp.Clashes, "\n")
	requireContains(t, clashes, otlpNamesMergedMessage+" metric=x prometheus_name=x_seconds_total left_out_prometheus_name=x_seconds left_out_points=1: "+errOTLPNamesMerged.Error(),
		otlpNameClashMessage+" metric=jobs kind=sum left_out_kind=gauge left_out_points=1")
	if strings.Contains(otlp.Body, `"asDouble": 1,`) || strings.Contains(otlp.Body, `"asDouble": 3`) {
		t.Errorf("the body has a point left out: %s", otlp.Body)
	}
}

// otlpWithoutTimes is a payload without its points' times and the
// exporter's own series and the Go runtime's, which an export has and a run
// does not. A time is replaced by what an export and a run of the same
// series share of it: its nanoseconds past the whole millisecond, none, as
// the queue keeps a time to the millisecond; and a start time by whether it
// is the point's time.
func otlpWithoutTimes(t *testing.T, raw []byte) otlpPayload {
	t.Helper()
	var payload otlpPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	times := func(at, start *string) {
		nanos, err := strconv.ParseInt(*at, 10, 64)
		if err != nil {
			t.Fatalf("a point's time %q: %v", *at, err)
		}
		switch *start {
		case "":
		case *at:
			*start = "the point's time"
		default:
			*start = "another time"
		}
		*at = "+" + strconv.FormatInt(nanos%int64(time.Millisecond), 10) + "ns"
	}
	var out otlpPayload
	for _, rm := range payload.ResourceMetrics {
		var metrics []otlpMetric
		for _, m := range rm.ScopeMetrics[0].Metrics {
			if strings.HasPrefix(m.Name, "http_exporter_") || strings.HasPrefix(m.Name, "go_") || strings.HasPrefix(m.Name, "process_") {
				continue
			}
			switch {
			case m.Gauge != nil:
				for i := range m.Gauge.DataPoints {
					times(&m.Gauge.DataPoints[i].TimeUnixNano, &m.Gauge.DataPoints[i].StartTimeUnixNano)
				}
			case m.Sum != nil:
				for i := range m.Sum.DataPoints {
					times(&m.Sum.DataPoints[i].TimeUnixNano, &m.Sum.DataPoints[i].StartTimeUnixNano)
				}
			case m.Histogram != nil:
				for i := range m.Histogram.DataPoints {
					times(&m.Histogram.DataPoints[i].TimeUnixNano, &m.Histogram.DataPoints[i].StartTimeUnixNano)
				}
			case m.Summary != nil:
				for i := range m.Summary.DataPoints {
					times(&m.Summary.DataPoints[i].TimeUnixNano, &m.Summary.DataPoints[i].StartTimeUnixNano)
				}
			}
			metrics = append(metrics, m)
		}
		if len(metrics) > 0 {
			rm.ScopeMetrics[0].Metrics = metrics
			out.ResourceMetrics = append(out.ResourceMetrics, rm)
		}
	}
	return out
}

// The body the tab shows is what a real export sends of the same series:
// a probe of a target, exported to a receiver, and a static target's
// scrape, each compared with the run of the same, but for the points'
// times, of which the precision, whole milliseconds, and whether a point
// starts at its time are compared, and the exporter's own series.
func TestTheOTLPTabsBodyIsWhatTheExportSends(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(kindsBody))
	}))
	defer target.Close()
	endpoint, received := otlpReceiver(t)
	passthrough := model.Collector{Name: "passthrough", Request: model.RequestConfig{Type: "http"}, Transform: model.TransformConfig{Type: "prometheus"}}
	cfg := &model.Config{Collectors: []model.Collector{passthrough}, OTLP: otlpConfig(endpoint)}
	cfg.OTLP.MetricNames = model.OTLPMetricNamesOTel
	cfg.OTLP.ProbeAttributes = true
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{
		Name: "berlin", Collector: "passthrough", Target: target.URL, ExportViaOTLP: true, Labels: map[string]string{"city": "berlin", "rack": "1"},
		OTLP: model.TargetOTLPConfig{LabelsAsResource: []string{"city"}, InstanceID: true},
	}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	exported := func() otlpPayload {
		t.Helper()
		server.exportOTLP(context.Background(), time.Minute)
		select {
		case payload := <-received:
			raw, _ := json.Marshal(payload)
			return otlpWithoutTimes(t, raw)
		case <-time.After(30 * time.Second):
			t.Fatal("no OTLP payload was delivered")
		}
		return otlpPayload{}
	}
	if probe := getPath(server, http.MethodGet, "/probe?collector=passthrough&target="+target.URL); probe.Code != http.StatusOK {
		t.Fatalf("the probe answered %d: %s", probe.Code, probe.Body)
	}
	sent := exported()
	run := postRun(t, server, map[string]any{"configured": "passthrough", "target": target.URL}, http.StatusOK).OTLP
	if shown := otlpWithoutTimes(t, []byte(run.Body)); !reflect.DeepEqual(shown, sent) {
		a, _ := json.Marshal(shown)
		b, _ := json.Marshal(sent)
		t.Errorf("the tab's body of a probe differs from the export:\ntab:    %s\nexport: %s", a, b)
	}
	server.scrapeStaticTargets(context.Background(), 0)
	sent = exported()
	run = postRun(t, server, map[string]any{"configured": "passthrough", "static_target": "berlin"}, http.StatusOK).OTLP
	if shown := otlpWithoutTimes(t, []byte(run.Body)); !reflect.DeepEqual(shown, sent) {
		a, _ := json.Marshal(shown)
		b, _ := json.Marshal(sent)
		t.Errorf("the tab's body of a static target differs from the export:\ntab:    %s\nexport: %s", a, b)
	}
}

// With otlp.logs, a run's log lines are the records the tee would make of
// them, those below otlp.logs.level said not to be exported; none is
// queued for the log export, and neither is the line that the run was
// served.
func TestTheOTLPTabShowsTheRunsLogRecords(t *testing.T) {
	receiver := newLogReceiver(t)
	server, _, _ := logExportServer(t, receiver, func(c *model.OTLPConfig) { c.Logs.Level = "info" })
	server.SetDebug(DebugEdit)
	failing := "name: failing\nrequest: {type: http}\ndecoder: {type: json}\ntransform: {type: jq}\nmetrics:\n  - {name: up, type: gauge, expression: .state, error_mode: fail}\n"
	answer := postRun(t, server, pasted(failing, `{"state": "broken"}`), http.StatusOK)
	logs := answer.OTLP.Logs
	if logs == nil || logs.Level != "info" {
		t.Fatalf("logs %+v", logs)
	}
	var failed *debugOTLPLogRecord
	for i, r := range logs.Records {
		if r.Body == "debug run failed" {
			failed = &logs.Records[i]
		}
	}
	if failed == nil {
		t.Fatalf("no record of the failure: %+v", logs.Records)
	}
	attributes := map[string]string{}
	for _, a := range failed.Attributes {
		attributes[a.Key] = a.Value
	}
	if failed.SeverityNumber != 17 || failed.SeverityText != "ERROR" || !failed.Exported || failed.Time == "" || attributes["collector"] != "failing" || !strings.Contains(attributes["error"], "broken") {
		t.Errorf("the failure's record is %+v", failed)
	}
	if !strings.Contains(logs.Body, `"resourceLogs": [`) || !strings.Contains(logs.Body, `"stringValue": "debug run failed"`) {
		t.Errorf("the logs body is %s", logs.Body)
	}
	// Nothing of the run reaches the log export: neither its lines nor the
	// line saying it was served, at info, which the tee takes at info.
	exportLogsNow(server)
	for _, r := range receiver.logRecords(t) {
		if strings.HasPrefix(text(r.Body), "debug run") {
			t.Errorf("the log export got %q", text(r.Body))
		}
	}
	// A warning under otlp.logs.level error is shown, and not exported.
	quiet, _, _ := logExportServer(t, receiver, func(c *model.OTLPConfig) { c.Logs.Level = "error" })
	quiet.SetDebug(DebugEdit)
	warning := strings.Replace(failing, "error_mode: fail", "error_mode: log", 1)
	records := postRun(t, quiet, pasted(warning, `{"state": "broken"}`), http.StatusOK).OTLP.Logs.Records
	if len(records) != 1 || records[0].SeverityText != "WARN" || records[0].Exported || records[0].Note != "not exported: below otlp.logs.level error" {
		t.Errorf("records at error %+v", records)
	}
}

// otlpTestRequest is an endpoint test as the page sends it.
func otlpTestRequest(t *testing.T, signal string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, debugOTLPTestPath, strings.NewReader(`{"signal": "`+signal+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Debug-Run", "otlp-test")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	return request
}

func postOTLPTest(t *testing.T, server *Server, signal string, code int) debugOTLPTestAnswer {
	t.Helper()
	recorder := serve(server, otlpTestRequest(t, signal))
	if recorder.Code != code {
		t.Fatalf("answered %d, want %d: %s", recorder.Code, code, recorder.Body)
	}
	var answer debugOTLPTestAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("%v: %s", err, recorder.Body)
	}
	return answer
}

// The request bodies are shown by the page's JSON viewer (jsonView): a tree
// whose objects and arrays fold behind a button that says whether it is
// open, made lazily, down to jsonOpenDepth at first, coloured by the
// theme's token colours in both themes, with Expand all and Collapse all
// beside Copy, which copies the body's text; every value goes in as text.
func TestTheOTLPTabsBodiesAreAJSONViewer(t *testing.T) {
	server := otlpDebugServer(t, "http://127.0.0.1:1/v1/metrics", func(c *model.OTLPConfig) { c.Logs.Enabled = true })
	page := getPage(t, server, "/debug")
	for _, body := range []struct{ id, copy string }{{"otlp-body", "otlp-body-copy"}, {"otlp-logs-body", "otlp-logs-copy"}} {
		requireContains(t, page,
			`<button type="button" class="secondary" id="`+body.copy+`">Copy</button><button type="button" class="secondary" data-json-all="open" aria-controls="`+body.id+`">Expand all</button><button type="button" class="secondary" data-json-all="close" aria-controls="`+body.id+`">Collapse all</button>`,
			`<div class="json-view" id="`+body.id+`"></div>`)
	}
	if strings.Contains(page, `<pre class="otlp-body"`) {
		t.Error("a body is still a <pre>")
	}
	requireContains(t, page,
		`.json-view { margin-top: 8px; padding: 10px 10px 10px 4px; max-height: 520px; overflow: auto;`,
		`overflow-wrap: anywhere; }`,
		`.json-view .jv-closed > .jv-kids, .json-view .jv-closed > .jv-tail { display: none; }`,
		`.json-view .jv-kids > .jv-node { content-visibility: auto;`,
		`.json-view button.jv-toggle:focus-visible { outline: 2px solid var(--accent);`,
		`.json-view .jv-key { color: var(--json-key); }`,
		`.json-view .jv-string { color: var(--json-string); }`,
		`.json-view .jv-number { color: var(--json-number); }`,
		`.json-view .jv-boolean { color: var(--json-boolean); }`,
		`.json-view .jv-null { color: var(--json-null); }`,
		`.json-view .jv-punct, .json-view .jv-summary { color: var(--json-punct); }`)
	// Each token has a colour in the light theme's rule, and its own in each
	// of the dark theme's two, the browser's preference and the picker's.
	themes := map[string]*regexp.Regexp{
		"light":            regexp.MustCompile(`:root \{ color-scheme: light dark; ([^}]*)\}`),
		"dark (preferred)": regexp.MustCompile(`:root:not\(\[data-theme="light"\]\) \{ color-scheme: dark; ([^}]*)\}`),
		"dark (chosen)":    regexp.MustCompile(`:root\[data-theme="dark"\] \{ color-scheme: dark; ([^}]*)\}`),
	}
	colours := map[string]map[string]string{}
	for theme, rule := range themes {
		m := rule.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no %s rule", theme)
		}
		colours[theme] = map[string]string{}
		for _, token := range []string{"key", "string", "number", "boolean", "null", "punct"} {
			c := regexp.MustCompile(`--json-` + token + `: (#[0-9a-f]{6});`).FindStringSubmatch(m[1])
			if c == nil {
				t.Errorf("the %s theme has no --json-%s", theme, token)
				continue
			}
			colours[theme][token] = c[1]
		}
	}
	if colours["dark (preferred)"]["key"] != colours["dark (chosen)"]["key"] || colours["light"]["key"] == colours["dark (chosen)"]["key"] {
		t.Errorf("the themes' colours %v", colours)
	}
	script := debugPageScript(page)
	requireContains(t, script,
		`const jsonOpenDepth = 4;`,
		`typeof value === "number" ? new JSONNumber(context && typeof context.source === "string" ? context.source : String(value)) : value);`,
		`const parts = text.split(/\n\n(?=[[{])/);`,
		`span.textContent = text;`,
		`toggle.type = "button";`,
		`toggle.setAttribute("aria-label", what);`,
		`const named = array ? undefined : [value.name, value.key].find((v) => typeof v === "string");`,
		`(array ? (entries.length === 1 ? " item" : " items") : (entries.length === 1 ? " key" : " keys"))`,
		`jsonSet(node, depth <= jsonOpenDepth);`,
		`if (open && !state.built) {`,
		`state.toggle.setAttribute("aria-expanded", String(open));`,
		`const until = performance.now() + 30;`,
		`if (next < todo.length) requestAnimationFrame(slice);`,
		`"This is not JSON (" + error.message + "), so it is shown as text."`,
		`pre.textContent = text;`,
		`button.addEventListener("click", () => jsonAll($(button.getAttribute("aria-controls")), button.dataset.jsonAll === "open"));`,
		`await navigator.clipboard.writeText(jsonTexts.get($(source)) || "");`,
		`jsonView($("otlp-body"), otlp.body);`,
		`jsonView($("otlp-logs-body"), logs.body);`)
	for _, never := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function"} {
		if strings.Contains(script, never) {
			t.Errorf("the script has %s", never)
		}
	}
}

// A folded node reads "{…} 2 keys · name," with the comma that parts it
// from the next at its end, after what is said of it; a folded OTLP
// attribute, an object of a "key" and a "value" of one *Value field, reads
// "key = value", the value's text put in as text.
func TestTheOTLPTabsFoldedNodesReadAsText(t *testing.T) {
	server := otlpDebugServer(t, "http://127.0.0.1:1/v1/metrics", nil)
	script := debugPageScript(getPage(t, server, "/debug"))
	requireContains(t, script,
		`fold.append(jsonSpan("jv-punct", "…" + close));`,
		"  }\n  fold.append(...comma());\n",
		`if (!jsonPlain(value) || Object.keys(value).length !== 2 || typeof value.key !== "string" || !jsonPlain(value.value)) return null;`,
		`if (fields.length !== 1 || !/^[a-z]+Value$/.test(fields[0])) return null;`,
		`if (field === "intValue" && typeof inner === "string" && /^-?\d+$/.test(inner)) shown = jsonSpan("jv-number", inner);`,
		`shown = jsonSpan("jv-summary", (field === "arrayValue" ? "[…] " : "{…} ") + values + (values === 1 ? " value" : " values"));`,
		`fold.append(jsonSpan("jv-summary", " "), jsonSpan("jv-hint", attribute.key), jsonSpan("jv-punct", " = "), attribute.value);`,
	)
	if strings.Contains(script, `fold.append(jsonSpan("jv-punct", "…" + close), ...comma(),`) {
		t.Error("a folded node's comma comes before its summary")
	}
}

// The endpoint test sends one empty export with the export's headers and
// reports what the endpoint answered: its status, its partialSuccess, a TLS
// error, a timeout and a 429, which it does not retry. It changes none of
// the export's statistics.
func TestTheOTLPEndpointTest(t *testing.T) {
	var requests atomic.Int32
	var status atomic.Int32
	var auth, body atomic.Value
	var slow atomic.Bool
	status.Store(http.StatusOK)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		auth.Store(r.Header.Get("Authorization") + " " + r.Header.Get("X-Honeycomb-Team"))
		body.Store(string(readOTLPBody(t, r)))
		if slow.Load() {
			time.Sleep(500 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"partialSuccess": {"rejectedDataPoints": "0", "errorMessage": "tenant over quota"}}`))
	}))
	defer endpoint.Close()
	server := otlpDebugServer(t, endpoint.URL+"/v1/metrics", func(c *model.OTLPConfig) {
		c.Headers = map[string]string{"Authorization": "Bearer s3cret", "x-honeycomb-team": "s3cret-team"}
		c.Timeout = model.Duration(200 * time.Millisecond)
	})
	server.SetDebug(DebugFull)
	answer := postOTLPTest(t, server, "metrics", http.StatusOK)
	if !answer.OK || answer.Status != "200 OK" || answer.Took == "" || answer.PartialSuccess == nil || answer.PartialSuccess.Message != "tenant over quota" {
		t.Errorf("answer %+v", answer)
	}
	if auth.Load() != "Bearer s3cret s3cret-team" || body.Load() != debugOTLPEmptyMetrics {
		t.Errorf("sent Authorization and X-Honeycomb-Team %q and %q", auth.Load(), body.Load())
	}
	// Every otlp.headers value is withheld, whatever its name, and what the
	// export sets itself is shown.
	if want := []string{"Authorization: " + fetch.Redacted, "Content-Encoding: gzip", "Content-Type: application/json", "X-Honeycomb-Team: " + fetch.Redacted}; !reflect.DeepEqual(answer.Sent, want) {
		t.Errorf("the headers sent are shown as %q, want %q", answer.Sent, want)
	}
	status.Store(http.StatusTooManyRequests)
	before := requests.Load()
	answer = postOTLPTest(t, server, "metrics", http.StatusOK)
	if answer.OK || answer.Status != "429 Too Many Requests" || !strings.Contains(answer.Outcome, "would retry") || requests.Load() != before+1 {
		t.Errorf("a 429: %+v, %d requests", answer, requests.Load()-before)
	}
	status.Store(http.StatusUnauthorized)
	if answer = postOTLPTest(t, server, "metrics", http.StatusOK); answer.OK || !strings.Contains(answer.Failure, "401") || !strings.HasPrefix(answer.Outcome, "refused") {
		t.Errorf("a 401: %+v", answer)
	}
	slow.Store(true)
	if answer = postOTLPTest(t, server, "metrics", http.StatusOK); answer.OK || !strings.Contains(answer.Failure, "Client.Timeout") {
		t.Errorf("a timeout: %+v", answer)
	}
	slow.Store(false)
	tlsEndpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer tlsEndpoint.Close()
	tlsServer := otlpDebugServer(t, tlsEndpoint.URL+"/v1/metrics", nil)
	tlsServer.SetDebug(DebugFull)
	if answer = postOTLPTest(t, tlsServer, "", http.StatusOK); answer.OK || !strings.Contains(answer.Failure, "certificate") || answer.Signal != "metrics" {
		t.Errorf("a TLS failure: %+v", answer)
	}
	for _, s := range []*Server{server, tlsServer} {
		s.otlp.mu.Lock()
		touched := s.otlp.successes+s.otlp.failures != 0 || s.otlp.retries != 0 || s.otlp.droppedLocked() != 0 || !s.otlp.history.last.started.IsZero() || len(s.otlp.history.failureEnds) != 0 || s.otlp.history.errorText != "" || s.otlp.endpoint != ""
		s.otlp.mu.Unlock()
		if touched {
			t.Error("an endpoint test changed the export's statistics")
		}
	}
}

// The logs endpoint is tested with an empty ExportLogsServiceRequest.
func TestTheOTLPEndpointTestOfTheLogsEndpoint(t *testing.T) {
	receiver := newLogReceiver(t)
	server, _, _ := logExportServer(t, receiver, nil)
	server.SetDebug(DebugFull)
	if answer := postOTLPTest(t, server, "logs", http.StatusOK); !answer.OK || answer.Signal != "logs" || !strings.HasSuffix(answer.Endpoint, "/v1/logs") {
		t.Errorf("answer %+v", answer)
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if len(receiver.paths) != 1 || receiver.paths[0] != "/v1/logs" || string(receiver.bodies[0]) != debugOTLPEmptyLogs {
		t.Errorf("received %q %q", receiver.paths, receiver.bodies)
	}
}

// The endpoint test is refused at edit, without OTLP, for the logs without
// otlp.logs, for the traces without otlp.traces, for a signal OTLP has not,
// without the run header, for another site, and while another
// is in progress.
func TestTheOTLPEndpointTestIsGuarded(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer endpoint.Close()
	server := otlpDebugServer(t, endpoint.URL+"/v1/metrics", nil)
	if answer := postOTLPTest(t, server, "metrics", http.StatusForbidden); !strings.Contains(answer.Error, "--web.debug=full") {
		t.Errorf("at edit: %+v", answer)
	}
	server.SetDebug(DebugFull)
	if answer := postOTLPTest(t, server, "logs", http.StatusConflict); !strings.Contains(answer.Error, "log export is off") {
		t.Errorf("logs off: %+v", answer)
	}
	if answer := postOTLPTest(t, server, "traces", http.StatusConflict); !strings.Contains(answer.Error, "trace export is off") {
		t.Errorf("traces off: %+v", answer)
	}
	postOTLPTest(t, server, "spans", http.StatusBadRequest)
	request := otlpTestRequest(t, "metrics")
	request.Header.Del("X-Debug-Run")
	if code := serve(server, request).Code; code != http.StatusForbidden {
		t.Errorf("without the header: %d", code)
	}
	request = otlpTestRequest(t, "metrics")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	if code := serve(server, request).Code; code != http.StatusForbidden {
		t.Errorf("from another site: %d", code)
	}
	if code := serve(server, httptest.NewRequest(http.MethodGet, debugOTLPTestPath, nil)).Code; code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", code)
	}
	server.debugOTLPTesting.Store(true)
	if answer := postOTLPTest(t, server, "metrics", http.StatusTooManyRequests); !strings.Contains(answer.Error, "in progress") {
		t.Errorf("a second test: %+v", answer)
	}
	server.debugOTLPTesting.Store(false)
	if requests.Load() != 0 {
		t.Errorf("a refused test sent %d requests", requests.Load())
	}
	off := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	off.SetDebug(DebugFull)
	if answer := postOTLPTest(t, off, "metrics", http.StatusConflict); !strings.Contains(answer.Error, "OTLP export is off") {
		t.Errorf("OTLP off: %+v", answer)
	}
	if answer := postOTLPTest(t, server, "metrics", http.StatusOK); !answer.OK || requests.Load() != 1 {
		t.Errorf("after the refusals: %+v", answer)
	}
}
