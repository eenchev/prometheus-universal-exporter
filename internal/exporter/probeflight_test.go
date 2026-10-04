package exporter

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Identical probes that arrive while one is in flight share its request to the
// target (probeflight.go).

// The shared work runs on its own goroutine, so a panic in it must answer the
// waiting probes with an error instead of crashing the exporter.
func TestAPanicInSharedWorkIsContained(t *testing.T) {
	testutil.CaptureLogs(t)
	flights := newProbeFlights()
	result, shared, err := flights.do(context.Background(), flightKey{probe: "key"}, func(context.Context) *probeResult {
		panic("boom")
	})
	if err != nil || shared || result.status != http.StatusInternalServerError || !strings.Contains(string(result.body), "internal error: boom") || result.ok {
		t.Fatalf("result=%+v shared=%t err=%v", result, shared, err)
	}
	if flights.inFlight() != 0 {
		t.Fatal("the panicked flight was left behind")
	}
}
