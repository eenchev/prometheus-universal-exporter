//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The sampling rules of otlp.traces end to end: the traces kept by the
// caller's sampled flag and by the ratio, with their sampling.reason and
// tracestate, the rate cap by the export's clock, the traces kept and
// dropped by why in the self-metrics and on the status page, the debug
// tab's line, and an export without the rules as it was before them.

const sampledCaller = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// A fast probe whose caller sampled it is kept by parent_based, its spans
// carrying the caller's tracestate as it came; one with no caller's is kept
// by a ratio of 1, its spans carrying ot=th:0; and the root of each says
// why. One whose caller did not sample it, at a ratio of 0, is not kept.
func TestTracesKeptByTheCallerAndTheRatioSayWhy(t *testing.T) {
	fast := tracedTarget(t, 0, http.StatusOK)
	probe := "/probe?collector=text&target=" + url.QueryEscape(fast.URL)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Hour, func(c *model.Config) {
		c.OTLP.Traces.ParentBased, c.OTLP.Traces.SampleRatio = true, 1
	}, nil)
	probeOnce(t, server, probe, http.Header{"Traceparent": {sampledCaller}, "Tracestate": {"rojo=00f067aa0ba902b7", "congo=t61rcWkgMzE"}})
	probeOnce(t, server, probe, nil)
	exportTracesNow(server)
	byTrace := map[string][]receivedSpan{}
	for _, span := range receiver.spans(t) {
		byTrace[span.TraceID] = append(byTrace[span.TraceID], span)
	}
	if len(byTrace) != 2 {
		t.Fatalf("%d traces were exported, want 2", len(byTrace))
	}
	for trace, spans := range byTrace {
		reason := spanAttributes(spans[0].otlpSpan)["sampling.reason"]
		want := map[string]string{"parent": "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE", "ratio": "ot=th:0"}[reason]
		if want == "" {
			t.Fatalf("trace %s is kept as %q", trace, reason)
		}
		if reason == "parent" && trace != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Errorf("the caller's trace is exported as %s", trace)
		}
		for _, span := range spans {
			if span.TraceState != want {
				t.Errorf("span %s of the %s trace carries tracestate %q, want %q", span.Name, reason, span.TraceState, want)
			}
		}
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_otlp_traces_kept_total{reason="parent"} 1`, `http_exporter_otlp_traces_kept_total{reason="ratio"} 1`, `http_exporter_otlp_traces_kept_total{reason="slow"} 0`, `http_exporter_otlp_traces_dropped_total{reason="not_sampled"} 0`} {
		requireContains(t, metrics, want+"\n")
	}

	receiver = newLogReceiver(t)
	server = tracesServer(t, receiver, time.Hour, func(c *model.Config) { c.OTLP.Traces.ParentBased = true }, nil)
	probeOnce(t, server, probe, http.Header{"Traceparent": {strings.TrimSuffix(sampledCaller, "01") + "00"}})
	probeOnce(t, server, probe, nil)
	exportTracesNow(server)
	if spans := receiver.spans(t); len(spans) != 0 {
		t.Errorf("%d spans of probes no rule keeps were exported", len(spans))
	}
	requireContains(t, selfMetrics(t, server), `http_exporter_otlp_traces_dropped_total{reason="not_sampled"} 2`+"\n")
}

// otlp.traces.max_per_second caps every trace kept, slow ones too, by the
// export's clock: a burst of the cap goes, the rest is dropped and counted
// as over the rate, and a second later as many go again. The status page
// shows the rules in force and the traces by why, as the JSON does.
func TestTheRateCapDropsKeptTracesOverIt(t *testing.T) {
	slow := tracedTarget(t, 2*time.Millisecond, http.StatusOK)
	probe := "/probe?collector=text&target=" + url.QueryEscape(slow.URL)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) {
		c.OTLP.Traces.MaxPerSecond, c.OTLP.Traces.SampleRatio = 2, 0.25
	}, nil)
	now := time.Unix(1_700_000_000, 0)
	server.otlpTraces.now = func() time.Time { return now }
	for range 3 {
		probeOnce(t, server, probe, nil)
	}
	now = now.Add(time.Second)
	probeOnce(t, server, probe, nil)
	exportTracesNow(server)
	roots := 0
	for _, span := range receiver.spans(t) {
		if span.ParentSpanID == "" {
			roots++
			if reason := spanAttributes(span.otlpSpan)["sampling.reason"]; reason != "slow" || span.TraceState != "" {
				t.Errorf("a slow trace says sampling.reason %q and tracestate %q", reason, span.TraceState)
			}
		}
	}
	if roots != 3 {
		t.Errorf("%d traces were exported, want 3: two of the burst and one a second later", roots)
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_otlp_traces_kept_total{reason="slow"} 3`, `http_exporter_otlp_traces_dropped_total{reason="rate"} 1`, `http_exporter_otlp_traces_dropped_total{reason="queue"} 0`} {
		requireContains(t, metrics, want+"\n")
	}
	section := otlpSection(t, server).Traces
	if d := section.OTLPTracesStatusDetails; d == nil || d.MaxPerSecond != 2 || d.SampleRatio != 0.25 || d.ParentBased || d.Threshold != "1ms" ||
		d.TracesKept != (otlpTracesKept{Slow: 3}) || d.TracesDropped != (otlpTracesDropped{Rate: 1}) {
		t.Errorf("the status page's trace export: %+v", section.OTLPTracesStatusDetails)
	}
	raw := getPath(server, http.MethodGet, "/static-targets/status?format=json").Body.String()
	for _, want := range []string{`"parent_based":false`, `"sample_ratio":0.25`, `"max_per_second":2`, `"traces_kept_by_reason":{"slow":3,"error":0,"parent":0,"ratio":0}`, `"traces_dropped_by_reason":{"not_sampled":0,"rate":1,"queue":0}`} {
		requireContains(t, raw, want)
	}
	page := getPage(t, server, "/static-targets/status")
	for _, want := range []string{
		"and 25% of the others by their trace ids, exported every 5s, at most 2 traces a second.",
		"<tr><th>Sampling rules</th><td>threshold 1ms; errors false; parent_based false; sample_ratio 25%; max_per_second 2</td></tr>",
		"<tr><th>Traces kept</th><td>3 slow, 0 failed, 0 sampled by the caller, 0 sampled by the ratio</td></tr>",
		"<tr><th>Traces dropped</th><td>0 not sampled, 1 over the rate cap, 0 with no room in the queue</td></tr>",
	} {
		requireContains(t, page, want)
	}
}

// A trace the queue has no room for is counted as dropped for the queue,
// as a whole trace, beside its spans.
func TestATraceWithNoRoomIsCountedAsATrace(t *testing.T) {
	slow := tracedTarget(t, 2*time.Millisecond, http.StatusOK)
	probe := "/probe?collector=text&target=" + url.QueryEscape(slow.URL)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) { c.OTLP.Traces.MaxPendingSpans = 8 }, nil)
	probeOnce(t, server, probe, nil)
	probeOnce(t, server, probe, nil)
	metrics := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_otlp_traces_kept_total{reason="slow"} 2`, `http_exporter_otlp_traces_dropped_total{reason="queue"} 1`} {
		requireContains(t, metrics, want+"\n")
	}
}

// Without parent_based and sample_ratio, a trace is exported as it was
// before them: its root says no sampling.reason, and no span has a
// traceState, whatever max_per_second says.
func TestWithoutSamplingRulesTheTraceIsAsBefore(t *testing.T) {
	slow := tracedTarget(t, 2*time.Millisecond, http.StatusOK)
	probe := "/probe?collector=text&target=" + url.QueryEscape(slow.URL)
	for _, perSecond := range []int{0, 100} {
		receiver := newLogReceiver(t)
		server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) { c.OTLP.Traces.MaxPerSecond = perSecond }, nil)
		probeOnce(t, server, probe, http.Header{"Traceparent": {sampledCaller}})
		exportTracesNow(server)
		receiver.mu.Lock()
		body := bytes.Join(receiver.bodies, nil)
		receiver.mu.Unlock()
		if !bytes.Contains(body, []byte(`"name":"probe text"`)) || bytes.Contains(body, []byte("traceState")) || bytes.Contains(body, []byte("sampling.reason")) {
			t.Errorf("max_per_second %d: the export is not as it was:\n%s", perSecond, body)
		}
	}
}

// The debug tab's line says which rule would keep the run's trace, with the
// ratio's randomness and threshold, or why none would, and that the rate
// cap is not taken by a run.
func TestTheOTLPTabSaysWhichRuleWouldKeepTheRun(t *testing.T) {
	fast := tracedTarget(t, 0, http.StatusOK)
	for _, test := range []struct {
		ratio    float64
		perSec   int
		exported bool
		reason   string
		says     []string
	}{
		{1, 5, true, "ratio", []string{"it was sampled at 100% by otlp.traces.sample_ratio (trace id randomness 0x", " ≥ threshold 0x00000000000000); it took ", ", unless otlp.traces.max_per_second 5 is spent, which a run does not take part in"}},
		{0, 0, false, "", []string{"under otlp.traces.threshold 1h0m0s; it has no caller's traceparent for otlp.traces.parent_based"}},
	} {
		receiver := newLogReceiver(t)
		server := tracesServer(t, receiver, time.Hour, func(c *model.Config) {
			c.OTLP.Traces.SampleRatio, c.OTLP.Traces.MaxPerSecond, c.OTLP.Traces.ParentBased = test.ratio, test.perSec, true
		}, nil)
		server.SetDebug(DebugFull)
		traces := postRun(t, server, map[string]any{"configured": "text", "target": fast.URL}, http.StatusOK).OTLP.Traces
		if traces == nil || traces.Exported != test.exported || traces.Reason != test.reason || traces.SampleRatio != test.ratio || traces.MaxPerSecond != test.perSec || !traces.ParentBased {
			t.Fatalf("ratio %v: the tab says %+v", test.ratio, traces)
		}
		for _, want := range test.says {
			requireContains(t, traces.Why, want)
		}
		if test.exported && !strings.Contains(traces.Body, `"traceState": "ot=th:0"`) {
			t.Errorf("the ratio's spans in the tab carry no threshold:\n%s", traces.Body)
		}
		if n := server.otlpTraces.tally.kept[keptRatio].Load() + server.otlpTraces.tally.dropped[traceDroppedNotSampled].Load(); n != 0 {
			t.Errorf("the run was counted %d times by the export", n)
		}
	}
}
