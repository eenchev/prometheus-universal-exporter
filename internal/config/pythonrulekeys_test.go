package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// What the load says of each key a python rule does not take, after the
// collector and the metric.
const (
	pythonRuleType        = `sets type, which a python rule does not take: the script gives each of its series its type, with metric(..., type="counter"), and a series is a gauge when it gives none; say the type in the script and leave type out of the rule`
	pythonRuleDescription = `sets description, which a python rule does not take: the script gives each of its series its help text, with metric(..., help="..."), and a series has none when it gives none; say the help in the script and leave description out of the rule`
	pythonRuleRequired    = `sets required, which a python rule does not take: the rule makes no series, so it has no value to miss; a script that cannot do without something fails the scrape itself, with fail("..."), so say it in the script and leave required out of the rule`
	pythonRuleErrorMode   = `sets error_mode, which a python rule does not take: the rule makes no series, so it has no failure to handle; a script fails the scrape itself, with fail("..."), and error_handling.on_transform_error says what the collector does then, so leave error_mode out of the rule`
	pythonRuleName        = `has no name, which a python rule needs: the rule makes no series and only names one of the script's, whose labels it cuts with truncate: true and which a debug probe's report lists when the script made none; write the name the script gives the series, as in metric("up", ...), or take the rule out`
	pythonRuleLabel       = `does not set truncate: true, which is all a python rule's label does: it names a label of the script's series to cut to limits.max_label_value_length, and its expression is not read; the script sets the labels of its series itself, with metric(..., labels={...}), so set truncate: true on the label or take it out`
)

// pythonRuleProblems is what the load refuses a rule of a python collector
// for that it took before, as the rule is written: nil for a rule of another
// transform and for a python rule that says nothing such a rule does not
// take. It is the specification of checkPythonRule, written apart from it.
// The rule is the one rule of its collector: without a name it is named by
// that place, rule 1.
func pythonRuleProblems(x *model.Collector, rule model.MetricRule) error {
	if x.Transform.Type != "python" {
		return nil
	}
	var problems []error
	refuse := func(format string, args ...any) {
		problems = append(problems, errors.New(saidNow(fmt.Sprintf(format, args...), x.Name, rule.Name, 0)))
	}
	if rule.Name == "" {
		refuse("collector %q metrics rule 1 %s", x.Name, pythonRuleName)
	}
	for _, key := range []struct {
		written bool
		message string
	}{{rule.Type != "", pythonRuleType}, {rule.Description != "", pythonRuleDescription}, {rule.Required != nil, pythonRuleRequired}, {rule.ErrorMode != "", pythonRuleErrorMode}} {
		if key.written {
			refuse("collector %q metric %q %s", x.Name, rule.Name, key.message)
		}
	}
	for _, label := range rule.Labels {
		if !label.Truncate {
			refuse("collector %q metric %q label %q %s", x.Name, rule.Name, label.Name, pythonRuleLabel)
		}
	}
	return model.JoinProblems(problems...)
}

// validateMetricRuleBeforePythonRuleKeys is validateMetricRule as it was
// before a python rule's type, description, required and error_mode, a
// python rule without a name and a python rule's label without
// truncate: true were refused, kept as an oracle: a rule this refuses must
// be refused by validateMetricRule in the same words, and a rule this takes
// must be taken, with the same defaults, unless it is a python rule that
// says one of those things. A rule that has no name is named since by its
// place among its collector's rules where this writes `metric ""` and "a
// metric without a name" (saidNow). Its last step is transform.CheckMetricRule as it
// is, whose own change, a prometheus rule's expression of nothing but
// blanks, is held to what it was in the transform package.
func validateMetricRuleBeforePythonRuleKeys(x *model.Collector, r *model.MetricRule) error {
	if r.ErrorMode == "" {
		r.ErrorMode = model.ErrorModeLog
	}
	if err := normalizeErrorPolicy(x.Name, fmt.Sprintf("metric %q error_mode", r.Name), &r.ErrorMode); err != nil {
		return err
	}
	// A prometheus transform's rule without a type keeps the type of
	// the series it passes through: a counter stays a counter, a
	// histogram a histogram. Every other rule makes its own samples,
	// gauges unless it says otherwise.
	if r.Type == "" && x.Transform.Type != "prometheus" {
		r.Type = model.GaugeMetricType
	}
	switch r.Type {
	case model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType:
	case "":
	case model.HistogramMetricType, model.SummaryMetricType:
		// Only a series that is one already has buckets or quantiles to
		// expose; a rule reading one number would expose a histogram
		// with a single plain sample, which no parser accepts.
		if x.Transform.Type != "prometheus" {
			return fmt.Errorf("collector %q metric %q has type %s, which only a prometheus transform can give, passing through a %s that has its buckets or quantiles; a %s rule reads one value, so use gauge, counter or untyped", x.Name, r.Name, r.Type, r.Type, x.Transform.Type)
		}
	default:
		return fmt.Errorf("collector %q metric %q has invalid type %q", x.Name, r.Name, r.Type)
	}
	if strings.TrimSpace(r.Name) == "" && x.Transform.Type != "prometheus" && x.Transform.Type != "python" {
		return fmt.Errorf("collector %q has a metric without a name", x.Name)
	}
	if strings.TrimSpace(r.Expression) == "" && x.Transform.Type != "python" && x.Transform.Type != "prometheus" {
		return fmt.Errorf("collector %q metric %q has no expression", x.Name, r.Name)
	}
	for _, label := range r.Labels {
		if strings.TrimSpace(label.Name) == "" {
			return fmt.Errorf("collector %q metric %q has a label without a name", x.Name, r.Name)
		}
		if !namePattern.MatchString(label.Name) {
			return fmt.Errorf("collector %q metric %q has invalid label name %q", x.Name, r.Name, label.Name)
		}
		if err := model.CheckLabelName(label.Name); err != nil {
			return fmt.Errorf("collector %q metric %q: %w", x.Name, r.Name, err)
		}
		// An expression written as nothing but blanks is neither the key left
		// out, which only "" is, nor anything to read a label with. The label
		// is not static then (model.LabelRule.Static), so a value beside it
		// was never exported, while a csv rule read the column of that name
		// and a prometheus rule the source label of that name.
		if label.Expression != "" && strings.TrimSpace(label.Expression) == "" {
			return fmt.Errorf("collector %q metric %q label %q expression %q is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant", x.Name, r.Name, label.Name, label.Expression)
		}
		hasValue, hasExpression := label.Value != "", strings.TrimSpace(label.Expression) != ""
		switch {
		case hasValue && hasExpression:
			return fmt.Errorf("collector %q metric %q label %q sets both value and expression; set value for a static label, or expression to read it from the response", x.Name, r.Name, label.Name)
		case !hasValue && !hasExpression:
			return fmt.Errorf("collector %q metric %q label %q needs a value, for a static label, or an expression, to read it from the response", x.Name, r.Name, label.Name)
		case hasValue && label.Required:
			return fmt.Errorf("collector %q metric %q label %q has a static value, so it cannot be required; its value is always there", x.Name, r.Name, label.Name)
		case label.Required && x.Transform.Type == "python":
			return fmt.Errorf("collector %q metric %q label %q cannot be required: a python transform's labels come from its script, not from label expressions", x.Name, r.Name, label.Name)
		}
	}
	if err := pythonRuleLabelsBeforePythonRuleKeys(x, r); err != nil {
		return err
	}
	return transform.CheckMetricRule(x, r)
}

// pythonRuleLabelsBeforePythonRuleKeys is checkPythonRuleLabels as it was.
func pythonRuleLabelsBeforePythonRuleKeys(x *model.Collector, r *model.MetricRule) error {
	if x.Transform.Type != "python" {
		return nil
	}
	for _, label := range r.Labels {
		if label.Value != "" {
			return fmt.Errorf("collector %q metric %q label %q sets value, which a python rule's label does not take: the script sets the labels of its series itself, with metric(..., labels={...}), and a rule's label only names one of them to cut with truncate: true; for a constant on every series of the collector, set transform.labels", x.Name, r.Name, label.Name)
		}
	}
	return nil
}

// shippedRules calls each with every rule of the shipped configurations —
// the examples, the ones under configs and the fixtures' — as it is
// written: read without being validated, which a build with only some
// request types could not do for them all. It returns how many files and
// rules there were.
func shippedRules(t *testing.T, each func(path string, x *model.Collector, rule model.MetricRule)) (files, rules int) {
	t.Helper()
	for _, root := range []string{"../../examples", "../../configs", "../../testdata"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			var written []model.Collector
			_, _ = load(path, nil, func(c *model.Config) {
				for _, collector := range c.Collectors {
					collector.Metrics = slices.Clone(collector.Metrics)
					for i := range collector.Metrics {
						collector.Metrics[i].Labels = slices.Clone(collector.Metrics[i].Labels)
					}
					written = append(written, collector)
				}
			})
			if len(written) > 0 {
				files++
			}
			for i := range written {
				x := &written[i]
				x.Transform.Type = strings.ToLower(strings.TrimSpace(x.Transform.Type))
				for _, rule := range x.Metrics {
					rules++
					each(path, x, rule)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files, rules
}

// Refusing what a python rule does not take changes the verdict on nothing
// else. Every rule of the shipped configurations, and of a generated table
// under each of the eight transforms, is put through the loader's check and
// through the check as it was. A rule the check as it was refused is
// refused in the same words, word for word, whatever else it says: an
// error_mode or a type that is none, type histogram, a name that is no
// metric's, a label that is required, has a value_map, a value, both a
// value and an expression or neither, items, scale, a value_map, a
// time_format and a time_zone. A rule it took is taken, with the same
// defaults, unless it is a python rule that sets type, description,
// required or error_mode, has no name, or has a label without
// truncate: true: those are refused for exactly that, every such thing of
// the rule named, in order, and no rule of another transform is. No shipped
// rule is among them.
//
// Under the race detector, which makes the check many times slower, the
// table is whole under the python transform, whose rules are the ones
// refused anew and the few that are taken, and every thirteenth rule of it
// under each of the others.
func TestOnlyWhatAPythonRuleDoesNotTakeIsRefusedAnew(t *testing.T) {
	tried, refusedBefore, taken, anew := 0, 0, map[string]int{}, map[string]int{}
	check := func(x *model.Collector, rule model.MetricRule) bool {
		t.Helper()
		tried++
		now, before := rule, rule
		now.Labels, before.Labels = slices.Clone(rule.Labels), slices.Clone(rule.Labels)
		err, was := validateRuleAlone(x, &now), validateMetricRuleBeforePythonRuleKeys(x, &before)
		if !reflect.DeepEqual(now, before) {
			t.Errorf("%s rule %+v is left as %+v, and was left as %+v", x.Transform.Type, rule, now, before)
		}
		if was != nil {
			refusedBefore++
			if err == nil || err.Error() != saidNow(was.Error(), x.Name, rule.Name, 0) {
				t.Errorf("%s rule %+v:\n now %v\n was %v", x.Transform.Type, rule, err, was)
			}
			return false
		}
		want := pythonRuleProblems(x, rule)
		if (err == nil) != (want == nil) || err != nil && err.Error() != want.Error() {
			t.Errorf("%s rule %+v:\n now %v\nwant %v", x.Transform.Type, rule, err, want)
		}
		if want == nil {
			taken[x.Transform.Type]++
			return false
		}
		anew[x.Transform.Type]++
		var problems model.Problems
		if errors.As(want, &problems) {
			anew["several things at once"]++
		}
		for what, message := range map[string]string{"type": pythonRuleType, "description": pythonRuleDescription, "required": pythonRuleRequired, "error_mode": pythonRuleErrorMode, "no name": pythonRuleName, "a label": pythonRuleLabel} {
			if strings.Contains(want.Error(), message) {
				anew[what]++
			}
		}
		return true
	}
	files, rules := shippedRules(t, func(path string, x *model.Collector, rule model.MetricRule) {
		if check(x, rule) {
			t.Errorf("%s: collector %q metric %q is a python rule the load refuses now", path, x.Name, rule.Name)
		}
	})
	if files < 13 || rules < 100 {
		t.Fatalf("%d files with %d rules were found", files, rules)
	}

	yes, no := true, false
	two := 2.0
	type shape struct{ expression, label, items string }
	shapes := map[string]shape{
		"jq": {".v", ".l", ""}, "yq": {".v", ".l", ""}, "xpath": {"//v", "@l", ""}, "css": {"td.v", "td.l", "tr"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, "l", ""}, "csv": {"v", "l", ""}, "prometheus": {"^up$", "l", ""}, "python": {"", "l", ""},
	}
	for name, shape := range shapes {
		x := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: name}}
		cut, plain := model.LabelRule{Name: "note", Expression: shape.label, Truncate: true}, model.LabelRule{Name: "site", Expression: shape.label}
		lists := [][]model.LabelRule{
			nil, {cut}, {plain}, {cut, plain}, {plain, cut, {Name: "zone", Expression: shape.label}},
			{{Name: "site", Value: "x"}}, {{Name: "site", Value: "x", Truncate: true}}, {plain, {Name: "zone", Value: "x"}},
			{{Name: "site", Expression: shape.label, Truncate: true, Required: true}}, {{Name: "site", Expression: shape.label, ValueMap: map[string]string{"a": "b"}}},
			{{Name: "site"}}, {{Name: "site", Value: "x", Expression: shape.label}}, {{Name: "bad-name", Expression: shape.label}}, {{Name: "site", Expression: " ", Truncate: true}},
		}
		at, every := 0, 1
		if alloctest.RaceDetector && name != "python" {
			every = 13
		}
		// Every way of writing the four keys and the name, beside each
		// list of labels.
		for _, ruleName := range []string{"m", "", "bad-name", "  "} {
			for _, ruleType := range []model.MetricType{"", model.CounterMetricType, model.GaugeMetricType, model.HistogramMetricType, "timer"} {
				for _, description := range []string{"", "A description.", " "} {
					for _, required := range []*bool{nil, &yes, &no} {
						for _, errorMode := range []string{"", "fail", "log", " LOG ", "panic"} {
							for _, labels := range lists {
								if at++; at%every != 0 {
									continue
								}
								check(x, model.MetricRule{Name: ruleName, Type: ruleType, Description: description, Required: required, ErrorMode: errorMode, Items: shape.items, Expression: shape.expression, Labels: labels})
							}
						}
					}
				}
			}
		}
		// Each of them beside what the expressions' check refuses or takes.
		for _, rule := range []model.MetricRule{
			{Name: "m", Items: shape.items, Expression: shape.expression},
			{Name: "m", Items: shape.items, Expression: "("},
			{Name: "m", Items: shape.items, Expression: "  "},
			{Name: "m", Items: ".items[]", Expression: shape.expression},
			{Name: "m", Items: shape.items, Expression: shape.expression, Scale: &two},
			{Name: "m", Items: shape.items, Expression: shape.expression, ValueMap: map[string]float64{"up": 1}},
			{Name: "m", Items: shape.items, Expression: shape.expression, TimeFormat: "rfc3339", TimeZone: "UTC"},
			{Name: "m", Items: shape.items, Expression: shape.expression, TimeZone: "UTC"},
			{Name: "", Items: shape.items, Expression: shape.expression, Scale: &two},
			{Name: "__m", Items: shape.items, Expression: shape.expression},
		} {
			for _, labels := range lists {
				for _, keys := range []model.MetricRule{{}, {Type: model.CounterMetricType}, {Description: "A description."}, {Required: &no}, {ErrorMode: "fail"}, {Type: model.UntypedMetricType, Description: "A description.", Required: &yes, ErrorMode: "ignore"}} {
					if at++; at%every != 0 {
						continue
					}
					rule.Type, rule.Description, rule.Required, rule.ErrorMode, rule.Labels = keys.Type, keys.Description, keys.Required, keys.ErrorMode, labels
					check(x, rule)
				}
			}
		}
	}
	for _, what := range []string{"python", "type", "description", "required", "error_mode", "no name", "a label", "several things at once"} {
		if anew[what] < 100 {
			t.Errorf("refused anew for %s: %d rules", what, anew[what])
		}
	}
	for name := range shapes {
		if name != "python" && anew[name] != 0 {
			t.Errorf("%d %s rules are refused anew", anew[name], name)
		}
		if taken[name] < 5 {
			t.Errorf("%d %s rules are taken", taken[name], name)
		}
	}
	if tried < alloctest.UnlessRaced(100000, 19000) || refusedBefore < alloctest.UnlessRaced(50000, 9800) {
		t.Fatalf("%d rules were tried, %d of them refused before", tried, refusedBefore)
	}
	t.Logf("%d files with %d rules, and %d rules in all: %d refused as they were, taken %v, and refused anew %v", files, rules, tried, refusedBefore, taken, anew)
}
