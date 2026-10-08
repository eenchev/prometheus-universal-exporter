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

// The load-time length check of a rule's label value that holds
// placeholders, where a value_map of the rule's name maps the label
// (checkTemplatedLabelLength), against the check as it was.

// checkMetricFamiliesBeforeMappedTemplates is checkMetricFamilies as it was
// before a templated value of a mapped label was measured as mapped.
func checkMetricFamiliesBeforeMappedTemplates(x *model.Collector) error {
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
				if limits.MaxLabelValueLength > 0 && len(least) > limits.MaxLabelValueLength && !labelIsMappedBefore(x, rule.Name, label.Name) {
					return fmt.Errorf("%s label %q value is %d bytes once its placeholders take their defaults, longer than limits.max_label_value_length %d, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, set truncate: true on the label, or raise the limit", transform.RuleWhere(x, i), label.Name, len(least), limits.MaxLabelValueLength)
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

// labelIsMappedBefore is labelIsMapped as it was.
func labelIsMappedBefore(x *model.Collector, metric, name string) bool {
	for i := range x.Metrics {
		if x.Metrics[i].Name != metric {
			continue
		}
		for _, label := range x.Metrics[i].Labels {
			if label.Name == name && len(label.ValueMap) > 0 {
				return true
			}
		}
	}
	return false
}

// Only a rule's label value with placeholders, mapped by a value_map of the
// rule's name, without truncate: true and not removed, under a limit, is
// measured otherwise than it was, and refused, where it loaded, exactly
// where a probe of a kind fails: its value_map's "*" entry is longer than
// the limit, or the value its defaults give is mapped to a text longer than
// the limit, or, mapped by no entry, is itself longer. Every other
// collector of a generated table of values, maps, limits, truncate and
// remove_labels settings, constants among them, gets what it got, the error
// byte for byte.
func TestATemplatedMappedLabelIsTheOnlyLabelMeasuredOtherwise(t *testing.T) {
	values := []string{"x", "acme", "overlongvalue", "{{param_t}}", "{{param_t:}}", "{{param_t:x}}", "{{param_t:acme}}", "{{param_t:acme-corp}}", "api-{{param_t:acme}}", "{{param_t: acme }}", "{{param_t:ac}}{{param_u:me}}"}
	valueMaps := []map[string]string{nil, {"acme": "short"}, {"acme": "overlongvalue"}, {"*": "short"}, {"*": "overlongvalue"}, {"*": ""}, {"acme": "overlongvalue", "*": "short"}, {"acme": "short", "*": "overlongvalue"}, {"acme-corp": "short"}, {"x": "overlongvalue"}}
	compared, differ := 0, 0
	for _, value := range values {
		for _, valueMap := range valueMaps {
			for _, otherName := range []string{"m", "n"} {
				for _, limit := range []int{0, 8, 13, 500} {
					for _, truncate := range []bool{false, true} {
						for _, removed := range []bool{false, true} {
							c := &model.Collector{
								Name:      "a",
								Transform: model.TransformConfig{Type: "jq"},
								Limits:    model.Limits{MaxLabelValueLength: limit},
								Metrics: []model.MetricRule{
									{Name: "m", Type: model.GaugeMetricType, Expression: ".x", Labels: []model.LabelRule{{Name: "tenant", Value: value, Truncate: truncate}}},
									{Name: otherName, Type: model.GaugeMetricType, Expression: ".y", Labels: []model.LabelRule{{Name: "tenant", Expression: ".t", ValueMap: valueMap}}},
								},
							}
							if removed {
								c.Transform.RemoveLabels = []string{"tenant"}
							}
							labels, err := fetch.ParseLabelParams(c, func(index int) string { return transform.RuleName(&c.Metrics[index], index) })
							if err != nil {
								t.Fatal(err)
							}
							c.LabelParams = labels
							was, is := errorText(checkMetricFamiliesBeforeMappedTemplates(c)), errorText(checkMetricFamilies(c))
							compared++
							least, templated := fetch.RuleLabelDefaults(c, 0, 0, value)
							mapped := otherName == "m" && len(valueMap) > 0
							where := fmt.Sprintf("%q, value_map %v of %q, limit %d, truncate %v, removed %v", value, valueMap, otherName, limit, truncate, removed)
							if !templated || !mapped || truncate || removed || limit == 0 {
								if is != was {
									t.Errorf("%s:\n got %q\nwant %q", where, is, was)
								}
								continue
							}
							target, refused := least, false
							if long, ok := valueMap["*"]; ok && len(long) > limit {
								refused = true
							}
							if to, ok := valueMap[strings.TrimSpace(least)]; ok && least != "" {
								target = to
							} else if to, ok := valueMap["*"]; ok && least != "" {
								target = to
							}
							refused = refused || len(target) > limit
							if was != "" || (is != "") != refused {
								t.Errorf("%s: refused %q, was %q, want refused %v", where, is, was, refused)
							}
							if is != was {
								differ++
							}
						}
					}
				}
			}
		}
	}
	if compared != len(values)*len(valueMaps)*2*4*2*2 || differ == 0 {
		t.Fatalf("%d collectors compared, %d refused anew", compared, differ)
	}
}

// errorText is err's text, or "" for none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
