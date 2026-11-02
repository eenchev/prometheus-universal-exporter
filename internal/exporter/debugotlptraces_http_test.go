//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// The debug page's OTLP tab shows the run's spans, by the export's own
// code, and whether the export would send a probe's trace like it; the run
// itself queues nothing and sends nothing. The endpoint test sends an empty
// ExportTraceServiceRequest to the traces endpoint.
func TestTheOTLPTabShowsTheRunsSpans(t *testing.T) {
	receiver := newLogReceiver(t)
	slow := tracedTarget(t, 20*time.Millisecond, http.StatusOK)
	server := tracesServer(t, receiver, 5*time.Millisecond, nil, nil)
	server.SetDebug(DebugFull)
	page := getPage(t, server, debugPagePath)
	for _, want := range []string{`id="otlp-traces-part"`, `data-signal="traces"`, "Request body (ExportTraceServiceRequest)"} {
		requireContains(t, page, want)
	}
	answer := postRun(t, server, map[string]any{"configured": "text", "target": slow.URL}, http.StatusOK)
	traces := answer.OTLP.Traces
	if traces == nil {
		t.Fatalf("the tab has no spans: %+v", answer.OTLP)
	}
	if !traces.Exported || traces.Failed || !strings.Contains(traces.Why, "at least otlp.traces.threshold 5ms") {
		t.Errorf("the tab says: exported %v, failed %v, %s", traces.Exported, traces.Failed, traces.Why)
	}
	var tree []string
	for _, span := range traces.Spans {
		tree = append(tree, strings.Repeat("  ", span.Depth)+span.Name+" "+span.Kind)
	}
	want := []string{"probe text SERVER", "  http INTERNAL", "    GET CLIENT", "  decode INTERNAL", "  transform INTERNAL", "  validation INTERNAL"}
	if !slices.Equal(tree, want) {
		t.Errorf("the tab's spans are\n%s\nwant\n%s", strings.Join(tree, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(traces.Body, `"resourceSpans": [`) || !strings.Contains(traces.Body, `"name": "probe text"`) || !strings.Contains(traces.Body, `"kind": 3`) {
		t.Errorf("the body is not the ExportTraceServiceRequest:\n%s", traces.Body)
	}
	if n := len(server.otlpTraces.queue); n != 0 {
		t.Errorf("the run queued %d spans", n)
	}
	exportTracesNow(server)
	if n := len(receiver.spans(t)); n != 0 {
		t.Errorf("the run's trace was exported: %d spans", n)
	}
	test := postOTLPTest(t, server, "traces", http.StatusOK)
	if !test.OK || test.Body != `{"resourceSpans":[]}` || test.Endpoint != receiver.server.URL+"/v1/traces" {
		t.Errorf("the traces endpoint test: %+v", test)
	}
	receiver.mu.Lock()
	last := receiver.paths[len(receiver.paths)-1]
	receiver.mu.Unlock()
	if last != "/v1/traces" {
		t.Errorf("the endpoint test went to %s", last)
	}
	if strings.Contains(selfMetrics(t, server), `http_exporter_otlp_trace_exports_total{result="success"} 1`) {
		t.Error("the endpoint test counted as an export")
	}
}
