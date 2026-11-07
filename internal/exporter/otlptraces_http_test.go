//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The traces of slow requests over OTLP (otlptraces.go): the spans a
// collector receives of a slow probe and of a failed one, their tree, ids,
// times and attributes, a static target's under its own resource, a
// caller's traceparent continued, the bound on what waits, the export's own
// lines kept out of the log export, the last export at shutdown, a reload
// switching it, and what it costs a probe while it is off.

// tracesPayload is an ExportTraceServiceRequest as OTLP/JSON writes it.
type tracesPayload struct {
	ResourceSpans []struct {
		Resource   otlpResource `json:"resource"`
		ScopeSpans []struct {
			Scope otlpScope  `json:"scope"`
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// traceRequests are the requests made to /v1/traces, decoded.
func (r *logReceiver) traceRequests(t *testing.T) []tracesPayload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []tracesPayload
	for i, path := range r.paths {
		if path != "/v1/traces" {
			continue
		}
		var payload tracesPayload
		decoder := json.NewDecoder(bytes.NewReader(r.bodies[i]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			t.Fatalf("a trace export does not decode: %v\n%s", err, r.bodies[i])
		}
		out = append(out, payload)
	}
	return out
}

// receivedSpans are the spans of every trace export, each with its
// resource's attributes as key=value and its scope.
type receivedSpan struct {
	otlpSpan
	resource []string
	scope    otlpScope
}

func (r *logReceiver) spans(t *testing.T) []receivedSpan {
	t.Helper()
	var out []receivedSpan
	for _, payload := range r.traceRequests(t) {
		for _, resource := range payload.ResourceSpans {
			var attrs []string
			for _, a := range resource.Resource.Attributes {
				attrs = append(attrs, a.Key+"="+a.Value.StringValue)
			}
			for _, scope := range resource.ScopeSpans {
				for _, span := range scope.Spans {
					out = append(out, receivedSpan{otlpSpan: span, resource: attrs, scope: scope.Scope})
				}
			}
		}
	}
	return out
}

// spanAttributes are a span's attributes as text by key.
func spanAttributes(span otlpSpan) map[string]string {
	out := map[string]string{}
	for _, a := range span.Attributes {
		out[a.Key] = debugAnyValue(a.Value)
	}
	return out
}

// tracesServer is a server whose OTLP export goes to the receiver, with
// otlp.traces on at threshold, and change made to its otlp block.
func tracesServer(t *testing.T, receiver *logReceiver, threshold time.Duration, change func(*model.Config), file *model.StaticTargetFile) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(receiver.server.URL + "/v1/metrics")}
	cfg.OTLP.Compression = model.OTLPCompressionNone
	cfg.OTLP.Traces = model.OTLPTracesConfig{Enabled: true, Threshold: model.Duration(threshold)}
	if change != nil {
		change(cfg)
	}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	return server
}

func exportTracesNow(server *Server) {
	server.exportOTLPTraces(context.Background(), server.manager.Get().OTLP, time.Minute, false)
}

// tracedTarget answers value=3 after wait, or with status when it is not 200.
func tracedTarget(t *testing.T, wait time.Duration, status int) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(wait)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("value=3\n"))
	}))
	t.Cleanup(target.Close)
	return target
}

var (
	hexTraceID = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hexSpanID  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	decimal    = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// A probe that took the threshold or longer is exported as a trace, and one
// that took less is not: a SERVER span for the probe, and under it an
// INTERNAL span for each stage of its trip, the fetch's CLIENT span under
// the fetch's stage. Every span is of one trace, with hex ids and times of
// decimal nanoseconds within the probe's; the probe's span carries the
// collector, the target as the log shows it, the request type, the series
// and the bytes, and the request's span OpenTelemetry's HTTP client
// attributes, its URL with the userinfo withheld and every query value
// masked, as an error in the log quotes one.
func TestTheTraceOfASlowProbeIsExported(t *testing.T) {
	receiver := newLogReceiver(t)
	slow := tracedTarget(t, 60*time.Millisecond, http.StatusOK)
	fast := tracedTarget(t, 0, http.StatusOK)
	server := tracesServer(t, receiver, 40*time.Millisecond, nil, nil)
	withSecrets := strings.Replace(slow.URL, "http://", "http://user:hunter2@", 1) + "/metrics?token=s3cret&tenant=a"
	if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(fast.URL), nil); answer.Code != http.StatusOK {
		t.Fatalf("the fast probe is answered %d", answer.Code)
	}
	if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(withSecrets), nil); answer.Code != http.StatusOK {
		t.Fatalf("the slow probe is answered %d", answer.Code)
	}
	exportTracesNow(server)
	spans := receiver.spans(t)
	if len(spans) == 0 {
		t.Fatal("no span was exported")
	}
	byName := map[string]receivedSpan{}
	var names []string
	for _, span := range spans {
		byName[span.Name] = span
		names = append(names, span.Name)
	}
	slices.Sort(names)
	if want := []string{"GET", "decode", "http", "http.connect", "http.first_byte", "probe text", "transform", "validation"}; !slices.Equal(names, want) {
		t.Fatalf("the spans are %v, want %v: the slow probe's alone", names, want)
	}
	root := byName["probe text"]
	if root.Kind != otlpSpanKindServer || root.ParentSpanID != "" || root.Status != nil {
		t.Errorf("the probe's span: %+v", root.otlpSpan)
	}
	for _, span := range spans {
		if span.TraceID != root.TraceID || !hexTraceID.MatchString(span.TraceID) || !hexSpanID.MatchString(span.SpanID) {
			t.Errorf("span %s has trace %q and id %q", span.Name, span.TraceID, span.SpanID)
		}
		if !decimal.MatchString(span.StartTimeUnixNano) || !decimal.MatchString(span.EndTimeUnixNano) || span.EndTimeUnixNano < span.StartTimeUnixNano || span.StartTimeUnixNano < root.StartTimeUnixNano || span.EndTimeUnixNano > root.EndTimeUnixNano {
			t.Errorf("span %s runs from %s to %s, the probe from %s to %s", span.Name, span.StartTimeUnixNano, span.EndTimeUnixNano, root.StartTimeUnixNano, root.EndTimeUnixNano)
		}
		if !slices.Equal(span.resource, []string{"service.name=prometheus-universal-exporter", "deployment.environment=test"}) || span.scope.Name != model.DefaultOTLPScopeName {
			t.Errorf("span %s is of the resource %v and the scope %+v", span.Name, span.resource, span.scope)
		}
	}
	for _, name := range []string{"http", "decode", "transform", "validation"} {
		if span := byName[name]; span.ParentSpanID != root.SpanID || span.Kind != otlpSpanKindInternal || spanAttributes(span.otlpSpan)["stage.outcome"] != "ok" {
			t.Errorf("the %s stage's span: %+v", name, span.otlpSpan)
		}
	}
	if start, _ := strconv.ParseInt(root.StartTimeUnixNano, 10, 64); start == 0 {
		t.Error("the probe's span has no start")
	}
	get := byName["GET"]
	if get.Kind != otlpSpanKindClient || get.ParentSpanID != byName["http"].SpanID {
		t.Errorf("the request's span is of kind %d under %s, want CLIENT under the http stage", get.Kind, get.ParentSpanID)
	}
	attrs := spanAttributes(root.otlpSpan)
	for key, want := range map[string]string{
		"http.request.method": "GET", "url.path": "/probe", "http.response.status_code": "200",
		"collector": "text", "target": fetch.DisplayTarget(&server.manager.Get().Collectors[0], withSecrets), "request.type": "http",
		"series": "1", "response.bytes": "8", "cache.hit": "false",
	} {
		if attrs[key] != want {
			t.Errorf("the probe's span has %s=%q, want %q", key, attrs[key], want)
		}
	}
	requireContains(t, attrs["target"], "tenant=a")
	host, port, _ := strings.Cut(strings.TrimPrefix(slow.URL, "http://"), ":")
	attrs = spanAttributes(get.otlpSpan)
	for key, want := range map[string]string{"http.request.method": "GET", "server.address": host, "server.port": port, "http.response.status_code": "200"} {
		if attrs[key] != want {
			t.Errorf("the request's span has %s=%q, want %q", key, attrs[key], want)
		}
	}
	full := attrs["url.full"]
	if strings.Contains(full, "hunter2") || strings.Contains(full, "s3cret") || strings.Contains(full, "tenant=a") || !strings.Contains(full, "/metrics?token=") {
		t.Errorf("url.full is %q: its credentials and query values are not withheld", full)
	}
	body := receiver.bodies[len(receiver.bodies)-1]
	if bytes.Contains(body, []byte("hunter2")) || bytes.Contains(body, []byte("s3cret")) {
		t.Errorf("the export carries a credential:\n%s", body)
	}
}

// A failed probe that took less than the threshold is exported with
// otlp.traces.errors alone, its span an ERROR with the probe's failure and
// error.type, and the failed request's span an ERROR with its status.
func TestTheTraceOfAFailedProbeIsExportedWithErrors(t *testing.T) {
	failing := tracedTarget(t, 0, http.StatusInternalServerError)
	for _, errorsToo := range []bool{false, true} {
		receiver := newLogReceiver(t)
		server := tracesServer(t, receiver, time.Hour, func(c *model.Config) { c.OTLP.Traces.Errors = errorsToo }, nil)
		if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(failing.URL), nil); answer.Code != http.StatusBadGateway {
			t.Fatalf("the probe is answered %d", answer.Code)
		}
		exportTracesNow(server)
		spans := receiver.spans(t)
		if !errorsToo {
			if len(spans) != 0 {
				t.Errorf("without errors, %d spans of a fast failed probe were exported", len(spans))
			}
			continue
		}
		var root, get *receivedSpan
		for i := range spans {
			switch spans[i].Name {
			case "probe text":
				root = &spans[i]
			case "GET":
				get = &spans[i]
			}
		}
		if root == nil || get == nil {
			t.Fatalf("with errors the spans are %+v", spans)
		}
		if root.Status == nil || root.Status.Code != otlpStatusError || !strings.Contains(root.Status.Message, "500") || spanAttributes(root.otlpSpan)["error.type"] != "502" || spanAttributes(root.otlpSpan)["http.response.status_code"] != "502" {
			t.Errorf("the failed probe's span: %+v", root.otlpSpan)
		}
		if get.Status == nil || get.Status.Code != otlpStatusError || spanAttributes(get.otlpSpan)["http.response.status_code"] != "500" || spanAttributes(get.otlpSpan)["error.type"] != "500" {
			t.Errorf("the failed request's span: %+v", get.otlpSpan)
		}
	}
}

// A request retried under request.retry is a CLIENT span for each attempt,
// with retry.attempt and http.request.resend_count from the second; and
// one of request.pagination a CLIENT span for each page, with its
// pagination.page. Both traces are kept by a sample_ratio of 1 under an
// hour's threshold, whatever they took: under a threshold of 1ms a fast
// local probe was sometimes not slow enough to be exported, and the test
// failed now and then.
func TestEveryAttemptAndPageIsASpan(t *testing.T) {
	var calls atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("value=3\n"))
	}))
	t.Cleanup(flaky.Close)
	pages := reposTarget(t, 3, nil)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Hour, func(c *model.Config) {
		c.OTLP.Traces.SampleRatio = 1
		retried := testutil.Collector("retried", "text")
		retried.Request.Retry = model.RetryConfig{Attempts: 2, Backoff: model.Duration(time.Millisecond)}
		retried.Request.Retry.Statuses = []string{"503"}
		c.Collectors = append(c.Collectors, retried, reposCollector(0))
	}, nil)
	for _, probe := range []string{"/probe?collector=retried&target=" + url.QueryEscape(flaky.URL), "/probe?collector=repos&target=" + url.QueryEscape(pages.URL)} {
		if answer := probeOnce(t, server, probe, nil); answer.Code != http.StatusOK {
			t.Fatalf("%s is answered %d: %s", probe, answer.Code, answer.Body)
		}
	}
	exportTracesNow(server)
	var attempts, paged []string
	for _, span := range receiver.spans(t) {
		if span.Kind != otlpSpanKindClient {
			continue
		}
		attrs := spanAttributes(span.otlpSpan)
		if strings.Contains(attrs["url.full"], "/orgs/acme/repos") {
			paged = append(paged, attrs["pagination.page"])
			continue
		}
		attempts = append(attempts, attrs["http.response.status_code"]+" attempt="+attrs["retry.attempt"]+" resend="+attrs["http.request.resend_count"])
	}
	if want := []string{"503 attempt= resend=", "503 attempt=1 resend=1", "200 attempt=2 resend=2"}; !slices.Equal(attempts, want) {
		t.Errorf("the attempts' spans are %v, want %v", attempts, want)
	}
	if want := []string{"1", "2", "3"}; !slices.Equal(paged, want) {
		t.Errorf("the pages' spans are %v, want %v", paged, want)
	}
}

// A static target's scrape is an INTERNAL span under the target's own
// resource, with the static target's name, as its series are exported; a
// target with no otlp block of its own is under the exporter-wide resource.
// otlp.exporter_instance names the replica on both, and otlp.scope is the
// spans' scope.
func TestAStaticTargetsTraceIsUnderItsOwnResource(t *testing.T) {
	slow := tracedTarget(t, 30*time.Millisecond, http.StatusOK)
	receiver := newLogReceiver(t)
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "own", ExportViaOTLP: true, Collector: "text", Target: slow.URL, OTLP: model.TargetOTLPConfig{ServiceName: "legacy", InstanceID: true}},
		{Name: "plain", Collector: "text", Target: slow.URL},
	}}
	server := tracesServer(t, receiver, 10*time.Millisecond, func(c *model.Config) {
		withExporterInstance("")(&c.OTLP)
		c.OTLP.Scope = &model.OTLPScopeConfig{Name: "traces-scope", Version: "2"}
	}, file)
	server.scrapeStaticTargets(context.Background(), 0)
	exportTracesNow(server)
	roots := map[string]receivedSpan{}
	for _, span := range receiver.spans(t) {
		if span.scope != (otlpScope{Name: "traces-scope", Version: "2"}) {
			t.Errorf("span %s is of the scope %+v", span.Name, span.scope)
		}
		if span.ParentSpanID == "" {
			roots[span.Name] = span
		}
	}
	own, plain := roots["scrape own"], roots["scrape plain"]
	if own.Kind != otlpSpanKindInternal || spanAttributes(own.otlpSpan)["static_target"] != "own" || plain.Kind != otlpSpanKindInternal {
		t.Fatalf("the scrapes' spans are %+v", roots)
	}
	if want := []string{"service.name=legacy", "deployment.environment=test", "pue.exporter.instance=replica-1", "service.instance.id=own"}; !slices.Equal(own.resource, want) {
		t.Errorf("the target's own resource is %v, want %v", own.resource, want)
	}
	if want := []string{"service.name=prometheus-universal-exporter", "deployment.environment=test", "pue.exporter.instance=replica-1"}; !slices.Equal(plain.resource, want) {
		t.Errorf("the plain target's resource is %v, want %v", plain.resource, want)
	}
}

// A probe whose caller sent a W3C traceparent is a span of the caller's
// trace, the caller's span its parent; one that cannot be read is no
// parent.
func TestAProbeContinuesItsCallersTrace(t *testing.T) {
	slow := tracedTarget(t, 20*time.Millisecond, http.StatusOK)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, 5*time.Millisecond, nil, nil)
	const traceID, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, header := range []string{"00-" + traceID + "-" + parent + "-01", "00-" + strings.ToUpper(traceID) + "-" + parent + "-01"} {
		probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(slow.URL), http.Header{"Traceparent": {header}})
	}
	exportTracesNow(server)
	var continued, own []string
	for _, span := range receiver.spans(t) {
		if span.Name != "probe text" {
			continue
		}
		if span.TraceID == traceID && span.ParentSpanID == parent {
			continued = append(continued, span.SpanID)
		} else if span.ParentSpanID == "" && span.TraceID != traceID {
			own = append(own, span.SpanID)
		}
	}
	if len(continued) != 1 || len(own) != 1 {
		t.Errorf("%d probes continued the caller's trace and %d began their own, want 1 and 1", len(continued), len(own))
	}
	for header, want := range map[string]bool{
		"00-" + traceID + "-" + parent + "-01": true, "01-" + traceID + "-" + parent + "-01-more": true,
		"ff-" + traceID + "-" + parent + "-01": false, "00-" + strings.Repeat("0", 32) + "-" + parent + "-01": false,
		"00-" + traceID + "-" + parent + "-01-more": false, "00-" + traceID + "-xyz-01": false, "": false,
	} {
		if got := parseTraceparent(header).ok; got != want {
			t.Errorf("traceparent %q read as a parent: %v, want %v", header, got, want)
		}
	}
}

// A trace that finds no room for all of its spans under
// otlp.traces.max_pending_spans is dropped whole and counted, in the
// self-metrics and on the status page, and the probe is answered as ever.
func TestATraceOverThePendingBoundIsDropped(t *testing.T) {
	// Each probe makes a connection of its own, so the two traces have the
	// same spans, the request's phases among them.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Connection", "close")
		_, _ = w.Write([]byte("value=3\n"))
	}))
	t.Cleanup(slow.Close)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) { c.OTLP.Traces.MaxPendingSpans = 8 }, nil)
	probeURL := "/probe?collector=text&target=" + url.QueryEscape(slow.URL)
	probeOnce(t, server, probeURL, nil)
	queued := server.otlpTraces.pending()
	if queued == 0 || queued > 8 {
		t.Fatalf("%d spans wait after one probe", queued)
	}
	if answer := probeOnce(t, server, probeURL, nil); answer.Code != http.StatusOK {
		t.Fatalf("the probe over the bound is answered %d", answer.Code)
	}
	if got := server.otlpTraces.pending(); got != queued {
		t.Errorf("%d spans wait after the second probe, want the first's %d alone", got, queued)
	}
	metrics := selfMetrics(t, server)
	requireContains(t, metrics, "http_exporter_otlp_spans_dropped_total "+strconv.Itoa(queued)+"\n")
	section := otlpSection(t, server).Traces
	if !section.Enabled || section.Dropped != queued || section.DroppedBy.PendingCap != queued || section.Pending != queued || section.MaxPending != 8 {
		t.Errorf("the status page's trace export: %+v", section.OTLPTracesStatusDetails)
	}
	exportTracesNow(server)
	metrics = selfMetrics(t, server)
	for _, want := range []string{`http_exporter_otlp_trace_exports_total{result="success"} 1`, `http_exporter_otlp_trace_exports_total{result="failure"} 0`, "http_exporter_otlp_spans_sent_total " + strconv.Itoa(queued)} {
		requireContains(t, metrics, want+"\n")
	}
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page, `<h3 id="otlp-traces">Trace export</h3>`)
	requireContains(t, page, "exported every 5s")
	requireContains(t, page, strconv.Itoa(queued)+" spans sent; 1 export succeeded, 0 failed")
}

// The trace export's own lines — of a failed export, and of its recovery —
// go to stderr alone and are never exported as log records: the trace
// export makes no line for the log export to send. Its spans are kept for
// the next export, which the last export at shutdown is, and which sends
// them.
func TestTheTraceExportLogsToStderrAloneAndFlushesAtShutdown(t *testing.T) {
	slow := tracedTarget(t, 10*time.Millisecond, http.StatusOK)
	receiver := newLogReceiver(t)
	server, _, stderr := logExportServer(t, receiver, func(o *model.OTLPConfig) {
		o.Traces = model.OTLPTracesConfig{Enabled: true, Endpoint: receiver.server.URL + "/v1/traces", Threshold: model.Duration(time.Millisecond)}
		o.Logs.Level = "debug"
	})
	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(slow.URL), nil)
	receiver.answer(http.StatusBadRequest)
	exportTracesNow(server)
	if !strings.Contains(stderr.String(), "OTLP endpoint refused a trace export") {
		t.Errorf("stderr has no line of the refused trace export:\n%s", stderr)
	}
	receiver.answer(http.StatusOK)
	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(slow.URL), nil)
	server.FlushOTLP()
	if len(receiver.spans(t)) == 0 {
		t.Error("the last export at shutdown sent no span")
	}
	for _, record := range receiver.logRecords(t) {
		if strings.Contains(text(record.Body), "trace export") {
			t.Errorf("a line of the trace export was exported as a log record: %s", text(record.Body))
		}
	}
	if pending := server.otlpTraces.pending(); pending != 0 {
		t.Errorf("%d spans wait after the last export", pending)
	}
}

// A reload that switches otlp.traces off drops the spans waiting and stops
// probes recording their traces; one that switches it on again starts them;
// the status page and the self-metrics follow.
func TestAReloadSwitchesTheOTLPTraceExport(t *testing.T) {
	receiver := newLogReceiver(t)
	slow := tracedTarget(t, 10*time.Millisecond, http.StatusOK)
	document := func(on string) string {
		return "otlp:\n  enabled: true\n  endpoint: " + receiver.server.URL + "/v1/metrics\n  compression: none\n  traces:\n    enabled: " + on + "\n    threshold: 1ms\n" + testutil.MinimalConfig
	}
	r := newReloadable(t, document("true"), "")
	r.server = NewServer(r.manager, "python3", testutil.QuietLogger(t))
	collector := r.manager.Get().Collectors[0].Name
	probeURL := "/probe?collector=" + collector + "&target=" + url.QueryEscape(slow.URL)
	probeOnce(t, r.server, probeURL, nil)
	if r.server.otlpTraces.pending() == 0 {
		t.Fatal("no span waits while the trace export is on")
	}
	r.write(r.path, document("false"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	probeOnce(t, r.server, probeURL, nil)
	if r.server.otlpTraces.queue != nil {
		t.Errorf("switched off, %d spans wait", len(r.server.otlpTraces.queue))
	}
	if section := otlpSection(t, r.server); section.Traces.Enabled {
		t.Errorf("the status page says the trace export is on: %+v", section.Traces)
	}
	requireContains(t, getPage(t, r.server, "/static-targets/status"), "Trace export is off.")
	if strings.Contains(selfMetrics(t, r.server), "http_exporter_otlp_trace_") || strings.Contains(selfMetrics(t, r.server), "http_exporter_otlp_spans_") {
		t.Errorf("the trace export's self-metrics are served while it is off")
	}
	r.write(r.path, document("true"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	probeOnce(t, r.server, probeURL, nil)
	exportTracesNow(r.server)
	roots := 0
	for _, span := range receiver.spans(t) {
		if span.ParentSpanID == "" {
			roots++
		}
	}
	if roots != 1 {
		t.Errorf("after switching it off and on the collector received %d traces, want the probe's since", roots)
	}
}

// While otlp.traces is off a probe records nothing of its trace: beginning
// it, and recording a stage, allocate nothing, so a probe allocates what it
// did without the trace export (BenchmarkProbe).
func TestTheTraceCostsAProbeNothingWhileOff(t *testing.T) {
	off := otlpConfig("http://127.0.0.1:1/v1/metrics")
	off.Traces = model.OTLPTracesConfig{Enabled: false, Endpoint: "http://127.0.0.1:1/v1/traces"}
	parent := context.Background()
	mark := time.Now()
	if got, _ := alloctest.Allocations(100, func() {
		ctx, spans := beginTrace(parent, off, traceParent{})
		stages := tripStagesFrom(ctx)
		stages.step("decode", "ok", mark, "json")
		stages.spans.fetched(10)
		spans.step("cache", "miss", mark, mark, "")
	}); got != 0 {
		t.Errorf("with the trace export off a probe's trace allocates %v times", got)
	}
	on := off
	on.Traces.Enabled = true
	if ctx, spans := beginTrace(parent, on, traceParent{}); spans == nil || tripSpansFrom(ctx) != spans {
		t.Error("with the trace export on a probe records no trace")
	}
}

// orphanSpans are the spans received whose parent is in none of the spans
// received, by name, but for a trace's root.
func orphanSpans(spans []receivedSpan) []string {
	ids := map[string]bool{}
	for _, span := range spans {
		ids[span.SpanID] = true
	}
	var out []string
	for _, span := range spans {
		if span.ParentSpanID != "" && !ids[span.ParentSpanID] {
			out = append(out, span.Name)
		}
	}
	return out
}

// An export that fails while another probe's trace is queued puts its
// spans back before that trace's, and the bound, which has no room for
// both, drops the oldest trace whole: the trace exported next is whole,
// with no span whose parent was dropped.
func TestAFailedExportPutsBackWholeTraces(t *testing.T) {
	slow := tracedTarget(t, 3*time.Millisecond, http.StatusOK)
	var failing atomic.Bool
	failing.Store(true)
	var during func()
	var once atomic.Bool
	receiver := &logReceiver{status: http.StatusOK}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := readOTLPBody(t, req)
		if failing.Load() {
			if once.CompareAndSwap(false, true) {
				during()
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		receiver.mu.Lock()
		receiver.paths = append(receiver.paths, req.URL.Path)
		receiver.bodies = append(receiver.bodies, body)
		receiver.mu.Unlock()
	}))
	t.Cleanup(receiver.server.Close)
	server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) { c.OTLP.Traces.MaxPendingSpans = 10 }, nil)
	probe := "/probe?collector=text&target=" + url.QueryEscape(slow.URL)
	during = func() { probeOnce(t, server, probe, nil) }
	probeOnce(t, server, probe, nil)
	first := server.otlpTraces.pending()
	if first < 6 || first > 10 {
		t.Fatalf("a probe's trace is %d spans; the test needs two not to fit under 10", first)
	}
	server.exportOTLPTraces(context.Background(), server.manager.Get().OTLP, 300*time.Millisecond, false)
	failing.Store(false)
	exportTracesNow(server)
	spans := receiver.spans(t)
	if orphans := orphanSpans(spans); len(orphans) > 0 {
		t.Errorf("the spans %v were exported without their parents", orphans)
	}
	roots := 0
	for _, span := range spans {
		if span.ParentSpanID == "" {
			roots++
		}
	}
	if roots != 1 || server.otlpTraces.dropped[otlpTraceDropPendingCap] != first {
		t.Errorf("%d traces exported and %d spans dropped, want the newer trace and the older's %d", roots, server.otlpTraces.dropped[otlpTraceDropPendingCap], first)
	}
}

// slowReader is a caller that takes its time to read its answer.
type slowReader struct{ *httptest.ResponseRecorder }

func (w slowReader) Write(b []byte) (int, error) {
	time.Sleep(300 * time.Millisecond)
	return w.ResponseRecorder.Write(b)
}

// A probe is timed to when its answer was ready, not to when its caller
// read it: a fast probe whose caller reads slowly is no slow probe, and
// its trace is not exported.
func TestASlowReaderMakesNoProbeSlow(t *testing.T) {
	fast := tracedTarget(t, 0, http.StatusOK)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, 200*time.Millisecond, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/probe?collector=text&target="+url.QueryEscape(fast.URL), nil)
	server.Handler().ServeHTTP(slowReader{httptest.NewRecorder()}, request)
	if pending := server.otlpTraces.pending(); pending != 0 {
		t.Errorf("a fast probe whose caller read slowly queued %d spans", pending)
	}
}

// spanNanos is a span's start and end.
func spanNanos(t *testing.T, span otlpSpan) (start, end int64) {
	t.Helper()
	start, err1 := strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
	end, err2 := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("span %s has times %q and %q", span.Name, span.StartTimeUnixNano, span.EndTimeUnixNano)
	}
	return start, end
}

// An HTTP request's span has under it the phases $timings measures:
// http.first_byte, from the request's start to the answer's first byte, and
// under it http.dns, http.connect and http.tls for the lookup, the
// connection and the handshake the request made, each within it. A target
// named by a name has its lookup, one over HTTPS its handshake, and a probe
// on the connection an earlier one left open has http.first_byte alone. A
// request whose connection is refused has the phase that went through, the
// lookup, under its span. A trip of a collector none of whose rules reads
// $timings is timed all the same while it is traced.
func TestAnHTTPRequestsSpanHasItsPhases(t *testing.T) {
	receiver := newLogReceiver(t)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("value=3\n"))
	}))
	t.Cleanup(target.Close)
	server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) {
		c.Collectors[0].Request.TLS.InsecureSkipVerify = true
		c.OTLP.Traces.Errors = true
	}, nil)
	if !server.manager.Get().Collectors[0].TimingsUnread {
		t.Fatal("the collector's rules read $timings; the test is of one whose do not")
	}
	// byName is the spans of the last trace exported, of the probe's span
	// root, by name.
	byName := func(root string) map[string]receivedSpan {
		t.Helper()
		exportTracesNow(server)
		spans := receiver.spans(t)
		trace := ""
		for _, span := range spans {
			if span.Name == root {
				trace = span.TraceID
			}
		}
		found := map[string]receivedSpan{}
		for _, span := range spans {
			if span.TraceID == trace {
				found[span.Name] = span
			}
		}
		return found
	}
	named := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(named), nil); answer.Code != http.StatusOK {
		t.Fatalf("the probe is answered %d: %s", answer.Code, answer.Body)
	}
	spans := byName("probe text")
	get, first := spans["GET"], spans["http.first_byte"]
	if first.ParentSpanID != get.SpanID || first.Kind != otlpSpanKindInternal || first.TraceID != get.TraceID {
		t.Fatalf("http.first_byte is %+v under %s, want INTERNAL under the request's span %s", first.otlpSpan, first.ParentSpanID, get.SpanID)
	}
	getStart, getEnd := spanNanos(t, get.otlpSpan)
	firstStart, firstEnd := spanNanos(t, first.otlpSpan)
	if firstStart < getStart || firstEnd > getEnd || firstEnd-firstStart < (20*time.Millisecond).Nanoseconds() {
		t.Errorf("http.first_byte runs from %d to %d, the request from %d to %d; want it within, the target's 20ms in it", firstStart, firstEnd, getStart, getEnd)
	}
	for _, phase := range []string{"http.dns", "http.connect", "http.tls"} {
		span, ok := spans[phase]
		if !ok {
			t.Errorf("no %s span", phase)
			continue
		}
		start, end := spanNanos(t, span.otlpSpan)
		if span.ParentSpanID != first.SpanID || start < firstStart || end > firstEnd || end < start {
			t.Errorf("%s runs from %d to %d under %s, want within http.first_byte, %d to %d, under it", phase, start, end, span.ParentSpanID, firstStart, firstEnd)
		}
	}
	if _, dnsEnd := spanNanos(t, spans["http.dns"].otlpSpan); dnsEnd > mustStart(t, spans["http.connect"].otlpSpan) {
		t.Error("the lookup ends after the connection begins")
	}

	// The connection the first probe left open.
	if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(named), nil); answer.Code != http.StatusOK {
		t.Fatalf("the second probe is answered %d", answer.Code)
	}
	spans = byName("probe text")
	if _, ok := spans["http.first_byte"]; !ok {
		t.Error("the request on a kept connection has no http.first_byte")
	}
	for _, phase := range []string{"http.dns", "http.connect", "http.tls"} {
		if _, ok := spans[phase]; ok {
			t.Errorf("the request on a kept connection has %s", phase)
		}
	}

	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := strings.Replace(refused.URL, "127.0.0.1", "localhost", 1)
	refused.Close()
	if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(refusedURL), nil); answer.Code == http.StatusOK {
		t.Fatal("the probe of a closed port is answered 200")
	}
	spans = byName("probe text")
	if dns, ok := spans["http.dns"]; !ok || dns.ParentSpanID != spans["GET"].SpanID {
		t.Errorf("the refused request's lookup is %+v, want under its span %s", dns.otlpSpan, spans["GET"].SpanID)
	}
	if _, ok := spans["http.first_byte"]; ok {
		t.Error("the refused request has http.first_byte")
	}
}

func mustStart(t *testing.T, span otlpSpan) int64 {
	t.Helper()
	start, _ := spanNanos(t, span)
	return start
}
