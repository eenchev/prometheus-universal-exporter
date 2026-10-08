package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The load-time length check of a rule's static label where the value maps
// and the cuts of truncate: true go by the series' name (seriesNames,
// labelTruncatedByName), against the check as it was.

// checkMetricFamiliesBeforeSeriesNames is checkMetricFamilies as it was
// before a rule without a name was measured by the maps of the names its
// expression matches, and truncate: true in another rule of the name spared
// a label.
func checkMetricFamiliesBeforeSeriesNames(x *model.Collector) error {
	limits := x.Limits
	for _, name := range model.SortedKeys(x.Transform.Labels) {
		value := x.Transform.Labels[name]
		if slices.Contains(x.Transform.RemoveLabels, name) {
			continue
		}
		if least, templated := fetch.CollectorLabelDefaults(x, name, value); templated {
			if limits.MaxLabelValueLength > 0 && len(least) > limits.MaxLabelValueLength {
				return fmt.Errorf("collector %q transform.labels %q is %d bytes once its placeholders take their defaults, longer than limits.max_label_value_length %d, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, or raise the limit", x.Name, name, len(least), limits.MaxLabelValueLength)
			}
			continue
		}
		if limits.MaxLabelValueLength > 0 && len(value) > limits.MaxLabelValueLength {
			return fmt.Errorf("collector %q transform.labels %q is %d bytes, longer than limits.max_label_value_length %d, so every series would fail validation; shorten it or raise the limit", x.Name, name, len(value), limits.MaxLabelValueLength)
		}
	}
	if x.Transform.Type == "python" {
		return nil
	}
	types := map[string]model.MetricType{}
	for i, rule := range x.Metrics {
		// A prometheus rule may have no name, and is then named by its place.
		if limits.MaxHelpLength > 0 && len(rule.Description) > limits.MaxHelpLength {
			return fmt.Errorf("%s description is %d bytes, longer than limits.max_help_length %d, so every series would fail validation; shorten it or raise the limit", transform.RuleWhere(x, i), len(rule.Description), limits.MaxHelpLength)
		}
		for j, label := range rule.Labels {
			if !label.Static() || label.Truncate || slices.Contains(x.Transform.RemoveLabels, label.Name) {
				continue
			}
			if least, templated := fetch.RuleLabelDefaults(x, i, j, label.Value); templated {
				if limits.MaxLabelValueLength > 0 {
					if err := checkTemplatedLabelLengthBeforeSeriesNames(x, i, label.Name, least); err != nil {
						return err
					}
				}
				continue
			}
			value := transform.MappedStaticLabelValue(x, rule.Name, label)
			if limits.MaxLabelValueLength > 0 && len(value) > limits.MaxLabelValueLength {
				return fmt.Errorf("%s label %q value is %d bytes, longer than limits.max_label_value_length %d, so every series would fail validation; shorten it, set truncate: true on the label, or raise the limit", transform.RuleWhere(x, i), label.Name, len(value), limits.MaxLabelValueLength)
			}
		}
		if rule.Name == "" || rule.Type == "" {
			continue
		}
		if prior, ok := types[rule.Name]; ok && prior != rule.Type {
			return fmt.Errorf("collector %q metric %q is declared as both %s and %s; the rules of one metric name make one family, which has one type, so every scrape would fail validation; give them the same type or different names", x.Name, rule.Name, prior, rule.Type)
		}
		types[rule.Name] = rule.Type
	}
	for _, name := range model.SortedKeys(types) {
		var suffixes []string
		switch types[name] {
		case model.HistogramMetricType:
			suffixes = []string{"_bucket", "_sum", "_count"}
		case model.SummaryMetricType:
			suffixes = []string{"_sum", "_count"}
		}
		for _, suffix := range suffixes {
			if other, clash := types[name+suffix]; clash {
				return fmt.Errorf("collector %q metric %q (%s) clashes with the %s %q, which is written as series of that name, so every scrape that has both would fail validation; rename one", x.Name, name+suffix, other, types[name], name)
			}
		}
	}
	return nil
}

// checkTemplatedLabelLengthBeforeSeriesNames is checkTemplatedLabelLength as
// it was.
func checkTemplatedLabelLengthBeforeSeriesNames(x *model.Collector, rule int, name, least string) error {
	limit := x.Limits.MaxLabelValueLength
	where := transform.RuleWhere(x, rule)
	values := labelValueMap(x, x.Metrics[rule].Name, name)
	if target, ok := values[valueMapAny]; ok && len(target) > limit {
		return fmt.Errorf("%s label %q value holds {{param_...}} placeholders, and the value_map of the metric's name maps every value it does not list, by its %q entry, to %d bytes, longer than limits.max_label_value_length %d, so every series of a probe that gives a value not listed would fail validation; shorten the %q entry or take it out, set truncate: true on the label, or raise the limit", where, name, valueMapAny, len(target), limit, valueMapAny)
	}
	if least != "" && values != nil {
		key := strings.TrimSpace(least)
		target, ok := values[key]
		if !ok {
			key = valueMapAny
			target, ok = values[key]
		}
		if ok {
			if len(target) > limit {
				return fmt.Errorf("%s label %q value is mapped once its placeholders take their defaults, by the %q entry of the value_map of the metric's name, to %d bytes, longer than limits.max_label_value_length %d, so every series of a probe that gives them no other value would fail validation; shorten that entry or change the defaults, set truncate: true on the label, or raise the limit", where, name, key, len(target), limit)
			}
			return nil
		}
	}
	if len(least) > limit {
		return fmt.Errorf("%s label %q value is %d bytes once its placeholders take their defaults, longer than limits.max_label_value_length %d, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, set truncate: true on the label, or raise the limit", where, name, len(least), limit)
	}
	return nil
}

// Over a generated table of collectors — a rule reading foo, without a name
// under the prometheus transform, named foo or named bar, its label tenant
// a constant or a value with placeholders, short or long, with truncate:
// true or not; a rule named foo or bar mapping tenant by a value_map or not,
// with truncate: true or not; a limit or none; the jq transform beside —
// only two kinds of collector get another verdict than they got: a rule
// without a name whose expression matches the name of a rule that maps the
// label, which is refused where it loaded and never loads where it was
// refused, and a label of a rule whose name another rule shares, setting
// truncate: true on the label, which loads where it was refused and is never
// refused where it loaded. Every other collector gets what it got, the error
// byte for byte.
func TestOnlyTheValueMapsAndCutsOfTheSeriesNameChangeALabelsVerdict(t *testing.T) {
	values := []string{"acme", "overlongvalue", "{{param_t:acme}}", "{{param_t:overlongvalue}}", "{{param_t}}"}
	valueMaps := []map[string]string{nil, {"acme": "short"}, {"acme": "overlongvalue"}, {"*": "short"}, {"*": "overlongvalue"}}
	compared, refusedAnew, sparedAnew := 0, 0, 0
	for _, kind := range []string{"prometheus", "jq"} {
		for _, aName := range []string{"", "foo", "bar"} {
			if aName == "" && kind == "jq" {
				continue
			}
			for _, pattern := range []string{"^foo$", "^baz$", "f"} {
				for _, value := range values {
					for _, aTruncate := range []bool{false, true} {
						for _, bName := range []string{"foo", "bar"} {
							for _, valueMap := range valueMaps {
								for _, bTruncate := range []bool{false, true} {
									for _, limit := range []int{0, 8} {
										c := &model.Collector{
											Name:      "a",
											Transform: model.TransformConfig{Type: kind},
											Limits:    model.Limits{MaxLabelValueLength: limit},
											Metrics: []model.MetricRule{
												{Name: aName, Expression: pattern, Labels: []model.LabelRule{{Name: "tenant", Value: value, Truncate: aTruncate}}},
												{Name: bName, Expression: "^other$", Labels: []model.LabelRule{{Name: "tenant", Expression: "tenant", ValueMap: valueMap, Truncate: bTruncate}}},
											},
										}
										labels, err := fetch.ParseLabelParams(c, func(index int) string { return transform.RuleName(&c.Metrics[index], index) })
										if err != nil {
											t.Fatal(err)
										}
										c.LabelParams = labels
										was, is := errorText(checkMetricFamiliesBeforeSeriesNames(c)), errorText(checkMetricFamilies(c))
										compared++
										where := fmt.Sprintf("%s, rule %q %q value %q truncate %v, rule %q map %v truncate %v, limit %d", kind, aName, pattern, value, aTruncate, bName, valueMap, bTruncate, limit)
										matches := aName == "" && bName == "foo" && pattern != "^baz$"
										mappedByMatch := matches && len(valueMap) > 0 && !aTruncate && limit > 0
										sparedByName := aName == bName && bTruncate && !aTruncate && limit > 0
										switch {
										case is == was:
										case mappedByMatch && was == "" && strings.Contains(is, `the value_map of the metric "foo", a name the rule's expression matches and keeps`):
											refusedAnew++
										case sparedByName && is == "":
											sparedAnew++
										default:
											t.Errorf("%s:\n got %q\nwant %q", where, is, was)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if refusedAnew == 0 || sparedAnew == 0 {
		t.Fatalf("%d collectors compared, %d refused anew, %d spared anew", compared, refusedAnew, sparedAnew)
	}
}
