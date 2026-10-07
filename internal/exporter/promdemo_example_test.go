//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.promdemo.prometheus-test.yaml passes a Prometheus server's
// own /metrics through, keeping the families its include and exclude pick
// and renaming some. testdata/prometheus/prometheus-server-metrics.prom is an
// exposition as a Prometheus server writes it, cut down to the families the
// example keeps and a few of each kind it leaves out.

const promDemoConfig = "../../examples/config.promdemo.prometheus-test.yaml"

// The families the example names are passed through as the server wrote
// them: the histogram with its buckets, sum and count, the summary with its
// quantiles, every label, and each family's own type. The process_ and go_
// families it keeps have their new names, and what it does not name is left
// out: the other go_ families, the WAL summary, the connection tracker, the
// handler's own counter, and the one process_ family exclude takes back.
// Nothing is logged, and /metrics is asked for once.
func TestThePrometheusDemoExamplePassesThroughWhatItPicks(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service, cfg := newStandIn(t, promDemoConfig, map[string]standInAnswer{
		"/metrics": {"text/plain; version=0.0.4; charset=utf-8", readTestdata(t, "prometheus/prometheus-server-metrics.prom")},
	})
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "prometheus_server" {
		t.Fatalf("%s no longer holds the one collector prometheus_server", promDemoConfig)
	}
	server := NewServer(config.NewManager(cfg, promDemoConfig, slog.Default()), "python3", slog.Default())

	response := probeOnce(t, server, "/probe?collector=prometheus_server&target="+service.URL, nil)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	sameSeries(t, samples(body), []string{
		`prometheus_goroutines 142`,
		`prometheus_process_cpu_seconds_total 48211.37`,
		`prometheus_process_max_fds 524288`,
		`prometheus_process_open_fds 61`,
		`prometheus_process_resident_memory_bytes 3.38137088e+08`,
		`prometheus_process_start_time_seconds 1.78991234567e+09`,
		`prometheus_process_virtual_memory_bytes 2.214592512e+09`,
		`prometheus_build_info{branch="HEAD",goarch="amd64",goos="linux",goversion="go1.25.1",revision="0a1b2c3d4e5f60718293a4b5c6d7e8f901234567",tags="netgo,builtinassets",version="3.6.0"} 1`,
		`prometheus_engine_query_duration_seconds{quantile="0.5",slice="inner_eval"} 0.000118402`,
		`prometheus_engine_query_duration_seconds{quantile="0.9",slice="inner_eval"} 0.001940117`,
		`prometheus_engine_query_duration_seconds{quantile="0.99",slice="inner_eval"} 0.014862993`,
		`prometheus_engine_query_duration_seconds_sum{slice="inner_eval"} 941.5521`,
		`prometheus_engine_query_duration_seconds_count{slice="inner_eval"} 1204518`,
		`prometheus_engine_query_duration_seconds{quantile="0.5",slice="queue_time"} 5.31e-06`,
		`prometheus_engine_query_duration_seconds{quantile="0.9",slice="queue_time"} 1.2044e-05`,
		`prometheus_engine_query_duration_seconds{quantile="0.99",slice="queue_time"} 4.8719e-05`,
		`prometheus_engine_query_duration_seconds_sum{slice="queue_time"} 12.8843`,
		`prometheus_engine_query_duration_seconds_count{slice="queue_time"} 1204518`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="0.1"} 80412`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="0.2"} 80790`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="0.4"} 80871`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="1"} 80902`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="3"} 80911`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="8"} 80913`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="20"} 80913`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="60"} 80913`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="120"} 80913`,
		`prometheus_http_request_duration_seconds_bucket{handler="/api/v1/query",le="+Inf"} 80913`,
		`prometheus_http_request_duration_seconds_sum{handler="/api/v1/query"} 1204.337`,
		`prometheus_http_request_duration_seconds_count{handler="/api/v1/query"} 80913`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="0.1"} 29871`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="0.2"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="0.4"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="1"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="3"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="8"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="20"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="60"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="120"} 29880`,
		`prometheus_http_request_duration_seconds_bucket{handler="/metrics",le="+Inf"} 29880`,
		`prometheus_http_request_duration_seconds_sum{handler="/metrics"} 418.92`,
		`prometheus_http_request_duration_seconds_count{handler="/metrics"} 29880`,
		`prometheus_http_requests_total{code="200",handler="/api/v1/query"} 80655`,
		`prometheus_http_requests_total{code="400",handler="/api/v1/query"} 258`,
		`prometheus_http_requests_total{code="200",handler="/metrics"} 29880`,
		`prometheus_ready 1`,
		`prometheus_tsdb_head_chunks 41876`,
		`prometheus_tsdb_head_samples_appended_total{type="float"} 2.41883512e+08`,
		`prometheus_tsdb_head_samples_appended_total{type="histogram"} 0`,
		`prometheus_tsdb_head_series 12409`,
	})
	var types []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			types = append(types, line)
		}
	}
	sameSeries(t, types, []string{
		`# TYPE prometheus_goroutines gauge`,
		`# TYPE prometheus_process_cpu_seconds_total counter`,
		`# TYPE prometheus_process_max_fds gauge`,
		`# TYPE prometheus_process_open_fds gauge`,
		`# TYPE prometheus_process_resident_memory_bytes gauge`,
		`# TYPE prometheus_process_start_time_seconds gauge`,
		`# TYPE prometheus_process_virtual_memory_bytes gauge`,
		`# TYPE prometheus_build_info gauge`,
		`# TYPE prometheus_engine_query_duration_seconds summary`,
		`# TYPE prometheus_http_request_duration_seconds histogram`,
		`# TYPE prometheus_http_requests_total counter`,
		`# TYPE prometheus_ready gauge`,
		`# TYPE prometheus_tsdb_head_chunks gauge`,
		`# TYPE prometheus_tsdb_head_samples_appended_total counter`,
		`# TYPE prometheus_tsdb_head_series gauge`,
	})
	for _, left := range []string{
		"go_gc_duration_seconds", "go_goroutines", "go_memstats_alloc_bytes", "net_conntrack_",
		"process_virtual_memory_max_bytes", "prometheus_tsdb_wal_fsync_duration_seconds", "promhttp_",
		"\nprocess_", "# HELP process_",
	} {
		if strings.Contains(body, left) {
			t.Errorf("the answer holds %q, which the example leaves out or renames", left)
		}
	}
	if asked := service.requests(); !slices.Equal(asked, []string{"/metrics"}) {
		t.Errorf("the stand-in was asked %v, want /metrics once", asked)
	}
	if logs.Len() != 0 {
		t.Errorf("passing the exposition through logged:\n%s", logs)
	}
}

// Within the fifteen seconds of cache.ttl a second scrape is answered from
// memory, with the same series.
//
// The fifteen seconds are read from the result the first probe left in the
// cache, and that result is then moved an hour ahead (ageEntries), so the
// second probe comes within them however long after the first a busy machine
// makes it. On the machine's clock the two probes had to be made within
// fifteen seconds of each other.
func TestThePrometheusDemoExampleAnswersARepeatedScrapeFromMemory(t *testing.T) {
	testutil.CaptureLogs(t)
	service, cfg := newStandIn(t, promDemoConfig, map[string]standInAnswer{
		"/metrics": {"text/plain; version=0.0.4; charset=utf-8", readTestdata(t, "prometheus/prometheus-server-metrics.prom")},
	})
	server := NewServer(config.NewManager(cfg, promDemoConfig, slog.Default()), "python3", slog.Default())
	first := probeStandIn(t, server, service, "prometheus_server", "")
	var fresh []time.Duration
	server.cache.mu.Lock()
	for _, entry := range server.cache.entries {
		fresh = append(fresh, entry.freshUntil.Sub(entry.fetched))
	}
	server.cache.mu.Unlock()
	if !slices.Equal(fresh, []time.Duration{15 * time.Second}) {
		t.Fatalf("the first probe left results fresh for %v, want one, for the example's cache.ttl of 15s", fresh)
	}
	ageEntries(server, -time.Hour)
	sameSeries(t, probeStandIn(t, server, service, "prometheus_server", ""), first)
	if asked := len(service.requests()); asked != 1 {
		t.Errorf("the stand-in was asked %d times by two probes within cache.ttl, want once", asked)
	}
}
