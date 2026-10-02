package transform

import (
	"context"
	"slices"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// limits.max_metrics bounds the series a scrape may produce, and it bounds
// the memory a scrape takes as well: every transform stops at the first
// series past the limit, with the error MetricSet.Validate gives a set that
// is too large (model.MetricCountError), rather than building every series
// the response describes and counting them afterwards. A 10 MiB body of
// "1\n" read with the regex (\d+) is five million series, several gigabytes
// of them, before a count afterwards could refuse it.
//
// The limit travels in the Transform's context, with the count of series made
// so far, so it spans every rule of the collector, as the limit does.

type seriesBudgetKey struct{}

// seriesBudget is how many series a Transform may still make.
type seriesBudget struct {
	limit, made int
}

// withSeriesBudget returns a context whose transforms may make at most limit
// series; none or a negative limit means no bound.
func withSeriesBudget(ctx context.Context, limit int) context.Context {
	if limit <= 0 {
		return ctx
	}
	return context.WithValue(ctx, seriesBudgetKey{}, &seriesBudget{limit: limit})
}

// takeSeries counts one more series, and fails once the count is past the
// limit. A transform calls it before it adds a series, and stops on its error.
func takeSeries(ctx context.Context) error {
	budget, ok := ctx.Value(seriesBudgetKey{}).(*seriesBudget)
	if !ok {
		return nil
	}
	budget.made++
	if budget.made > budget.limit {
		return model.MetricCountError(budget.made, budget.limit)
	}
	return nil
}

// takeSeriesN counts n more series at once, as takeSeries does one.
func takeSeriesN(ctx context.Context, n int) error {
	budget, ok := ctx.Value(seriesBudgetKey{}).(*seriesBudget)
	if !ok {
		return nil
	}
	if budget.made+n > budget.limit {
		budget.made = budget.limit + 1
		return model.MetricCountError(budget.made, budget.limit)
	}
	budget.made += n
	return nil
}

// releaseSeries gives back n series taken for a rule that then dropped them.
func releaseSeries(ctx context.Context, n int) {
	if budget, ok := ctx.Value(seriesBudgetKey{}).(*seriesBudget); ok {
		budget.made -= n
	}
}

// seriesRoom is how many more series the context's budget allows, or -1 when
// there is no bound.
func seriesRoom(ctx context.Context) int {
	budget, ok := ctx.Value(seriesBudgetKey{}).(*seriesBudget)
	if !ok {
		return -1
	}
	return max(budget.limit-budget.made, 0)
}

// growSeries makes room in metrics for the series of n more things a rule is
// about to read — items, rows, nodes, matches, a response's own series —
// each of which makes one series at most: the slice is then made once, at
// the size it ends with, instead of growing from nothing, which copies every
// series made so far each time it fills and allocates several times what it
// ends up holding. The room is never more than limits.max_metrics still
// leaves, since a transform stops at the first series past that: a response
// that describes more series than the limit allows gets no more memory for
// them than the limit does.
func growSeries(ctx context.Context, metrics []model.Metric, n int) []model.Metric {
	if room := seriesRoom(ctx); room >= 0 && room < n {
		n = room
	}
	return slices.Grow(metrics, n)
}

// noSeriesIsNil gives a set that holds no series no slice of them either, as
// a transform that never added one always left it, whatever room was made.
func noSeriesIsNil(set *model.MetricSet) *model.MetricSet {
	if len(set.Metrics) == 0 {
		set.Metrics = nil
	}
	return set
}
