package transform

import (
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A prometheus rule passes on the metrics whose names its expression
// matches, or, without an expression, the metric its name names. A rule with
// neither is matched by ^$, which is no metric's name: whatever the response
// holds it makes no series, and what it sets beside — a type, a description,
// a scale, labels — applies to nothing. Being required unless it says
// otherwise, it is missing its value on every scrape, `metric "" is not in
// the response`, which reads as the target's fault and is the
// configuration's; with required: false nothing is said at all. The rule
// check, which is handed a rule and not its place among the collector's,
// takes such a rule as it did: the loader refuses it, naming the place
// (config.checkPrometheusRuleSelects). A rule with either key selects by it,
// and one of the pattern the message gives for every metric passes on every
// one.
func TestAPrometheusRuleOfNeitherANameNorAnExpressionMatchesNoMetric(t *testing.T) {
	no, yes, two := false, true, 2.0
	rules := func(written ...model.MetricRule) *model.Collector {
		for i := range written {
			written[i].ErrorMode = model.ErrorModeLog
		}
		return &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "prometheus"}, Metrics: written}
	}
	names := []string{"up", "node_load1", "disk free", "é", "_", ":"}
	site := []model.LabelRule{{Name: "site", Value: "rack1"}}
	for what, rule := range map[string]model.MetricRule{
		"nothing":            {},
		"required: true":     {Required: &yes},
		"a type":             {Type: model.CounterMetricType},
		"a description":      {Description: "Whether it is up."},
		"a scale":            {Scale: &two},
		"a label":            {Labels: site},
		"all of them":        {Type: model.GaugeMetricType, Description: "Whether it is up.", Scale: &two, Labels: site},
		"a label that reads": {Labels: []model.LabelRule{{Name: "site", Expression: "instance", Truncate: true}}},
	} {
		c := rules(rule)
		if err := CheckMetricRule(c, &c.Metrics[0]); err != nil {
			t.Errorf("a rule of %s: the rule check says %v", what, err)
		}
		for _, response := range [][]string{names, {"up"}, {}} {
			made, failures := rulesPassOn(t, c, response...)
			if len(made) != 0 || len(failures) != 1 || failures[0].First.Error() != `metric "" is not in the response` || failures[0].Missing != 1 {
				t.Errorf("a rule of %s makes %q of %q, with failures %+v", what, made, response, failures)
			}
		}
		rule.Required = &no
		if made, failures := rulesPassOn(t, rules(rule), names...); len(made) != 0 || len(failures) != 0 {
			t.Errorf("a rule of %s that is not required makes %q, with failures %+v", what, made, failures)
		}
	}
	// Beside rules that select, it adds its failure to what they make.
	if made, failures := rulesPassOn(t, rules(model.MetricRule{Name: "up"}, model.MetricRule{Labels: site}, model.MetricRule{Expression: "^node_"}), names...); !slices.Equal(made, []string{"up", "node_load1"}) || len(failures) != 1 || failures[0].First.Error() != `metric "" is not in the response` {
		t.Errorf("between two rules that select it makes %q, with failures %+v", made, failures)
	}
	// With one of the two keys the rule selects by it.
	for what, test := range map[string]struct {
		rule model.MetricRule
		want []string
	}{
		"a name":                    {model.MetricRule{Name: "up", Labels: site}, []string{"up"}},
		"an expression":             {model.MetricRule{Expression: "^node_", Labels: site}, []string{"node_load1"}},
		"both":                      {model.MetricRule{Name: "load", Expression: "^node_", Labels: site}, []string{"load"}},
		"the pattern of every name": {model.MetricRule{Expression: ".*", Labels: site}, names},
	} {
		c := rules(test.rule)
		if err := CheckMetricRule(c, &c.Metrics[0]); err != nil {
			t.Errorf("a rule of %s: %v", what, err)
		}
		if made, failures := rulesPassOn(t, c, names...); !slices.Equal(made, test.want) || len(failures) != 0 {
			t.Errorf("a rule of %s makes %q with failures %+v, want %q", what, made, failures, test.want)
		}
	}
}
