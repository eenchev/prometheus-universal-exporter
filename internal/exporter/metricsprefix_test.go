package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// End to end through /probe: HELP and TYPE lines carry the prefixed name, a
// histogram's series are the prefixed family, other collectors are untouched,
// and the exporter's own metrics are never prefixed.
func TestMetricsPrefixOnTheProbeResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# HELP upstream_latency_seconds Latency\n# TYPE upstream_latency_seconds histogram\nupstream_latency_seconds_bucket{le=\"1\"} 2\nupstream_latency_seconds_bucket{le=\"+Inf\"} 3\nupstream_latency_seconds_sum 1.5\nupstream_latency_seconds_count 3\n"))
	}))
	defer upstream.Close()

	prefixed := model.Collector{Name: "prefixed", MetricsPrefix: "acme", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "prometheus"}}
	plain := model.Collector{Name: "plain", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "prometheus"}}
	cfg := &model.Config{Collectors: []model.Collector{prefixed, plain}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())

	body := probeOnce(t, server, "/probe?collector=prefixed&target="+url.QueryEscape(upstream.URL), nil).Body.String()
	for _, want := range []string{
		"# HELP acme_upstream_latency_seconds Latency\n",
		"# TYPE acme_upstream_latency_seconds histogram\n",
		`acme_upstream_latency_seconds_bucket{le="1"} 2` + "\n",
		`acme_upstream_latency_seconds_bucket{le="+Inf"} 3` + "\n",
		"acme_upstream_latency_seconds_sum 1.5\n",
		"acme_upstream_latency_seconds_count 3\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\nupstream_latency_seconds") || strings.HasPrefix(body, "upstream_latency_seconds") {
		t.Errorf("an unprefixed series leaked:\n%s", body)
	}

	body = probeOnce(t, server, "/probe?collector=plain&target="+url.QueryEscape(upstream.URL), nil).Body.String()
	if !strings.Contains(body, "upstream_latency_seconds_count 3\n") || strings.Contains(body, "acme_") {
		t.Errorf("a collector without a prefix was prefixed:\n%s", body)
	}

	self := selfMetrics(t, server)
	if strings.Contains(self, "acme_") || !strings.Contains(self, "http_exporter_") {
		t.Errorf("the exporter's own metrics must not be prefixed:\n%s", testutil.FirstLines(self, 20))
	}
}

// OTLP export sees the same names as /probe, for probes and static targets
// alike; a static target's health metrics are the exporter's and stay
// unprefixed.
func TestMetricsPrefixOnOTLPExport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer upstream.Close()

	c := testutil.Collector("text", "text")
	c.MetricsPrefix = "acme"
	cfg := &model.Config{Collectors: []model.Collector{c}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "eu", Collector: "text", Target: upstream.URL}}}
	server := newStaticServer(t, cfg, file)

	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	resources := server.drainOTLP()
	if len(resources) != 1 {
		t.Fatalf("resources=%+v", resources)
	}
	if m := metricByName(resources[0].Set, "acme_demo_value"); m == nil || m.Value != 42 {
		t.Fatalf("scheduled metrics=%+v", resources[0].Set.Metrics)
	}
	if metricByName(resources[0].Set, "demo_value") != nil {
		t.Fatal("the unprefixed name was exported too")
	}
	if metricByName(resources[0].Set, "http_exporter_target_up") == nil {
		t.Fatal("the health metric must keep its own name")
	}

	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(upstream.URL), nil)
	resources = server.drainOTLP()
	if len(resources) != 1 || metricByName(resources[0].Set, "acme_demo_value") == nil {
		t.Fatalf("probe metrics queued for OTLP=%+v", resources)
	}
}

// The response cache is keyed by the collector's definition, so changing the
// prefix on reload never serves metrics cached under the old names.
func TestChangingTheMetricsPrefixChangesTheCacheKey(t *testing.T) {
	a := testutil.Collector("prefixed", "text")
	b := a
	b.MetricsPrefix = "acme"
	if collectorFingerprint(&a) == collectorFingerprint(&b) {
		t.Fatal("the prefix is not part of the cache key")
	}
}
