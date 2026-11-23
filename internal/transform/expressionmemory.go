package transform

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
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
// What is live is known only as of a garbage collection, and the runtime
// starts one when the heap has doubled, by when a program that builds
// without end holds twice what it may. So a collection is asked for when
// the heap, garbage included, has grown by the limit, and what it shows
// held is what is held to the limit (heapHeld). A transform that only
// makes garbage that fast has a collection asked for once for each limit's
// worth of it, or a quarter of the limit's when what is held is close under
// the limit.
//
// Only a collection asked for here is believed. What the runtime's own last
// collection found live is of a moment before the watch began, and holds
// what has died since: a collection that was marking another transform's
// hundreds of megabytes when that transform ended reports them live, and
// what was made while it marked with them, which stopped a transform that
// held nothing.
//
// Nor does a collection say exactly what is live, so it gives two numbers
// (collectHeap): what it counts live, which is no less than what is, and
// what is certainly held, which is no more. What was held when the watch
// began is taken as the whole heap of then, garbage and all, until the
// first collection asked for counts less live than that: the least of the
// two, neither of them less than what was held. The transform is stopped
// when a collection shows certainly held more than the limit over that. So
// it is never stopped for holding less than the limit, whatever else the
// exporter is doing.
func watchExpressionMemory(ended <-chan struct{}, bound model.ByteSize, cancel context.CancelCauseFunc) {
	limit := uint64(bound) // #nosec G115 -- a limit is a size, which is not negative
	from := heapObjects()
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
		if heapObjects() <= collectAt {
			continue
		}
		heap, live, held := heapHeld()
		if !collected && live < from {
			from, most = live, live+limit
		}
		collected = true
		if held > most {
			cancel(&expressionMemoryExceeded{limit: bound})
			return
		}
		collectAt = heap + max(limit/4, most-held)
	}
}

// heapObjects is the bytes of the heap's objects, garbage not yet collected
// included, and heapHeld runs a collection and says what the heap is after
// it, what the collection counts live and what is certainly held. They are
// variables so that a test can say what a watch reads.
var (
	heapObjects = func() uint64 { return readHeap("/memory/classes/heap/objects:bytes") }
	heapHeld    = collectHeap
)

// collectHeap runs a garbage collection and says how large the heap is
// after it, how much the collection counts live, and how much of that is
// certainly held.
//
// A collection counts live what was reachable when it began and everything
// made while it marked, garbage or not. Beside a heap of some hundred
// megabytes a program that makes garbage fast had tens to hundreds of
// megabytes of it counted so, by an amount that differed from one
// collection to the next by more than a small limit, and was stopped
// holding nothing. What was made while it marked is no more than what was
// made while the collection ran, start to end, which is known: what is
// counted live less that is certainly held.
//
// That alone leaves nothing held of a program on one processor, which
// makes gigabytes while a collection waits its turn. So the collection is
// run with the heap given no room to grow (a GC percent of 0): whoever
// allocates while it runs is then made to do its marking first, and makes
// little. The percent is the process's, so one collection is run at a
// time, and each puts back what it found, after it has read what is live:
// a collection of the runtime's own that ended first would be read instead.
func collectHeap() (heap, live, held uint64) {
	collecting.Lock()
	defer collecting.Unlock()
	percent := debug.SetGCPercent(0)
	before := readHeap("/gc/heap/allocs:bytes")
	runtime.GC()
	live = readHeap("/gc/heap/live:bytes")
	made := readHeap("/gc/heap/allocs:bytes") - before
	debug.SetGCPercent(percent)
	return readHeap("/memory/classes/heap/objects:bytes"), live, live - min(made, live)
}

// collecting is held by the one collection that is being run.
var collecting sync.Mutex

// readHeap reads one of the runtime's metrics of the heap, in bytes.
// Reading it does not stop the program.
func readHeap(name string) uint64 {
	sample := [...]metrics.Sample{{Name: name}}
	metrics.Read(sample[:])
	return sample[0].Value.Uint64()
}
