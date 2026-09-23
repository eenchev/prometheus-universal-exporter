package main

import (
	"strings"
	"testing"
)

// Everything about a metric rule that can be known before a scrape is checked
// when the configuration loads: its name, and that every expression compiles.

func ruleCollector(transform string, rule MetricRule) Collector {
	return Collector{Name: "checked", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: TransformConfig{Type: transform}, Metrics: []MetricRule{rule}}
}

func validateOne(c Collector) error {
	return (&Config{Collectors: []Collector{c}}).Validate()
}

func TestMetricNamesAreCheckedAtLoad(t *testing.T) {
	for _, name := range []string{"my-metric", "1st_metric", "metric name", "métrique", "__reserved"} {
		err := validateOne(ruleCollector("jq", MetricRule{Name: name, Type: GaugeMetricType, Expression: ".v"}))
		if err == nil || !strings.Contains(err.Error(), `collector "checked" metric `+quote(name)) {
			t.Errorf("%q: err=%v", name, err)
		}
	}
	for _, name := range []string{"requests_total", "_private", "job:requests:rate5m", "A1"} {
		if err := validateOne(ruleCollector("jq", MetricRule{Name: name, Type: GaugeMetricType, Expression: ".v"})); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// Each transform's expressions are compiled in its own language, and an error
// names the collector, the metric and, for a label, the label.
func TestExpressionsAreCompiledAtLoad(t *testing.T) {
	label := func(expression string) []LabelRule {
		return []LabelRule{{Name: "l", Type: "expression", Expression: expression}}
	}
	tests := []struct {
		name      string
		transform string
		rule      MetricRule
		want      string
	}{
		{"jq syntax", "jq", MetricRule{Name: "m", Expression: ".value |"}, `metric "m" expression`},
		{"jq undefined function", "jq", MetricRule{Name: "m", Expression: "nosuchfunction(.x)"}, `metric "m" expression`},
		{"jq undefined variable", "jq", MetricRule{Name: "m", Expression: "$nosuch"}, `metric "m" expression`},
		{"jq label", "jq", MetricRule{Name: "m", Expression: ".v", Labels: label(".a | [")}, `metric "m" label "l" expression`},
		{"jq items", "jq", MetricRule{Name: "m", Items: ".rows[", Expression: ".v"}, `metric "m" items`},
		{"regex", "regex", MetricRule{Name: "m", Expression: `value=(\d+`}, `metric "m" regex`},
		{"regex label capture", "regex", MetricRule{Name: "m", Expression: `(?P<value>\d+)`, Labels: label("server")}, `label "l" refers to capture group "server", which the regex does not have`},
		{"regex label index", "regex", MetricRule{Name: "m", Expression: `(\d+)`, Labels: label("2")}, `refers to capture group "2"`},
		{"css", "css", MetricRule{Name: "m", Expression: "td:nth-child("}, `metric "m" CSS selector "td:nth-child("`},
		{"css label", "css", MetricRule{Name: "m", Expression: "td", Labels: label("[[")}, `label "l" CSS selector "[["`},
		{"xpath", "xpath", MetricRule{Name: "m", Expression: "//item["}, `metric "m" XPath "//item["`},
		{"xpath label", "xpath", MetricRule{Name: "m", Expression: "//item", Labels: label("name[")}, `label "l" XPath "name["`},
		{"prometheus pattern", "prometheus", MetricRule{Name: "m", Expression: "^vendor_(.*"}, `metric "m" expression`},
		{"items on regex", "regex", MetricRule{Name: "m", Items: ".rows[]", Expression: `(\d+)`}, "sets items, which only the jq and yq transforms support"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.rule.Type = GaugeMetricType
			err := validateOne(ruleCollector(test.transform, test.rule))
			if err == nil || !strings.Contains(err.Error(), `collector "checked"`) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want it to contain %q", err, test.want)
			}
		})
	}
}

// What does compile passes: named and numbered capture groups, XPath attribute
// labels, and XPath with namespaces.
func TestValidExpressionsPass(t *testing.T) {
	for name, c := range map[string]Collector{
		"regex named":     ruleCollector("regex", MetricRule{Name: "m", Type: GaugeMetricType, Expression: `(?P<server>\w+)=(\d+)`, Labels: []LabelRule{{Name: "s", Type: "expression", Expression: "server"}}}),
		"regex numbered":  ruleCollector("regex", MetricRule{Name: "m", Type: GaugeMetricType, Expression: `(\w+)=(\d+)`, Labels: []LabelRule{{Name: "s", Type: "expression", Expression: "1"}}}),
		"xpath attribute": ruleCollector("xpath", MetricRule{Name: "m", Type: GaugeMetricType, Expression: "//item", Labels: []LabelRule{{Name: "s", Type: "expression", Expression: "@name"}}}),
		"css":             ruleCollector("css", MetricRule{Name: "m", Type: GaugeMetricType, Expression: "table#servers td.cpu", Labels: []LabelRule{{Name: "s", Type: "expression", Expression: "td:first-child"}}}),
		"jq with $root":   ruleCollector("jq", MetricRule{Name: "m", Type: GaugeMetricType, Items: ".rows[]", Expression: ".v", Labels: []LabelRule{{Name: "s", Type: "expression", Expression: "$root.site"}}}),
	} {
		if err := validateOne(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	ns := ruleCollector("xpath", MetricRule{Name: "m", Type: GaugeMetricType, Expression: "//a:item"})
	ns.Response.Namespaces = map[string]string{"a": "urn:example"}
	if err := validateOne(ns); err != nil {
		t.Errorf("namespaced XPath: %v", err)
	}
}

// A prometheus passthrough's filters and renames are checked too.
func TestPrometheusTransformSettingsAreChecked(t *testing.T) {
	base := func(edit func(*TransformConfig)) Collector {
		c := Collector{Name: "checked", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: TransformConfig{Type: "prometheus"}}
		edit(&c.Transform)
		return c
	}
	for want, c := range map[string]Collector{
		`transform.include "(unclosed"`:                     base(func(t *TransformConfig) { t.Include = []string{"(unclosed"} }),
		`transform.exclude "[z-a]"`:                         base(func(t *TransformConfig) { t.Exclude = []string{"[z-a]"} }),
		`transform.rename "a" to "bad-name"`:                base(func(t *TransformConfig) { t.Rename = map[string]string{"a": "bad-name"} }),
		`transform.labels has invalid label name "a-b"`:     base(func(t *TransformConfig) { t.Labels = map[string]string{"a-b": "x"} }),
		`transform.rename_labels "a" to invalid label name`: base(func(t *TransformConfig) { t.RenameLabels = map[string]string{"a": "1b"} }),
	} {
		if err := validateOne(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}

// --dry-run reports an expression that does not compile, because it loads the
// configuration the way startup does.
func TestDryRunReportsAnExpressionThatDoesNotCompile(t *testing.T) {
	path := writeFile(t, "config.yaml", `collectors:
  - name: html
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: value
        description: A value
        type: gauge
        expression: 'td:nth-child('
`)
	out := runCheckCLI(t, "--config.file="+path)
	result := out.result(t, "config")
	if out.code != 1 || result.Status != checkFailed || !strings.Contains(strings.Join(result.Errors, " "), `CSS selector "td:nth-child("`) {
		t.Fatalf("exit=%d config=%+v", out.code, result)
	}
}

// error_handling and error_mode share one vocabulary, fail, log and ignore.
// warn, the older spelling of log in error_handling, still works, is reported
// as deprecated, and means log.
func TestErrorPolicyVocabulary(t *testing.T) {
	c := testCollector("policies", "text")
	c.ErrorHandling = ErrorHandling{OnFetchError: "warn", OnDecodeError: "LOG", OnTransformError: "ignore"}
	c.Metrics[0].ErrorMode = "warn"
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	got := cfg.Collectors[0]
	if got.ErrorHandling.OnFetchError != "log" || got.ErrorHandling.OnDecodeError != "log" || got.ErrorHandling.OnTransformError != "ignore" || got.Metrics[0].ErrorMode != "log" {
		t.Fatalf("normalised to %+v, error_mode %q", got.ErrorHandling, got.Metrics[0].ErrorMode)
	}
	if len(cfg.Deprecations) != 2 ||
		!strings.Contains(cfg.Deprecations[0]+cfg.Deprecations[1], `collector "policies" error_handling.on_fetch_error: "warn" is deprecated; use "log"`) ||
		!strings.Contains(cfg.Deprecations[0]+cfg.Deprecations[1], `metric "demo_value" error_mode: "warn" is deprecated`) {
		t.Fatalf("deprecations=%q", cfg.Deprecations)
	}
	for _, bad := range []string{"panic", "warning", "skip"} {
		c := testCollector("policies", "text")
		c.ErrorHandling.OnDecodeError = bad
		err := (&Config{Collectors: []Collector{c}}).Validate()
		if err == nil || !strings.Contains(err.Error(), `collector "policies" error_handling.on_decode_error has invalid value "`+bad+`"; want fail, log or ignore`) {
			t.Errorf("%q: err=%v", bad, err)
		}
	}
}

// The dry-run report and the log carry the deprecations, and the check still
// passes: a deprecated spelling works until it is removed.
func TestDryRunReportsDeprecations(t *testing.T) {
	path := writeFile(t, "config.yaml", `collectors:
  - name: legacy
    request:
      type: http
    error_handling:
      on_fetch_error: warn
    transform:
      type: regex
    metrics:
      - name: value
        description: A value
        type: gauge
        expression: 'value=(\d+)'
`)
	out := runCheckCLI(t, "--config.file="+path)
	result := out.result(t, "config")
	deprecations, _ := result.Details["deprecations"].([]any)
	if out.code != 0 || result.Status != checkOK || len(deprecations) != 1 || !strings.Contains(deprecations[0].(string), `"warn" is deprecated`) {
		t.Fatalf("exit=%d config=%+v", out.code, result)
	}
	logged := false
	for _, record := range out.logs {
		if record["msg"] == "deprecated configuration" && strings.Contains(record["deprecation"].(string), "on_fetch_error") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no deprecation in the log:\n%s", out.stderr)
	}
}
