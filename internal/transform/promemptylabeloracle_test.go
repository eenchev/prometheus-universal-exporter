package transform

// The prometheus transform and the cut of a label under truncate: true as
// they were before a label cut to nothing was left off its series
// (setTruncated), kept as the oracles of the differential test
// (promemptylabel_test.go). They share with the code they are compared with
// what that did not change: the rules' checks, the series budget and
// truncateLabelValue itself. They are not to be changed with it.

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func applyPrometheusTransformAsItWas(ctx context.Context, in model.MetricSet, c *model.Collector, t model.TransformConfig, rules []model.MetricRule) (set *model.MetricSet, borrowed bool, err error) {
	if len(rules) > 0 {
		out := model.MetricSet{}
		// Each rule's pattern is compiled once, not once per series; a rule
		// whose pattern does not compile is left without one.
		patterns := make([]*regexp.Regexp, len(rules))
		for i, rule := range rules {
			pattern := rule.Expression
			if pattern == "" {
				pattern = "^" + regexp.QuoteMeta(rule.Name) + "$"
			}
			re, err := expr.CompileRegex(pattern)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, false, ruleFailure(c, rule, fmt.Errorf("metric %q expression: %w", rule.Name, err))
			}
			patterns[i] = re
		}
		matched := make([]bool, len(rules))
		// The decoder kept only the series a rule's name or pattern can
		// match (decode.Decode), so nearly every one of them becomes a
		// series here.
		out.Metrics = growSeries(ctx, out.Metrics, len(in.Metrics))
		for s := range in.Metrics {
			source := &in.Metrics[s]
			for i := range rules {
				rule := &rules[i]
				if patterns[i] == nil || !patterns[i].MatchString(source.Name) {
					continue
				}
				matched[i] = true
				metric := *source
				if rule.Name != "" {
					metric.Name = rule.Name
				}
				if rule.Description != "" {
					metric.Help = rule.Description
				}
				if rule.Type != "" && rule.Type != metric.Type {
					// A histogram's or summary's type is its shape: it
					// cannot be exported as another, nor another as one.
					if metric.Histogram != nil || metric.Summary != nil || rule.Type == model.HistogramMetricType || rule.Type == model.SummaryMetricType {
						err := fmt.Errorf("metric %q type %s cannot apply to %s, a %s: a histogram or summary keeps its own type, and no other series can become one", rule.Name, rule.Type, source.Name, source.Type)
						if handleMetricError(ctx, c, *rule, err) {
							continue
						}
						return nil, false, ruleFailure(c, *rule, err)
					}
					metric.Type = rule.Type
				}
				if rule.Scale != nil {
					if metric.Histogram != nil || metric.Summary != nil {
						err := fmt.Errorf("metric %q scale cannot apply to %s, a %s: its buckets and quantiles are bounds as well as counts", rule.Name, source.Name, source.Type)
						if handleMetricError(ctx, c, *rule, err) {
							continue
						}
						return nil, false, ruleFailure(c, *rule, err)
					}
					metric.Value = scaled(*rule, metric.Value)
				}
				switch {
				case len(rule.Labels) > 0:
					// The series gets labels of its own, with room for the
					// rule's: the rule adds and removes them, and another
					// rule may match the same source metric.
					metric.Labels = make(map[string]string, len(source.Labels)+len(rule.Labels))
					for name, value := range source.Labels {
						metric.Labels[name] = value
					}
				case source.Labels == nil:
					// Never nil, as the labels of a rule's series never
					// were.
					metric.Labels = map[string]string{}
				}
				for _, label := range rule.Labels {
					if label.Static() {
						metric.Labels[label.Name] = label.Value
					} else if value, ok := metric.Labels[label.Expression]; ok {
						metric.Labels[label.Name] = value
					}
					// A rule without a name keeps each series' own, which
					// truncateLabels cannot look it up by, so its labels are
					// cut here.
					if label.Truncate && c.Limits.MaxLabelValueLength > 0 {
						if value, ok := metric.Labels[label.Name]; ok {
							metric.Labels[label.Name] = truncateLabelValue(value, c.Limits.MaxLabelValueLength)
						}
					}
				}
				if len(rule.Labels) > 0 {
					if missing := missingRequiredLabel(*rule, metric.Labels); missing != nil {
						if handleMetricError(ctx, c, *rule, missing) {
							continue
						}
						return nil, false, ruleFailure(c, *rule, missing)
					}
				}
				if err := takeSeries(ctx); err != nil {
					return nil, false, err
				}
				out.Metrics = append(out.Metrics, metric)
			}
		}
		// A rule no series' name matched has no value, as a regex that
		// matched no text has none: a required rule is missing its value,
		// and its error mode decides, rather than the scrape passing
		// without the metric it was written for.
		for i, rule := range rules {
			if patterns[i] == nil || matched[i] || !requiredRule(rule, c) {
				continue
			}
			missing := model.MarkError(prometheusRuleUnmatched(rule), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return nil, false, ruleFailure(c, rule, missing)
		}
		return noSeriesIsNil(&out), false, nil
	}
	if len(t.Include) == 0 && len(t.Exclude) == 0 && len(t.Rename) == 0 {
		// Counted at once, with the error the first series past the limit
		// would have had.
		if err := takeSeriesN(ctx, len(in.Metrics)); err != nil {
			return nil, false, err
		}
		switch {
		case len(in.Metrics) == 0:
			return &model.MetricSet{}, false, nil
		case passesThrough(c):
			return &model.MetricSet{Metrics: in.Metrics}, true, nil
		}
		return &model.MetricSet{Metrics: slices.Clone(in.Metrics)}, false, nil
	}
	out := model.MetricSet{}
	includes := make([]*regexp.Regexp, 0, len(t.Include))
	for _, expression := range t.Include {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, false, err
		}
		includes = append(includes, re)
	}
	excludes := make([]*regexp.Regexp, 0, len(t.Exclude))
	for _, expression := range t.Exclude {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, false, err
		}
		excludes = append(excludes, re)
	}
	// The decoder kept only the series include and exclude pass
	// (decode.Decode), so nearly every one of them is a series here.
	out.Metrics = growSeries(ctx, out.Metrics, len(in.Metrics))
	for i := range in.Metrics {
		name := in.Metrics[i].Name
		included := len(includes) == 0
		for _, expression := range includes {
			if expression.MatchString(name) {
				included = true
			}
		}
		for _, expression := range excludes {
			if expression.MatchString(name) {
				included = false
			}
		}
		if !included {
			continue
		}
		if err := takeSeries(ctx); err != nil {
			return nil, false, err
		}
		out.Metrics = append(out.Metrics, in.Metrics[i])
		if renamed, ok := t.Rename[name]; ok {
			out.Metrics[len(out.Metrics)-1].Name = renamed
		}
	}
	return noSeriesIsNil(&out), false, nil
}

func truncateLabelsAsItWas(set *model.MetricSet, c *model.Collector) {
	limit := c.Limits.MaxLabelValueLength
	if limit <= 0 {
		return
	}
	var byMetric map[string][]string
	for _, rule := range c.Metrics {
		for _, label := range rule.Labels {
			if label.Truncate && rule.Name != "" {
				if byMetric == nil {
					byMetric = map[string][]string{}
				}
				byMetric[rule.Name] = append(byMetric[rule.Name], label.Name)
			}
		}
	}
	if byMetric == nil {
		return
	}
	for i := range set.Metrics {
		metric := &set.Metrics[i]
		copied := false
		for _, name := range byMetric[metric.Name] {
			if value, ok := metric.Labels[name]; ok && len(value) > limit {
				// Copied before the first cut, since series may share
				// their labels (applyPrometheusTransform).
				if !copied {
					metric.Labels, copied = model.CloneLabels(metric.Labels), true
				}
				metric.Labels[name] = truncateLabelValue(value, limit)
			}
		}
	}
}
