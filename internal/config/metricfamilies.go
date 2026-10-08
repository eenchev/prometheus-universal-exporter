package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
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
// find. Such a value of a rule's label that a value_map of the rule's name
// maps is measured as mapped (checkTemplatedLabelLength).
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
			least, templated := fetch.RuleLabelDefaults(x, i, j, label.Value)
			for _, metric := range seriesNames(x, i) {
				if labelTruncatedByName(x, metric, label.Name) {
					continue
				}
				if templated {
					if limits.MaxLabelValueLength > 0 {
						if err := checkTemplatedLabelLength(x, i, metric, label.Name, least); err != nil {
							return err
						}
					}
					continue
				}
				value := transform.MappedStaticLabelValue(x, metric, label)
				if limits.MaxLabelValueLength > 0 && len(value) > limits.MaxLabelValueLength {
					if metric != rule.Name {
						return fmt.Errorf("%s label %q value is mapped by the value_map of the metric %q, a name the rule's expression matches and keeps, to %d bytes, longer than limits.max_label_value_length %d, so every series of that name would fail validation; shorten that entry, set truncate: true on the label, or give the rule a name or an expression that does not match %q", transform.RuleWhere(x, i), label.Name, metric, len(value), limits.MaxLabelValueLength, metric)
					}
					return fmt.Errorf("%s label %q value is %d bytes, longer than limits.max_label_value_length %d, so every series would fail validation; shorten it, set truncate: true on the label, or raise the limit", transform.RuleWhere(x, i), label.Name, len(value), limits.MaxLabelValueLength)
				}
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

// checkTemplatedLabelLength refuses the static value of the label name of
// the metric rule at index rule, which holds placeholders and is least once
// they take their defaults, where every probe of a kind would fail
// validation for its length: as a constant is refused, with the value a
// value_map of the rule's name maps it to measured in its place
// (transform.MappedStaticLabelValue).
//
// Where such a map maps the label, its "*" entry is what every value the
// probe gives that the map does not list becomes, and one longer than the
// limit is refused whatever the defaults are: a probe can give a value
// listed, but the label is written to take any. A value the map lists only
// some of is the probe's to fill: a listed value mapped to too long a text
// fails the probe that gives it, as any long value does, and is refused here
// only where the defaults are that value, since then it fails every probe
// that leaves the parameters out. A value filled to nothing is the label
// left out, which no map sees.
func checkTemplatedLabelLength(x *model.Collector, rule int, metric, name, least string) error {
	limit := x.Limits.MaxLabelValueLength
	where := transform.RuleWhere(x, rule)
	values := labelValueMap(x, metric, name)
	owner := "the metric's name"
	if metric != x.Metrics[rule].Name {
		owner = fmt.Sprintf("the metric %q, a name the rule's expression matches and keeps,", metric)
	}
	if target, ok := values[valueMapAny]; ok && len(target) > limit {
		return fmt.Errorf("%s label %q value holds {{param_...}} placeholders, and the value_map of %s maps every value it does not list, by its %q entry, to %d bytes, longer than limits.max_label_value_length %d, so every series of a probe that gives a value not listed would fail validation; shorten the %q entry or take it out, set truncate: true on the label, or raise the limit", where, name, owner, valueMapAny, len(target), limit, valueMapAny)
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
				return fmt.Errorf("%s label %q value is mapped once its placeholders take their defaults, by the %q entry of the value_map of %s, to %d bytes, longer than limits.max_label_value_length %d, so every series of a probe that gives them no other value would fail validation; shorten that entry or change the defaults, set truncate: true on the label, or raise the limit", where, name, key, owner, len(target), limit)
			}
			return nil
		}
	}
	if len(least) > limit {
		return fmt.Errorf("%s label %q value is %d bytes once its placeholders take their defaults, longer than limits.max_label_value_length %d, so every series of a probe that gives them no other value would fail validation; shorten the text or the defaults, set truncate: true on the label, or raise the limit", where, name, len(least), limit)
	}
	return nil
}

// seriesNames are the names under which the series of the metric rule at
// index rule meet the value maps and the cuts of truncate: true, which go by
// the series' name (mapLabelValues, truncateLabels): a rule's own name, and
// for a prometheus rule without one, which keeps each series' own name, ""
// for the names no rule has, which nothing maps, then each name of another
// rule that the rule's expression matches, as the scrape matches it against
// the name the series was read with, before metrics_prefix and
// name_escaping, which apply after the maps and the cuts. An expression
// that does not compile matches nothing: the scrape fails that rule anyway.
func seriesNames(x *model.Collector, rule int) []string {
	r := &x.Metrics[rule]
	if r.Name != "" || r.Expression == "" || x.Transform.Type != "prometheus" {
		return []string{r.Name}
	}
	names := []string{""}
	re, err := expr.CompileRegex(r.Expression)
	if err != nil {
		return names
	}
	for i := range x.Metrics {
		if name := x.Metrics[i].Name; name != "" && !slices.Contains(names, name) && re.MatchString(name) {
			names = append(names, name)
		}
	}
	return names
}

// labelTruncatedByName says whether the label name of the series of the
// name metric is cut to the limit at the scrape whatever the rule that made
// them: some rule of that name sets truncate: true on it, which
// truncateLabels applies to every series of the name, after the value maps.
// The series of a rule without a name, "", are cut by the rule's own setting
// alone.
func labelTruncatedByName(x *model.Collector, metric, name string) bool {
	if metric == "" {
		return false
	}
	for i := range x.Metrics {
		if x.Metrics[i].Name != metric {
			continue
		}
		for _, label := range x.Metrics[i].Labels {
			if label.Name == name && label.Truncate {
				return true
			}
		}
	}
	return false
}

// valueMapAny is the value_map key for any value the map does not list, as
// mapLabelValues reads it.
const valueMapAny = "*"

// labelValueMap is the value_map that maps the label name of the series of
// the rules named metric, as mapLabelValues finds it: the first any rule of
// that name gives the label, which CheckLabelValueMapsAgree holds the others
// to. It is nil where none does.
func labelValueMap(x *model.Collector, metric, name string) map[string]string {
	for i := range x.Metrics {
		if x.Metrics[i].Name != metric {
			continue
		}
		for _, label := range x.Metrics[i].Labels {
			if label.Name == name && len(label.ValueMap) > 0 {
				return label.ValueMap
			}
		}
	}
	return nil
}
