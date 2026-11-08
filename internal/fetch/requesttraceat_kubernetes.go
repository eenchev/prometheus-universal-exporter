//go:build !select_request_types || request_type_kubernetes

package fetch

// What a kubernetes collector, whose requests are sent at once, records of
// each at its own place in the trace (tracedAt) that no other type does;
// the phases, which an HTTP trip records too, are in requesttrace.go, and
// how a request ended in requesttraceat.go.

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
