package exporter

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func newStaticServer(t *testing.T, cfg *model.Config, file *model.StaticTargetFile) *Server {
	t.Helper()
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if file != nil {
		if err := config.ValidateStaticTargets(file); err != nil {
			t.Fatal(err)
		}
		if err := config.ValidateStaticTargetsAgainst(file, cfg); err != nil {
			t.Fatal(err)
		}
	}
	manager := config.NewManager(cfg, "", slog.Default())
	manager.SetTargets("", file)
	return NewServer(manager, "python3", slog.Default())
}

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

func TestLoadStaticTargetFileParsesEveryRequestParameter(t *testing.T) {
	path := t.TempDir() + "/targets.yaml"
	document := `interval: 1m
targets:
  - name: legacy_eu
    collector: text
    target: http://legacy.example:8080
    request:
      method: POST
      path: /api/status
      body: raw payload
      timeout: 5s
      insecure_skip_verify: true
      retry:
        attempts: 2
        backoff: 1s
      headers:
        X-Tenant: team-a
      bearer_token: monitor-token
    labels:
      environment: production
    export_via_otlp: true
    otlp:
      service_name: legacy-app
      resource_attributes:
        deployment.environment: production
`
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadStaticTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	target := file.Targets[0]
	if !target.ExportViaOTLP {
		t.Fatal("export_via_otlp was not read")
	}

	overrides := fetch.TargetOverrides(&target)
	if overrides.Method != http.MethodPost || !overrides.PathSet || overrides.Path != "/api/status" {
		t.Fatalf("method/path overrides: %+v", overrides)
	}
	if overrides.Body == nil || *overrides.Body != "raw payload" || overrides.Timeout != 5*time.Second {
		t.Fatalf("body/timeout overrides: %+v", overrides)
	}
	if overrides.InsecureSkipVerify == nil || !*overrides.InsecureSkipVerify {
		t.Fatalf("tls override: %+v", overrides.InsecureSkipVerify)
	}
	if overrides.RetryAttempts == nil || *overrides.RetryAttempts != 2 || overrides.RetryBackoff == nil || *overrides.RetryBackoff != time.Second {
		t.Fatalf("retry overrides: %+v", overrides)
	}
	headers, err := fetch.TargetHeaders(&target)
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("X-Tenant") != "team-a" || headers.Get("Authorization") != "Bearer monitor-token" {
		t.Fatalf("headers=%v", headers)
	}
	query := targetCacheQuery(&target)
	for key, want := range map[string]string{
		"target": "http://legacy.example:8080", "collector": "text", "method": "POST",
		"path": "/api/status", "body": "raw payload", "timeout": "5s",
		"insecure_skip_verify": "true", "retry_attempts": "2", "retry_backoff": "1s",
	} {
		if query.Get(key) != want {
			t.Fatalf("cache query %q=%q, want %q", key, query.Get(key), want)
		}
	}
	identity := targetResource(&target, otlpConfig("http://collector.invalid/v1/metrics"))
	if identity.ServiceName != "legacy-app" || identity.Attributes["deployment.environment"] != "production" {
		t.Fatalf("resource identity=%+v", identity)
	}
}

func TestStaticTargetResourceInheritsExporterDefaults(t *testing.T) {
	cfg := otlpConfig("http://collector.invalid/v1/metrics")
	target := model.StaticTarget{ExportViaOTLP: true, Name: "plain", Collector: "text", Target: "http://a.invalid"}
	identity := targetResource(&target, cfg)
	if identity.ServiceName != cfg.ServiceName || identity.Attributes["deployment.environment"] != "test" {
		t.Fatalf("identity=%+v, want the exporter defaults", identity)
	}
	target.OTLP = model.TargetOTLPConfig{ResourceAttributes: map[string]string{"deployment.environment": "staging", "team": "platform"}}
	identity = targetResource(&target, cfg)
	if identity.ServiceName != cfg.ServiceName {
		t.Fatalf("service name should fall back to the exporter default: %+v", identity)
	}
	if identity.Attributes["deployment.environment"] != "staging" || identity.Attributes["team"] != "platform" {
		t.Fatalf("per-target attributes should merge over the defaults: %+v", identity)
	}
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
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
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

	server.scrapeStaticTargets(context.Background(), 10*time.Second)
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

	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	_ = server.drainOTLP()
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
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

func TestStaticTargetLabelsDoNotOverrideMetricLabels(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{{Name: "demo", Labels: map[string]string{"region": "from-metric"}}}}
	out := withTargetLabels(set, map[string]string{"region": "from-target", "environment": "production"})
	if out.Metrics[0].Labels["region"] != "from-metric" {
		t.Fatalf("target label overwrote an extracted label: %v", out.Metrics[0].Labels)
	}
	if out.Metrics[0].Labels["environment"] != "production" {
		t.Fatalf("target label was not applied: %v", out.Metrics[0].Labels)
	}
	if set.Metrics[0].Labels["environment"] != "" {
		t.Fatalf("the source metric set was mutated: %v", set.Metrics[0].Labels)
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
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	server.exportOTLP(context.Background(), 5*time.Second)

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
	case <-time.After(5 * time.Second):
		t.Fatal("no OTLP payload was delivered")
	}
}
