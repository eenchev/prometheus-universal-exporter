package expr

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
// memory a program may take is bounded by limits.max_expression_memory:
// that of a transform's, and that of the expressions a pagination and a
// login run on a target's answers.
//
// Go counts no memory by goroutine, so what is measured is the heap of the
// whole process: the limit is on what the heap's live objects grow by while
// the program runs. A program is not looked at before memoryLook has
// passed, which nearly every one ends within, so it costs them a timer that
// never fires; one that runs longer is looked at that often until it ends.

// memoryLook is how long a program runs before its memory is first looked
// at, and between one look and the next.
const memoryLook = 20 * time.Millisecond

// MemoryExceeded is the cause a program's context is cancelled with when
// it is stopped for its memory, and says what it was stopped for.
type MemoryExceeded struct{ Limit model.ByteSize }

func (e *MemoryExceeded) Error() string {
	return fmt.Sprintf("the exporter's memory grew by more than limits.max_expression_memory, %d bytes, while it ran; make the expression keep less at once, or raise the limit", e.Limit)
}

// WatchMemory gives the context a jq program is to run with so that it is
// stopped when it takes more memory than limit, as a deadline stops it, and
// the function to call when the program has ended, which says whether it
// was stopped so and for what. A limit of 0 or less is no limit.
func WatchMemory(ctx context.Context, limit model.ByteSize) (context.Context, func() *MemoryExceeded) {
	if limit <= 0 {
		return ctx, func() *MemoryExceeded { return nil }
	}
	ctx, cancel := context.WithCancelCause(ctx)
	ended := make(chan struct{})
	timer := time.AfterFunc(memoryLook, func() { watchMemory(ended, limit, cancel) })
	return ctx, func() *MemoryExceeded {
		timer.Stop()
		close(ended)
		var exceeded *MemoryExceeded
		errors.As(context.Cause(ctx), &exceeded)
		cancel(nil)
		return exceeded
	}
}

// memoryLimitKey is the key a context holds its memory limit under.
type memoryLimitKey struct{}

// WithMemoryLimit is ctx saying what the jq programs run with it may take
// (WatchMemoryOf): the collector's limit, for the expressions that run
// where the collector is not at hand.
func WithMemoryLimit(ctx context.Context, limit model.ByteSize) context.Context {
	return context.WithValue(ctx, memoryLimitKey{}, limit)
}

// WatchMemoryOf is WatchMemory with the limit ctx was given, and with none
// when it was given none.
func WatchMemoryOf(ctx context.Context) (context.Context, func() *MemoryExceeded) {
	limit, _ := ctx.Value(memoryLimitKey{}).(model.ByteSize)
	return WatchMemory(ctx, limit)
}

// watchMemory cancels a program, with MemoryExceeded,
// once the heap's live objects are limit more than they were when the watch
// began, and returns when the program has ended.
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
//
// A program that doubles a text at every step is the one that shows
// nothing certainly held: each text is as large as all before it, so what
// is made while a collection runs is as much as it counts live. It is
// known by another sign, that the collection counts live more than the
// limit over what was held and that a single value larger than the whole
// limit was made while it ran (largest), and is stopped for that: at the
// first collection, where it otherwise ran on until its steps took longer
// than a collection does, holding a hundred times a small limit by then.
func watchMemory(ended <-chan struct{}, bound model.ByteSize, cancel context.CancelCauseFunc) {
	limit := uint64(bound) // #nosec G115 -- a limit is a size, which is not negative
	from := heapObjects()
	most := from + limit
	collectAt, collected := most, false
	ticker := time.NewTicker(memoryLook)
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
		found := heapHeld()
		if !collected && found.live < from {
			from, most = found.live, found.live+limit
		}
		collected = true
		if found.held > most || found.live > most && found.largest > limit {
			cancel(&MemoryExceeded{Limit: bound})
			return
		}
		collectAt = found.heap + max(limit/4, most-found.held)
	}
}

// heapObjects is the bytes of the heap's objects, garbage not yet collected
// included, and heapHeld runs a collection and says what it found. They
// are variables so that a test can say what a watch reads.
var (
	heapObjects = func() uint64 { return readHeap("/memory/classes/heap/objects:bytes") }
	heapHeld    = collectHeap
)

// collection is what a garbage collection asked for found: how large the
// heap is after it, how much it counts live, how much of that is certainly
// held, and the least that the largest single value made while it ran can
// have been, 0 when no large one was made.
type collection struct{ heap, live, held, largest uint64 }

// collectHeap runs a garbage collection and says what it found.
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
//
// The largest value made is not told, but the runtime counts the values
// made by size (allocatedBySize): what was made, less the most that the
// small ones can have come to, is the least the large ones came to, and
// that over their number is the least the largest of them was.
func collectHeap() collection {
	collecting.Lock()
	defer collecting.Unlock()
	percent := debug.SetGCPercent(0)
	small, large := allocatedBySize()
	before := readHeap("/gc/heap/allocs:bytes")
	runtime.GC()
	live := readHeap("/gc/heap/live:bytes")
	made := readHeap("/gc/heap/allocs:bytes") - before
	smallNow, largeNow := allocatedBySize()
	debug.SetGCPercent(percent)
	found := collection{heap: readHeap("/memory/classes/heap/objects:bytes"), live: live, held: live - min(made, live)}
	if count := largeNow - large; count > 0 && made > smallNow-small {
		found.largest = (made - (smallNow - small)) / count
	}
	return found
}

// allocatedBySize says, of every value the process has made, the most
// that those of the runtime's size classes can have come to in bytes, and
// how many were larger than any class. It is read before the bytes made
// are and after them, so that the small ones are counted over a longer
// while than the bytes, and never for less than they were.
func allocatedBySize() (small, large uint64) {
	sample := [...]metrics.Sample{{Name: "/gc/heap/allocs-by-size:bytes"}}
	metrics.Read(sample[:])
	sizes := sample[0].Value.Float64Histogram()
	last := len(sizes.Counts) - 1
	for i, count := range sizes.Counts[:last] {
		small += count * uint64(sizes.Buckets[i+1])
	}
	return small, sizes.Counts[last]
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
