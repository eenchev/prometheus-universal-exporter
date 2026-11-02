package exporter

import (
	"fmt"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The trace export's queue (otlptraces.go) is cut by whole traces: what an
// export puts back, and a lower otlp.traces.max_pending_spans, drop the
// oldest traces whole, never the spans of one alone, which would leave
// spans whose parent is gone.

// queuedTrace is a trace of spans spans, each marked with name and its
// place, as the queue holds them.
func queuedTrace(name string, spans int) []otlpQueuedSpan {
	resource := &otlpSpanResource{key: "r", json: []byte(`{}`)}
	out := make([]otlpQueuedSpan, spans)
	for i := range out {
		out[i] = otlpQueuedSpan{resource: resource, span: fmt.Appendf(nil, "%s%d", name, i)}
	}
	return out
}

// pending is how many spans wait.
func (t *otlpTraces) pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.queue)
}

// queuedNames are the spans waiting, by their marks.
func queuedNames(t *otlpTraces) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, span := range t.queue {
		out = append(out, string(span.span))
	}
	return out
}

func TestALowerPendingBoundDropsTheOldestWholeTraces(t *testing.T) {
	traces := &otlpTraces{}
	for _, name := range []string{"a", "b", "c"} {
		traces.add(queuedTrace(name, 4), 100)
	}
	cfg := otlpConfig("http://127.0.0.1:1/v1/metrics")
	cfg.Traces = model.OTLPTracesConfig{Enabled: true, Endpoint: "http://127.0.0.1:1/v1/traces", MaxPendingSpans: 6}
	traces.configure(cfg)
	if got, want := queuedNames(traces), []string{"c0", "c1", "c2", "c3"}; !slices.Equal(got, want) {
		t.Errorf("under a bound of 6 the spans waiting are %v, want the newest trace whole, %v", got, want)
	}
	if got := traces.dropped[otlpTraceDropPendingCap]; got != 8 || traces.pending() != 4 {
		t.Errorf("%d spans counted dropped, want the two oldest traces' 8", got)
	}
}

// An export that delivered the first trace and the start of the second, and
// puts the rest back before a trace queued since, keeps what is left of the
// second as a trace of its own, and drops it whole when the bound has no
// room for it.
func TestARequeueDropsTheOldestWholeTraces(t *testing.T) {
	traces := &otlpTraces{}
	for _, name := range []string{"a", "b", "c"} {
		traces.add(queuedTrace(name, 3), 100)
	}
	drained, counts := traces.drain()
	traces.add(queuedTrace("d", 3), 100)
	// a0..a2 and b0 went through.
	traces.requeue(drained, counts, 4, 5)
	if got, want := queuedNames(traces), []string{"d0", "d1", "d2"}; !slices.Equal(got, want) {
		t.Errorf("the spans waiting are %v, want %v: the rest of b and c dropped whole", got, want)
	}
	if got := traces.dropped[otlpTraceDropPendingCap]; got != 5 {
		t.Errorf("%d spans counted dropped, want b's 2 left and c's 3", got)
	}
	traces = &otlpTraces{}
	for _, name := range []string{"a", "b", "c"} {
		traces.add(queuedTrace(name, 3), 100)
	}
	drained, counts = traces.drain()
	traces.requeue(drained, counts, 4, 6)
	if got, want := queuedNames(traces), []string{"b1", "b2", "c0", "c1", "c2"}; !slices.Equal(got, want) {
		t.Errorf("the spans waiting are %v, want %v", got, want)
	}
	if traces.dropped[otlpTraceDropPendingCap] != 0 || !slices.Equal(traces.traces, []int{2, 3}) {
		t.Errorf("within the bound: dropped %d, traces %v", traces.dropped[otlpTraceDropPendingCap], traces.traces)
	}
}
