package config

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func otlpConfig(endpoint string) model.OTLPConfig {
	return model.OTLPConfig{
		Enabled:            true,
		Endpoint:           endpoint,
		ServiceName:        "prometheus-universal-exporter",
		ResourceAttributes: map[string]string{"deployment.environment": "test"},
		Interval:           model.Duration(time.Minute),
		Timeout:            model.Duration(5 * time.Second),
	}
}

func TestTargetFileRejectedWithoutOTLPExport(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "one", Collector: "text", Target: "http://target.invalid"}}}
	if err := ValidateTargets(file); err != nil {
		t.Fatal(err)
	}
	err := ValidateTargetsAgainst(file, cfg)
	if err == nil || !strings.Contains(err.Error(), "otlp.enabled") {
		t.Fatalf("ValidateTargetsAgainst() error=%v, want an OTLP requirement error", err)
	}

	cfg.OTLP = otlpConfig("http://collector.invalid/v1/metrics")
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargetsAgainst(file, cfg); err != nil {
		t.Fatalf("ValidateTargetsAgainst() with OTLP enabled: %v", err)
	}
}

// What a target may be is the collector's request type's to say, so it is
// checked against the configuration: an http collector needs an absolute URL.
func TestHTTPTargetsNeedAnAbsoluteURL(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]string{"": `target "one" has no target address`, "not-a-url": `target "one": must have an absolute target URL`} {
		file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "one", Collector: "text", Target: target}}}
		if err := ValidateTargets(file); err != nil {
			t.Fatal(err)
		}
		if err := ValidateTargetsAgainst(file, cfg); err == nil || err.Error() != want {
			t.Errorf("target %q: ValidateTargetsAgainst() error=%v, want %q", target, err, want)
		}
	}
}

func TestTargetFileRejectsUnknownCollector(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "one", Collector: "missing", Target: "http://target.invalid"}}}
	if err := ValidateTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargetsAgainst(file, cfg); err == nil || !strings.Contains(err.Error(), "unknown collector") {
		t.Fatalf("ValidateTargetsAgainst() error=%v, want an unknown collector error", err)
	}
}

func TestTargetFileValidationRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		file *model.TargetFile
		want string
	}{
		{name: "empty", file: &model.TargetFile{}, want: "must not be empty"},
		{name: "no collector", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Target: "http://a.invalid"}}}, want: "has no collector"},
		{name: "invalid name", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "bad-name", Collector: "text", Target: "http://a.invalid"}}}, want: "invalid name"},
		{name: "duplicate name", file: &model.TargetFile{Targets: []model.ScheduledTarget{
			{Name: "one", Collector: "text", Target: "http://a.invalid"},
			{Name: "one", Collector: "text", Target: "http://b.invalid"},
		}}, want: "duplicate target"},
		{name: "bad method", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{Method: "TRACE"}}}}, want: "unsupported method"},
		{name: "negative timeout", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{Timeout: model.Duration(-time.Second)}}}}, want: "timeout must not be negative"},
		{name: "negative retries", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{Retry: &model.RetryConfig{Attempts: -1}}}}}, want: "retry.attempts"},
		{name: "two bearer sources", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{BearerToken: "t", BearerTokenFile: "/f"}}}}, want: "bearer_token_file"},
		{name: "basic and bearer", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{BasicAuth: &model.BasicAuth{Username: "u", Password: "p"}, BearerToken: "t"}}}}, want: "basic and bearer"},
		{name: "invalid label", file: &model.TargetFile{Targets: []model.ScheduledTarget{{Collector: "text", Target: "http://a.invalid", Labels: map[string]string{"not a label": "x"}}}}, want: "invalid label name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateTargets(test.file)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadTargetFileRejectsUnknownFields(t *testing.T) {
	path := t.TempDir() + "/targets.yaml"
	if err := os.WriteFile(path, []byte("targets:\n  - collector: text\n    surprise: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTargets(path); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("LoadTargets() error=%v, want an unknown-field error", err)
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
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, configPath, slog.Default())
	manager.SetTargets("", &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "one", Collector: "text", Target: "http://a.invalid"}}})

	disabled := strings.Replace(enabled, "enabled: true", "enabled: false", 1)
	if err := os.WriteFile(configPath, []byte(disabled), 0600); err != nil {
		t.Fatal(err)
	}
	manager.LastMod = time.Time{}
	manager.ReloadConfig()
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
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file, err := LoadTargets(targetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargets(file); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, dir+"/config.yaml", slog.Default())
	manager.SetTargets(targetsPath, file)

	if err := os.WriteFile(targetsPath, []byte("targets:\n  - name: two\n    collector: missing\n    target: http://a.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.TargetsLastMod = time.Time{}
	manager.ReloadTargets()
	targets := manager.Targets()
	if len(targets) != 1 || targets[0].Name != "one" {
		t.Fatalf("an invalid target reload must keep the previous document, got %+v", targets)
	}

	if err := os.WriteFile(targetsPath, []byte("targets:\n  - name: three\n    collector: text\n    target: http://b.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.TargetsLastMod = time.Time{}
	manager.ReloadTargets()
	if targets := manager.Targets(); len(targets) != 1 || targets[0].Name != "three" {
		t.Fatalf("a valid target reload should take effect, got %+v", targets)
	}
}
