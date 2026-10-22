//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/base64"
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

// An exporter in allocator mode that starts without a bundle awaits the
// first (allocatormode.go): /health answers, /ready answers 503, and every
// other endpoint answers 503 with the sentence it was given, whatever the
// method. The bundle carries web.basic_auth, and the landing page, the
// collectors page, the self-metrics, the static targets, the debug page and
// /-/reload were served to anyone until it came. The first configuration put
// in force makes it ready, and its credential is asked for.
func TestAnExporterAwaitingItsConfigurationAnswers503UntilOneIsInForce(t *testing.T) {
	dir := t.TempDir()
	manager := config.NewAwaitingManager(filepath.Join(dir, "config.yaml"), slog.Default())
	manager.SetTargets(filepath.Join(dir, "static-targets.yaml"), nil)
	manager.AllowAbsentTargets()
	server := NewServer(manager, "python3", slog.Default())
	server.SetDebug(DebugFull)
	const waiting = "no configuration yet: waiting for the allocator at http://pue-allocator:8080"
	server.SetAwaitingReason(waiting)
	get := func(path string) *httptest.ResponseRecorder { return probeOnce(t, server, path, nil) }
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("value=7\n")) }))
	defer target.Close()
	probe := "/probe?collector=a&target=" + url.QueryEscape(target.URL)

	if r := get("/ready"); r.Code != http.StatusServiceUnavailable || r.Body.String() != "not ready: no configuration yet: waiting for the allocator\n" {
		t.Errorf("/ready: %d %q", r.Code, r.Body.String())
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, probe}, {http.MethodPost, probe}, {http.MethodGet, "/static-targets"}, {http.MethodGet, "/self-metrics"},
		{http.MethodGet, "/"}, {http.MethodGet, "/collectors"}, {http.MethodGet, "/debug"}, {http.MethodPost, "/debug/run"},
		{http.MethodPost, "/-/reload"}, {http.MethodPut, "/-/reload"}, {http.MethodGet, "/-/reload"},
	} {
		r := get(request.path)
		if request.method != http.MethodGet {
			r = httptest.NewRecorder()
			server.Handler().ServeHTTP(r, httptest.NewRequest(request.method, request.path, strings.NewReader("{}")))
		}
		if r.Code != http.StatusServiceUnavailable || r.Body.String() != waiting+"\n" {
			t.Errorf("%s %s: %d %q", request.method, request.path, r.Code, r.Body.String())
		}
	}
	if r := get("/health"); r.Code != http.StatusOK {
		t.Errorf("/health: %d", r.Code)
	}

	testutil.WriteIn(t, dir, "config.yaml", "web:\n  basic_auth:\n    enabled: true\n    username: exporter\n    password: s3cret\ncollectors:\n  - name: a\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: a_value\n        expression: 'value=(\\d+)'\n")
	if err := manager.Reload("allocator"); err != nil {
		t.Fatal(err)
	}
	if r := get("/ready"); r.Code != http.StatusOK {
		t.Errorf("/ready once configured: %d %q", r.Code, r.Body.String())
	}
	for _, path := range []string{probe, "/self-metrics", "/"} {
		if r := get(path); r.Code != http.StatusUnauthorized {
			t.Errorf("%s once configured, without the credential: %d %q", path, r.Code, r.Body.String())
		}
	}
	credential := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("exporter:s3cret"))}}
	if r := probeOnce(t, server, probe, credential); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "a_value 7") {
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
// allocator: the generation of the configuration in force, when it last synced, its sync errors
// by stage and the bundles it refused, each family with its description.
// Outside allocator mode there are no such series.
func TestTheAllocatorSelfMetrics(t *testing.T) {
	server, _ := newCacheTestServer(t, testutil.Collector("a", "text"))
	if got := selfMetrics(t, server); strings.Contains(got, "http_exporter_allocator_") {
		t.Fatalf("an exporter outside allocator mode has allocator series:\n%s", got)
	}
	synced := time.Unix(1790000000, 0)
	server.manager.SetGeneration("0a1b")
	server.SetAllocatorStatus(func() AllocatorStatus {
		return AllocatorStatus{LastSync: synced, SyncErrors: map[string]uint64{"write": 0, "heartbeat": 3, "fetch": 1}, Refused: 2}
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
	server.manager.SetGeneration("")
	server.SetAllocatorStatus(func() AllocatorStatus { return AllocatorStatus{SyncErrors: map[string]uint64{}} })
	if got := selfMetrics(t, server); strings.Contains(got, "http_exporter_allocator_generation_info") || !strings.Contains(got, "http_exporter_allocator_last_sync_timestamp_seconds 0\n") {
		t.Errorf("before the first bundle:\n%s", got)
	}
}
