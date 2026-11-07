//go:build !select_request_types || request_type_kubernetes || request_type_icmp

package fetch

import (
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// How a request recorded at its own place in the trace (tracedAt) ended,
// for the types whose requests are sent at once: kubernetes, and icmp's
// all_addresses.

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
