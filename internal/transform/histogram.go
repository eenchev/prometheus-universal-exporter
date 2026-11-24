package transform

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"sort"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A rule with a histogram block (model.HistogramRule) reads its values like
// every rule of its transform, with the same expressions, items, value_map,
// scale and error_mode. It counts each value in a histogram instead of
// making it a series. The values of one set of labels make one histogram. A
// rule with static labels makes one histogram; a rule whose labels read the
// response makes one per distinct set of label values. Only a histogram is a
// series, taken from limits.max_metrics when its first value arrives. So
// each value in a response costs only a count, however many there are, and
// their number is bounded by the size of the response.
//
// The histogram describes the values of this response. It is made anew on
// every scrape and counts nothing from the previous scrape, so its buckets
// can go down as well as up when the target's list changes.

// histogramFoldsKey is the context key of a Transform's histogramFolds.
type histogramFoldsKey struct{}

// histogramFolds are the histograms each histogram rule has made so far in
// one Transform, keyed by the rule's block. The block is unique to the rule:
// each rule of a configuration has its own block, and a Transform reads each
// rule once.
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

// observeValue counts value, read by the histogram rule rule with labels, in
// the histogram for those labels in out. It creates that histogram for the
// first value of its labels. Its error is the scrape's failure: a histogram
// past limits.max_metrics. NaN, which no histogram can count, never reaches
// here (histogramValue).
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

// findHistogram returns the position in out of the histogram of rule with
// the labels of key. In a Transform the rule's fold is used. Without one,
// as in a test that calls a transform's own function, the series are
// searched. A position in the fold that no longer holds a histogram of the
// rule, because its series was dropped, counts as not found.
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

// histogramKey is the key of a set of labels. Two sets that are one series
// to Prometheus get the same key: labels are ordered by name, and a label
// with an empty value, which is no label at all, is left out. Each name and
// value is written after its length, so no two sets share a key whatever
// bytes they hold. This matters because a value read from a response may
// hold any byte (non-UTF-8 is repaired only after the transform), so no
// byte can serve as a terminator.
func histogramKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	size := 0
	for name, value := range labels {
		if value != "" {
			names = append(names, name)
			size += len(name) + len(value) + 2*binary.MaxVarintLen64
		}
	}
	sort.Strings(names)
	b := make([]byte, 0, size)
	for _, name := range names {
		b = appendKeyPart(b, name)
		b = appendKeyPart(b, labels[name])
	}
	return string(b)
}

// appendKeyPart writes part to a key after its length.
func appendKeyPart(dst []byte, part string) []byte {
	return append(binary.AppendUvarint(dst, uint64(len(part))), part...)
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

// emptyHistogram adds the histogram of a rule that read no value at all. It
// counts nothing, but the series is present whether or not the response's
// list has anything in it. Only a rule whose labels are all static has one.
// A rule that reads a label from the response has no labels to give a
// histogram with no values.
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

// growItemsSeries makes room for the series of rule, which is about to read
// n items, and of every later rule over the same items, which reads the
// same n (jqSharing). Two rules over one list of items, such as a value and
// a size for each, are the most common shape of a jq collector. Room made
// for the first rule's series alone was made again for the second, at twice
// the size, with a copy. Each of those rules makes room for its own series
// as it begins, and finds it already made. growSeries bounds the room to
// what limits.max_metrics leaves.
func growItemsSeries(ctx context.Context, metrics []model.Metric, rule *model.MetricRule, later []model.MetricRule, n int) []model.Metric {
	rules := 0
	if rule.Histogram == nil {
		rules++
	}
	for i := range later {
		if later[i].Items == rule.Items && later[i].Histogram == nil {
			rules++
		}
	}
	if rules == 0 {
		return metrics
	}
	return growSeries(ctx, metrics, n*rules)
}
