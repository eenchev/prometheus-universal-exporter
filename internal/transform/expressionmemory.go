package transform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// transformJQWithinMemory is transformJQ, stopped when it takes more memory
// than the collector's limit (expr.WatchMemory): its context is then
// cancelled, which stops a jq program as a deadline does and lets no rule
// carry on (handleMetricError), and what the transform failed with is the
// limit's failure, at the rule it was at.
//
// For a caller that asked (WithMemoryMeasured), what the transform held at
// its most is measured and written to the rule logger, beside the limit.
func transformJQWithinMemory(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	var measure *expr.MemoryMeasure
	if memoryMeasured(ctx) {
		ctx, measure = expr.WithMemoryMeasure(ctx)
	}
	watched, ended := expr.WatchMemory(ctx, c.Limits.MaxExpressionMemory)
	set, err := transformJQ(watched, data, rules, c)
	exceeded := ended()
	if measure != nil {
		ruleLogger(ctx).Debug(c.Transform.Type+" transform memory", "collector", c.Name, "held_bytes", measure.Most(), "limit_bytes", int64(c.Limits.MaxExpressionMemory))
	}
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

type memoryMeasuredKey struct{}

// WithMemoryMeasured returns a context whose Transforms measure the memory
// they take and write it to the rule logger at debug level, for someone
// choosing a limit: what a jq or yq transform's expressions held at their
// most, beside limits.max_expression_memory, and the most address space
// the worker of a Python script has had, beside limits.max_script_memory.
// The first costs garbage collections of the whole exporter, so it is for
// a debug probe and not for a scrape.
func WithMemoryMeasured(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoryMeasuredKey{}, true)
}

func memoryMeasured(ctx context.Context) bool {
	measured, _ := ctx.Value(memoryMeasuredKey{}).(bool)
	return measured
}

// workerPeakBytes is the most address space the process pid has had, which
// is what limits.max_script_memory bounds of a worker, and 0 where there is
// no /proc to read it from: where the limit is not enforced either.
func workerPeakBytes(pid int) int64 {
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for line := range bytes.SplitSeq(status, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("VmPeak:")); ok {
			if fields := bytes.Fields(value); len(fields) > 0 {
				kb, _ := strconv.ParseInt(string(fields[0]), 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}
