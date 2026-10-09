//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The last export at shutdown that fails drops its data points, since no
// export follows to keep them for: they are counted in
// http_exporter_otlp_points_dropped_total, nothing stays pending, and the
// warning says they are dropped and how many, not that they are kept. An
// export before the shutdown that fails the same way still keeps its points,
// counts none dropped and says they are kept. The endpoint answers 503 to
// every export, which is retried until the budget runs out.
func TestAFailedLastOTLPExportDropsAndCountsItsPoints(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	server := otlpServer(t, endpoint.server.URL)
	server.manager.Get().OTLP.Timeout = model.Duration(50 * time.Millisecond)
	var log bytes.Buffer
	server.logger = slog.New(slog.NewTextHandler(&log, nil))
	queueProbeMetric(server, "first_value", 1)
	queueProbeMetric(server, "second_value", 1)

	server.exportOTLP(context.Background(), 50*time.Millisecond)
	if _, kept := pendingValue(server, "first_value"); !kept {
		t.Fatal("an export before the shutdown that failed did not keep its points")
	}
	if got := seriesValue(t, selfMetrics(t, server), "http_exporter_otlp_points_dropped_total"); got != 0 {
		t.Fatalf("an export before the shutdown that failed counted %v points dropped", got)
	}
	if before := log.String(); !strings.Contains(before, `msg="OTLP export failed; its data points are kept for the next export"`) || strings.Contains(before, "dropped") {
		t.Fatalf("an export before the shutdown that failed logged:\n%s", before)
	}
	log.Reset()

	queueProbeMetric(server, "third_value", 1)
	requests := endpoint.count()
	server.FlushOTLP()
	if endpoint.count() == requests {
		t.Fatal("the last export was not attempted")
	}
	for _, name := range []string{"first_value", "second_value", "third_value"} {
		if _, kept := pendingValue(server, name); kept {
			t.Errorf("%s is still pending after the last export failed", name)
		}
	}
	exposition := selfMetrics(t, server)
	if got := seriesValue(t, exposition, "http_exporter_otlp_points_dropped_total"); got != 3 {
		t.Errorf("the last export that failed counted %v points dropped, want its 3", got)
	}
	if got := seriesValue(t, exposition, `http_exporter_otlp_exports_total{result="failure"}`); got != 2 {
		t.Errorf("%v failed exports are counted, want 2", got)
	}
	last := log.String()
	if !strings.Contains(last, `msg="the last OTLP export before exiting failed; its data points are dropped"`) || !strings.Contains(last, "dropped_points=3") || strings.Contains(last, "kept for the next export") {
		t.Errorf("the last export that failed logged:\n%s", last)
	}
}
