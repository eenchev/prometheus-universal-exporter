package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func otlpConfig(endpoint string) OTLPConfig {
	return OTLPConfig{
		Enabled:            true,
		Endpoint:           endpoint,
		ServiceName:        "prometheus-universal-exporter",
		ResourceAttributes: map[string]string{"deployment.environment": "test"},
		Interval:           Duration(time.Minute),
		Timeout:            Duration(5 * time.Second),
	}
}

func newScheduledServer(t *testing.T, cfg *Config, file *TargetFile) *Server {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if file != nil {
		if err := file.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := file.ValidateAgainst(cfg); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewConfigManager(cfg, "", slog.Default())
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

func metricByName(set MetricSet, name string) *Metric {
	for i := range set.Metrics {
		if set.Metrics[i].Name == name {
			return &set.Metrics[i]
		}
	}
	return nil
}

func TestTargetFileRejectedWithoutOTLPExport(t *testing.T) {
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "text", Target: "http://target.invalid"}}}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	err := file.ValidateAgainst(cfg)
	if err == nil || !strings.Contains(err.Error(), "otlp.enabled") {
		t.Fatalf("ValidateAgainst() error=%v, want an OTLP requirement error", err)
	}

	cfg.OTLP = otlpConfig("http://collector.invalid/v1/metrics")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateAgainst(cfg); err != nil {
		t.Fatalf("ValidateAgainst() with OTLP enabled: %v", err)
	}
}

// What a target may be is the collector's request type's to say, so it is
// checked against the configuration: an http collector needs an absolute URL.
func TestHTTPTargetsNeedAnAbsoluteURL(t *testing.T) {
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]string{"": `target "one" has no target address`, "not-a-url": `target "one": must have an absolute target URL`} {
		file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "text", Target: target}}}
		if err := file.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := file.ValidateAgainst(cfg); err == nil || err.Error() != want {
			t.Errorf("target %q: ValidateAgainst() error=%v, want %q", target, err, want)
		}
	}
}

func TestTargetFileRejectsUnknownCollector(t *testing.T) {
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "missing", Target: "http://target.invalid"}}}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateAgainst(cfg); err == nil || !strings.Contains(err.Error(), "unknown collector") {
		t.Fatalf("ValidateAgainst() error=%v, want an unknown collector error", err)
	}
}

func TestTargetFileValidationRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		file *TargetFile
		want string
	}{
		{name: "empty", file: &TargetFile{}, want: "must not be empty"},
		{name: "no collector", file: &TargetFile{Targets: []ScheduledTarget{{Target: "http://a.invalid"}}}, want: "has no collector"},
		{name: "invalid name", file: &TargetFile{Targets: []ScheduledTarget{{Name: "bad-name", Collector: "text", Target: "http://a.invalid"}}}, want: "invalid name"},
		{name: "duplicate name", file: &TargetFile{Targets: []ScheduledTarget{
			{Name: "one", Collector: "text", Target: "http://a.invalid"},
			{Name: "one", Collector: "text", Target: "http://b.invalid"},
		}}, want: "duplicate target"},
		{name: "bad method", file: &TargetFile{Targets: []ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: TargetRequestConfig{Method: "TRACE"}}}}, want: "unsupported method"},
		{name: "negative timeout", file: &TargetFile{Targets: []ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: TargetRequestConfig{Timeout: Duration(-time.Second)}}}}, want: "timeout must not be negative"},
		{name: "negative retries", file: &TargetFile{Targets: []ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: TargetRequestConfig{Retry: &RetryConfig{Attempts: -1}}}}}, want: "retry.attempts"},
		{name: "two bearer sources", file: &TargetFile{Targets: []ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: TargetRequestConfig{BearerToken: "t", BearerTokenFile: "/f"}}}}, want: "bearer_token_file"},
		{name: "basic and bearer", file: &TargetFile{Targets: []ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: TargetRequestConfig{BasicAuth: &BasicAuth{Username: "u", Password: "p"}, BearerToken: "t"}}}}, want: "basic and bearer"},
		{name: "invalid label", file: &TargetFile{Targets: []ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Labels: map[string]string{"not a label": "x"}}}}, want: "invalid label name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.file.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadTargetFileParsesEveryRequestParameter(t *testing.T) {
	path := t.TempDir() + "/targets.yaml"
	document := `targets:
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
    otlp:
      service_name: legacy-app
      resource_attributes:
        deployment.environment: production
`
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := LoadTargetFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	target := file.Targets[0]

	overrides := target.overrides()
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
	headers, err := target.headers()
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("X-Tenant") != "team-a" || headers.Get("Authorization") != "Bearer monitor-token" {
		t.Fatalf("headers=%v", headers)
	}
	query := target.cacheQuery()
	for key, want := range map[string]string{
		"target": "http://legacy.example:8080", "collector": "text", "method": "POST",
		"path": "/api/status", "body": "raw payload", "timeout": "5s",
		"insecure_skip_verify": "true", "retry_attempts": "2", "retry_backoff": "1s",
	} {
		if query.Get(key) != want {
			t.Fatalf("cache query %q=%q, want %q", key, query.Get(key), want)
		}
	}
	identity := target.resource(otlpConfig("http://collector.invalid/v1/metrics"))
	if identity.ServiceName != "legacy-app" || identity.Attributes["deployment.environment"] != "production" {
		t.Fatalf("resource identity=%+v", identity)
	}
}

func TestLoadTargetFileRejectsUnknownFields(t *testing.T) {
	path := t.TempDir() + "/targets.yaml"
	if err := os.WriteFile(path, []byte("targets:\n  - collector: text\n    surprise: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTargetFile(path); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("LoadTargetFile() error=%v, want an unknown-field error", err)
	}
}

func TestScheduledTargetResourceInheritsExporterDefaults(t *testing.T) {
	cfg := otlpConfig("http://collector.invalid/v1/metrics")
	target := ScheduledTarget{Name: "plain", Collector: "text", Target: "http://a.invalid"}
	identity := target.resource(cfg)
	if identity.ServiceName != cfg.ServiceName || identity.Attributes["deployment.environment"] != "test" {
		t.Fatalf("identity=%+v, want the exporter defaults", identity)
	}
	target.OTLP = TargetOTLPConfig{ResourceAttributes: map[string]string{"deployment.environment": "staging", "team": "platform"}}
	identity = target.resource(cfg)
	if identity.ServiceName != cfg.ServiceName {
		t.Fatalf("service name should fall back to the exporter default: %+v", identity)
	}
	if identity.Attributes["deployment.environment"] != "staging" || identity.Attributes["team"] != "platform" {
		t.Fatalf("per-target attributes should merge over the defaults: %+v", identity)
	}
}

func TestScheduledScrapeGroupsMetricsByTargetResource(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer monitor-token" || r.Header.Get("X-Tenant") != "team-a" {
			t.Errorf("request headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()

	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &TargetFile{Targets: []ScheduledTarget{
		{
			Name: "eu", Collector: "text", Target: target.URL,
			Request: TargetRequestConfig{Headers: map[string]string{"X-Tenant": "team-a"}, BearerToken: "monitor-token"},
			Labels:  map[string]string{"region": "eu"},
			OTLP:    TargetOTLPConfig{ServiceName: "legacy-app", ResourceAttributes: map[string]string{"team": "platform"}},
		},
		{
			Name: "us", Collector: "text", Target: target.URL,
			Request: TargetRequestConfig{Headers: map[string]string{"X-Tenant": "team-a"}, BearerToken: "monitor-token"},
			Labels:  map[string]string{"region": "us"},
		},
	}}
	server := newScheduledServer(t, cfg, file)
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
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
	if up := metricByName(legacy.Set, "http_exporter_target_up"); up == nil || up.Value != 1 || up.Labels["scheduled_target"] != "eu" {
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

func TestScheduledScrapeReportsFailureAsTargetDown(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer target.Close()
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "down", Collector: "text", Target: target.URL}}}
	server := newScheduledServer(t, cfg, file)

	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
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

func TestScheduledScrapeUsesTheCollectorCache(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	collector := testCollector("text", "text")
	collector.Cache = Duration(time.Minute)
	cfg := &Config{Collectors: []Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "cached", Collector: "text", Target: target.URL}}}
	server := newScheduledServer(t, cfg, file)

	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	_ = server.drainOTLP()
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
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
		"http_exporter_scheduled_targets 1",
	} {
		if !strings.Contains(exposition, want) {
			t.Fatalf("self-metrics missing %q:\n%s", want, exposition)
		}
	}
}

func TestScheduledTargetLabelsDoNotOverrideMetricLabels(t *testing.T) {
	set := MetricSet{Metrics: []Metric{{Name: "demo", Labels: map[string]string{"region": "from-metric"}}}}
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

func TestScheduledScrapePayloadCarriesSeparateResources(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()

	received := make(chan otlpPayload, 1)
	collectorEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload otlpPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("payload: %v", err)
		}
		received <- payload
		w.WriteHeader(http.StatusOK)
	}))
	defer collectorEndpoint.Close()

	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig(collectorEndpoint.URL + "/v1/metrics")}
	file := &TargetFile{Targets: []ScheduledTarget{{
		Name: "eu", Collector: "text", Target: target.URL,
		Labels: map[string]string{"region": "eu"},
		OTLP:   TargetOTLPConfig{ServiceName: "legacy-app"},
	}}}
	server := newScheduledServer(t, cfg, file)
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	server.pushOTLP(appendToResource(server.drainOTLP(), defaultResourceIdentity(cfg.OTLP), server.selfMetricSet()))

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

func TestConfigReloadRejectedWhenItWouldDisableOTLPWithTargets(t *testing.T) {
	dir := t.TempDir()
	configPath := dir + "/config.yaml"
	enabled := "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\n" +
		"collectors:\n  - name: text\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	if err := os.WriteFile(configPath, []byte(enabled), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, configPath, slog.Default())
	manager.SetTargets("", &TargetFile{Targets: []ScheduledTarget{{Name: "one", Collector: "text", Target: "http://a.invalid"}}})

	disabled := strings.Replace(enabled, "enabled: true", "enabled: false", 1)
	if err := os.WriteFile(configPath, []byte(disabled), 0600); err != nil {
		t.Fatal(err)
	}
	manager.lastMod = time.Time{}
	manager.reloadConfig()
	if !manager.Get().OTLP.Enabled {
		t.Fatal("a reload that disables OTLP while targets are loaded must be rejected")
	}
}

func TestTargetFileReloadRejectsInvalidDocument(t *testing.T) {
	dir := t.TempDir()
	targetsPath := dir + "/targets.yaml"
	valid := "targets:\n  - name: one\n    collector: text\n    target: http://a.invalid\n"
	if err := os.WriteFile(targetsPath, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	file, err := LoadTargetFile(targetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, dir+"/config.yaml", slog.Default())
	manager.SetTargets(targetsPath, file)

	if err := os.WriteFile(targetsPath, []byte("targets:\n  - name: two\n    collector: missing\n    target: http://a.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.targetsLastMod = time.Time{}
	manager.reloadTargets()
	targets := manager.Targets()
	if len(targets) != 1 || targets[0].Name != "one" {
		t.Fatalf("an invalid target reload must keep the previous document, got %+v", targets)
	}

	if err := os.WriteFile(targetsPath, []byte("targets:\n  - name: three\n    collector: text\n    target: http://b.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.targetsLastMod = time.Time{}
	manager.reloadTargets()
	if targets := manager.Targets(); len(targets) != 1 || targets[0].Name != "three" {
		t.Fatalf("a valid target reload should take effect, got %+v", targets)
	}
}

// The shipped examples must stay loadable and consistent with each other.
func TestShippedExampleFilesLoadTogether(t *testing.T) {
	cfg, err := LoadConfig("config.otlp.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OTLP.Enabled {
		t.Fatal("config.otlp.example.yaml must enable OTLP export")
	}
	file, err := LoadTargetFile("targets.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateAgainst(cfg); err != nil {
		t.Fatalf("targets.example.yaml does not match config.otlp.example.yaml: %v", err)
	}

	plain, err := LoadConfig("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateAgainst(plain); err == nil {
		t.Fatal("config.example.yaml leaves OTLP disabled, so the target file must be rejected against it")
	}
}
