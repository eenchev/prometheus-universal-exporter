//go:build !select_request_types || request_type_http

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

// Static targets need no OTLP export: they are served on the static targets
// endpoint. Only a target with export_via_otlp needs it, and its otlp block is
// only allowed with it.
func TestAStaticTargetExportedViaOTLPNeedsOTLPExport(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://target.invalid"}}}
	if err := ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatalf("a static target without export_via_otlp needs no OTLP export: %v", err)
	}

	file.Targets[0].ExportViaOTLP = true
	err := ValidateStaticTargetsAgainst(file, cfg)
	if err == nil || !strings.Contains(err.Error(), `target "one" sets export_via_otlp`) || !strings.Contains(err.Error(), "otlp.enabled") {
		t.Fatalf("ValidateStaticTargetsAgainst() error=%v, want the OTLP requirement named", err)
	}
	cfg.OTLP = otlpConfig("http://collector.invalid/v1/metrics")
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatalf("ValidateStaticTargetsAgainst() with OTLP enabled: %v", err)
	}

	withIdentity := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://target.invalid", OTLP: model.TargetOTLPConfig{ServiceName: "app"}}}}
	if err := ValidateStaticTargets(withIdentity); err == nil || !strings.Contains(err.Error(), "only a target with export_via_otlp: true uses") {
		t.Fatalf("an otlp block without export_via_otlp: got %v, want it refused", err)
	}
	labelled := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://target.invalid", Labels: map[string]string{"static_target": "x"}}}}
	if err := ValidateStaticTargets(labelled); err == nil || !strings.Contains(err.Error(), "static_target") {
		t.Fatalf("a static_target label: got %v, want it refused", err)
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
		file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: target}}}
		if err := ValidateStaticTargets(file); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaticTargetsAgainst(file, cfg); err == nil || err.Error() != want {
			t.Errorf("target %q: ValidateStaticTargetsAgainst() error=%v, want %q", target, err, want)
		}
	}
}

func TestStaticTargetFileRejectsUnknownCollector(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "missing", Target: "http://target.invalid"}}}
	if err := ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(file, cfg); err == nil || !strings.Contains(err.Error(), "unknown collector") {
		t.Fatalf("ValidateStaticTargetsAgainst() error=%v, want an unknown collector error", err)
	}
}

// A reload that turns OTLP export off is refused while a loaded target sets
// export_via_otlp.
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
	manager.SetTargets("", &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://a.invalid", ExportViaOTLP: true}}})

	disabled := strings.Replace(enabled, "enabled: true", "enabled: false", 1)
	if err := os.WriteFile(configPath, []byte(disabled), 0600); err != nil {
		t.Fatal(err)
	}
	manager.lastMod = time.Time{}
	manager.reloadChanged()
	if !manager.Get().OTLP.Enabled {
		t.Fatal("a reload that disables OTLP while targets are loaded must be rejected")
	}
}

func TestStaticTargetFileReloadRejectsInvalidDocument(t *testing.T) {
	dir := t.TempDir()
	targetsPath := dir + "/targets.yaml"
	valid := "interval: 1m\ntargets:\n  - name: one\n    collector: text\n    target: http://a.invalid\n"
	if err := os.WriteFile(targetsPath, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(targetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, dir+"/config.yaml", slog.Default())
	manager.SetTargets(targetsPath, file)

	if err := os.WriteFile(targetsPath, []byte("interval: 1m\ntargets:\n  - name: two\n    collector: missing\n    target: http://a.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.targetsLastMod = time.Time{}
	manager.reloadChanged()
	targets := manager.StaticTargets()
	if len(targets) != 1 || targets[0].Name != "one" {
		t.Fatalf("an invalid target reload must keep the previous document, got %+v", targets)
	}

	if err := os.WriteFile(targetsPath, []byte("interval: 1m\ntargets:\n  - name: three\n    collector: text\n    target: http://b.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.targetsLastMod = time.Time{}
	manager.reloadChanged()
	if targets := manager.StaticTargets(); len(targets) != 1 || targets[0].Name != "three" {
		t.Fatalf("a valid target reload should take effect, got %+v", targets)
	}
}

// A target missing a parameter is told how to supply it; setting request.path
// is offered only when the placeholder is in the path, the one a target can
// replace.
func TestAMissingTargetParameterSaysHowToSupplyIt(t *testing.T) {
	collector := func(request model.RequestConfig) *model.Config {
		c := testutil.Collector("templated", "text")
		request.Type = "http"
		c.Request = request
		cfg := &model.Config{Collectors: []model.Collector{c}, OTLP: model.OTLPConfig{Enabled: true, Endpoint: "http://otel.invalid/v1/metrics"}}
		if err := Validate(cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	targets := func() *model.StaticTargetFile {
		f := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "acme", Collector: "templated", Target: "http://orders.invalid"}}}
		if err := ValidateStaticTargets(f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for name, test := range map[string]struct {
		request     model.RequestConfig
		where       string
		offersPaths bool
	}{
		"path":   {model.RequestConfig{Path: "/tenants/{{param_tenant}}"}, "request.path", true},
		"query":  {model.RequestConfig{Query: map[string]string{"tenant": "{{param_tenant}}"}}, "request.query.tenant", false},
		"header": {model.RequestConfig{Headers: map[string]string{"X-Tenant": "{{param_tenant}}"}}, "request.headers.X-Tenant", false},
	} {
		err := ValidateStaticTargetsAgainst(targets(), collector(test.request))
		if err == nil || !strings.Contains(err.Error(), "whose "+test.where+" needs param_tenant") || !strings.Contains(err.Error(), "set it under the target's params") {
			t.Errorf("%s: err=%v", name, err)
			continue
		}
		if got := strings.Contains(err.Error(), "set request.path on the target"); got != test.offersPaths {
			t.Errorf("%s: offers request.path %v, want %v: %v", name, got, test.offersPaths, err)
		}
	}
}

// A scrape ends with its interval, so retries whose waits alone fill it are
// refused, whether the collector or the target sets them; retries that fit are
// accepted.
func TestStaticTargetRetriesMustFitTheInterval(t *testing.T) {
	collector := testutil.Collector("text", "text")
	collector.Request.Retry = model.RetryConfig{Attempts: 3, Backoff: model.Duration(20 * time.Second)}
	cfg := &model.Config{Collectors: []model.Collector{collector}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	check := func(interval time.Duration, retry *model.TargetRetryConfig) error {
		file := &model.StaticTargetFile{Interval: model.Duration(interval), Targets: []model.StaticTarget{{Name: "one", Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{Retry: retry}}}}
		if err := ValidateStaticTargets(file); err != nil {
			t.Fatal(err)
		}
		return ValidateStaticTargetsAgainst(file, cfg)
	}
	if err := check(time.Minute, nil); err == nil || !strings.Contains(err.Error(), `target "one" retries 3 times, 20s apart, which is 1m0s of waiting alone`) {
		t.Errorf("the collector's retries filling the interval: %v", err)
	}
	if err := check(2*time.Minute, nil); err != nil {
		t.Errorf("the collector's retries within the interval: %v", err)
	}
	if err := check(time.Minute, &model.TargetRetryConfig{Attempts: ptrTo(2), Backoff: ptrTo(model.Duration(10 * time.Second))}); err != nil {
		t.Errorf("the target's own retries within the interval: %v", err)
	}
	if err := check(time.Minute, &model.TargetRetryConfig{Attempts: ptrTo(6), Backoff: ptrTo(model.Duration(10 * time.Second))}); err == nil || !strings.Contains(err.Error(), "retries 6 times, 10s apart") {
		t.Errorf("the target's own retries filling the interval: %v", err)
	}
	if err := check(time.Minute, &model.TargetRetryConfig{Attempts: ptrTo(0)}); err != nil {
		t.Errorf("a target turning retries off: %v", err)
	}
}
