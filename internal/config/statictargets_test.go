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

func TestStaticTargetFileValidationRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		file *model.StaticTargetFile
		want string
	}{
		{name: "empty", file: &model.StaticTargetFile{}, want: "must not be empty"},
		{name: "no collector", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Target: "http://a.invalid"}}}, want: "has no collector"},
		{name: "invalid name", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "bad-name", Collector: "text", Target: "http://a.invalid"}}}, want: "invalid name"},
		{name: "duplicate name", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
			{Name: "one", Collector: "text", Target: "http://a.invalid"},
			{Name: "one", Collector: "text", Target: "http://b.invalid"},
		}}, want: "duplicate target"},
		{name: "bad method", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{Method: "TRACE"}}}}, want: "unsupported method"},
		{name: "negative timeout", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{Timeout: model.Duration(-time.Second)}}}}, want: "timeout must not be negative"},
		{name: "negative retries", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{Retry: &model.RetryConfig{Attempts: -1}}}}}, want: "retry.attempts"},
		{name: "two bearer sources", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{BearerToken: "t", BearerTokenFile: "/f"}}}}, want: "bearer_token_file"},
		{name: "basic and bearer", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Collector: "text", Target: "http://a.invalid", Request: model.TargetRequestConfig{BasicAuth: &model.BasicAuth{Username: "u", Password: "p"}, BearerToken: "t"}}}}, want: "basic and bearer"},
		{name: "invalid label", file: &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Collector: "text", Target: "http://a.invalid", Labels: map[string]string{"not a label": "x"}}}}, want: "invalid label name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateStaticTargets(test.file)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadStaticTargetFileRejectsUnknownFields(t *testing.T) {
	path := t.TempDir() + "/targets.yaml"
	if err := os.WriteFile(path, []byte("interval: 1m\ntargets:\n  - collector: text\n    surprise: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStaticTargets(path); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("LoadStaticTargets() error=%v, want an unknown-field error", err)
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

// Each static target has an interval: its own, else the file's, which is
// required. It is at least a second, and a request timeout may not outlast it.
func TestStaticTargetIntervals(t *testing.T) {
	target := func(name string, interval, timeout time.Duration) model.StaticTarget {
		return model.StaticTarget{Name: name, Collector: "text", Target: "http://t.invalid", Interval: model.Duration(interval), Request: model.TargetRequestConfig{Timeout: model.Duration(timeout)}}
	}
	minute := model.Duration(time.Minute)
	file := &model.StaticTargetFile{Targets: []model.StaticTarget{target("own", 15*time.Second, 10*time.Second)}}
	if err := ValidateStaticTargets(file); err == nil || !strings.Contains(err.Error(), "interval is required") {
		t.Fatalf("a file without an interval: got %v, want it refused as required", err)
	}
	file = &model.StaticTargetFile{Interval: model.Duration(30 * time.Second), Targets: []model.StaticTarget{target("from_file", 0, 0), target("own", 5*time.Second, 0)}}
	if err := ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	if file.Targets[0].Interval != model.Duration(30*time.Second) || file.Targets[1].Interval != model.Duration(5*time.Second) {
		t.Fatalf("intervals %s, %s", time.Duration(file.Targets[0].Interval), time.Duration(file.Targets[1].Interval))
	}
	for name, test := range map[string]struct {
		file *model.StaticTargetFile
		want string
	}{
		"too short":        {&model.StaticTargetFile{Interval: minute, Targets: []model.StaticTarget{target("quick", 500*time.Millisecond, 0)}}, `target "quick" interval 500ms is under the least, 1s`},
		"negative":         {&model.StaticTargetFile{Interval: minute, Targets: []model.StaticTarget{target("back", -time.Second, 0)}}, `target "back" interval must not be negative`},
		"negative file":    {&model.StaticTargetFile{Interval: model.Duration(-time.Second), Targets: []model.StaticTarget{target("t", 0, 0)}}, "interval -1s is under the least, 1s"},
		"file too short":   {&model.StaticTargetFile{Interval: model.Duration(time.Millisecond), Targets: []model.StaticTarget{target("t", 0, 0)}}, "interval 1ms is under the least, 1s"},
		"timeout too long": {&model.StaticTargetFile{Interval: minute, Targets: []model.StaticTarget{target("slow", 10*time.Second, 20*time.Second)}}, `target "slow" request.timeout 20s is longer than its interval 10s`},
	} {
		if err := ValidateStaticTargets(test.file); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err=%v, want %q", name, err, test.want)
		}
	}
	// The file's interval and a target's are read from the document.
	path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", "interval: 2m\ntargets:\n  - name: a\n    collector: text\n    target: http://a.invalid\n  - name: b\n    collector: text\n    target: http://b.invalid\n    interval: 15s\n")
	loaded, err := LoadStaticTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargets(loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Targets[0].Interval != model.Duration(2*time.Minute) || loaded.Targets[1].Interval != model.Duration(15*time.Second) {
		t.Fatalf("intervals %s, %s", time.Duration(loaded.Targets[0].Interval), time.Duration(loaded.Targets[1].Interval))
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

func TestStaticTargetConcurrencyIsChecked(t *testing.T) {
	file := func(concurrency int) *model.StaticTargetFile {
		return &model.StaticTargetFile{Interval: model.Duration(time.Minute), Concurrency: concurrency, Targets: []model.StaticTarget{{Name: "t", Collector: "text", Target: "http://t.invalid"}}}
	}
	for _, ok := range []int{0, 1, 50} {
		if err := ValidateStaticTargets(file(ok)); err != nil {
			t.Errorf("concurrency %d: %v", ok, err)
		}
	}
	if err := ValidateStaticTargets(file(-1)); err == nil || !strings.Contains(err.Error(), "concurrency must not be negative") {
		t.Fatalf("concurrency -1: got %v", err)
	}
}

// job and instance are Prometheus's, set when it scrapes the endpoint; kept
// as the series' own labels, a target's would move its series out of the job
// that scrapes the endpoint, so they are refused as target labels.
func TestStaticTargetLabelsRefuseJobAndInstance(t *testing.T) {
	for _, name := range []string{"job", "instance"} {
		file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "text", Target: "http://t.invalid", Labels: map[string]string{name: "x"}}}}
		if err := ValidateStaticTargets(file); err == nil || !strings.Contains(err.Error(), "labels sets "+name) || !strings.Contains(err.Error(), "such as task") {
			t.Errorf("label %s: got %v", name, err)
		}
	}
}

// A static target has no probe, so a {{param_...}} placeholder in a value it
// writes itself — its target, body or a header — could never be filled and
// would reach the target as text. Each is refused, naming where; ordinary
// braces in a body are not placeholders.
func TestStaticTargetsRefuseParamPlaceholdersInTheirOwnValues(t *testing.T) {
	target := func(edit func(*model.StaticTarget)) *model.StaticTargetFile {
		st := model.StaticTarget{Name: "one", Collector: "text", Target: "http://a.invalid"}
		edit(&st)
		return &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{st}}
	}
	for name, tc := range map[string]struct {
		edit func(*model.StaticTarget)
		want string
	}{
		"target":           {func(st *model.StaticTarget) { st.Target = "http://{{param_host}}.example" }, `target "one" target cannot use {{param_...}}`},
		"body":             {func(st *model.StaticTarget) { st.Request.Body, st.Request.BodySet = `{"q": {{param_q|json}}}`, true }, `target "one" request.body cannot use {{param_...}}`},
		"body with spaces": {func(st *model.StaticTarget) { st.Request.Body = `{{ param_q }}` }, `request.body cannot use`},
		"header": {func(st *model.StaticTarget) {
			st.Request.Headers = map[string]string{"X-Ok": "fine", "X-Tenant": "{{param_tenant}}"}
		}, `target "one" request.headers X-Tenant cannot use {{param_...}}`},
		"path, as before": {func(st *model.StaticTarget) { st.Request.Path, st.Request.PathSet = "/{{param_v}}", true }, `request.path cannot use {{param_...}}`},
		"a default, too": {func(st *model.StaticTarget) {
			st.Request.Headers = map[string]string{"X-Tenant": "{{param_tenant:acme}}"}
		}, `request.headers X-Tenant cannot use`},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateStaticTargets(target(tc.edit))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "params") && name != "path, as before" {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	for name, edit := range map[string]func(*model.StaticTarget){
		"a JSON body":             func(st *model.StaticTarget) { st.Request.Body = `{"a": {"b": [1, {"c": 2}]}}` },
		"braces that are not one": func(st *model.StaticTarget) { st.Request.Headers = map[string]string{"X-Template": "{{name}}"} },
		"params":                  func(st *model.StaticTarget) { st.Params = map[string]string{"param_tenant": "acme"} },
	} {
		if err := ValidateStaticTargets(target(edit)); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

// The file is checked after it is expanded, so a placeholder an environment
// variable supplies is refused as one written in the file is.
func TestAParamPlaceholderFromTheEnvironmentIsRefused(t *testing.T) {
	t.Setenv("DEMO_TENANT_HEADER", "{{param_tenant}}")
	path := testutil.WriteIn(t, t.TempDir(), "targets.yaml", "interval: 1m\ntargets:\n  - name: one\n    collector: text\n    target: http://a.invalid\n    request:\n      headers:\n        X-Tenant: \"${DEMO_TENANT_HEADER}\"\n")
	file, err := LoadStaticTargets(path, WithStaticTargetsEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargets(file); err == nil || !strings.Contains(err.Error(), "request.headers X-Tenant cannot use {{param_...}}") {
		t.Fatalf("err=%v", err)
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
	check := func(interval time.Duration, retry *model.RetryConfig) error {
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
	if err := check(time.Minute, &model.RetryConfig{Attempts: 2, Backoff: model.Duration(10 * time.Second)}); err != nil {
		t.Errorf("the target's own retries within the interval: %v", err)
	}
	if err := check(time.Minute, &model.RetryConfig{Attempts: 6, Backoff: model.Duration(10 * time.Second)}); err == nil || !strings.Contains(err.Error(), "retries 6 times, 10s apart") {
		t.Errorf("the target's own retries filling the interval: %v", err)
	}
	if err := check(time.Minute, &model.RetryConfig{Attempts: 0}); err != nil {
		t.Errorf("a target turning retries off: %v", err)
	}
}
