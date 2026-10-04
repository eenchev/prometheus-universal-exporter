//go:build !select_request_types || (request_type_http && request_type_grpc)

package exporter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// grpcPlainCollector is the http collector the grpc fixture carries beside
// its grpc one where the build has http too, for the tests to show what is
// for grpc collectors alone, such as the gRPC code gauge.
const grpcPlainCollector = `  - name: plain
    request:
      type: http
    decoder:
      type: text
    transform:
      type: regex
    metrics:
      - name: v
        expression: 'v=(\d+)'
`

// An exporter without grpc collectors does not have the gRPC code gauge at
// all. TestTheGRPCCodeGaugeReadsMinusOneBeforeTheFirstCall has what it reads
// for a grpc collector.
func TestTheGRPCCodeGaugeIsOnlyForGRPCCollectors(t *testing.T) {
	plain := verboseServer(t, false, testutil.Collector("plain", "text"))
	if metrics := selfMetrics(t, plain); strings.Contains(metrics, "grpc_status_code") {
		t.Fatalf("the family is there without grpc collectors:\n%s", metrics)
	}
}

// A probe parameter another type owns is refused before any call, where the
// build has that type: path is a parameter of http's, and in a build with
// grpc alone it is one no request type knows. TestGRPCProbeFailures has the
// other ways a grpc probe fails.
func TestAGRPCProbeRefusesAProbeParameterOfAnotherType(t *testing.T) {
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: queueAnswer})
	server := grpcExporter(t, false, "0s")
	probe := "/probe?collector=queue_stats&param_queue=orders&target=" + url.QueryEscape(upstream.Addr)
	recorder := probeOnce(t, server, probe+"&path=/x", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `request.type is "grpc"`) {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if calls := upstream.Calls(); len(calls) != 0 {
		t.Fatalf("a refused probe was called: %v", calls)
	}
}
