package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A configuration whose rules would fail every scrape's validation, whatever
// the target answers, is refused when it loads, naming the collector and the
// metric (checkMetricFamilies).

func familyCollector(transformType string, rules ...model.MetricRule) model.Collector {
	return model.Collector{Name: "families", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: transformType}, Metrics: rules}
}

func TestRulesGivingOneNameTwoTypesAreRefused(t *testing.T) {
	err := validateOne(familyCollector("jq",
		model.MetricRule{Name: "jobs", Type: model.GaugeMetricType, Expression: ".a"},
		model.MetricRule{Name: "jobs", Type: model.CounterMetricType, Expression: ".b"}))
	if err == nil || !strings.Contains(err.Error(), `collector "families" metric "jobs" is declared as both gauge and counter`) {
		t.Fatalf("err=%v", err)
	}
	// A rule left without a type is a gauge, so it clashes with a counter too.
	err = validateOne(familyCollector("regex",
		model.MetricRule{Name: "jobs", Expression: `a=(\d+)`},
		model.MetricRule{Name: "jobs", Type: model.CounterMetricType, Expression: `b=(\d+)`}))
	if err == nil || !strings.Contains(err.Error(), `metric "jobs" is declared as both gauge and counter`) {
		t.Fatalf("err=%v", err)
	}
}

// Several rules feeding one family with the same type are how one metric is
// read from several places, and stay allowed; so do prometheus rules that keep
// the type of the series they pass through.
func TestRulesSharingANameAndTypeAreKept(t *testing.T) {
	if err := validateOne(familyCollector("jq",
		model.MetricRule{Name: "jobs", Type: model.GaugeMetricType, Expression: ".a", Labels: []model.LabelRule{{Name: "queue", Value: "a"}}},
		model.MetricRule{Name: "jobs", Type: model.GaugeMetricType, Expression: ".b", Labels: []model.LabelRule{{Name: "queue", Value: "b"}}})); err != nil {
		t.Fatal(err)
	}
	if err := validateOne(familyCollector("prometheus",
		model.MetricRule{Name: "jobs", Expression: "^a$"},
		model.MetricRule{Name: "jobs", Type: model.CounterMetricType, Expression: "^b$"})); err != nil {
		t.Fatal(err)
	}
}

func TestRuleNamedAsAnotherRulesHistogramSeriesIsRefused(t *testing.T) {
	for _, tc := range []struct {
		kind  model.MetricType
		clash string
	}{
		{model.HistogramMetricType, "latency_bucket"},
		{model.HistogramMetricType, "latency_sum"},
		{model.SummaryMetricType, "latency_count"},
	} {
		err := validateOne(familyCollector("prometheus",
			model.MetricRule{Name: "latency", Type: tc.kind, Expression: "^latency$"},
			model.MetricRule{Name: tc.clash, Type: model.GaugeMetricType, Expression: "^other$"}))
		want := `collector "families" metric "` + tc.clash + `" (gauge) clashes with the ` + string(tc.kind) + ` "latency"`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s %s: err=%v, want %q", tc.kind, tc.clash, err, want)
		}
	}
	// A summary is written without _bucket series, so a gauge of that name
	// is fine beside one.
	if err := validateOne(familyCollector("prometheus",
		model.MetricRule{Name: "latency", Type: model.SummaryMetricType, Expression: "^latency$"},
		model.MetricRule{Name: "latency_bucket", Type: model.GaugeMetricType, Expression: "^other$"})); err != nil {
		t.Fatal(err)
	}
}

func TestDescriptionLongerThanTheHelpLimitIsRefused(t *testing.T) {
	c := familyCollector("jq", model.MetricRule{Name: "jobs", Expression: ".a", Description: strings.Repeat("x", 11)})
	c.Limits.MaxHelpLength = 10
	err := validateOne(c)
	if err == nil || !strings.Contains(err.Error(), `collector "families" metric "jobs" description is 11 bytes, longer than limits.max_help_length 10`) {
		t.Fatalf("err=%v", err)
	}
	c.Metrics[0].Description = strings.Repeat("x", 10)
	if err := validateOne(c); err != nil {
		t.Fatal(err)
	}
}

func TestStaticLabelLongerThanTheValueLimitIsRefused(t *testing.T) {
	long := strings.Repeat("x", 11)
	c := familyCollector("jq", model.MetricRule{Name: "jobs", Expression: ".a", Labels: []model.LabelRule{{Name: "site", Value: long}}})
	c.Limits.MaxLabelValueLength = 10
	err := validateOne(c)
	if err == nil || !strings.Contains(err.Error(), `collector "families" metric "jobs" label "site" value is 11 bytes, longer than limits.max_label_value_length 10`) {
		t.Fatalf("err=%v", err)
	}

	// Cut by truncate: true, mapped to something shorter, or removed by
	// remove_labels, it never reaches validation as it was written.
	allowed := map[string]func(c *model.Collector){
		"truncated": func(c *model.Collector) { c.Metrics[0].Labels[0].Truncate = true },
		"mapped": func(c *model.Collector) {
			c.Metrics = append(c.Metrics, model.MetricRule{Name: "jobs", Expression: ".b", Labels: []model.LabelRule{{Name: "site", Expression: ".site", ValueMap: map[string]string{long: "short"}}}})
		},
		"removed": func(c *model.Collector) { c.Transform.RemoveLabels = []string{"site"} },
	}
	for name, change := range allowed {
		c := familyCollector("jq", model.MetricRule{Name: "jobs", Expression: ".a", Labels: []model.LabelRule{{Name: "site", Value: long}}})
		c.Limits.MaxLabelValueLength = 10
		change(&c)
		if err := validateOne(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A collector-wide label applies to every series, of any transform.
	py := familyCollector("python")
	py.Transform.Script = "metric('up', 1)"
	py.Transform.Labels = map[string]string{"site": long}
	py.Limits.MaxLabelValueLength = 10
	err = validateOne(py)
	if err == nil || !strings.Contains(err.Error(), `collector "families" transform.labels "site" is 11 bytes, longer than limits.max_label_value_length 10`) {
		t.Fatalf("err=%v", err)
	}
}
