package transform

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A rule with a histogram block (model.HistogramRule) reads its values as
// every rule of its transform does, with the same expressions, items,
// value_map, scale and error_mode, and counts each in a histogram instead of
// making it a series: the values of one set of labels make one histogram, so
// a rule whose labels are static makes one, and one whose labels read the
// response one per distinct set of their values. Only a histogram is a
// series, taken from limits.max_metrics when its first value comes, so the
// values a response holds cost a count each, however many there are; how
// many there can be is bounded by the size of the response.
//
// The histogram describes the values of this response: it is made anew on
// every scrape, and counts nothing of the scrape before, so its buckets go
// down as well as up when the target's list changes.

// histogramFoldsKey is the context key of a Transform's histogramFolds.
type histogramFoldsKey struct{}

// histogramFolds are the histograms each histogram rule has made so far in
// one Transform, found by the rule's block, which is the rule's own: each
// rule of a configuration has its own block, and a Transform reads each rule
// once.
type histogramFolds map[*model.HistogramRule]*histogramFold

// histogramFold is where in out the histograms of one rule are, by the key
// of their labels (histogramKey).
type histogramFold struct {
	out *model.MetricSet
	at  map[string]int
}

// withHistogramFolds returns a context in which the histogram rules of c
// find the histograms they have made so far: ctx itself for a collector
// without one, whose scrape allocates nothing for them.
func withHistogramFolds(ctx context.Context, c *model.Collector) context.Context {
	for i := range c.Metrics {
		if c.Metrics[i].Histogram != nil {
			return context.WithValue(ctx, histogramFoldsKey{}, histogramFolds{})
		}
	}
	return ctx
}

// observeValue counts value, which rule, a histogram rule, read with labels,
// in the histogram of those labels in out, making that histogram when it is
// the first value of its labels. Its error is the scrape's failure: a
// histogram past limits.max_metrics. A value no histogram can count, NaN,
// never comes here (histogramValue).
func observeValue(ctx context.Context, out *model.MetricSet, rule *model.MetricRule, value float64, labels map[string]string) error {
	key := histogramKey(labels)
	folds, _ := ctx.Value(histogramFoldsKey{}).(histogramFolds)
	fold := folds[rule.Histogram]
	if folds != nil && (fold == nil || fold.out != out) {
		fold = &histogramFold{out: out, at: map[string]int{}}
		folds[rule.Histogram] = fold
	}
	at, found := findHistogram(out, rule, fold, key)
	if !found {
		if err := takeSeries(ctx); err != nil {
			return err
		}
		out.Metrics = append(out.Metrics, newHistogramSeries(rule, labels))
		at = len(out.Metrics) - 1
		if fold != nil {
			fold.at[key] = at
		}
	}
	observe(out.Metrics[at].Histogram, rule.Histogram.Buckets, value)
	return nil
}

// findHistogram is where in out the histogram of rule with the labels of key
// is. A Transform has the fold of the rule; without one, as a test that
// calls a transform's own function has, the series are looked through. A
// place the fold has that no longer holds a histogram of the rule, its
// series having been dropped, is none.
func findHistogram(out *model.MetricSet, rule *model.MetricRule, fold *histogramFold, key string) (int, bool) {
	holds := func(at int) bool {
		m := &out.Metrics[at]
		return m.Histogram != nil && m.Name == rule.Name && histogramKey(m.Labels) == key
	}
	if fold != nil {
		at, ok := fold.at[key]
		return at, ok && at < len(out.Metrics) && holds(at)
	}
	for at := len(out.Metrics) - 1; at >= 0; at-- {
		if holds(at) {
			return at, true
		}
	}
	return 0, false
}

// histogramKey is the key of a set of labels, the same for two sets that are
// one series to Prometheus: in the order of their names, without a label of
// an empty value, which is no label at all.
func histogramKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name, value := range labels {
		if value != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		// Neither byte is in a label name, and a value is written after
		// its name's, so no two sets have one key.
		b.WriteString(name)
		b.WriteByte(0xff)
		b.WriteString(labels[name])
		b.WriteByte(0xfe)
	}
	return b.String()
}

// newHistogramSeries is a histogram of rule with labels that has counted
// nothing: a bucket of each of the rule's bounds and the bucket +Inf, all of
// them 0, and a sum and a count of 0.
func newHistogramSeries(rule *model.MetricRule, labels map[string]string) model.Metric {
	buckets := make([]model.Bucket, 0, len(rule.Histogram.Buckets)+1)
	for _, bound := range rule.Histogram.Buckets {
		buckets = append(buckets, model.Bucket{UpperBound: bound})
	}
	buckets = append(buckets, model.Bucket{UpperBound: math.Inf(1)})
	return model.Metric{Name: rule.Name, Help: rule.Description, Type: model.HistogramMetricType, Labels: labels, Histogram: &model.Histogram{Buckets: buckets}}
}

// observe counts value in h, whose buckets are bounds and +Inf: in every
// bucket whose bound it is at most, as the buckets are cumulative, in the
// count, and in the sum.
func observe(h *model.Histogram, bounds []float64, value float64) {
	first, _ := slices.BinarySearch(bounds, value)
	for i := first; i < len(h.Buckets); i++ {
		h.Buckets[i].CumulativeCount++
	}
	h.Count++
	h.Sum += value
}

// emptyHistogram adds the histogram of a rule that read no value at all,
// which counts nothing, so that the series is there whether or not the
// response's list has anything in it. Only a rule whose labels are all
// static has one: one that reads a label from the response has no labels
// to give a histogram of no value.
func emptyHistogram(ctx context.Context, out *model.MetricSet, rule *model.MetricRule) error {
	labels := make(map[string]string, len(rule.Labels))
	for _, label := range rule.Labels {
		if !label.Static() {
			return nil
		}
		labels[label.Name] = label.Value
	}
	if err := takeSeries(ctx); err != nil {
		return err
	}
	out.Metrics = append(out.Metrics, newHistogramSeries(rule, labels))
	return nil
}

// growRuleSeries is growSeries for a rule about to read n things: a
// histogram rule's values are not series, so there is no room to make.
func growRuleSeries(ctx context.Context, metrics []model.Metric, rule *model.MetricRule, n int) []model.Metric {
	if rule.Histogram != nil {
		return metrics
	}
	return growSeries(ctx, metrics, n)
}
