package config

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// prometheusRuleOfNeither is what the load says of a prometheus rule that
// has neither a name nor an expression, after the collector and the rule's
// place among the collector's.
const prometheusRuleOfNeither = `has neither a name nor an expression, and a prometheus rule needs one of them to say which of the target's metrics it passes on: an expression, a regular expression matched against a metric's name as the target gives it, anywhere in it, to match metrics by, or a name, to match the metric of that name; with neither it matches no metric, so write one, as in expression: '^node_' or name: up, or expression: '.*' for a rule about every metric, or take the rule out`

// ruleOfNeither is that message for the rule at index of the collector's
// metrics, which the load counts from 1.
func ruleOfNeither(collector string, index int) string {
	return fmt.Sprintf("collector %q metrics rule %d %s", collector, index+1, prometheusRuleOfNeither)
}

// validateMetricRulesBeforeRulesOfNeither is validateMetricRules as it was
// before a prometheus rule with neither a name nor an expression was
// refused, kept as an oracle: a collector whose rules this refuses and none
// of which is such a rule must be refused by validateMetricRules in the same
// words, and one whose rules this takes must have them taken, with the same
// defaults, unless one of them is such a rule. The check of one rule it
// calls, validateMetricRule, is as it was: this change is no part of it.
// Nor is the check that came after it, of two rules that are the same rule
// (checkRulesDiffer, held to what was by
// TestOnlyTheSameRuleTwiceIsRefusedAnew), which it makes as the check does,
// of the rules it takes.
func validateMetricRulesBeforeRulesOfNeither(_ *model.Config, x *model.Collector) error {
	var errs []error
	sound := make([]bool, len(x.Metrics))
	for i := range x.Metrics {
		err := validateMetricRule(x, i)
		sound[i] = err == nil
		errs = append(errs, err)
	}
	errs = append(errs, transform.CheckLabelValueMapsAgree(x))
	errs = append(errs, checkRulesDiffer(x, sound)...)
	return model.JoinProblems(errs...)
}

// withRules is the collector with a copy of the rules, which the check
// fills the defaults of.
func withRules(x *model.Collector, rules []model.MetricRule) *model.Collector {
	copied := *x
	copied.Metrics = slices.Clone(rules)
	for i := range copied.Metrics {
		copied.Metrics[i].Labels = slices.Clone(copied.Metrics[i].Labels)
	}
	return &copied
}

// rulesAsBefore puts the rules of a collector through the loader's check of
// them and through the check as it was, and fails unless they agree on
// everything but a prometheus rule that has neither a name nor an
// expression and nothing else the check as it was refused it for: where the
// check as it was said nothing of that rule, the check says that the rule
// has neither, naming its place, and it says of every other rule, and of
// the rules together, word for word what it said, in the order it said it.
// The defaults both fill in are the same. It returns how many rules are
// refused anew, and whether the check as it was refused the collector.
func rulesAsBefore(t *testing.T, x *model.Collector, rules []model.MetricRule) (anew int, refusedBefore bool) {
	t.Helper()
	now, before := withRules(x, rules), withRules(x, rules)
	err, was := validateMetricRules(nil, now), validateMetricRulesBeforeRulesOfNeither(nil, before)
	if !reflect.DeepEqual(now.Metrics, before.Metrics) {
		t.Errorf("%s rules %+v are left as %+v, and were left as %+v", x.Transform.Type, rules, now.Metrics, before.Metrics)
	}
	// What the check must say: of each rule what the check as it was says
	// of it by itself, at its place, or, where that is nothing and the rule
	// is a prometheus rule written with neither key, that it has neither;
	// then what it said of the rules together, and of the rules it takes
	// what the check that came since says of two that are the same rule.
	var want []error
	placed, sound := withRules(x, rules), make([]bool, len(rules))
	for i, rule := range rules {
		problem := validateMetricRule(placed, i)
		if problem == nil && x.Transform.Type == "prometheus" && rule.Name == "" && rule.Expression == "" {
			problem = fmt.Errorf("%s", ruleOfNeither(x.Name, i))
			anew++
		}
		sound[i] = problem == nil
		want = append(want, problem)
	}
	want = append(want, transform.CheckLabelValueMapsAgree(withRules(x, rules)))
	wanted := model.JoinProblems(append(want, checkRulesDiffer(placed, sound)...)...)
	if (err == nil) != (wanted == nil) || err != nil && err.Error() != wanted.Error() {
		t.Errorf("%s rules %+v:\n now %v\nwant %v", x.Transform.Type, rules, err, wanted)
	}
	// And so a collector without such a rule gets exactly what it got, and
	// one with a single rule that was refused is refused in the same words.
	if anew == 0 || len(rules) == 1 && was != nil {
		if (err == nil) != (was == nil) || err != nil && err.Error() != was.Error() {
			t.Errorf("%s rules %+v:\n now %v\n was %v", x.Transform.Type, rules, err, was)
		}
	}
	if anew > 0 && err == nil {
		t.Errorf("%s rules %+v are taken", x.Transform.Type, rules)
	}
	return anew, was != nil
}

// Refusing a prometheus rule that has neither a name nor an expression
// changes the verdict on nothing else. The rules of every collector of the
// shipped configurations, and generated ones under each of the eight
// transforms, are put through the loader's check of a collector's rules and
// through that check as it was. Alone in its collector, a rule the check as
// it was refused is refused in the same words, whatever else it says: a
// type or an error_mode that is none, a name that is no metric's or is
// reserved, an expression of blanks or one that does not compile, scale,
// items, a label with a value_map, with both keys or with neither. A rule
// it took is taken, with the same defaults, unless it is a prometheus rule
// with neither key, written "" or left out, which is refused for exactly
// that, by its place. Among other rules, each rule gets what it gets alone,
// in order, such a rule being named by its place among them, and what the
// check says of the rules together, two value_maps of one label, comes
// after as it did. No rule of another transform, and no shipped rule, is
// refused anew.
//
// Under the race detector, which makes the check many times slower, the
// generated collectors of one rule are every seventeenth of the table, among
// which the ways of writing any three of a rule's keys still meet each
// other, every one of them, and those of three rules end in one of three.
func TestOnlyAPrometheusRuleOfNeitherANameNorAnExpressionIsRefusedAnew(t *testing.T) {
	// The shipped collectors, each with the rules it has.
	var shipped []*model.Collector
	files, shippedRuleCount := shippedRules(t, func(_ string, x *model.Collector, _ model.MetricRule) {
		if len(shipped) == 0 || shipped[len(shipped)-1] != x {
			shipped = append(shipped, x)
		}
	})
	if files < 13 || shippedRuleCount < 100 || len(shipped) < 13 {
		t.Fatalf("%d files with %d collectors and %d rules were found", files, len(shipped), shippedRuleCount)
	}
	for _, x := range shipped {
		if anew, _ := rulesAsBefore(t, x, x.Metrics); anew != 0 {
			t.Errorf("collector %q of the shipped configurations has %d rules the load refuses now", x.Name, anew)
		}
	}

	no := false
	two := 2.0
	type shape struct{ expression, label, items string }
	shapes := map[string]shape{
		"jq": {".v", ".l", ""}, "yq": {".v", ".l", ""}, "xpath": {"//v", "@l", ""}, "css": {"td.v", "td.l", "tr"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, "l", ""}, "csv": {"v", "l", ""}, "prometheus": {"^up$", "l", ""}, "python": {"", "l", ""},
	}
	tried, collectors, refusedBefore, anewBeside := 0, 0, 0, 0
	anew, taken := map[string]int{}, map[string]int{}
	for name, shape := range shapes {
		x := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: name}}
		check := func(rules ...model.MetricRule) {
			t.Helper()
			collectors++
			tried += len(rules)
			refused, before := rulesAsBefore(t, x, rules)
			anew[name] += refused
			switch {
			case before && refused > 0:
				anewBeside++
				refusedBefore++
			case before:
				refusedBefore++
			case refused == 0:
				taken[name]++
			}
		}
		lists := [][]model.LabelRule{
			nil, {{Name: "site", Expression: shape.label, Truncate: true}}, {{Name: "site", Value: "x"}}, {{Name: "site"}},
			{{Name: "site", Expression: shape.label, ValueMap: map[string]string{"a": "b"}}}, {{Name: "site", Expression: " "}},
		}
		// One rule: every way of writing the two keys, beside everything
		// else a rule says.
		at, every := 0, alloctest.UnlessRaced(1, 17)
		for _, ruleName := range []string{"", "m", "bad-name", "  ", "__m"} {
			for _, expression := range []string{"", shape.expression, "  ", "(", "^$", ".*"} {
				for _, ruleType := range []model.MetricType{"", model.CounterMetricType, "timer"} {
					for _, required := range []*bool{nil, &no} {
						for _, errorMode := range []string{"", " LOG ", "panic"} {
							for _, labels := range lists {
								for _, rest := range []model.MetricRule{{}, {Description: "A description."}, {Scale: &two}, {Items: ".rows[]"}, {ValueMap: map[string]float64{"up": 1}}} {
									if at++; at%every != 0 {
										continue
									}
									rest.Name, rest.Expression, rest.Type, rest.Required, rest.ErrorMode, rest.Labels = ruleName, expression, ruleType, required, errorMode, labels
									if rest.Items == "" {
										rest.Items = shape.items
									}
									check(rest)
								}
							}
						}
					}
				}
			}
		}
		// Several rules: each of these beside each other, in twos and
		// threes, a rule of neither first, last, between and twice.
		mapped := func(to string) []model.LabelRule {
			return []model.LabelRule{{Name: "site", Expression: shape.label, ValueMap: map[string]string{"a": to}}}
		}
		pool := []model.MetricRule{
			{}, {Labels: lists[2]}, {Type: model.CounterMetricType, Required: &no}, {Type: "timer"}, {Labels: lists[3]},
			{Name: "m", Expression: shape.expression, Items: shape.items}, {Name: "m", Items: shape.items}, {Expression: shape.expression, Items: shape.items},
			{Name: "m", Expression: shape.expression, Items: shape.items, Labels: mapped("b")}, {Name: "m", Expression: shape.expression, Items: shape.items, Labels: mapped("c")},
			{Name: "bad-name", Expression: "("}, {Name: "n", Expression: shape.expression, Items: shape.items, Type: model.CounterMetricType},
		}
		thirds := pool
		if alloctest.RaceDetector {
			thirds = []model.MetricRule{pool[0], pool[5], pool[9]}
		}
		for _, first := range pool {
			for _, second := range pool {
				check(first, second)
				for _, third := range thirds {
					check(first, second, third)
				}
			}
		}
	}
	for name := range shapes {
		if name != "prometheus" && anew[name] != 0 {
			t.Errorf("%d %s rules are refused anew", anew[name], name)
		}
		if taken[name] < 5 {
			t.Errorf("%d %s collectors have their rules taken", taken[name], name)
		}
	}
	if anew["prometheus"] < alloctest.UnlessRaced(1000, 300) || anewBeside < alloctest.UnlessRaced(500, 130) || tried < alloctest.UnlessRaced(150000, 17500) || refusedBefore < alloctest.UnlessRaced(100000, 8300) {
		t.Fatalf("%d rules of %d collectors were tried, %d collectors refused before, %d of them with a rule refused anew beside; refused anew %v", tried, collectors, refusedBefore, anewBeside, anew)
	}
	t.Logf("%d files with %d collectors and %d rules; %d generated rules of %d collectors: %d collectors refused as they were, %d of them with a rule refused anew beside what they were refused for, taken %v, and rules refused anew %v", files, len(shipped), shippedRuleCount, tried, collectors, refusedBefore, anewBeside, taken, anew)
}
