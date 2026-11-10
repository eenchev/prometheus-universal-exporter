package transform

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/metrics"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A jq or yq program is stopped when its probe's time is spent, and time
// does not bound memory: [range(40000000)] holds gigabytes within a second,
// and a definition that calls itself without end grows until the kernel
// kills the exporter, and every other collector's scrapes with it. So the
// memory a transform may take is bounded by limits.max_expression_memory.
//
// Go counts no memory by goroutine, so what is measured is the heap of the
// whole process: the limit is on what the heap's live objects grow by while
// the transform runs. A transform is not looked at before
// expressionMemoryLook has passed, which nearly every one ends within, so it
// costs them a timer that never fires; one that runs longer is looked at
// that often until it ends.

// expressionMemoryLook is how long a transform runs before its memory is
// first looked at, and between one look and the next.
const expressionMemoryLook = 20 * time.Millisecond

// expressionMemoryExceeded is the cause a transform's context is cancelled
// with when it is stopped for its memory.
type expressionMemoryExceeded struct{ limit model.ByteSize }

func (e *expressionMemoryExceeded) Error() string {
	return "the transform took the exporter's memory past limits.max_expression_memory"
}

// transformJQWithinMemory is transformJQ, stopped when it takes more memory
// than the collector's limit: its context is then cancelled, which stops a
// jq program as a deadline does and lets no rule carry on (handleMetricError),
// and what the transform failed with is the limit's failure, at the rule it
// was at.
func transformJQWithinMemory(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	limit := c.Limits.MaxExpressionMemory
	if limit <= 0 {
		return transformJQ(ctx, data, rules, c)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	ended := make(chan struct{})
	timer := time.AfterFunc(expressionMemoryLook, func() {
		watchExpressionMemory(ended, limit, cancel)
	})
	set, err := transformJQ(ctx, data, rules, c)
	timer.Stop()
	close(ended)
	var exceeded *expressionMemoryExceeded
	stopped := err != nil && errors.As(context.Cause(ctx), &exceeded)
	cancel(nil)
	if !stopped {
		return set, err
	}
	at := ""
	var failure *MetricFailure
	if errors.As(err, &failure) && failure.Metric != "" {
		at = fmt.Sprintf(" at metric %q", failure.Metric)
	}
	return nil, model.MarkError(fmt.Errorf("the transform was stopped%s: the exporter's memory grew by more than limits.max_expression_memory, %d bytes, while it ran; make the expression keep less at once, or raise the limit", at, exceeded.limit), model.ErrLimitExceeded)
}

// watchExpressionMemory cancels a transform, with expressionMemoryExceeded,
// once the heap's live objects are limit more than they were when the watch
// began, and returns when the transform has ended.
//
// What is live is known only as of the last garbage collection, and the
// runtime starts one when the heap has doubled, by when a program that
// builds without end holds twice what it may. So a collection is asked for
// when the heap, garbage included, has grown by the limit, and what it
// leaves live is what is held to the limit. A transform that only makes
// garbage that fast has a collection asked for once for each limit's worth
// of it, or a quarter of the limit's when what is live is close under the
// limit: no more often than the runtime collects a heap of that size
// itself.
//
// What was live when the watch began is not known either: the heap then
// held garbage too. It is taken as the whole heap of then, until the first
// collection asked for leaves less than that live, which is then what the
// growth is counted from. The transform had made at most the limit by that
// collection, so it is stopped holding between the limit and twice it, and
// never for holding less.
func watchExpressionMemory(ended <-chan struct{}, bound model.ByteSize, cancel context.CancelCauseFunc) {
	limit := uint64(bound) // #nosec G115 -- a limit is a size, which is not negative
	from, _ := heapBytes()
	most := from + limit
	collectAt, collected := most, false
	ticker := time.NewTicker(expressionMemoryLook)
	defer ticker.Stop()
	for {
		select {
		case <-ended:
			return
		case <-ticker.C:
		}
		heap, live := heapBytes()
		if live <= most && heap > collectAt {
			runtime.GC()
			heap, live = heapBytes()
			if !collected && live < from {
				from, most = live, live+limit
			}
			collected = true
			collectAt = heap + max(limit/4, most-min(live, most))
		}
		if live > most {
			cancel(&expressionMemoryExceeded{limit: bound})
			return
		}
	}
}

// heapBytes are the bytes of the heap's objects, garbage not yet collected
// among them, and of those the last garbage collection found live. Reading
// them does not stop the program.
func heapBytes() (heap, live uint64) {
	samples := [...]metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/gc/heap/live:bytes"}}
	metrics.Read(samples[:])
	return samples[0].Value.Uint64(), samples[1].Value.Uint64()
}
