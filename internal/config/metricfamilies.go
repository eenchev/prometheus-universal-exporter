package config

import (
	"fmt"
	"slices"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// checkMetricFamilies refuses a collector whose rules are bound to fail the
// validation every scrape's metrics go through (model.MetricSet.Validate),
// whatever the target answers: two rules giving one metric name different
// types; a rule named as a series of another rule's histogram or summary; a
// description longer than limits.max_help_length; and a static label value
// longer than limits.max_label_value_length that nothing cuts or maps first.
// Each would otherwise load, and then fail every scrape with an error that
// names the series rather than the setting to change.
//
// A label value that holds {{param_...}} placeholders is measured as a probe
// that gives no parameter fills it, each placeholder replaced by its default
// and by nothing where it has none (fetch.CollectorLabelDefaults): too long
// by its own text and its defaults, it fails every probe that leaves the
// parameters out. What a probe's own values make of it is the scrape's to
// find. Such a value of a rule's label is not measured at all where a
// value_map of the rule's name maps the label, since what it is mapped to
// depends on the value the probe gives.
//
// Several rules may give one name the same type: they feed one family, as
// one rule per source field of the same metric does, and that is kept.
//
// A python transform's rules make no series, its script does, so only
// transform.labels, which apply to every transform's series, are checked for
// it. A prometheus rule without a name or a type keeps the series' own,
// which cannot be known before a scrape, so it is not compared.
func checkMetricFamilies(x *model.Collector) error {
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
				if limits.MaxLabelValueLength > 0 && len(least) > limits.MaxLabelValueLength && !labelIsMapped(x, rule.Name, label.Name) {
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

// labelIsMapped says whether a value_map of a rule named metric maps the
// label name, as mapLabelValues finds one: by the rule's name, so for every
// rule of that name, the one that sets it or another.
func labelIsMapped(x *model.Collector, metric, name string) bool {
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
