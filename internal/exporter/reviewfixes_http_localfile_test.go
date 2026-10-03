//go:build !select_request_types || (request_type_http && request_type_localfile)

package exporter

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// http_exporter_targets_refused_total belongs to the request types with
// targets to refuse: a localfile collector has no series in it.
func TestRefusedTargetsAreCountedOnlyWhereThereAreTargets(t *testing.T) {
	files := model.Collector{Name: "files", Request: model.RequestConfig{Type: "localfile", Root: t.TempDir(), Path: "m.prom"}, Transform: model.TransformConfig{Type: "prometheus"}}
	server := flightServer(t, testutil.Collector("web", "text"), files)
	metrics := selfMetrics(t, server)
	if !strings.Contains(metrics, `http_exporter_targets_refused_total{collector="web"} 0`) {
		t.Fatalf("no series for the http collector:\n%s", metrics)
	}
	if strings.Contains(metrics, `http_exporter_targets_refused_total{collector="files"}`) {
		t.Fatal("a localfile collector has a refused-targets series")
	}
}
