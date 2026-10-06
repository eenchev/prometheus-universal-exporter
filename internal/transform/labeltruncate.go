package transform

import (
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A label value longer than limits.max_label_value_length fails the whole
// scrape: exposing a truncated value nobody asked for would be a silent
// surprise. A label that carries free text — an incident update, a
// description — can opt in with truncate: true, and is then cut to fit
// instead, ending in "…" so a reader can tell it was cut. A limit of one or
// two bytes has no room for the mark, and none for a value's first character
// when that is longer than the limit: a value cut to nothing leaves the
// label off its series, as every label with an empty value is left off
// (setTruncated).

const truncationMark = "…"

// truncateLabels applies truncate: true to the metrics a collector's rules
// produced. It runs on the transform's output, before rename_labels and the
// prefix, while the metrics still carry their rules' names and labels. A
// prometheus rule without a name cuts its labels itself
// (applyPrometheusTransform).
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

// setTruncated gives labels, which are the caller's to change, the label
// name with value cut to limit bytes, or takes the label off them when
// nothing of the value is left: a limit under the three bytes of the mark
// and a value whose first character is longer than it. To Prometheus a
// label with an empty value is no label, and none is exported as one.
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
