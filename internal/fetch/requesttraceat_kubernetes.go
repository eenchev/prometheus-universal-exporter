//go:build !select_request_types || request_type_kubernetes

package fetch

import (
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What a kubernetes collector, whose requests are sent at once, records of
// each at its own place in the trace (tracedAt); the phases, which an HTTP
// trip records too, are in requesttrace.go.

// outcome records how the request ended, as traceOutcome does, and that it
// failed, as traceFailedOutcome does, unless its end is recorded already.
func (a tracedAt) outcome(outcome string, failed bool) {
	a.edit(func(r *TracedRequest) {
		if r.Outcome == "" {
			r.Outcome, r.Duration = model.CutTo(outcome, model.MaxFailureBytes, false), time.Since(r.started)
			r.Failed = failed
		}
	})
}

// spanID records the span id the request was sent under (traceSpanID).
func (a tracedAt) spanID(span [8]byte) {
	if span != [8]byte{} {
		a.edit(func(r *TracedRequest) { r.SpanID = span })
	}
}

// wait records the wait before the request was asked for again (traceWait).
func (a tracedAt) wait(wait string) {
	a.edit(func(r *TracedRequest) { r.Wait = joinWaits(r.Wait, wait) })
}
