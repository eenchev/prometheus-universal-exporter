package expr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/itchyny/gojq"
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
//
// The function must be called, on every way out: until it is, the watch
// looks at the heap every memoryLook and asks for collections. It may be
// called more than once, and says each time what it said first.
func WatchMemory(ctx context.Context, limit model.ByteSize) (context.Context, func() *MemoryExceeded) {
	measure, _ := ctx.Value(memoryMeasureKey{}).(*MemoryMeasure)
	var before uint64
	if measure != nil {
		before = heapHeld().held
	}
	if limit <= 0 {
		return ctx, func() *MemoryExceeded {
			measure.grew(heapHeld, before)
			return nil
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	// One value past the limit is past it whatever else is held, and is
	// known when it is made, without a look at the heap (see below).
	ctx = gojq.WithMaxValueBytes(ctx, int(min(limit, math.MaxInt)), func() { cancel(&MemoryExceeded{Limit: limit}) })
	ended := make(chan struct{})
	timer := time.AfterFunc(memoryLook, func() { watchMemory(ended, limit, cancel, measure, before) })
	return ctx, sync.OnceValue(func() *MemoryExceeded {
		timer.Stop()
		close(ended)
		measure.grew(heapHeld, before)
		var exceeded *MemoryExceeded
		errors.As(context.Cause(ctx), &exceeded)
		cancel(nil)
		return exceeded
	})
}

// MemoryMeasure is how much the programs run with a context held at their
// most, for whoever asked (WithMemoryMeasure): a debug probe, which says it
// to someone choosing limits.max_expression_memory.
//
// It is what the watch measures, the growth of the whole process's heap
// while a program ran, so what other scrapes took meanwhile is in it; and
// it is the most a collection counted live over what was certainly held
// before the program began, which is no less than what the program held
// when the collection ran. A collection is run before the program, at
// every look while it runs, 20 milliseconds apart, and after it: more than
// a scrape is worth, and what a debug probe is for. What a program held
// and let go of between two looks is not seen.
type MemoryMeasure struct{ most atomic.Uint64 }

type memoryMeasureKey struct{}

// WithMemoryMeasure is ctx with the memory of the programs run with it
// measured, and the measure.
func WithMemoryMeasure(ctx context.Context) (context.Context, *MemoryMeasure) {
	measure := &MemoryMeasure{}
	return context.WithValue(ctx, memoryMeasureKey{}, measure), measure
}

// Most is the most the programs held, in bytes over what was held before
// each began.
func (m *MemoryMeasure) Most() uint64 { return m.most.Load() }

// grew runs a collection and keeps what it counts live over before, where
// that is more than was kept. A nil measure measures nothing.
func (m *MemoryMeasure) grew(collect func() collection, before uint64) {
	if m == nil {
		return
	}
	m.saw(collect().live, before)
}

// saw keeps live over before where that is more than was kept.
func (m *MemoryMeasure) saw(live, before uint64) {
	if m == nil || live <= before {
		return
	}
	for {
		most := m.most.Load()
		if live-before <= most || m.most.CompareAndSwap(most, live-before) {
			return
		}
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
// is made while a collection runs is as much as it counts live, and it ran
// on until its steps took longer than a collection does, holding a hundred
// times a small limit by then. It is not left to the collections: one
// value larger than the whole limit is past it whatever else is held, and
// the interpreter says so when a function gives one (WatchMemory), which
// stops the program before its next step.
//
// With a measure, a collection is asked for at every look, and what it
// counts live over before is kept (MemoryMeasure).
func watchMemory(ended <-chan struct{}, bound model.ByteSize, cancel context.CancelCauseFunc, measure *MemoryMeasure, before uint64) {
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
		if measure == nil && heapObjects() <= collectAt {
			continue
		}
		found := heapHeld()
		measure.saw(found.live, before)
		if !collected && found.live < from {
			from, most = found.live, found.live+limit
		}
		collected = true
		if found.held > most {
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
// heap is after it, how much it counts live, and how much of that is
// certainly held.
type collection struct{ heap, live, held uint64 }

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
// Many transforms that run long ask at once, and each collection is of the
// whole heap: a watch that waited its turn while a collection began and
// ended is given what that one found, which is of after it asked, where it
// ran another. One that was already running when the watch asked is not
// given: it may count what has died since, before the watch began.
func collectHeap() collection {
	asked := collections.Load()
	collecting.Lock()
	defer collecting.Unlock()
	if collections.Load() > asked {
		return collected
	}
	collections.Add(1)
	percent := debug.SetGCPercent(0)
	before := readHeap("/gc/heap/allocs:bytes")
	runtime.GC()
	live := readHeap("/gc/heap/live:bytes")
	made := readHeap("/gc/heap/allocs:bytes") - before
	debug.SetGCPercent(percent)
	collected = collection{heap: readHeap("/memory/classes/heap/objects:bytes"), live: live, held: live - min(made, live)}
	return collected
}

// collecting is held by the one collection that is being run, collections
// is how many have begun, and collected is what the last of them found,
// read and written with collecting held.
var (
	collecting  sync.Mutex
	collections atomic.Uint64
	collected   collection
)

// readHeap reads one of the runtime's metrics of the heap, in bytes.
// Reading it does not stop the program.
func readHeap(name string) uint64 {
	sample := [...]metrics.Sample{{Name: name}}
	metrics.Read(sample[:])
	return sample[0].Value.Uint64()
}
