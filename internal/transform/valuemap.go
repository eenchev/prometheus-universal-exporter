package transform

import (
	"fmt"
	"math"
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
			return 0, fmt.Errorf("value %q is neither in value_map nor a number; add it to value_map, or map \"*\" for any other value", key)
		}
		return scaled(rule, n), nil
	}
	n, err := model.Number(raw)
	if err != nil {
		return 0, err
	}
	return scaled(rule, n), nil
}

func scaled(rule model.MetricRule, value float64) float64 {
	if rule.Scale == nil {
		return value
	}
	return value * *rule.Scale
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
