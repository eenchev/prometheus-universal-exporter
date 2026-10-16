//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An exporter in allocator mode that starts without a bundle serves its
// endpoints while it awaits the first (allocatormode.go): /ready and /probe
// answer 503, /probe with the sentence it was given, the static targets
// endpoint has none and the self-metrics are served. The first
// configuration put in force makes it ready, and probes are answered.
func TestAnExporterAwaitingItsConfigurationAnswers503UntilOneIsInForce(t *testing.T) {
	dir := t.TempDir()
	manager := config.NewAwaitingManager(filepath.Join(dir, "config.yaml"), slog.Default())
	manager.SetTargets(filepath.Join(dir, "static-targets.yaml"), nil)
	manager.AllowAbsentTargets()
	server := NewServer(manager, "python3", slog.Default())
	server.SetAwaitingReason("no configuration yet: waiting for the allocator at http://pue-allocator:8080")
	get := func(path string) *httptest.ResponseRecorder { return probeOnce(t, server, path, nil) }
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("value=7\n")) }))
	defer target.Close()
	probe := "/probe?collector=a&target=" + url.QueryEscape(target.URL)

	if r := get("/ready"); r.Code != http.StatusServiceUnavailable || r.Body.String() != "not ready: no configuration yet: waiting for the allocator\n" {
		t.Errorf("/ready: %d %q", r.Code, r.Body.String())
	}
	if r := get(probe); r.Code != http.StatusServiceUnavailable || r.Body.String() != "no configuration yet: waiting for the allocator at http://pue-allocator:8080\n" {
		t.Errorf("/probe: %d %q", r.Code, r.Body.String())
	}
	if r := get("/static-targets"); r.Code != http.StatusOK || strings.Contains(r.Body.String(), "http_exporter_target_up") {
		t.Errorf("/static-targets: %d %q", r.Code, r.Body.String())
	}
	if r := get("/self-metrics"); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "http_exporter_build_info") {
		t.Errorf("/self-metrics: %d", r.Code)
	}
	if r := get("/health"); r.Code != http.StatusOK {
		t.Errorf("/health: %d", r.Code)
	}

	testutil.WriteIn(t, dir, "config.yaml", "collectors:\n  - name: a\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: a_value\n        expression: 'value=(\\d+)'\n")
	if err := manager.Reload("allocator"); err != nil {
		t.Fatal(err)
	}
	if r := get("/ready"); r.Code != http.StatusOK {
		t.Errorf("/ready once configured: %d %q", r.Code, r.Body.String())
	}
	if r := get(probe); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "a_value 7") {
		t.Errorf("/probe once configured: %d %q", r.Code, r.Body.String())
	}
}

// An exporter made with a configuration, as in file mode, never answers as
// one awaiting its configuration.
func TestAnExporterWithAConfigurationIsNotAwaitingOne(t *testing.T) {
	server, _ := newCacheTestServer(t, testutil.Collector("a", "text"))
	server.SetAwaitingReason("no configuration yet")
	if r := probeOnce(t, server, "/ready", nil); r.Code != http.StatusOK {
		t.Errorf("/ready: %d %q", r.Code, r.Body.String())
	}
	if r := probeOnce(t, server, "/probe?collector=a&target=http://127.0.0.1:1", nil); r.Code == http.StatusServiceUnavailable {
		t.Errorf("/probe: %d %q", r.Code, r.Body.String())
	}
}

// In allocator mode the self-metrics say how the exporter keeps up with the
// allocator: the generation in force, when it last synced, its sync errors
// by stage and the bundles it refused, each family with its description.
// Outside allocator mode there are no such series.
func TestTheAllocatorSelfMetrics(t *testing.T) {
	server, _ := newCacheTestServer(t, testutil.Collector("a", "text"))
	if got := selfMetrics(t, server); strings.Contains(got, "http_exporter_allocator_") {
		t.Fatalf("an exporter outside allocator mode has allocator series:\n%s", got)
	}
	synced := time.Unix(1790000000, 0)
	server.SetAllocatorStatus(func() AllocatorStatus {
		return AllocatorStatus{Generation: "0a1b", LastSync: synced, SyncErrors: map[string]uint64{"write": 0, "heartbeat": 3, "fetch": 1}, Refused: 2}
	})
	got := selfMetrics(t, server)
	for _, want := range []string{
		"# HELP http_exporter_allocator_generation_info " + exporterMetricHelp["http_exporter_allocator_generation_info"] + "\n# TYPE http_exporter_allocator_generation_info gauge\nhttp_exporter_allocator_generation_info{generation=\"0a1b\"} 1\n",
		"# HELP http_exporter_allocator_last_sync_timestamp_seconds " + exporterMetricHelp["http_exporter_allocator_last_sync_timestamp_seconds"] + "\n# TYPE http_exporter_allocator_last_sync_timestamp_seconds gauge\nhttp_exporter_allocator_last_sync_timestamp_seconds 1.79e+09\n",
		"# TYPE http_exporter_allocator_sync_errors_total counter\nhttp_exporter_allocator_sync_errors_total{stage=\"fetch\"} 1\nhttp_exporter_allocator_sync_errors_total{stage=\"heartbeat\"} 3\nhttp_exporter_allocator_sync_errors_total{stage=\"write\"} 0\n",
		"# HELP http_exporter_allocator_bundle_refused_total " + exporterMetricHelp["http_exporter_allocator_bundle_refused_total"] + "\n# TYPE http_exporter_allocator_bundle_refused_total counter\nhttp_exporter_allocator_bundle_refused_total 2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the self-metrics lack\n%s\nin\n%s", want, got)
		}
	}
	server.SetAllocatorStatus(func() AllocatorStatus { return AllocatorStatus{SyncErrors: map[string]uint64{}} })
	if got := selfMetrics(t, server); strings.Contains(got, "http_exporter_allocator_generation_info") || !strings.Contains(got, "http_exporter_allocator_last_sync_timestamp_seconds 0\n") {
		t.Errorf("before the first bundle:\n%s", got)
	}
}
