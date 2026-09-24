package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Everything about a metric rule that can be known before a scrape is checked
// when the configuration loads: its name, and that every expression compiles.

func ruleCollector(transformType string, rule model.MetricRule) model.Collector {
	return model.Collector{Name: "checked", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: transformType}, Metrics: []model.MetricRule{rule}}
}

func validateOne(c model.Collector) error {
	return Validate(&model.Config{Collectors: []model.Collector{c}})
}

func TestMetricNamesAreCheckedAtLoad(t *testing.T) {
	for _, name := range []string{"my-metric", "1st_metric", "metric name", "métrique", "__reserved"} {
		err := validateOne(ruleCollector("jq", model.MetricRule{Name: name, Type: model.GaugeMetricType, Expression: ".v"}))
		if err == nil || !strings.Contains(err.Error(), `collector "checked" metric `+quote(name)) {
			t.Errorf("%q: err=%v", name, err)
		}
	}
	for _, name := range []string{"requests_total", "_private", "job:requests:rate5m", "A1"} {
		if err := validateOne(ruleCollector("jq", model.MetricRule{Name: name, Type: model.GaugeMetricType, Expression: ".v"})); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// Each transform's expressions are compiled in its own language, and an error
// names the collector, the metric and, for a label, the label.
func TestExpressionsAreCompiledAtLoad(t *testing.T) {
	label := func(expression string) []model.LabelRule {
		return []model.LabelRule{{Name: "l", Type: "expression", Expression: expression}}
	}
	tests := []struct {
		name      string
		transform string
		rule      model.MetricRule
		want      string
	}{
		{"jq syntax", "jq", model.MetricRule{Name: "m", Expression: ".value |"}, `metric "m" expression`},
		{"jq undefined function", "jq", model.MetricRule{Name: "m", Expression: "nosuchfunction(.x)"}, `metric "m" expression`},
		{"jq undefined variable", "jq", model.MetricRule{Name: "m", Expression: "$nosuch"}, `metric "m" expression`},
		{"jq label", "jq", model.MetricRule{Name: "m", Expression: ".v", Labels: label(".a | [")}, `metric "m" label "l" expression`},
		{"jq items", "jq", model.MetricRule{Name: "m", Items: ".rows[", Expression: ".v"}, `metric "m" items`},
		{"regex", "regex", model.MetricRule{Name: "m", Expression: `value=(\d+`}, `metric "m" regex`},
		{"regex label capture", "regex", model.MetricRule{Name: "m", Expression: `(?P<value>\d+)`, Labels: label("server")}, `label "l" refers to capture group "server", which the regex does not have`},
		{"regex label index", "regex", model.MetricRule{Name: "m", Expression: `(\d+)`, Labels: label("2")}, `refers to capture group "2"`},
		{"css", "css", model.MetricRule{Name: "m", Expression: "td:nth-child("}, `metric "m" CSS selector "td:nth-child("`},
		{"css items", "css", model.MetricRule{Name: "m", Items: "tr:has(", Expression: "td"}, `metric "m" items CSS selector "tr:has("`},
		{"css label", "css", model.MetricRule{Name: "m", Expression: "td", Labels: label("[[")}, `label "l" CSS selector "[["`},
		{"xpath", "xpath", model.MetricRule{Name: "m", Expression: "//item["}, `metric "m" XPath "//item["`},
		{"xpath label", "xpath", model.MetricRule{Name: "m", Expression: "//item", Labels: label("name[")}, `label "l" XPath "name["`},
		{"prometheus pattern", "prometheus", model.MetricRule{Name: "m", Expression: "^vendor_(.*"}, `metric "m" expression`},
		{"items on regex", "regex", model.MetricRule{Name: "m", Items: ".rows[]", Expression: `(\d+)`}, "sets items, which only the jq, yq and css transforms support"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.rule.Type = model.GaugeMetricType
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
	for name, c := range map[string]model.Collector{
		"regex named":     ruleCollector("regex", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: `(?P<server>\w+)=(\d+)`, Labels: []model.LabelRule{{Name: "s", Type: "expression", Expression: "server"}}}),
		"regex numbered":  ruleCollector("regex", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: `(\w+)=(\d+)`, Labels: []model.LabelRule{{Name: "s", Type: "expression", Expression: "1"}}}),
		"xpath attribute": ruleCollector("xpath", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//item", Labels: []model.LabelRule{{Name: "s", Type: "expression", Expression: "@name"}}}),
		"css":             ruleCollector("css", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "table#servers td.cpu", Labels: []model.LabelRule{{Name: "s", Type: "expression", Expression: "td:first-child"}}}),
		"jq with $root":   ruleCollector("jq", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Items: ".rows[]", Expression: ".v", Labels: []model.LabelRule{{Name: "s", Type: "expression", Expression: "$root.site"}}}),
	} {
		if err := validateOne(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	ns := ruleCollector("xpath", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//a:item"})
	ns.Response.Namespaces = map[string]string{"a": "urn:example"}
	if err := validateOne(ns); err != nil {
		t.Errorf("namespaced XPath: %v", err)
	}
}

// A prometheus passthrough's filters and renames are checked too.
func TestPrometheusTransformSettingsAreChecked(t *testing.T) {
	base := func(edit func(*model.TransformConfig)) model.Collector {
		c := model.Collector{Name: "checked", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "prometheus"}}
		edit(&c.Transform)
		return c
	}
	for want, c := range map[string]model.Collector{
		`transform.include "(unclosed"`:                     base(func(t *model.TransformConfig) { t.Include = []string{"(unclosed"} }),
		`transform.exclude "[z-a]"`:                         base(func(t *model.TransformConfig) { t.Exclude = []string{"[z-a]"} }),
		`transform.rename "a" to "bad-name"`:                base(func(t *model.TransformConfig) { t.Rename = map[string]string{"a": "bad-name"} }),
		`transform.labels has invalid label name "a-b"`:     base(func(t *model.TransformConfig) { t.Labels = map[string]string{"a-b": "x"} }),
		`transform.rename_labels "a" to invalid label name`: base(func(t *model.TransformConfig) { t.RenameLabels = map[string]string{"a": "1b"} }),
	} {
		if err := validateOne(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}

// error_handling and error_mode share one vocabulary, fail, log and ignore.
// warn, the older spelling of log in error_handling, still works, is reported
// as deprecated, and means log.
func TestErrorPolicyVocabulary(t *testing.T) {
	c := testutil.Collector("policies", "text")
	c.ErrorHandling = model.ErrorHandling{OnFetchError: "warn", OnDecodeError: "LOG", OnTransformError: "ignore"}
	c.Metrics[0].ErrorMode = "warn"
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err != nil {
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
		c := testutil.Collector("policies", "text")
		c.ErrorHandling.OnDecodeError = bad
		err := Validate(&model.Config{Collectors: []model.Collector{c}})
		if err == nil || !strings.Contains(err.Error(), `collector "policies" error_handling.on_decode_error has invalid value "`+bad+`"; want fail, log or ignore`) {
			t.Errorf("%q: err=%v", bad, err)
		}
	}
}
