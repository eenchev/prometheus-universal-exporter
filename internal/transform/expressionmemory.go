package transform

import (
	"context"
	"errors"
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// transformJQWithinMemory is transformJQ, stopped when it takes more memory
// than the collector's limit (expr.WatchMemory): its context is then
// cancelled, which stops a jq program as a deadline does and lets no rule
// carry on (handleMetricError), and what the transform failed with is the
// limit's failure, at the rule it was at.
func transformJQWithinMemory(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	ctx, ended := expr.WatchMemory(ctx, c.Limits.MaxExpressionMemory)
	set, err := transformJQ(ctx, data, rules, c)
	exceeded := ended()
	if err == nil || exceeded == nil {
		return set, err
	}
	at := ""
	var failure *MetricFailure
	if errors.As(err, &failure) && failure.Metric != "" {
		at = fmt.Sprintf(" at metric %q", failure.Metric)
	}
	return nil, model.MarkError(fmt.Errorf("the transform was stopped%s: %w", at, exceeded), model.ErrLimitExceeded)
}
