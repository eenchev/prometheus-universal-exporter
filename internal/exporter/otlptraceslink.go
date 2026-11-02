package exporter

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// With otlp.traces on, a probe's or a scrape's trace is linked to what else
// the exporter tells of it, and each link points only at a trace that is
// exported:
//
//   - the log records of the lines logged while it ran (otlp.logs) carry
//     its trace id, the span id of the span that was running when the line
//     was logged and the sampled flag (otlplogs.go);
//   - the trip's observation in http_exporter_collector_scrape_duration_seconds
//     is the exemplar of its bucket, with the trace id and the root span's
//     id (verbosemetrics.go);
//   - with otlp.traces.propagate, its requests to the target carry its
//     traceparent (fetch.WithTracePropagation).
//
// Its trace is decided only at its end, when the log lines have been
// queued and the observation made, and its requests sent. So its ids are
// made when it begins — the trace id, the caller's where it sent a
// traceparent, and the root span's id — and kept in a traceLink, which the
// trip's context carries. What links to the trace holds the link, and asks
// it when the link is written out: a log record, when the log export
// drains it, which then carries the ids only if the trace was exported, and
// none if it was dropped or not yet decided; an exemplar is only set once
// the trace is exported. A request cannot wait for the decision, and says
// what is known of it when it is sent (headTraceSampled).
//
// Which stage was running when a line was logged is told by the times: the
// line's time against the stages' (spanAt), the innermost that holds it,
// else the root. A stage that fails logs its failure before it ends, so
// that the line is the failed stage's.

// traceLinkState is what became of a linked trace.
type traceLinkState uint8

const (
	traceUndecided traceLinkState = iota
	traceExported
	traceDropped
)

// traceLink is a probe's or a scrape's trace as what links to it knows it.
type traceLink struct {
	traceID [16]byte
	rootID  [8]byte
	// run says it is a debug run's, which is never exported, but which the
	// debug page shows the links of as they would be (debugotlp.go).
	run bool

	mu    sync.Mutex
	state traceLinkState
	// stages are the stage spans of the trace, with their times, set when it
	// is exported, or of a run when its spans are made.
	stages []spanWindow
	// exported are what waits for the trace to be exported: each is called
	// once it is, and dropped when it is not.
	exported []func(traceID, spanID string)
}

// spanWindow is a span's id, as OTLP/JSON writes it, and its times.
type spanWindow struct {
	id         string
	start, end time.Time
}

type traceLinkKey struct{}

// newTraceLink is the link of a trace that continues parent's, or begins
// one of its own.
func newTraceLink(parent traceParent) *traceLink {
	l := &traceLink{rootID: newSpanID()}
	if parent.ok {
		l.traceID = parent.traceID
	} else {
		l.traceID = newTraceID()
	}
	return l
}

// withTraceLink returns a context that carries l.
func withTraceLink(ctx context.Context, l *traceLink) context.Context {
	return context.WithValue(ctx, traceLinkKey{}, l)
}

// traceLinkFrom is the link of the trace ctx is for, nil when there is none.
func traceLinkFrom(ctx context.Context) *traceLink {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(traceLinkKey{}).(*traceLink)
	return l
}

// settle records what became of the trace: exported, with its stage spans,
// stages and made, as buildSpans made them, or not. What waited for its
// export is called, outside the lock, or dropped. A run's link keeps its
// spans and is never exported.
func (l *traceLink) settle(exported bool, stages []spanStage, made []otlpSpan) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.state != traceUndecided {
		l.mu.Unlock()
		return
	}
	waiting := l.exported
	l.exported = nil
	switch {
	case l.run:
		l.stages = stageWindows(stages, made)
	case exported:
		l.state, l.stages = traceExported, stageWindows(stages, made)
	default:
		l.state, waiting = traceDropped, nil
	}
	l.mu.Unlock()
	if l.run {
		return
	}
	trace, root := l.ids()
	for _, f := range waiting {
		f(trace, root)
	}
}

// stageWindows are the stage spans of made, a trace buildSpans made of
// stages: the root first, then one for each stage, in their order.
func stageWindows(stages []spanStage, made []otlpSpan) []spanWindow {
	if len(made) < len(stages)+1 {
		return nil
	}
	out := make([]spanWindow, len(stages))
	for i, stage := range stages {
		out[i] = spanWindow{id: made[1+i].SpanID, start: stage.start, end: stage.end}
	}
	return out
}

// ids are the trace id and the root span's id as OTLP/JSON writes them.
func (l *traceLink) ids() (trace, root string) {
	return hex.EncodeToString(l.traceID[:]), hex.EncodeToString(l.rootID[:])
}

// whenExported calls f with the trace id and the root span's id once the
// trace is exported: at once if it is already, never if it was dropped.
func (l *traceLink) whenExported(f func(traceID, spanID string)) {
	if l == nil || l.run {
		return
	}
	l.mu.Lock()
	switch l.state {
	case traceUndecided:
		l.exported = append(l.exported, f)
		l.mu.Unlock()
		return
	case traceDropped:
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	f(l.ids())
}

// spanAt is the trace id and the id of the span that was running at, the
// innermost stage that holds it, else the root, of a trace exported; ok is
// false for a trace dropped or not yet decided. A run's are those it would
// have, once its spans are made.
func (l *traceLink) spanAt(at time.Time) (trace, span string, ok bool) {
	if l == nil {
		return "", "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != traceExported && (!l.run || l.stages == nil) {
		return "", "", false
	}
	trace, span = l.ids()
	length := time.Duration(-1)
	for _, w := range l.stages {
		if !at.Before(w.start) && !at.After(w.end) && (length < 0 || w.end.Sub(w.start) < length) {
			span, length = w.id, w.end.Sub(w.start)
		}
	}
	return trace, span, true
}

// headTraceSampled is what is known, when a trip sends its requests, of
// whether its trace will be exported, which their traceparent's sampled
// flag says, and the tracestate they carry. A trace is decided at its end,
// and only two of the rules can decide before: parent_based, by the
// caller's sampled flag, and sample_ratio, by the trace id. Either keeps a
// trace whatever else becomes of it, and so sets the flag, and the ratio's
// threshold goes in the tracestate (spanTraceState). The threshold and the
// errors, which keep most of what is exported, are known only at the end:
// the flag is not set for them, as the W3C asks it to be set only when the
// caller may have recorded the trace, and a target's tracer that samples
// by its parent then records its part only of the traces the exporter is
// sure to keep. otlp.traces.max_per_second may still drop one, as it may
// drop one its caller sampled.
func headTraceSampled(cfg model.OTLPConfig, parent traceParent, id [16]byte) (sampled bool, state string) {
	t := cfg.Traces
	switch {
	case t.ParentBased && parent.ok && parent.sampled:
		return true, spanTraceState(keptParent, t.SampleRatio, parent.state)
	case t.SampleRatio > 0 && traceRandomness(id, parent.state) >= ratioThreshold(t.SampleRatio):
		return true, spanTraceState(keptRatio, t.SampleRatio, parent.state)
	}
	return false, spanTraceState(keptNone, t.SampleRatio, parent.state)
}

// withPropagation returns a context whose trips' requests carry the trace
// context of the probe or the scrape ctx is for, with otlp.traces.propagate
// on: link's trace while the trace export is on, and while it is off the
// caller's, parent, read from header's traceparent, sent as it came, and
// its tracestate as it was read. A traceparent that could not be read is
// not passed on, nor a tracestate that could not, as the W3C asks; ctx is
// returned as it is when there is nothing to send.
func withPropagation(ctx context.Context, cfg model.OTLPConfig, link *traceLink, parent traceParent, header http.Header) context.Context {
	if !cfg.Traces.Propagate {
		return ctx
	}
	if link != nil {
		sampled, state := headTraceSampled(cfg, parent, link.traceID)
		return fetch.WithTracePropagation(ctx, fetch.TracePropagation{TraceID: link.traceID, Sampled: sampled, State: state})
	}
	if !parent.ok {
		return ctx
	}
	return fetch.WithTracePropagation(ctx, fetch.TracePropagation{Parent: strings.TrimSpace(header.Get("Traceparent")), ParentState: parent.state.String()})
}
