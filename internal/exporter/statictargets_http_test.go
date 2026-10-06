//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func resourceByService(resources []otlpResourceSet, service string) *otlpResourceSet {
	for i := range resources {
		if resources[i].Identity.ServiceName == service {
			return &resources[i]
		}
	}
	return nil
}

func metricByName(set model.MetricSet, name string) *model.Metric {
	for i := range set.Metrics {
		if set.Metrics[i].Name == name {
			return &set.Metrics[i]
		}
	}
	return nil
}

func TestStaticScrapeGroupsMetricsByTargetResource(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer monitor-token" || r.Header.Get("X-Tenant") != "team-a" {
			t.Errorf("request headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()

	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{
			Name: "eu", Collector: "text", Target: target.URL, ExportViaOTLP: true,
			Request: model.TargetRequestConfig{Headers: map[string]string{"X-Tenant": "team-a"}, BearerToken: "monitor-token"},
			Labels:  map[string]string{"region": "eu"},
			OTLP:    model.TargetOTLPConfig{ServiceName: "legacy-app", ResourceAttributes: map[string]string{"team": "platform"}},
		},
		{
			Name: "us", Collector: "text", Target: target.URL, ExportViaOTLP: true,
			Request: model.TargetRequestConfig{Headers: map[string]string{"X-Tenant": "team-a"}, BearerToken: "monitor-token"},
			Labels:  map[string]string{"region": "us"},
		},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 0)
	resources := server.drainOTLP()
	if len(resources) != 2 {
		t.Fatalf("expected one resource per service name, got %d", len(resources))
	}

	legacy := resourceByService(resources, "legacy-app")
	if legacy == nil {
		t.Fatalf("missing the per-target resource: %+v", resources)
	}
	if legacy.Identity.Attributes["team"] != "platform" || legacy.Identity.Attributes["deployment.environment"] != "test" {
		t.Fatalf("per-target attributes did not merge over the defaults: %+v", legacy.Identity)
	}
	value := metricByName(legacy.Set, "demo_value")
	if value == nil || value.Value != 42 || value.Labels["region"] != "eu" {
		t.Fatalf("collector metric=%+v", value)
	}
	if up := metricByName(legacy.Set, "http_exporter_target_up"); up == nil || up.Value != 1 || up.Labels["static_target"] != "eu" {
		t.Fatalf("health metric=%+v", up)
	}
	if metricByName(legacy.Set, "http_exporter_target_scrape_duration_seconds") == nil {
		t.Fatalf("missing scrape duration metric: %+v", legacy.Set.Metrics)
	}

	fallback := resourceByService(resources, cfg.OTLP.ServiceName)
	if fallback == nil {
		t.Fatalf("target without its own service name should use the exporter default: %+v", resources)
	}
	if value := metricByName(fallback.Set, "demo_value"); value == nil || value.Labels["region"] != "us" {
		t.Fatalf("second target metric=%+v", value)
	}
}

func TestStaticScrapeReportsFailureAsTargetDown(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "down", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)

	server.scrapeStaticTargets(context.Background(), 0)
	resources := server.drainOTLP()
	if len(resources) != 1 {
		t.Fatalf("resources=%d", len(resources))
	}
	up := metricByName(resources[0].Set, "http_exporter_target_up")
	if up == nil || up.Value != 0 {
		t.Fatalf("failed scrape should export a zero health metric: %+v", resources[0].Set.Metrics)
	}
	if metricByName(resources[0].Set, "demo_value") != nil {
		t.Fatalf("a failed scrape must not export collector metrics: %+v", resources[0].Set.Metrics)
	}
}

func TestStaticScrapeUsesTheCollectorCache(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	collector := testutil.Collector("text", "text")
	collector.Cache.TTL = model.Duration(time.Minute)
	cfg := &model.Config{Collectors: []model.Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "cached", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)

	server.scrapeStaticTargets(context.Background(), 0)
	_ = server.drainOTLP()
	server.scrapeStaticTargets(context.Background(), 0)
	resources := server.drainOTLP()
	if got := requests.Load(); got != 1 {
		t.Fatalf("target requests=%d, want 1", got)
	}
	if value := metricByName(resources[0].Set, "demo_value"); value == nil || value.Value != 42 {
		t.Fatalf("cached scrape did not export metrics: %+v", resources[0].Set.Metrics)
	}
	exposition := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_cache_hits_total{collector="text"} 1`,
		`http_exporter_scrapes_total{collector="text"} 2`,
		"http_exporter_static_targets 1",
	} {
		if !strings.Contains(exposition, want) {
			t.Fatalf("self-metrics missing %q:\n%s", want, exposition)
		}
	}
}

// A static target's own request parameters are part of its cache key: two
// targets of one collector at one address that differ only in the values of
// one, here request.accept_status, each make their own trip, and the one
// that refuses the status fails rather than being answered with the other's
// result.
func TestStaticTargetsOwnRequestKeepsTheirCacheEntriesApart(t *testing.T) {
	var requests atomic.Int64
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer unavailable.Close()
	collector := testutil.Collector("text", "text")
	collector.Cache.TTL = model.Duration(time.Minute)
	cfg := &model.Config{Collectors: []model.Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	accepting := model.StaticTarget{ExportViaOTLP: true, Name: "accepting", Collector: "text", Target: unavailable.URL, Request: model.TargetRequestConfig{AcceptStatus: []string{"503"}}}
	plain := model.StaticTarget{ExportViaOTLP: true, Name: "plain", Collector: "text", Target: unavailable.URL, Request: model.TargetRequestConfig{AcceptStatus: []string{"2xx"}}}
	server := newStaticServer(t, cfg, &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{accepting, plain}})
	// One at a time, the accepting target first, so its result is cached
	// before the other looks.
	up := map[string]float64{}
	for _, static := range []model.StaticTarget{accepting, plain} {
		server.scrapeTarget(context.Background(), server.manager.Get(), static)
		for _, resource := range server.drainOTLP() {
			for _, metric := range resource.Set.Metrics {
				if metric.Name == "http_exporter_target_up" {
					up[metric.Labels["static_target"]] = metric.Value
				}
			}
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("target requests=%d, want one for each static target", got)
	}
	if up["accepting"] != 1 || up["plain"] != 0 {
		t.Fatalf("up=%v, want the target accepting 503 up and the one accepting only 2xx down", up)
	}
}

func TestStaticScrapePayloadCarriesSeparateResources(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()

	received := make(chan otlpPayload, 1)
	collectorEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readOTLPBody(t, r)
		var payload otlpPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("payload: %v", err)
		}
		received <- payload
		w.WriteHeader(http.StatusOK)
	}))
	defer collectorEndpoint.Close()

	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(collectorEndpoint.URL + "/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{
		Name: "eu", Collector: "text", Target: target.URL, ExportViaOTLP: true,
		Labels: map[string]string{"region": "eu"},
		OTLP:   model.TargetOTLPConfig{ServiceName: "legacy-app"},
	}}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 0)
	server.exportOTLP(context.Background(), time.Minute)

	select {
	case payload := <-received:
		if len(payload.ResourceMetrics) != 2 {
			t.Fatalf("expected the target resource and the exporter resource, got %d", len(payload.ResourceMetrics))
		}
		services := map[string]bool{}
		for _, resource := range payload.ResourceMetrics {
			for _, attribute := range resource.Resource.Attributes {
				if attribute.Key == "service.name" {
					services[attribute.Value.StringValue] = true
				}
			}
		}
		if !services["legacy-app"] || !services[cfg.OTLP.ServiceName] {
			t.Fatalf("payload services=%v", services)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no OTLP payload was delivered")
	}
}
