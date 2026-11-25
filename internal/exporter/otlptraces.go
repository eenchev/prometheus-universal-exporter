package exporter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// With otlp.traces, some probes and static target scrapes are exported over
// OTLP as a trace: those that took otlp.traces.threshold or longer, with
// otlp.traces.errors those that failed, and those the other sampling rules
// keep (otlptracesampling.go). A trace has a span for the probe or the
// scrape, and under it one span for each stage of its trip and each request
// it sent. The export is OTLP/HTTP with a JSON body, sent like the metrics
// and the log records (deliverOTLPOf). It uses the otlp block's headers,
// TLS, compression, timeout, retries and batch bounds, under the same scope
// and resource as the metrics.
//
// Which probes are slow is known only once they have ended, so the trace is
// decided at the end (tail sampling). While the export is on, every probe
// and scrape records its stages and requests in memory, like the times the
// debug report shows for a run (tripStages, fetch.WithSpanTrace). At its
// end, the spans are built from them if it is to be exported; otherwise the
// record is forgotten. Nothing is recorded for a probe while the export is
// off: its trip carries no recorder, and the stages ask for none.
//
// A debug run makes the same record (runDebug). The debug page's OTLP tab
// shows the spans built from it by the same code (debugotlp.go), and whether
// the export would send them. The export never does: it never sees a run,
// just as it never sees a run's series or log.
//
// The spans wait in a queue bounded by their count,
// otlp.traces.max_pending_spans. A trace that finds no room for all of its
// spans is dropped whole and counted, rather than waited for or cut. When a
// failed export puts spans back, or a reload sets a lower bound, the oldest
// traces are dropped whole. The queue is never cut inside a trace. A probe
// never waits for the export: the queue's lock is held only to append to
// it, and the queue is sent every otlp.traces.interval in the background.
// The export logs its outcome to stderr only, like the log export
// (otlplogs.go), and readiness does not depend on it.

// Span kinds and status codes as OTLP/JSON writes them: numbers.
const (
	otlpSpanKindInternal = 1
	otlpSpanKindServer   = 2
	otlpSpanKindClient   = 3

	otlpStatusError = 2
)

// otlpSpan is an OTLP Span as OTLP/JSON writes it. Its trace and span ids
// are hex, as OTLP/JSON writes them where protobuf's JSON mapping would
// write base64, and its times decimal text, as every int64 is.
type otlpSpan struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
	// TraceState is the trace's W3C tracestate, its sampling threshold
	// among it where the ratio kept it (spanTraceState).
	TraceState        string             `json:"traceState,omitempty"`
	ParentSpanID      string             `json:"parentSpanId,omitempty"`
	Name              string             `json:"name"`
	Kind              int                `json:"kind"`
	StartTimeUnixNano string             `json:"startTimeUnixNano"`
	EndTimeUnixNano   string             `json:"endTimeUnixNano"`
	Attributes        []otlpAnyAttribute `json:"attributes,omitempty"`
	Status            *otlpSpanStatus    `json:"status,omitempty"`
}

// otlpSpanStatus is a span's status: ERROR with what failed. A span that
// did not fail has none, which is UNSET, as OpenTelemetry's conventions
// leave it.
type otlpSpanStatus struct {
	Message string `json:"message,omitempty"`
	Code    int    `json:"code"`
}

// tripSpans is what a probe or a scrape records for its trace: its stages,
// with their times, and the requests its trip sent (requests). A trip shared
// by several probes records into its own recorder (probeFlight.spans), not
// into that of the probe that started it, which may leave before the trip
// ends. Each probe that used the trip copies the trip's record into its own
// (adopt), so its trace has the trip's spans under its own span ids. What a
// probe records after its record was closed is dropped (done).
type tripSpans struct {
	mu       sync.Mutex
	stages   []spanStage
	requests *fetch.RequestTrace
	// adopted are the requests of a shared trip the probe used, copied
	// from the trip's record.
	adopted []fetch.TracedRequest
	// bytes is the size of the response the trip fetched, -1 when it
	// fetched none.
	bytes int
	done  bool
	// link is the probe's or the scrape's trace, as known to what links to
	// it (otlptraceslink.go). A shared trip's own record has none.
	// sentUnder is the trace named by its requests' traceparent: that of the
	// probe that started it.
	link, sentUnder *traceLink
}

// traceLink is the record's link, nil for none or no record.
func (t *tripSpans) traceLink() *traceLink {
	if t == nil {
		return nil
	}
	return t.link
}

// unfinishedFact marks a request still unanswered when the probe whose
// trace has it was given up by its caller (adopt).
const unfinishedFact = "request.unfinished"

// adopt copies into t (a probe's record) what trip (the record of a shared
// trip the probe used) holds: its stages, its requests and the size of the
// response it fetched. A stage that names no parent of its own is put under
// the stage named under, if that is set. If the probe's caller left before
// the trip ended, the probe copies what the trip had recorded by cut: the
// stages that had ended, and its requests. A request still unanswered is
// marked unfinished (unfinishedFact) and failed, and ends at cut. A zero cut
// means the trip ended. t and trip may be nil.
func (t *tripSpans) adopt(trip *tripSpans, under string, cut time.Time) {
	if t == nil || trip == nil {
		return
	}
	trip.mu.Lock()
	stages, bytes := append([]spanStage(nil), trip.stages...), trip.bytes
	trip.mu.Unlock()
	requests := trip.requests.Requests()
	for i := range stages {
		if stages[i].parent == "" {
			stages[i].parent = under
		}
	}
	for i := range requests {
		r := &requests[i]
		// A request was sent under a span id of the trace of the probe that
		// started the trip (otlp.traces.propagate): only that probe's span
		// of it has the id, every other probe's one of its own.
		if trip.sentUnder != t.link {
			r.SpanID = [8]byte{}
		}
		if cut.IsZero() || r.Outcome != "" {
			continue
		}
		r.Failed, r.Duration = true, max(cut.Sub(r.Start()), 0)
		r.Facts = append(append([]fetch.TraceFact(nil), r.Facts...), fetch.TraceFact{Key: unfinishedFact, Kind: fetch.TraceFactBool, Bool: true})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.stages = append(t.stages, stages...)
	t.adopted = append(t.adopted, requests...)
	if bytes >= 0 {
		t.bytes = bytes
	}
}

// requestUnfinished says whether r was still unanswered when its probe's
// caller left (adopt).
func requestUnfinished(r fetch.TracedRequest) bool {
	for _, fact := range r.Facts {
		if fact.Key == unfinishedFact && fact.Kind == fetch.TraceFactBool && fact.Bool {
			return true
		}
	}
	return false
}

// spanStage is one stage of a trip: its name, as the debug report's stages
// have it, how it ended and its note, when it began and when it ended.
// parent, when set, names the stage it ran within.
type spanStage struct {
	name, outcome, note, parent string
	start, end                  time.Time
}

type tripSpansKey struct{}

// withTripSpans returns a context whose trip records its stages and
// requests for its trace, and the recorder. requests is the request trace
// of a debug run, which already records the requests. If it is nil, one is
// made that keeps no headers (fetch.WithSpanTrace).
func withTripSpans(ctx context.Context, requests *fetch.RequestTrace) (context.Context, *tripSpans) {
	if requests == nil {
		ctx, requests = fetch.WithSpanTrace(ctx)
	}
	t := &tripSpans{requests: requests, bytes: -1}
	return context.WithValue(ctx, tripSpansKey{}, t), t
}

// tripSpansFrom is the recorder of the trip ctx is for, nil when its trace
// is not recorded.
func tripSpansFrom(ctx context.Context) *tripSpans {
	t, _ := ctx.Value(tripSpansKey{}).(*tripSpans)
	return t
}

// step records a stage that ran from start to end; t may be nil.
func (t *tripSpans) step(stage, outcome string, start, end time.Time, note string) {
	t.add(spanStage{name: stage, outcome: outcome, note: note, start: start, end: end})
}

func (t *tripSpans) add(stage spanStage) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.done {
		t.stages = append(t.stages, stage)
	}
}

// fetched records the size of the response the trip fetched; t may be nil.
func (t *tripSpans) fetched(bytes int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bytes = bytes
}

// discard ends the record of a trace not exported: what the trip records
// from now on is dropped, nothing is copied, and nothing links to it.
func (t *tripSpans) discard() {
	t.mu.Lock()
	t.done = true
	t.mu.Unlock()
	t.link.settle(false, nil, nil)
}

// close ends the record and returns it: what the trip records from now on
// is dropped.
func (t *tripSpans) close() ([]spanStage, []fetch.TracedRequest, int) {
	t.mu.Lock()
	t.done = true
	stages, bytes, adopted := t.stages, t.bytes, t.adopted
	t.mu.Unlock()
	return stages, append(t.requests.Requests(), adopted...), bytes
}

// tripStages are where a trip's stages are recorded: the debug report's
// trace, the trace's recorder, both or neither. collect records each stage
// once, here, so the report and the spans have the same times.
type tripStages struct {
	trace *probeTrace
	spans *tripSpans
}

func tripStagesFrom(ctx context.Context) tripStages {
	return tripStages{trace: probeTraceFrom(ctx), spans: tripSpansFrom(ctx)}
}

// step records a stage that began at mark and ends now.
func (s tripStages) step(stage, outcome string, mark time.Time, note string) {
	if s.trace == nil && s.spans == nil {
		return
	}
	now := time.Now()
	s.trace.step(stage, outcome, now.Sub(mark), note)
	s.spans.step(stage, outcome, mark, now, note)
}

// scripts records, for the trace, the time the transform's Python scripts
// ran, as added up by timer. It records it as a python stage within the
// transform that began at mark. The scripts' runs have no times of their
// own to tell them apart, so the stage is their sum, from the transform's
// start.
func (s tripStages) scripts(timer *transform.ScriptTimer, mark time.Time) {
	if s.spans == nil {
		return
	}
	if seconds, ran := timer.Seconds(); ran {
		took := time.Duration(seconds * float64(time.Second))
		s.spans.add(spanStage{name: "python", outcome: "ok", note: "the Python scripts' time, added up", parent: "transform", start: mark, end: mark.Add(took)})
	}
}

// spanRoot is the span of the probe or the scrape itself.
type spanRoot struct {
	name       string
	kind       int
	start, end time.Time
	attributes []otlpAnyAttribute
	// failed says the probe or the scrape failed, message why and
	// errorType where, its error.type.
	failed             bool
	message, errorType string
	// parent is the span of a caller that sent a traceparent, whose trace
	// the probe's continues.
	parent traceParent
	// traceID is the trace's id, which the spans then belong to, and spanID
	// is the root span's id. Both are made when the trace began (traceLink);
	// if they are zero, buildSpans makes them. traceState is the spans'
	// tracestate, and reason, when set, is the root's sampling.reason.
	traceID    [16]byte
	spanID     [8]byte
	traceState string
	reason     string
}

// traceParent is a W3C traceparent's trace id and span id, and whether its
// sampled flag is set; ok says there was one that could be read. state is
// the tracestate sent with it, which is read only beside one that could be.
type traceParent struct {
	traceID [16]byte
	spanID  [8]byte
	sampled bool
	ok      bool
	state   traceState
}

// callerTrace is the trace a probe's caller sent, by its traceparent and
// tracestate headers.
func callerTrace(h http.Header) traceParent {
	p := parseTraceparent(h.Get("traceparent"))
	if p.ok {
		p.state = parseTraceState(h.Values("tracestate"))
	}
	return p
}

// parseTraceparent reads a W3C traceparent header: version-traceid-
// parentid-flags, each lowercase hex. A version this exporter does not know
// is read like version 00, as the W3C rules for a newer version ask, except
// ff, which is not a version. An id of all zeros is no id. A header that
// cannot be read gives no parent, and the probe begins its own trace.
func parseTraceparent(header string) traceParent {
	if header == "" {
		return traceParent{}
	}
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || parts[0] == "ff" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return traceParent{}
	}
	if parts[0] == "00" && len(parts) != 4 {
		return traceParent{}
	}
	for _, part := range parts[:4] {
		if strings.ToLower(part) != part {
			return traceParent{}
		}
	}
	var p traceParent
	if _, err := hex.Decode(p.traceID[:], []byte(parts[1])); err != nil {
		return traceParent{}
	}
	if _, err := hex.Decode(p.spanID[:], []byte(parts[2])); err != nil {
		return traceParent{}
	}
	flags, err := hex.DecodeString(parts[0] + parts[3])
	if err != nil {
		return traceParent{}
	}
	p.sampled = flags[1]&1 == 1
	if p.traceID == [16]byte{} || p.spanID == [8]byte{} {
		return traceParent{}
	}
	p.ok = true
	return p
}

// newTraceID and newSpanID make trace and span ids from crypto/rand, which a trace's
// receiver tells traces apart by, and which must not repeat across the
// replicas of an exporter.
func newTraceID() [16]byte {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return id
}

func newSpanID() [8]byte {
	var id [8]byte
	_, _ = rand.Read(id[:])
	return id
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func intAttribute(key string, value int) otlpAnyAttribute {
	return otlpAnyAttribute{Key: key, Value: otlpAnyValue{IntValue: strconv.Itoa(value)}}
}

func stringAttribute(key, value string) otlpAnyAttribute {
	return otlpAnyAttribute{Key: key, Value: otlpString(value)}
}

func boolAttribute(key string, value bool) otlpAnyAttribute {
	return otlpAnyAttribute{Key: key, Value: otlpAnyValue{BoolValue: &value}}
}

// factAttribute is a request's fact (fetch.TracedRequest.Facts) as an
// attribute of the value its kind is.
func factAttribute(fact fetch.TraceFact) otlpAnyAttribute {
	switch fact.Kind {
	case fetch.TraceFactBool:
		return boolAttribute(fact.Key, fact.Bool)
	case fetch.TraceFactDouble:
		value := fact.Double
		return otlpAnyAttribute{Key: fact.Key, Value: otlpAnyValue{DoubleValue: &value}}
	case fetch.TraceFactName:
		return stringAttribute(fact.Key, fact.Name)
	}
	return otlpAnyAttribute{Key: fact.Key, Value: otlpAnyValue{IntValue: strconv.FormatInt(fact.Int, 10)}}
}

// buildSpans makes the spans of a trip: the root's span, and under it:
//   - one INTERNAL span for each stage, under the stage it ran within if it
//     names one;
//   - one CLIENT span for each request, under the stage it was sent in, with
//     an INTERNAL rate_limit span before it if it waited for its turn under
//     request.rate_limit.
func buildSpans(root spanRoot, stages []spanStage, requests []fetch.TracedRequest) []otlpSpan {
	traceID := root.traceID
	switch {
	case root.parent.ok:
		traceID = root.parent.traceID
	case traceID == [16]byte{}:
		traceID = newTraceID()
	}
	trace := hex.EncodeToString(traceID[:])
	state := root.traceState
	rootID := root.spanID
	if rootID == [8]byte{} {
		rootID = newSpanID()
	}
	top := otlpSpan{
		TraceID: trace, SpanID: hex.EncodeToString(rootID[:]), TraceState: state, Name: root.name, Kind: root.kind,
		StartTimeUnixNano: nanos(root.start), EndTimeUnixNano: nanos(root.end), Attributes: root.attributes,
	}
	if root.parent.ok {
		top.ParentSpanID = hex.EncodeToString(root.parent.spanID[:])
	}
	if root.errorType != "" {
		top.Attributes = append(top.Attributes, stringAttribute("error.type", root.errorType))
	}
	if root.reason != "" {
		top.Attributes = append(top.Attributes, stringAttribute("sampling.reason", root.reason))
	}
	if root.failed {
		top.Status = &otlpSpanStatus{Code: otlpStatusError, Message: root.message}
	}
	spans := []otlpSpan{top}
	stageIDs := make([]string, len(stages))
	for i := range stages {
		id := newSpanID()
		stageIDs[i] = hex.EncodeToString(id[:])
	}
	for i, stage := range stages {
		parent := top.SpanID
		if stage.parent != "" {
			for k := range stages {
				if k != i && stages[k].name == stage.parent {
					parent = stageIDs[k]
				}
			}
		}
		span := otlpSpan{TraceID: trace, SpanID: stageIDs[i], TraceState: state, ParentSpanID: parent, Name: stage.name, Kind: otlpSpanKindInternal,
			StartTimeUnixNano: nanos(stage.start), EndTimeUnixNano: nanos(stage.end),
			Attributes: []otlpAnyAttribute{stringAttribute("stage.outcome", stage.outcome)}}
		if stage.outcome == "ok" || stage.outcome == "hit" || stage.outcome == "miss" {
			if stage.note != "" {
				span.Attributes = append(span.Attributes, stringAttribute("stage.note", stage.note))
			}
		} else if stageFailed(stage.outcome) {
			span.Status = &otlpSpanStatus{Code: otlpStatusError, Message: stage.note}
		}
		spans = append(spans, span)
	}
	// within is the stage a moment of the trip fell in: the one whose
	// times hold it, the innermost where several do.
	within := func(at time.Time) string {
		parent, length := top.SpanID, time.Duration(-1)
		for i, stage := range stages {
			if !at.Before(stage.start) && !at.After(stage.end) && (length < 0 || stage.end.Sub(stage.start) < length) {
				parent, length = stageIDs[i], stage.end.Sub(stage.start)
			}
		}
		return parent
	}
	// The last request sent within each stage: a stage that failed failed
	// on it, and its span says why as the stage does (requestFailure).
	last := map[string]int{}
	for i, r := range requests {
		last[within(r.Start())] = i
	}
	failedStage := map[string]string{}
	for i, stage := range stages {
		if stageFailed(stage.outcome) {
			failedStage[stageIDs[i]] = stage.note
		}
	}
	resends := 0
	for i, r := range requests {
		if r.Attempt == 0 && !r.Redirect {
			resends = 0
		} else {
			resends++
		}
		start := r.Start()
		parent := within(start)
		if r.RateLimitWait > 0 {
			id := newSpanID()
			spans = append(spans, otlpSpan{TraceID: trace, SpanID: hex.EncodeToString(id[:]), TraceState: state, ParentSpanID: parent, Name: "rate_limit", Kind: otlpSpanKindInternal,
				StartTimeUnixNano: nanos(start.Add(-r.RateLimitWait)), EndTimeUnixNano: nanos(start),
				Attributes: []otlpAnyAttribute{stringAttribute("stage.note", r.Wait)}})
		}
		// A request sent with the trip's traceparent has the span id it was
		// sent under, which the target's span names as its parent.
		id := r.SpanID
		if id == [8]byte{} {
			id = newSpanID()
		}
		span := otlpSpan{TraceID: trace, SpanID: hex.EncodeToString(id[:]), TraceState: state, ParentSpanID: parent, Name: r.Method, Kind: otlpSpanKindClient,
			StartTimeUnixNano: nanos(start), EndTimeUnixNano: nanos(start.Add(r.Duration)), Attributes: requestAttributes(r, resends)}
		switch {
		case requestUnfinished(r):
			// Its stage had not ended either, so it is under the stage
			// that holds that one, or under the root.
			span.Status = &otlpSpanStatus{Code: otlpStatusError, Message: "unfinished: the caller went away before it was answered"}
			span.Attributes = append(span.Attributes, stringAttribute("error.type", "unfinished"))
		case requestFailed(r):
			note, failed := failedStage[parent]
			span.Status = &otlpSpanStatus{Code: otlpStatusError, Message: requestFailure(r, failed && last[parent] == i, note)}
			errorType := "request_failed"
			if r.Status >= 400 {
				errorType = strconv.Itoa(r.Status)
			}
			span.Attributes = append(span.Attributes, stringAttribute("error.type", errorType))
		}
		spans = append(spans, span)
		spans = append(spans, phaseSpans(trace, state, span.SpanID, r.Phases)...)
	}
	return spans
}

// phaseSpans are the spans of an HTTP request's phases, under its span,
// parent. http.first_byte runs from the request's start, when it asked for
// a connection, to the answer's first byte, as $timings.first_byte measures
// it. Under it, within its time, are http.dns, http.connect and http.tls:
// the name lookup, the connection and the TLS handshake, each only if the
// request made it. A request on a reused connection has only
// http.first_byte. A request that failed before its first byte has the
// phases completed before the failure, with no http.first_byte to hold
// them, so they are under its span.
func phaseSpans(trace, state, parent string, phases []fetch.TracedPhase) []otlpSpan {
	if len(phases) == 0 {
		return nil
	}
	var spans []otlpSpan
	under := parent
	for _, phase := range phases {
		if phase.Name == "first_byte" {
			id := newSpanID()
			under = hex.EncodeToString(id[:])
			spans = append(spans, otlpSpan{TraceID: trace, SpanID: under, TraceState: state, ParentSpanID: parent, Name: "http.first_byte", Kind: otlpSpanKindInternal,
				StartTimeUnixNano: nanos(phase.Start), EndTimeUnixNano: nanos(phase.End)})
		}
	}
	for _, phase := range phases {
		if phase.Name == "first_byte" {
			continue
		}
		id := newSpanID()
		spans = append(spans, otlpSpan{TraceID: trace, SpanID: hex.EncodeToString(id[:]), TraceState: state, ParentSpanID: under, Name: phaseSpanName(phase.Name), Kind: otlpSpanKindInternal,
			StartTimeUnixNano: nanos(phase.Start), EndTimeUnixNano: nanos(phase.End)})
	}
	return spans
}

// phaseSpanName is a phase's span's name: http.<phase> of an http
// request's, the name itself of a connection another type made for its
// request, which is connection.<phase> already (fetch.TracedPhase).
func phaseSpanName(name string) string {
	if strings.Contains(name, ".") {
		return name
	}
	return "http." + name
}

// stageFailed says whether a stage that ended as outcome failed: neither
// went through nor was cancelled.
func stageFailed(outcome string) bool {
	switch outcome {
	case "ok", "hit", "miss", "cancelled":
		return false
	}
	return true
}

// requestAttributes are the attributes of a request's span. Where they
// apply, they follow OpenTelemetry's conventions for an HTTP client:
//   - its method;
//   - its URL as an error in the log quotes it: userinfo withheld, every
//     query value masked, no fragment (fetch.MaskQueryValues);
//   - the server's address and port, and the answer's status;
//   - how many times the request was sent again before, for a retry or a
//     redirect.
//
// The exporter adds its own: the attempt under request.retry, the page of
// request.pagination, whether it followed a redirect or made
// request.login's login, and the facts its request type gives about how it
// ended (fetch.TracedRequest.Facts). The outcome as a debug report gives it
// (fetch.TracedRequest.Outcome) is not included. It may quote what the
// target sent, such as a greeting or a command's stderr, which the log never
// shows for a request that went through.
func requestAttributes(r fetch.TracedRequest, resends int) []otlpAnyAttribute {
	var attrs []otlpAnyAttribute
	u, err := url.Parse(r.URL)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		attrs = append(attrs, stringAttribute("http.request.method", r.Method))
	}
	attrs = append(attrs, stringAttribute("url.full", fetch.RedactURLString(r.URL, fetch.MaskQueryValues)))
	if err == nil && u.Hostname() != "" {
		attrs = append(attrs, stringAttribute("server.address", u.Hostname()))
		port := u.Port()
		switch {
		case port != "":
		case u.Scheme == "https":
			port = "443"
		case u.Scheme == "http":
			port = "80"
		}
		if n, err := strconv.Atoi(port); err == nil {
			attrs = append(attrs, intAttribute("server.port", n))
		}
	}
	if r.Status > 0 {
		attrs = append(attrs, intAttribute("http.response.status_code", r.Status))
	}
	for _, fact := range r.Facts {
		attrs = append(attrs, factAttribute(fact))
	}
	if resends > 0 {
		attrs = append(attrs, intAttribute("http.request.resend_count", resends))
	}
	if r.Attempt > 0 {
		attrs = append(attrs, intAttribute("retry.attempt", r.Attempt))
	}
	if r.Page > 0 {
		attrs = append(attrs, intAttribute("pagination.page", r.Page))
	}
	if r.Redirect {
		attrs = append(attrs, boolAttribute("redirect", true))
	}
	if r.Login {
		attrs = append(attrs, boolAttribute("login", true))
	}
	return attrs
}

// requestFailed says whether a request failed: it got no answer, or an
// HTTP answer of 400 or more, as OpenTelemetry's conventions make a
// client's span an error.
func requestFailed(r fetch.TracedRequest) bool {
	if r.Status > 0 {
		return r.Status >= 400
	}
	return r.Failed
}

// requestFailure is the message of a failed request's span. It says what the
// log line of its failure says, and nothing the log does not:
//   - the request the fetch failed on (failedFetch) is its stage's failure,
//     told as the stage's note tells it (note);
//   - an HTTP answer of 400 or more gives its status, as the log does;
//   - a request that failed and was sent again, or was followed by one that
//     went through, is in no log line, so its span says no more than its
//     error.type.
func requestFailure(r fetch.TracedRequest, failedFetch bool, note string) string {
	switch {
	case failedFetch:
		return note
	case r.Status > 0:
		return fmt.Sprintf("received HTTP status %d", r.Status)
	}
	return ""
}

// rootAttributes are the attributes of a probe's or a scrape's span: the
// collector, the target as the log shows it, the static target, the request
// type, the series answered, the size of the response fetched and whether
// the cache answered.
func rootAttributes(c *model.Collector, target, static string, series, bytes int, cached bool) []otlpAnyAttribute {
	attrs := []otlpAnyAttribute{stringAttribute("collector", c.Name)}
	if target != "" {
		attrs = append(attrs, stringAttribute("target", target))
	}
	if static != "" {
		attrs = append(attrs, stringAttribute("static_target", static))
	}
	attrs = append(attrs, stringAttribute("request.type", c.Request.Type), intAttribute("series", series))
	if bytes >= 0 {
		attrs = append(attrs, intAttribute("response.bytes", bytes))
	}
	return append(attrs, boolAttribute("cache.hit", cached))
}

// otlpTracesOn says whether cfg exports traces.
func otlpTracesOn(cfg model.OTLPConfig) bool {
	return cfg.Enabled && cfg.Endpoint != "" && cfg.Traces.Enabled && cfg.Traces.Endpoint != ""
}

// otlpTraceThreshold is otlp.traces.threshold, its default where it is
// unset.
func otlpTraceThreshold(cfg model.OTLPConfig) time.Duration {
	if t := time.Duration(cfg.Traces.Threshold); t > 0 {
		return t
	}
	return model.DefaultOTLPTracesThreshold
}

// otlpTraces is the trace export's queue and its record.
type otlpTraces struct {
	mu sync.Mutex
	// queue holds the waiting spans, oldest first, each with its resource.
	// traces holds how many of them each trace has, in the same order. The
	// queue is cut only at whole traces, never inside one, which would leave
	// spans whose parent is gone.
	queue  []otlpQueuedSpan
	traces []int
	// The counts since the start, the status page's and the
	// self-metrics'.
	sent                int
	dropped             [otlpTraceDropReasons]int
	successes, failures int
	// endpoint and history are what the status page shows of the exports
	// to the endpoint in force; a reload that changes it starts them
	// again.
	endpoint string
	history  otlpLogsHistory
	// rate is otlp.traces.max_per_second, and tally the traces decided of
	// by why; now is the clock the rate goes by, time.Now unless a test
	// sets its own.
	rate  traceRate
	tally otlpTraceTally
	now   func() time.Time
}

// otlpQueuedSpan is a span waiting, as JSON, with its resource.
type otlpQueuedSpan struct {
	resource *otlpSpanResource
	span     []byte
}

// otlpSpanResource is a trace's resource: its key, which tells it from every
// other (otlpResourceIdentity.key), and its JSON.
type otlpSpanResource struct {
	key  string
	json []byte
}

// otlpTraceDropReason is why spans were given up on.
type otlpTraceDropReason int

const (
	// otlpTraceDropPendingCap: the queue had no room for the trace's spans
	// under otlp.traces.max_pending_spans.
	otlpTraceDropPendingCap otlpTraceDropReason = iota
	// otlpTraceDropRefused: the endpoint refused a request with an answer
	// that is not retried, or rejected some spans of one it accepted.
	otlpTraceDropRefused
	// otlpTraceDropShutdown: those of the last export before exiting, which
	// failed.
	otlpTraceDropShutdown
	otlpTraceDropReasons
)

// configure drops the spans waiting when cfg switches the trace export off,
// and the oldest whole traces past a lower bound.
func (t *otlpTraces) configure(cfg model.OTLPConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !otlpTracesOn(cfg) {
		t.queue, t.traces = nil, nil
		return
	}
	t.capLocked(otlpMaxPendingSpans(cfg))
}

// otlpMaxPendingSpans is otlp.traces.max_pending_spans, its default where it
// is unset.
func otlpMaxPendingSpans(cfg model.OTLPConfig) int {
	if n := cfg.Traces.MaxPendingSpans; n > 0 {
		return n
	}
	return model.DefaultOTLPTracesMaxPendingSpans
}

// add queues a trace's spans, or drops and counts them all when they do not
// fit under limit.
func (t *otlpTraces) add(spans []otlpQueuedSpan, limit int) {
	_ = t.addWhileOn(spans, limit, nil)
}

// addWhileOn is add, except that it drops the spans, counting them nowhere,
// when on (asked under the queue's lock) says the configuration in force no
// longer exports traces. A reload that switches the export off empties the
// queue under the same lock (configure) once its configuration is in force.
// So a trace is either queued before that and emptied with the rest, or
// sees the export off and is never queued. No trace waits through the time
// the export is off, to be sent if a later reload switches it on again.
func (t *otlpTraces) addWhileOn(spans []otlpQueuedSpan, limit int, on func() bool) bool {
	if len(spans) == 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if on != nil && !on() {
		return false
	}
	if len(t.queue)+len(spans) > limit {
		t.dropped[otlpTraceDropPendingCap] += len(spans)
		t.tally.dropped[traceDroppedQueue].Add(1)
		return false
	}
	t.queue = append(t.queue, spans...)
	t.traces = append(t.traces, len(spans))
	return true
}

// drain takes every span waiting, and how many of them each trace has.
func (t *otlpTraces) drain() ([]otlpQueuedSpan, []int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	spans, traces := t.queue, t.traces
	t.queue, t.traces = nil, nil
	return spans, traces
}

// requeue puts back the spans of drained that an export could not deliver
// (from the from-th on), before those queued since, within limit. Past the
// limit the oldest whole traces are dropped. traces is how many spans each
// trace of drained has. If an earlier request delivered the start of a
// trace, the rest of it counts as a trace of its own here.
func (t *otlpTraces) requeue(drained []otlpQueuedSpan, traces []int, from, limit int) {
	spans := drained[from:]
	if len(spans) == 0 {
		return
	}
	var counts []int
	for _, n := range traces {
		switch {
		case from >= n:
			from -= n
		case from > 0:
			counts = append(counts, n-from)
			from = 0
		default:
			counts = append(counts, n)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.queue = append(spans[:len(spans):len(spans)], t.queue...)
	t.traces = append(counts, t.traces...)
	t.capLocked(limit)
}

// capLocked drops the oldest whole traces until the spans waiting are
// within limit.
func (t *otlpTraces) capLocked(limit int) {
	over, cut, traces := len(t.queue)-limit, 0, 0
	for over > 0 && traces < len(t.traces) {
		over -= t.traces[traces]
		cut += t.traces[traces]
		traces++
	}
	if cut > 0 {
		t.dropped[otlpTraceDropPendingCap] += cut
		t.tally.dropped[traceDroppedQueue].Add(int64(traces))
		t.queue = append([]otlpQueuedSpan(nil), t.queue[cut:]...)
		t.traces = append([]int(nil), t.traces[traces:]...)
	}
}

func (t *otlpTraces) drop(spans int, why otlpTraceDropReason) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropped[why] += spans
}

func (t *otlpTraces) droppedLocked() int {
	n := 0
	for _, spans := range t.dropped {
		n += spans
	}
	return n
}

// record records an export of spans to endpoint, of which sent got through,
// that failed with err unless err is nil.
func (t *otlpTraces) record(endpoint string, started time.Time, spans, sent int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if endpoint != t.endpoint {
		t.endpoint, t.history = endpoint, otlpLogsHistory{}
	}
	t.sent += sent
	h := &t.history
	h.lastStarted, h.lastOK, h.lastRecords = started, err == nil, spans
	if err == nil {
		t.successes++
		h.lastSuccess, h.failures = time.Now(), 0
		return
	}
	t.failures++
	h.failures++
	h.errorText, h.failedAt = shownOTLPError(err), started
}

// beginTrace returns, while cfg exports traces, a context whose trip
// records its trace, and the recorder. The trace continues parent's if the
// caller sent one, and its ids are made now (traceLink). Otherwise it
// returns ctx and nil, which costs a probe nothing more.
func beginTrace(ctx context.Context, cfg model.OTLPConfig, parent traceParent) (context.Context, *tripSpans) {
	if !otlpTracesOn(cfg) {
		return ctx, nil
	}
	link := newTraceLink(parent)
	ctx, t := withTripSpans(withTraceLink(ctx, link), nil)
	t.link = link
	return ctx, t
}

// endTrace ends the record of a probe or a scrape that ran from start to
// end, failed as failed says, and whose caller sent parent, if any. Nearly
// all are not exported and are forgotten: the record is dropped and nothing
// more is built from it. If a rule keeps it (decideTrace) and
// otlp.traces.max_per_second lets it through, root makes its span from the
// size of the response fetched, and its spans are queued under the resource
// that identity makes.
//
// cfg is the configuration the probe or the scrape began under, whose rules
// decide its trace. If a reload since then switched the export off, the
// trace is dropped and counted nowhere, just as a trace that began after it
// is never recorded. No rule decided it while the export was off, and
// nothing of it waits for an export that is off (addWhileOn).
func (s *Server) endTrace(cfg model.OTLPConfig, spans *tripSpans, start, end time.Time, failed bool, parent traceParent, root func(bytes int) spanRoot, identity func() otlpResourceIdentity) {
	t := s.otlpTraces
	inForce := func() bool { return otlpTracesOn(s.manager.Get().OTLP) }
	if !inForce() {
		spans.discard()
		return
	}
	decision := decideTraceOf(cfg, end.Sub(start), failed, parent, spans.link.traceID)
	if decision.reason == keptNone {
		t.tally.dropped[traceDroppedNotSampled].Add(1)
		spans.discard()
		return
	}
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	if !t.rate.take(cfg.Traces.MaxPerSecond, decision.reason, now()) {
		t.tally.dropped[traceDroppedRate].Add(1)
		spans.discard()
		return
	}
	t.tally.kept[decision.reason].Add(1)
	stages, requests, fetched := spans.close()
	made := root(fetched)
	sampledRoot(&made, cfg, decision, parent)
	made.spanID = spans.link.rootID
	built := buildSpans(made, stages, requests)
	// The queue's bound is the one in force, which a reload since may have
	// lowered (configure). Only a trace queued is linked to.
	queued := t.addWhileOn(queuedSpans(identity(), built), otlpMaxPendingSpans(s.manager.Get().OTLP), inForce)
	spans.link.settle(queued, stages, built)
}

// sampledRoot gives root what the sampling decision says of its trace: the
// id the ratio's rule went by, the tracestate its spans carry, and, while a
// rule beside the threshold and errors is on, why it was kept.
func sampledRoot(root *spanRoot, cfg model.OTLPConfig, decision traceDecision, parent traceParent) {
	root.traceID = decision.traceID
	root.traceState = spanTraceState(decision.reason, cfg.Traces.SampleRatio, parent.state)
	if otlpTraceSamplingOn(cfg) {
		root.reason = samplingReasonNames[decision.reason]
	}
}

// queuedSpans are spans as they wait for the export, under identity, which
// the debug page shows a run's as the export would send them too.
func queuedSpans(identity otlpResourceIdentity, made []otlpSpan) []otlpQueuedSpan {
	resource, err := json.Marshal(otlpResource{Attributes: identity.attributes()})
	if err != nil {
		// Not reached: a resource is strings.
		return nil
	}
	shared := &otlpSpanResource{key: identity.key(), json: resource}
	queued := make([]otlpQueuedSpan, 0, len(made))
	for i := range made {
		raw, err := json.Marshal(&made[i])
		if err != nil {
			// Not reached: a span holds nothing JSON refuses.
			continue
		}
		queued = append(queued, otlpQueuedSpan{resource: shared, span: raw})
	}
	return queued
}

// probeTraceEnd is how a probe ended, for its span.
type probeTraceEnd struct {
	status int
	series int
	cached bool
	// failed says the probe failed: it was answered with an error, or with
	// a stale result in place of a fresh one; stage is where, failure why.
	failed         bool
	stage, failure string
}

// probeTraceEndOf is how a probe answered with result ended. A stale result
// in place of a fresh one is a failure, as it is to the self-metrics.
func probeTraceEndOf(result *probeResult) probeTraceEnd {
	end := probeTraceEnd{status: result.status, cached: result.outcome == probeCached, failed: !result.ok, failure: result.failure}
	if result.metrics != nil {
		end.status, end.series = http.StatusOK, len(result.metrics.Metrics)
	}
	if !end.failed {
		return end
	}
	switch result.outcome {
	case probeStale:
		end.stage = "stale"
	case probeRejected:
		end.stage = "rejected"
	case probeRefused:
		end.stage = "refused"
	case probeBackoff:
		end.stage = "backoff"
	default:
		// A failure answered with its status; one report_failures
		// answered 200 has its stage instead (reportedTrace).
		end.stage = strconv.Itoa(end.status)
	}
	if end.failure == "" {
		end.failure = "the probe failed"
	}
	return end
}

// endProbeTrace ends a probe's trace, under the exporter-wide resource, as
// its series are exported. The probe ran from start to finished, when its
// answer was ready. The time its caller takes to read the answer is not
// counted, just as it is not in the self-metrics' durations.
func (s *Server) endProbeTrace(cfg model.OTLPConfig, spans *tripSpans, c *model.Collector, target, method string, start, finished time.Time, parent traceParent, end probeTraceEnd) {
	s.endTrace(cfg, spans, start, finished, end.failed, parent, func(bytes int) spanRoot {
		return probeSpanRoot(c, target, method, start, finished, parent, end, bytes)
	}, func() otlpResourceIdentity { return defaultResourceIdentity(cfg) })
}

// probeSpanRoot is a probe's span: a SERVER span, named for its collector,
// with the probe's method, path and status as OpenTelemetry's conventions
// name them, and what rootAttributes says of it.
func probeSpanRoot(c *model.Collector, target, method string, start, end time.Time, parent traceParent, e probeTraceEnd, bytes int) spanRoot {
	attrs := []otlpAnyAttribute{stringAttribute("http.request.method", method), stringAttribute("url.path", "/probe"), intAttribute("http.response.status_code", e.status)}
	return spanRoot{name: "probe " + c.Name, kind: otlpSpanKindServer, start: start, end: end,
		attributes: append(attrs, rootAttributes(c, target, "", e.series, bytes, e.cached)...),
		failed:     e.failed, message: e.failure, errorType: e.stage, parent: parent}
}

// endScrapeTrace ends a static target's scrape's trace, under the target's
// own resource, as its series are exported (targetResource).
func (s *Server) endScrapeTrace(cfg model.OTLPConfig, spans *tripSpans, target *model.StaticTarget, c *model.Collector, address string, start time.Time, series int, cached bool, failure error) {
	finished := time.Now()
	s.endTrace(cfg, spans, start, finished, failure != nil, traceParent{}, func(bytes int) spanRoot {
		return scrapeSpanRoot(target, c, address, start, finished, series, cached, failure, bytes)
	}, func() otlpResourceIdentity { return targetResource(target, cfg) })
}

// scrapeSpanRoot is a static target's scrape's span: an INTERNAL span, named
// for the target, which no request asked for.
func scrapeSpanRoot(target *model.StaticTarget, c *model.Collector, address string, start, end time.Time, series int, cached bool, failure error, bytes int) spanRoot {
	root := spanRoot{name: "scrape " + target.Name, kind: otlpSpanKindInternal, start: start, end: end, failed: failure != nil,
		attributes: rootAttributes(c, address, target.Name, series, bytes, cached)}
	if failure != nil {
		root.message, root.errorType = shownOTLPError(failure), "scrape_failed"
	}
	return root
}

// otlpTraceRequest is one request of a trace export: its body, and which of
// the export's spans it carries, from up to to.
type otlpTraceRequest struct {
	body     []byte
	from, to int
}

// otlpTraceRequests makes the requests that send spans to the endpoint.
// Each holds at most otlp.batch_max_size spans and otlp.batch_max_bytes
// bytes of JSON; a larger single span goes in a request of its own, as with
// the log records' requests (otlpLogRequests). A request holds the spans of
// each of its resources under that resource, and the scope.
func otlpTraceRequests(cfg model.OTLPConfig, spans []otlpQueuedSpan) ([]otlpTraceRequest, error) {
	scope, err := json.Marshal(otlpScopeOf(cfg))
	if err != nil {
		return nil, err
	}
	const head, tail, closing = `{"resourceSpans":[`, `]}`, `]}]}`
	opening := func(r *otlpSpanResource) string {
		return `{"resource":` + string(r.json) + `,"scopeSpans":[{"scope":` + string(scope) + `,"spans":[`
	}
	maxSpans, maxBytes := otlpBatchBounds(cfg)
	var requests []otlpTraceRequest
	for from := 0; from < len(spans); {
		seen := map[string]bool{}
		size, to := len(head)+len(tail), from
		for to < len(spans) {
			r := spans[to].resource
			adds := len(spans[to].span)
			switch {
			case !seen[r.key] && len(seen) > 0:
				adds += 1 + len(opening(r)) + len(closing)
			case !seen[r.key]:
				adds += len(opening(r)) + len(closing)
			default:
				adds++
			}
			if to > from && (to-from+1 > maxSpans || size+adds > maxBytes) {
				break
			}
			seen[r.key] = true
			size += adds
			to++
		}
		// The spans of each resource, in the order the resources come.
		var order []*otlpSpanResource
		of := map[string][][]byte{}
		for _, span := range spans[from:to] {
			if _, has := of[span.resource.key]; !has {
				order = append(order, span.resource)
			}
			of[span.resource.key] = append(of[span.resource.key], span.span)
		}
		raw := make([]byte, 0, size)
		raw = append(raw, head...)
		for k, r := range order {
			if k > 0 {
				raw = append(raw, ',')
			}
			raw = append(raw, opening(r)...)
			for i, span := range of[r.key] {
				if i > 0 {
					raw = append(raw, ',')
				}
				raw = append(raw, span...)
			}
			raw = append(raw, closing...)
		}
		raw = append(raw, tail...)
		body, err := compressOTLP(raw, cfg.Compression)
		if err != nil {
			return nil, err
		}
		requests = append(requests, otlpTraceRequest{body: body, from: from, to: to})
		from = to
	}
	return requests, nil
}

// otlpTracesKey is the key the trace export's failures are remembered under
// in the failure log: there is one trace export.
var otlpTracesKey = subjectKey{bytes: subjectBytes(traceExportSubject, "")}

// OTLPTraceExportLoop sends the spans queued every otlp.traces.interval
// until ctx ends. It returns without a last export, which FlushOTLP makes
// after the metrics' last.
func (s *Server) OTLPTraceExportLoop(ctx context.Context) {
	s.otlpEvery(ctx, func(cfg model.OTLPConfig) time.Duration {
		if interval := time.Duration(cfg.Traces.Interval); interval > 0 {
			return interval
		}
		return model.DefaultOTLPTracesInterval
	}, func(cfg model.OTLPConfig, interval time.Duration) {
		s.exportOTLPTraces(ctx, cfg, interval, false)
	})
}

// flushOTLPTraces makes the last trace export at shutdown, until deadline
// and for at least otlpLogsLastAttempt, as the log export's last is given.
func (s *Server) flushOTLPTraces(cfg model.OTLPConfig, deadline time.Time) {
	s.exportOTLPTraces(context.Background(), cfg, max(time.Until(deadline), otlpLogsLastAttempt), true)
}

// exportOTLPTraces sends every waiting span, retrying within budget, one
// request after another, in as many requests as otlp.batch_max_size and
// otlp.batch_max_bytes require, as the log export sends its records
// (exportOTLPLogs):
//   - spans not delivered for a reason worth retrying wait for the next
//     export, before the spans queued since;
//   - spans the endpoint refused outright are dropped and counted;
//   - at shutdown, when last says there is no next export, what is left is
//     dropped and counted.
//
// The outcome is logged to stderr only (unexportedLogger).
func (s *Server) exportOTLPTraces(ctx context.Context, cfg model.OTLPConfig, budget time.Duration, last bool) {
	t := s.otlpTraces
	spans, traces := t.drain()
	if !otlpTracesOn(cfg) || len(spans) == 0 {
		return
	}
	logger := s.unexportedLogger()
	limit := otlpMaxPendingSpans(cfg)
	signal := otlpSignal{kind: otlpSignalTraces, logger: logger}
	target := cfg
	target.Endpoint = cfg.Traces.Endpoint
	start := time.Now()
	deadline := start.Add(budget)
	requests, err := otlpTraceRequests(cfg, spans)
	sent, retries := 0, 0
	// rest is where the spans not delivered begin, len(spans) when every
	// one was.
	rest := len(spans)
	var failure error
	var partial otlpPartialSuccess
	for k := 0; err == nil && k < len(requests); k++ {
		request := requests[k]
		if k > 0 && time.Until(deadline) < otlpMinAttempt {
			err = fmt.Errorf("the trace export ran out of time after %d of its %d requests", k, len(requests))
			rest = request.from
			break
		}
		var tries int
		var answer otlpPartialSuccess
		tries, answer, err = s.deliverOTLPOf(ctx, signal, target, request.body, deadline)
		retries += tries
		if err != nil && ctx.Err() != nil {
			// Shutting down: the last export sends these.
			t.requeue(spans, traces, request.from, limit)
			return
		}
		count := request.to - request.from
		if err == nil {
			rejected := int(min(answer.rejected, int64(count)))
			sent += count - rejected
			if rejected > 0 || answer.message != "" {
				partial.rejected += int64(rejected)
				partial.message = answer.message
				t.drop(rejected, otlpTraceDropRefused)
			}
			continue
		}
		var refused *otlpRefusedError
		if errors.As(err, &refused) {
			t.drop(count, otlpTraceDropRefused)
			failure, err = err, nil
			continue
		}
		rest = request.from
	}
	if err != nil {
		failure = err
	}
	t.record(target.Endpoint, start, len(spans), sent, failure)
	attrs := []any{"endpoint", redactedOTLPEndpoint(target.Endpoint), "spans", len(spans), "retries", retries}
	switch {
	case failure != nil && rest < len(spans) && last:
		t.drop(len(spans)-rest, otlpTraceDropShutdown)
		s.failures.failed(logger, slog.LevelWarn, otlpTracesKey, "the last OTLP trace export before exiting failed; its spans are dropped; this line is not exported", "export", failure, append(attrs, "dropped_spans", len(spans)-rest)...)
	case failure != nil && rest < len(spans):
		t.requeue(spans, traces, rest, limit)
		s.failures.failed(logger, slog.LevelWarn, otlpTracesKey, "OTLP trace export failed; its spans are kept for the next export; this line is not exported", "export", failure, append(attrs, "kept_spans", len(spans)-rest)...)
	case failure != nil:
		var refused *otlpRefusedError
		if errors.As(failure, &refused) && refused.body != "" {
			attrs = append(attrs, "response_body", refused.body)
		}
		s.failures.failed(logger, slog.LevelWarn, otlpTracesKey, "OTLP endpoint refused a trace export; its spans are dropped; this line is not exported", "refused", failure, attrs...)
	case partial.rejected > 0 || partial.message != "":
		why := partial.message
		if why == "" {
			why = "the OTLP endpoint gave no reason"
		}
		s.failures.failed(logger, slog.LevelWarn, otlpTracesKey, "OTLP endpoint accepted a trace export but rejected some of its spans; they are dropped; this line is not exported", "partial_success", errors.New(why), append(attrs, "rejected_spans", partial.rejected)...)
	default:
		s.failures.recovered(logger, otlpTracesKey, "OTLP trace export recovered; this line is not exported", "endpoint", redactedOTLPEndpoint(target.Endpoint))
	}
}

// otlpTraceStatusMetrics renders the trace export's status while it is on.
func (s *Server) otlpTraceStatusMetrics() []model.Metric {
	if !otlpTracesOn(s.manager.Get().OTLP) {
		return nil
	}
	t := s.otlpTraces
	t.mu.Lock()
	defer t.mu.Unlock()
	metric := func(name string, value int, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Help: exporterMetricHelp[name], Type: model.CounterMetricType, Value: float64(value), Labels: labels}
	}
	metrics := []model.Metric{
		metric("http_exporter_otlp_trace_exports_total", t.successes, map[string]string{"result": "success"}),
		metric("http_exporter_otlp_trace_exports_total", t.failures, map[string]string{"result": "failure"}),
		metric("http_exporter_otlp_spans_sent_total", t.sent, nil),
		metric("http_exporter_otlp_spans_dropped_total", t.droppedLocked(), nil),
	}
	for reason := keptSlow; reason < samplingReasons; reason++ {
		metrics = append(metrics, metric("http_exporter_otlp_traces_kept_total", int(t.tally.kept[reason].Load()), map[string]string{"reason": samplingReasonNames[reason]}))
	}
	for reason := range traceDropReasons {
		metrics = append(metrics, metric("http_exporter_otlp_traces_dropped_total", int(t.tally.dropped[reason].Load()), map[string]string{"reason": traceDropReasonNames[reason]}))
	}
	return metrics
}
