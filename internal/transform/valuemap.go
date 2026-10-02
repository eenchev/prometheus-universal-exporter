package transform

import (
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A metric rule's value_map and scale turn what its expression gives into the
// series' value, the same way in every transform: the text a regex captured,
// a CSS element's text, an XPath node's or computed value, a CSV cell, a jq or
// yq result. value_map looks the value up as text — a number as JSON writes
// it, a boolean as true or false, text without surrounding blanks — and "*"
// maps any value it does not list, numbers included; without a match and
// without "*", the value is read as a number as before. scale then multiplies
// the value, mapped or read.

// valueMapDefault is the value_map key for any value the map does not list.
const valueMapDefault = "*"

// ruleValue is the value a rule's expression gave, raw, as the series' value.
func ruleValue(rule model.MetricRule, raw any) (float64, error) {
	if len(rule.ValueMap) > 0 {
		key, err := labelText(raw)
		if err == nil {
			if mapped, ok := rule.ValueMap[strings.TrimSpace(key)]; ok {
				return scaled(rule, mapped), nil
			}
		}
		if mapped, ok := rule.ValueMap[valueMapDefault]; ok {
			return scaled(rule, mapped), nil
		}
		n, numberErr := model.Number(raw)
		if numberErr != nil {
			if err != nil {
				return 0, fmt.Errorf("value is %s, which is neither text value_map can look up nor a number; select one value inside it", model.ShowValue(raw))
			}
			return 0, fmt.Errorf("value %s is neither in value_map nor a number; add it to value_map, or map \"*\" for any other value", model.QuoteValue(key))
		}
		return scaled(rule, n), nil
	}
	n, err := model.Number(raw)
	if err != nil {
		return 0, err
	}
	return scaled(rule, n), nil
}

// ruleTextValue is ruleValue for text, as a regex capture, an element's text
// and a node's are: the same value and the same error, without handing the
// text over as a value of any type, which allocates a copy of the string's
// header for every series. Only a text that is no number, whose error
// ruleValue words, is handed to it.
func ruleTextValue(rule model.MetricRule, text string) (float64, error) {
	trimmed := strings.TrimSpace(text)
	if len(rule.ValueMap) > 0 {
		if mapped, ok := rule.ValueMap[trimmed]; ok {
			return scaled(rule, mapped), nil
		}
		if mapped, ok := rule.ValueMap[valueMapDefault]; ok {
			return scaled(rule, mapped), nil
		}
	}
	if n, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return scaled(rule, n), nil
	}
	return ruleValue(rule, text)
}

func scaled(rule model.MetricRule, value float64) float64 {
	if rule.Scale == nil {
		return value
	}
	return scale(value, *rule.Scale)
}

// scale multiplies value by s, dividing instead when s is exactly one over
// a whole number, so milliseconds scaled by 0.001 become 0.412 seconds
// rather than 0.41200000000000003: 0.001 has no exact binary form, 1000
// has.
//
// Exactly: s is the float64 nearest to one over the whole number, which is
// what the scale written as 0.001, 1e-9 or 0.5 parses to. A scale that only
// comes close to such a number is itself: 1.5e-9 is within a billionth of
// its inverse's distance from 1/666666667, as every scale that small is,
// and 0.3333333333 from 1/3, and dividing by the rounded inverse gave
// 2.9999999985 for 2e9 scaled by 1.5e-9 and 1e10 for 3e10 scaled by
// 0.3333333333. Those multiply, as every scale did before one divided.
func scale(value, s float64) float64 {
	if whole := math.Round(1 / s); math.Abs(whole) >= 2 && 1/whole == s {
		return value / whole
	}
	return value * s
}

// checkValueRules checks a rule's value_map and scale at load.
func checkValueRules(x *model.Collector, r *model.MetricRule, where string) error {
	if r.Scale != nil {
		if s := *r.Scale; s == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
			return fmt.Errorf("%s scale must be a finite number other than 0, got %v", where, s)
		}
	}
	for key := range r.ValueMap {
		if strings.TrimSpace(key) != key || key == "" {
			return fmt.Errorf("%s value_map key %q has surrounding blanks or is empty; values are looked up without them", where, key)
		}
	}
	if (len(r.ValueMap) > 0 || r.Scale != nil) && x.Transform.Type == "python" {
		return fmt.Errorf("%s sets value_map or scale, which the python transform does not use: its script sets each value with metric(...)", where)
	}
	if len(r.ValueMap) > 0 && x.Transform.Type == "prometheus" {
		return fmt.Errorf("%s sets value_map, which the prometheus transform does not use: its values are numbers already; scale applies", where)
	}
	return nil
}

// mapLabelValues applies each label's value_map to the series of its rule,
// found by the rule's name. A series whose name several rules share takes
// each label's map from the first rule that maps that label. The labels of
// a series are copied before they change, since series may share them.
func mapLabelValues(set *model.MetricSet, c *model.Collector) {
	maps := map[string]map[string]map[string]string{}
	for _, rule := range c.Metrics {
		for _, label := range rule.Labels {
			if len(label.ValueMap) == 0 {
				continue
			}
			byLabel := maps[rule.Name]
			if byLabel == nil {
				byLabel = map[string]map[string]string{}
				maps[rule.Name] = byLabel
			}
			if _, taken := byLabel[label.Name]; !taken {
				byLabel[label.Name] = label.ValueMap
			}
		}
	}
	if len(maps) == 0 {
		return
	}
	for i := range set.Metrics {
		m := &set.Metrics[i]
		byLabel := maps[m.Name]
		copied := false
		for name, values := range byLabel {
			value, ok := m.Labels[name]
			if !ok {
				continue
			}
			mapped, found := values[strings.TrimSpace(value)]
			if !found {
				if mapped, found = values[valueMapDefault]; !found {
					continue
				}
			}
			if !copied {
				m.Labels = model.CloneLabels(m.Labels)
				copied = true
			}
			if mapped == "" {
				delete(m.Labels, name)
			} else {
				m.Labels[name] = mapped
			}
		}
	}
}

// checkLabelValueMaps checks a rule's label value maps at load.
func checkLabelValueMaps(x *model.Collector, r *model.MetricRule, where string) error {
	for _, label := range r.Labels {
		if len(label.ValueMap) == 0 {
			continue
		}
		switch {
		case label.Static():
			return fmt.Errorf("%s label %q sets value_map with a static value; write the value it maps to instead", where, label.Name)
		case x.Transform.Type == "python":
			return fmt.Errorf("%s label %q sets value_map, which the python transform does not use: its script sets each label", where, label.Name)
		case r.Name == "":
			return fmt.Errorf("%s label %q sets value_map on a rule without a name; name the rule, so its series are known", where, label.Name)
		}
		for key, value := range label.ValueMap {
			if strings.TrimSpace(key) != key || key == "" {
				return fmt.Errorf("%s label %q value_map key %q has surrounding blanks or is empty; values are looked up without them", where, label.Name, key)
			}
			if value == "" && label.Required {
				return fmt.Errorf("%s label %q is required, and its value_map maps %q to \"\", which would leave it off", where, label.Name, key)
			}
		}
	}
	return nil
}

// CheckLabelValueMapsAgree refuses two rules of one name that map one label
// differently: a label's value_map is found by its rule's name, so the
// series of both would take the first rule's map without a word.
func CheckLabelValueMapsAgree(x *model.Collector) error {
	type mapped struct {
		rule   int
		values map[string]string
	}
	seen := map[string]mapped{}
	for i, rule := range x.Metrics {
		for _, label := range rule.Labels {
			if len(label.ValueMap) == 0 || rule.Name == "" {
				continue
			}
			key := rule.Name + "\x00" + label.Name
			first, ok := seen[key]
			if !ok {
				seen[key] = mapped{rule: i, values: label.ValueMap}
				continue
			}
			if !maps.Equal(first.values, label.ValueMap) {
				return fmt.Errorf("collector %q metric %q label %q has one value_map in rule %d and another in rule %d; rules of one name share their series, so give them one value_map, or different names", x.Name, rule.Name, label.Name, first.rule+1, i+1)
			}
		}
	}
	return nil
}

// MappedStaticLabelValue is the value the static label of metric rules has on
// its series once mapLabelValues has run: mapped by the first value_map any
// rule of that name gives the label, which CheckLabelValueMapsAgree requires
// to agree with the others, or left as it is.
func MappedStaticLabelValue(x *model.Collector, metric string, label model.LabelRule) string {
	for _, rule := range x.Metrics {
		if rule.Name != metric {
			continue
		}
		for _, other := range rule.Labels {
			if other.Name != label.Name || len(other.ValueMap) == 0 {
				continue
			}
			if mapped, ok := other.ValueMap[strings.TrimSpace(label.Value)]; ok {
				return mapped
			}
			if mapped, ok := other.ValueMap[valueMapDefault]; ok {
				return mapped
			}
			return label.Value
		}
	}
	return label.Value
}
