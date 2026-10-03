//go:build !select_request_types || (request_type_http && request_type_grpc)

package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// Before the first call the gauge reads -1; an exporter without grpc
// collectors does not have it at all.
func TestTheGRPCCodeGaugeIsOnlyForGRPCCollectors(t *testing.T) {
	server := grpcExporter(t, false, "0s")
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} -1`) {
		t.Fatalf("%s", metrics)
	}
	plain := verboseServer(t, false, testutil.Collector("plain", "text"))
	if metrics := selfMetrics(t, plain); strings.Contains(metrics, "grpc_status_code") {
		t.Fatalf("the family is there without grpc collectors:\n%s", metrics)
	}
}

// A status other than OK fails the probe under the grpc stage, with the
// code in the answer and the log and in the gauge; a message that does not
// fit fails at the message stage, with no call made.
func TestGRPCProbeFailures(t *testing.T) {
	var denied atomic.Bool
	denied.Store(true)
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(ctx context.Context, m, r string) (string, error) {
		if denied.Load() {
			return "", status.Error(codes.PermissionDenied, "tenant may not read queues")
		}
		return queueAnswer(ctx, m, r)
	}})
	logs := testutil.CaptureLogs(t)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.NewReplacer("VERBOSE", "false", "CACHE", "0s").Replace(grpcExporterConfig)))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	probe := "/probe?collector=queue_stats&param_queue=orders&target=" + url.QueryEscape(upstream.Addr)
	recorder := probeOnce(t, server, probe, nil)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "collector queue_stats grpc failed: gRPC call failed: PERMISSION_DENIED: tenant may not read queues") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(logs.String(), `"grpc_code":"PERMISSION_DENIED"`) || !strings.Contains(logs.String(), `"stage":"grpc"`) {
		t.Fatalf("logs:\n%s", logs)
	}
	metrics := selfMetrics(t, server)
	if !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} 7`) || !strings.Contains(metrics, `http_exporter_scrape_http_status_code{collector="queue_stats"} 0`) {
		t.Fatalf("%s", metrics)
	}

	calls := len(upstream.Calls())
	// A message probe parameter replaces the collector's, placeholders and
	// all, so param_queue has nothing to fill.
	recorder = probeOnce(t, server, probe+"&message="+url.QueryEscape(`{"queue": 5}`), nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "the message probe parameter replaces request.message") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	recorder = probeOnce(t, server, "/probe?collector=queue_stats&target="+url.QueryEscape(upstream.Addr)+"&message="+url.QueryEscape(`{"queue": 5}`), nil)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "collector queue_stats message failed: request.message does not fit acme.queue.v1.GetStatsRequest") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if len(upstream.Calls()) != calls {
		t.Fatal("a message that does not fit was sent")
	}
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} -1`) {
		t.Fatalf("%s", metrics)
	}

	// A probe parameter another type owns is refused before any call.
	recorder = probeOnce(t, server, probe+"&path=/x", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `request.type is "grpc"`) {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	// So is a malformed target.
	recorder = probeOnce(t, server, "/probe?collector=queue_stats&param_queue=orders&target=http://q:1/x", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "has the scheme http://") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}

	denied.Store(false)
	if recorder = probeOnce(t, server, probe, nil); recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_scrape_grpc_status_code{collector="queue_stats"} 0`) {
		t.Fatalf("%s", metrics)
	}
}
