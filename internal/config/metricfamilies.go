package config

import (
	"fmt"
	"slices"

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
		if limits.MaxLabelValueLength > 0 && len(value) > limits.MaxLabelValueLength {
			return fmt.Errorf("collector %q transform.labels %q is %d bytes, longer than limits.max_label_value_length %d, so every series would fail validation; shorten it or raise the limit", x.Name, name, len(value), limits.MaxLabelValueLength)
		}
	}
	if x.Transform.Type == "python" {
		return nil
	}
	types := map[string]model.MetricType{}
	for _, rule := range x.Metrics {
		if limits.MaxHelpLength > 0 && len(rule.Description) > limits.MaxHelpLength {
			return fmt.Errorf("collector %q metric %q description is %d bytes, longer than limits.max_help_length %d, so every series would fail validation; shorten it or raise the limit", x.Name, rule.Name, len(rule.Description), limits.MaxHelpLength)
		}
		for _, label := range rule.Labels {
			if !label.Static() || label.Truncate || slices.Contains(x.Transform.RemoveLabels, label.Name) {
				continue
			}
			value := transform.MappedStaticLabelValue(x, rule.Name, label)
			if limits.MaxLabelValueLength > 0 && len(value) > limits.MaxLabelValueLength {
				return fmt.Errorf("collector %q metric %q label %q value is %d bytes, longer than limits.max_label_value_length %d, so every series would fail validation; shorten it, set truncate: true on the label, or raise the limit", x.Name, rule.Name, label.Name, len(value), limits.MaxLabelValueLength)
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
