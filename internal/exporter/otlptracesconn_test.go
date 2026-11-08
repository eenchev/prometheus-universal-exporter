package exporter

import (
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// The phases of a connection a request type other than http made for its
// request are spans of their own names, connection.dns, connection.connect
// and connection.tls, directly under the request's span; an http request's
// are http.<phase>, under its http.first_byte.
func TestAConnectionsPhasesAreSpansUnderItsRequest(t *testing.T) {
	start := time.Now()
	phases := []fetch.TracedPhase{
		{Name: "connection.dns", Start: start, End: start.Add(time.Millisecond)},
		{Name: "connection.connect", Start: start.Add(time.Millisecond), End: start.Add(2 * time.Millisecond)},
		{Name: "connection.tls", Start: start.Add(2 * time.Millisecond), End: start.Add(3 * time.Millisecond)},
	}
	spans := phaseSpans("trace", "", "parent", phases)
	if len(spans) != 3 {
		t.Fatalf("spans %+v", spans)
	}
	for i, span := range spans {
		if span.Name != phases[i].Name || span.ParentSpanID != "parent" || span.Kind != otlpSpanKindInternal {
			t.Errorf("span %d: %+v", i, span)
		}
	}
	http := phaseSpans("trace", "", "parent", []fetch.TracedPhase{{Name: "first_byte", Start: start, End: start.Add(time.Second)}, {Name: "connect", Start: start, End: start.Add(time.Millisecond)}})
	if len(http) != 2 || http[0].Name != "http.first_byte" || http[1].Name != "http.connect" || http[1].ParentSpanID != http[0].SpanID {
		t.Errorf("an http request's: %+v", http)
	}
}
