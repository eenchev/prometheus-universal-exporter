//go:build !select_request_types || request_type_tcp || request_type_ldap || request_type_redis || request_type_tlscert || request_type_sshexec || request_type_sshfile || request_type_grpc

package fetch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// phaseNames are the names of a request's phases, joined by commas, each
// checked to end after it began, and to begin no earlier than the request.
func phaseNames(t *testing.T, r TracedRequest) string {
	t.Helper()
	names := make([]string, 0, len(r.Phases))
	for _, phase := range r.Phases {
		if phase.End.Before(phase.Start) || phase.Start.Before(r.Start()) {
			t.Errorf("%s %s: the phase %s is from %v to %v, the request from %v", r.Method, r.URL, phase.Name, phase.Start, phase.End, r.Start())
		}
		names = append(names, phase.Name)
	}
	return strings.Join(names, ",")
}

// spanTracedRequests is a fetch of target through c whose requests are
// recorded as an exported trip's spans need them, and the requests.
func spanTracedRequests(t *testing.T, c *model.Collector, target string) []TracedRequest {
	t.Helper()
	ctx, trace := WithSpanTrace(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	return trace.Requests()
}

// A connection's timing of a trip that records nothing times nothing, and
// its phases are none.
func TestAConnectionOfAnUntracedTripIsNotTimed(t *testing.T) {
	ctx, timing := withConnTiming(context.Background())
	if timing != nil || connTimingOf(ctx) != nil || timing.phases() != nil {
		t.Errorf("timing %+v", timing)
	}
	if err := timing.handshake("tls", func() error { return nil }); err != nil {
		t.Error(err)
	}
}

// The next request recorded takes the phases held for it, its span begun
// with them; one added to the last request recorded is appended to its own.
func TestHeldPhasesAreTheNextRequestsAndAddedOnesTheLasts(t *testing.T) {
	ctx, trace := WithSpanTrace(context.Background())
	began := time.Now().Add(-time.Second)
	traceNextPhases(ctx, []TracedPhase{{Name: "connection.connect", Start: began, End: began.Add(time.Millisecond)}})
	traceRequest(ctx, "AUTH", "redis://x", nil, "", false)
	traceAddPhases(ctx, []TracedPhase{{Name: "connection.tls", Start: began.Add(time.Millisecond), End: began.Add(2 * time.Millisecond)}})
	traceRequest(ctx, "INFO", "redis://x", nil, "", false)
	requests := trace.Requests()
	if got := phaseNames(t, requests[0]); got != "connection.connect,connection.tls" || !requests[0].Start().Equal(began) {
		t.Errorf("the first request: %s from %v", got, requests[0].Start())
	}
	if len(requests[1].Phases) != 0 {
		t.Errorf("the second request: %+v", requests[1].Phases)
	}
}
