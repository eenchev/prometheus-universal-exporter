package config

import (
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// Refusing a constant value on a python rule's label changes the verdict on
// nothing else. Over a generated table of rules under each transform — a
// label with each of some values beside each of some expressions, alone,
// with truncate, required, with a value_map, before and after a label that
// is in order and one that is not, in a rule with each of the settings a
// rule takes, in order and not — every rule gets from the loader what it
// got before, the same error word for word or none and the same defaults,
// but for the python rules whose settings were in order and one of whose
// labels sets a value: those, and no rule of another transform, are refused
// in the new words, for the first such label. A python rule that was
// refused for one of its settings — a label that is required, has both a
// value and an expression or neither, a type or an error_mode that is none —
// is refused for that as it was.
//
// Under the race detector, which makes the check many times slower, the
// table is whole under the python transform, whose rules are the ones
// refused anew, and every fifth rule of it under each of the others.
func TestOnlyAPythonRulesLabelValueIsRefusedAnew(t *testing.T) {
	yes, no := true, false
	two := 2.0
	type shape struct{ expression, label, items string }
	shapes := map[string]shape{
		"jq": {".v", ".l", ""}, "yq": {".v", ".l", ""}, "xpath": {"//v", "@l", ""}, "css": {"td.v", "td.l", "tr"},
		"regex": {`v=(\d+) l=(?P<l>\w+)`, "l", ""}, "csv": {"v", "l", ""}, "prometheus": {"^up$", "l", ""}, "python": {"", "l", ""},
	}
	tried, refused := 0, map[string]int{}
	for name, shape := range shapes {
		x := &model.Collector{Name: "demo", Transform: model.TransformConfig{Type: name}}
		good := model.LabelRule{Name: "zone", Expression: shape.label}
		var lists [][]model.LabelRule
		for _, value := range []string{"", "x", " ", "0"} {
			for _, expression := range []string{"", shape.label} {
				for _, label := range []model.LabelRule{
					{Name: "site", Value: value, Expression: expression},
					{Name: "site", Value: value, Expression: expression, Truncate: true},
					{Name: "site", Value: value, Expression: expression, Required: true},
					{Name: "site", Value: value, Expression: expression, ValueMap: map[string]string{"a": "b"}},
					{Name: "bad-name", Value: value, Expression: expression},
				} {
					lists = append(lists, []model.LabelRule{label}, []model.LabelRule{good, label}, []model.LabelRule{label, good},
						[]model.LabelRule{label, {Name: "zone"}}, []model.LabelRule{label, {Name: "zone", Value: "y", Required: true}}, []model.LabelRule{{Name: "zone", Value: "y"}, label})
				}
			}
		}
		lists = append(lists, nil)
		at, every := 0, 1
		if alloctest.RaceDetector && name != "python" {
			every = 5
		}
		for _, labels := range lists {
			for _, rule := range []model.MetricRule{
				{Name: "m", Items: shape.items, Expression: shape.expression},
				{Name: "", Items: shape.items, Expression: shape.expression},
				{Name: "bad-name", Items: shape.items, Expression: shape.expression},
				{Name: "m", Items: shape.items, Expression: "("},
				{Name: "m", Items: ".items[]", Expression: shape.expression},
				{Name: "m", Items: shape.items, Expression: shape.expression, Type: model.CounterMetricType, Description: "A description.", Required: &yes, ErrorMode: "fail"},
				{Name: "m", Items: shape.items, Expression: shape.expression, Type: model.HistogramMetricType, Required: &no},
				{Name: "m", Items: shape.items, Expression: shape.expression, Type: "timer"},
				{Name: "m", Items: shape.items, Expression: shape.expression, ErrorMode: "panic"},
				{Name: "m", Items: shape.items, Expression: shape.expression, Scale: &two},
				{Name: "m", Items: shape.items, Expression: shape.expression, ValueMap: map[string]float64{"up": 1}},
				{Name: "m", Items: shape.items, Expression: shape.expression, TimeFormat: "rfc3339", TimeZone: "UTC"},
			} {
				if at++; at%every != 0 {
					continue
				}
				rule.Labels = slices.Clone(labels)
				tried++
				if checkedAsBefore(t, x, rule) == forAPythonValue {
					refused[name]++
				}
			}
		}
	}
	if tried < alloctest.UnlessRaced(20000, 6000) || refused["python"] < 300 || len(refused) != 1 {
		t.Fatalf("%d rules were tried, and refused for a python rule's label with a value were %v", tried, refused)
	}
	t.Logf("%d generated rules, %d of them python rules refused for a label with a value", tried, refused["python"])
}
