package transform

import (
	"context"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A label value longer than limits.max_label_value_length fails the whole
// scrape, because exposing a truncated value nobody asked for would be a
// silent surprise. A label that carries free text, such as an incident
// update or a description, can opt in with truncate: true. It is then cut to
// fit instead, ending in "…" so a reader can tell it was cut. A limit of one
// or two bytes has no room for the mark. It also has no room for a value's
// first character when that character is longer than the limit. A value cut
// to nothing leaves the label off its series, like every label with an
// empty value (setTruncated).

const truncationMark = "…"

// truncateLabels applies truncate: true to the metrics a collector's rules
// produced. It runs on the transform's output after the labels' value maps
// (mapLabelValues), so the value cut is the one exported. It runs before
// rename_labels and the prefix, while the metrics still carry their rules'
// names and labels. A prometheus rule without a name cuts its labels itself
// (applyPrometheusTransform), where no value map applies. For a label that
// a value map maps, it leaves the cut until after the map (labelCuts).
func truncateLabels(set *model.MetricSet, c *model.Collector) {
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
				setTruncated(metric.Labels, name, value, limit)
			}
		}
	}
}

// labelCuts are the truncate: true cuts that a prometheus rule defers until
// after the labels' value maps.
//
// The prometheus transform cuts a rule's labels as it makes the series. A
// rule without a name needs this: it keeps each series' own name, so
// truncateLabels cannot find the rule by it. But a value_map of the series'
// name maps the label after the transform (mapLabelValues). A value cut
// before that was looked up already cut, and could be mapped to one that is
// too long again.
//
// So a label that a value map maps is not cut when the series is made. A
// rule with a name has it cut by truncateLabels after the map. A rule
// without a name notes the cut by the series' position in the transform's
// output, and the cut is made once the value maps have run (apply). The
// exported value is then the cut one, for every kind of rule. A label that
// no value map maps is cut when the series is made, as before.
type labelCuts struct {
	// mapped are the labels a value map maps, by the name of the series it
	// maps them on.
	mapped map[string]map[string]bool
	cuts   []labelCut
}

// labelCut is the label name of the series at index in the transform's
// output, to be cut.
type labelCut struct {
	index int
	label string
}

type labelCutsKey struct{}

// withLabelCuts returns a context in which a prometheus transform defers the
// cuts of mapped labels until after the value maps, plus the labelCuts that
// notes them. For a collector with no rule whose cut a value map can precede
// (labelCutsFor), it returns the context unchanged and nil.
func withLabelCuts(ctx context.Context, c *model.Collector) (context.Context, *labelCuts) {
	cuts := labelCutsFor(c)
	if cuts == nil {
		return ctx, nil
	}
	return context.WithValue(ctx, labelCutsKey{}, cuts), cuts
}

// labelCutsFor returns what notes the cuts for a prometheus collector whose
// rules set truncate: true on a label, under a limit, where a value_map maps
// a label. It returns nil for every other collector, which pays only for a
// look at its rules. A test replaces it with one that notes no cuts, which
// gives the transform's old behaviour.
var labelCutsFor = func(c *model.Collector) *labelCuts {
	if c.Transform.Type != "prometheus" || c.Limits.MaxLabelValueLength <= 0 {
		return nil
	}
	cut := false
	for i := range c.Metrics {
		for _, label := range c.Metrics[i].Labels {
			cut = cut || label.Truncate
		}
	}
	if !cut {
		return nil
	}
	var mapped map[string]map[string]bool
	for i := range c.Metrics {
		rule := &c.Metrics[i]
		for _, label := range rule.Labels {
			if len(label.ValueMap) == 0 || rule.Name == "" {
				continue
			}
			if mapped == nil {
				mapped = map[string]map[string]bool{}
			}
			if mapped[rule.Name] == nil {
				mapped[rule.Name] = map[string]bool{}
			}
			mapped[rule.Name][label.Name] = true
		}
	}
	if mapped == nil {
		return nil
	}
	return &labelCuts{mapped: mapped}
}

// labelCutsOf is what notes the cuts in ctx, or nil.
func labelCutsOf(ctx context.Context) *labelCuts {
	cuts, _ := ctx.Value(labelCutsKey{}).(*labelCuts)
	return cuts
}

// later reports whether the label name of a series of the name metric is
// mapped by a value map, and so cut after it.
func (l *labelCuts) later(metric, name string) bool {
	return l != nil && l.mapped[metric][name]
}

// note notes the cuts that later leaves of the labels of a rule without a
// name on the series at index.
func (l *labelCuts) note(index int, rule *model.MetricRule, metric string) {
	for _, label := range rule.Labels {
		if label.Truncate && l.later(metric, label.Name) {
			l.cuts = append(l.cuts, labelCut{index: index, label: label.Name})
		}
	}
}

// apply makes the noted cuts on set, the transform's output once the value
// maps have run. The labels it cuts are the series' own: the prometheus
// transform gave each series of a rule with labels a map of its own, and
// mapLabelValues one it copied.
func (l *labelCuts) apply(set *model.MetricSet, limit int) {
	if l == nil {
		return
	}
	for _, cut := range l.cuts {
		labels := set.Metrics[cut.index].Labels
		if value, ok := labels[cut.label]; ok && len(value) > limit {
			setTruncated(labels, cut.label, value, limit)
		}
	}
}

// setTruncated sets the label name in labels (which the caller may change)
// to value cut to limit bytes. It removes the label instead when nothing of
// the value is left: a limit under the three bytes of the mark, and a value
// whose first character is longer than that limit. To Prometheus a label
// with an empty value is no label, and none is exported as one.
func setTruncated(labels map[string]string, name, value string, limit int) {
	if cut := truncateLabelValue(value, limit); cut != "" {
		labels[name] = cut
		return
	}
	delete(labels, name)
}

// truncateLabelValue cuts a value to at most limit bytes, on a character
// boundary, with the mark counted in the limit. The limit is in bytes because
// that is what max_label_value_length measures.
func truncateLabelValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	mark := truncationMark
	if limit < len(mark) {
		mark = ""
	}
	cut := limit - len(mark)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + mark
}
