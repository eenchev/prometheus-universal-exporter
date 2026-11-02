//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The traces of probes sharing one trip (probeflight.go): the trip's spans
// are in the trace of every probe that used it, the one that started it
// included, whether or not that one's caller stayed; and a trace that ends
// after a reload switched the export off is never sent.

// answerOnRelease answers value=3 once release is closed, and tells arrived of
// each request it is sent.
func answerOnRelease(t *testing.T) (target *httptest.Server, arrived chan struct{}, release func()) {
	t.Helper()
	arrived = make(chan struct{}, 16)
	held := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(held) }) }
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-held:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=3\n"))
	}))
	t.Cleanup(target.Close)
	t.Cleanup(release)
	return target, arrived, release
}

// waitOutcome waits for a probe's outcome.
func waitOutcome(t *testing.T, outcome <-chan probeOutcome, what string) probeOutcome {
	t.Helper()
	select {
	case o := <-outcome:
		return o
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never ended", what)
	}
	return probeOutcome{}
}

// traces are the spans received by trace id, each trace's root first.
func tracesOf(spans []receivedSpan) map[string][]receivedSpan {
	out := map[string][]receivedSpan{}
	for _, span := range spans {
		if span.ParentSpanID == "" {
			out[span.TraceID] = append([]receivedSpan{span}, out[span.TraceID]...)
		} else {
			out[span.TraceID] = append(out[span.TraceID], span)
		}
	}
	return out
}

func spanNamed(trace []receivedSpan, name string) (receivedSpan, bool) {
	for _, span := range trace {
		if span.Name == name {
			return span, true
		}
	}
	return receivedSpan{}, false
}

// Probe A starts a trip, B joins it, and A's caller leaves before the
// target answers: the trip goes on for B. B's trace has the trip whole —
// its stages and its request — under its shared_trip span; A's trace ends
// in error, saying its caller went away before its own trip ended, and has
// the request it sent, unfinished. The two have no span id in common and no
// span whose parent is not in its trace.
func TestASharedTripsSpansAreInTheTraceOfEveryProbeThatUsedIt(t *testing.T) {
	receiver := newLogReceiver(t)
	target, arrived, release := answerOnRelease(t)
	server := tracesServer(t, receiver, time.Millisecond, nil, nil)
	path := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	ctxA, leaveA := context.WithCancel(context.Background())
	defer leaveA()
	doneA := probeAsync(ctxA, server, path, nil)
	<-arrived
	doneB := probeAsync(context.Background(), server, path, nil)
	waitForWaiters(t, server, 2)
	leaveA()
	waitOutcome(t, doneA, "the probe whose caller left")
	release()
	if answer := waitOutcome(t, doneB, "the probe that joined"); answer.code != http.StatusOK {
		t.Fatalf("the probe that joined is answered %d", answer.code)
	}
	exportTracesNow(server)
	traces := tracesOf(receiver.spans(t))
	if len(traces) != 2 {
		t.Fatalf("%d traces were exported, want both probes'", len(traces))
	}
	var traceA, traceB []receivedSpan
	for _, trace := range traces {
		if _, ok := spanNamed(trace, "shared_trip"); ok {
			traceB = trace
		} else {
			traceA = trace
		}
	}
	if traceA == nil || traceB == nil {
		t.Fatalf("no trace or both have a shared_trip span: %+v", traces)
	}
	// B: the trip under shared_trip.
	shared, _ := spanNamed(traceB, "shared_trip")
	if shared.ParentSpanID != traceB[0].SpanID || traceB[0].Status != nil {
		t.Errorf("the joined probe's root %+v and shared_trip %+v", traceB[0].otlpSpan, shared.otlpSpan)
	}
	for _, name := range []string{"http", "decode", "transform", "validation"} {
		if span, ok := spanNamed(traceB, name); !ok || span.ParentSpanID != shared.SpanID || spanAttributes(span.otlpSpan)["stage.outcome"] != "ok" {
			t.Errorf("the joined probe's trace has the trip's %s stage as %+v (found %v), want it under shared_trip", name, span.otlpSpan, ok)
		}
	}
	httpB, _ := spanNamed(traceB, "http")
	getB, ok := spanNamed(traceB, "GET")
	if !ok || getB.ParentSpanID != httpB.SpanID || getB.Status != nil || spanAttributes(getB.otlpSpan)["http.response.status_code"] != "200" {
		t.Errorf("the joined probe's trace has the trip's request as %+v", getB.otlpSpan)
	}
	if attrs := spanAttributes(traceB[0].otlpSpan); attrs["response.bytes"] != "8" || attrs["series"] != "1" {
		t.Errorf("the joined probe's span says %v", attrs)
	}
	// A: abandoned, its unfinished request in its trace.
	rootA := traceA[0]
	if rootA.Status == nil || rootA.Status.Code != otlpStatusError || rootA.Status.Message != "the caller went away before the probe's trip ended" ||
		spanAttributes(rootA.otlpSpan)["error.type"] != "abandoned" {
		t.Errorf("the left probe's span: %+v", rootA.otlpSpan)
	}
	getA, ok := spanNamed(traceA, "GET")
	if !ok || getA.Status == nil || getA.Status.Code != otlpStatusError || !strings.HasPrefix(getA.Status.Message, "unfinished") ||
		spanAttributes(getA.otlpSpan)["request.unfinished"] != "true" || getA.EndTimeUnixNano > rootA.EndTimeUnixNano {
		t.Errorf("the left probe's trace has its request as %+v (found %v), want it unfinished, ending with the probe", getA.otlpSpan, ok)
	}
	if _, ok := spanAttributes(getA.otlpSpan)["http.response.status_code"]; ok {
		t.Errorf("the unfinished request has a status: %+v", getA.otlpSpan)
	}
	ids := map[string]bool{}
	for _, span := range append(slices.Clone(traceA), traceB...) {
		if ids[span.SpanID] {
			t.Errorf("span id %s is in both traces", span.SpanID)
		}
		ids[span.SpanID] = true
	}
	if orphans := orphanSpans(append(slices.Clone(traceA), traceB...)); len(orphans) > 0 {
		t.Errorf("spans without their parent: %v", orphans)
	}
}

// Probe A starts a trip and B joins it; B's caller leaves. A's trace has
// the trip whole, as a probe's own trip, and B's says it waited for an
// identical probe's trip, its shared_trip span cancelled, with what the
// trip had done by then under it.
func TestAProbeThatLeftAJoinedTripSaysItWaitedForIt(t *testing.T) {
	receiver := newLogReceiver(t)
	target, arrived, release := answerOnRelease(t)
	server := tracesServer(t, receiver, time.Millisecond, nil, nil)
	path := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	doneA := probeAsync(context.Background(), server, path, nil)
	<-arrived
	ctxB, leaveB := context.WithCancel(context.Background())
	defer leaveB()
	doneB := probeAsync(ctxB, server, path, nil)
	waitForWaiters(t, server, 2)
	leaveB()
	waitOutcome(t, doneB, "the probe whose caller left")
	release()
	if answer := waitOutcome(t, doneA, "the probe that started the trip"); answer.code != http.StatusOK {
		t.Fatalf("the probe that started the trip is answered %d", answer.code)
	}
	exportTracesNow(server)
	traces := tracesOf(receiver.spans(t))
	if len(traces) != 2 {
		t.Fatalf("%d traces were exported, want both probes'", len(traces))
	}
	for _, trace := range traces {
		root := trace[0]
		shared, joined := spanNamed(trace, "shared_trip")
		get, _ := spanNamed(trace, "GET")
		if !joined {
			httpA, _ := spanNamed(trace, "http")
			if root.Status != nil || httpA.ParentSpanID != root.SpanID || get.ParentSpanID != httpA.SpanID || get.Status != nil {
				t.Errorf("the starting probe's trace: root %+v, http %+v, GET %+v", root.otlpSpan, httpA.otlpSpan, get.otlpSpan)
			}
			continue
		}
		if root.Status == nil || root.Status.Message != "the caller went away while the probe waited for an identical probe's trip" ||
			spanAttributes(shared.otlpSpan)["stage.outcome"] != "cancelled" || shared.ParentSpanID != root.SpanID {
			t.Errorf("the left probe's trace: root %+v, shared_trip %+v", root.otlpSpan, shared.otlpSpan)
		}
		if get.Name != "GET" || spanAttributes(get.otlpSpan)["request.unfinished"] != "true" {
			t.Errorf("the left probe's trace has the trip's request as %+v, want it unfinished", get.otlpSpan)
		}
		if orphans := orphanSpans(trace); len(orphans) > 0 {
			t.Errorf("spans without their parent: %v", orphans)
		}
	}
}

// A probe in flight when a reload switches the trace export off ends after
// it: its trace is not queued, so a reload switching the export on again
// before the next export sends nothing of it.
func TestATraceEndedAfterAReloadSwitchedTracesOffIsNeverSent(t *testing.T) {
	receiver := newLogReceiver(t)
	target, arrived, release := answerOnRelease(t)
	document := func(on string) string {
		return "otlp:\n  enabled: true\n  endpoint: " + receiver.server.URL + "/v1/metrics\n  compression: none\n  traces:\n    enabled: " + on + "\n    threshold: 1ms\n" + testutil.MinimalConfig
	}
	r := newReloadable(t, document("true"), "")
	r.server = NewServer(r.manager, "python3", testutil.QuietLogger(t))
	collector := r.manager.Get().Collectors[0].Name
	done := probeAsync(context.Background(), r.server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil)
	<-arrived
	r.write(r.path, document("false"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	release()
	if answer := waitOutcome(t, done, "the probe"); answer.code != http.StatusOK {
		t.Fatalf("the probe is answered %d", answer.code)
	}
	if pending := r.server.otlpTraces.pending(); pending != 0 {
		t.Errorf("%d spans wait while the trace export is off", pending)
	}
	r.write(r.path, document("true"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	exportTracesNow(r.server)
	if spans := receiver.spans(t); len(spans) != 0 {
		t.Errorf("the probe that ended while the export was off was exported once it was on again: %d spans", len(spans))
	}
}
