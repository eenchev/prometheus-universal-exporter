package exporter

import (
	"log/slog"
	"os"
	"reflect"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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

// A target's le is not given to a histogram, nor its quantile to a summary:
// the series has that label on every bucket or quantile already, and one of
// its own would be written on _sum and _count too, which no parser reads.
// Every other series gets both.
func TestStaticTargetLabelsLeaveAHistogramItsLeAndASummaryItsQuantile(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Count: 1}},
		{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{Count: 1}},
		{Name: "g", Type: model.GaugeMetricType},
	}}
	out := withTargetLabels(set, map[string]string{"le": "target", "quantile": "target", "site": "a"})
	for i, want := range []map[string]string{
		{"quantile": "target", "site": "a"},
		{"le": "target", "site": "a"},
		{"le": "target", "quantile": "target", "site": "a"},
	} {
		if got := out.Metrics[i].Labels; !reflect.DeepEqual(got, want) {
			t.Errorf("%s has the labels %v, want %v", out.Metrics[i].Name, got, want)
		}
	}
	if err := out.Validate(model.Limits{}); err != nil {
		t.Errorf("the labelled set is refused: %v", err)
	}
}
