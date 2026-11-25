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

// A jq or yq program is stopped when its probe's time is spent, but time does
// not bound memory: [range(40000000)] holds gigabytes within a second, and a
// definition that calls itself without end grows until the kernel kills the
// exporter, and every other collector's scrapes with it. The memory that a
// program may take is therefore bounded by limits.max_expression_memory,
// both for a transform's programs and for the expressions that a pagination
// and a login run on a target's answers.
//
// Go does not count memory per goroutine, so what is measured is the heap of
// the whole process: the limit is on how much the heap's live objects grow
// while the program runs. A program is not checked before memoryLook has
// passed, and nearly every program ends within it, so the check costs them a
// timer that never fires. A program that runs longer is checked that often
// until it ends.

// memoryLook is how long a program runs before its memory is first looked
// at, and between one look and the next.
const memoryLook = 20 * time.Millisecond

// MemoryExceeded is the cause a program's context is cancelled with when
// it is stopped for its memory, and says what it was stopped for.
type MemoryExceeded struct{ Limit model.ByteSize }

func (e *MemoryExceeded) Error() string {
	return fmt.Sprintf("the exporter's memory grew by more than limits.max_expression_memory, %d bytes, while it ran; make the expression keep less at once, or raise the limit", e.Limit)
}

// WatchMemory returns the context that a jq program should run with, so that
// it is stopped when it takes more memory than limit, as a deadline stops
// it, and the function to call when the program has ended, which says
// whether it was stopped for that reason and with what result. A limit of 0
// or less means no limit.
//
// The function must be called on every path out: until it is, the watch
// checks the heap every memoryLook and asks for collections. It may be
// called more than once, and returns the same result each time.
func WatchMemory(ctx context.Context, limit model.ByteSize) (context.Context, func() *MemoryExceeded) {
	measure, _ := ctx.Value(memoryMeasureKey{}).(*MemoryMeasure)
	var before uint64
	if measure != nil {
		before = heapHeld().live
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

// MemoryMeasure is the most memory that the programs run with a context held,
// for whoever asked (WithMemoryMeasure): a debug probe, which reports it to
// someone choosing limits.max_expression_memory.
//
// It is what the watch measures: the growth of the whole process's heap
// while a program ran, so what other scrapes took meanwhile is included. It
// is the most that a collection counted live, over what one counted live
// before the program began. Both counts include what was allocated while
// they ran, so one is subtracted from the other, like from like; subtracted
// from what was certainly held before, which on a busy exporter is far less
// than what was held, it would attribute to the program what the exporter
// held before it. A collection is run before the program, at every check
// while it runs (20 milliseconds apart), and after it: more than a scrape is
// worth, but that is what a debug probe is for. Memory that a program held
// and released between two checks is not seen.
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

// watchMemory cancels a program, with MemoryExceeded, once the heap's live
// objects are limit more than they were when the watch began, and returns
// when the program has ended.
//
// What is live is known only as of a garbage collection, and the runtime
// starts one when the heap has doubled, by which time a program that builds
// without end holds twice what it may. A collection is therefore requested
// when the heap, garbage included, has grown by the limit, and what it shows
// as held is checked against the limit (heapHeld). A transform that only
// makes garbage that fast gets a collection requested once per limit's worth
// of it, or per quarter of the limit when what is held is just under the
// limit.
//
// Only a collection requested here is trusted. What the runtime's own last
// collection found live dates from before the watch began, and includes what
// has died since: a collection that was marking another transform's hundreds
// of megabytes when that transform ended reports them as live, plus what was
// allocated while it marked, and that once stopped a transform that held
// nothing.
//
// Nor does a collection say exactly what is live, so it gives two numbers
// (collectHeap): what it counts as live, which is no less than what is, and
// what is certainly held, which is no more. What was held when the watch
// began is taken as the whole heap at that time, garbage and all, until the
// first requested collection counts less live than that: the lower of the
// two, neither of them less than what was held. The transform is stopped
// when a collection shows more than the limit certainly held above that. It
// is therefore never stopped for holding less than the limit, whatever else
// the exporter is doing.
//
// A program that doubles a text at every step shows nothing certainly held:
// each text is as large as all the earlier ones, so what is allocated while a
// collection runs equals what it counts as live. Such a program once ran on
// until its steps took longer than a collection does, by then holding a
// hundred times a small limit. It is not left to the collections: a single
// value larger than the whole limit is past it whatever else is held, and
// the interpreter reports that when a function returns one (WatchMemory),
// which stops the program before its next step.
//
// With a measure, a collection is requested at every check, and what it
// counts as live above the starting level is recorded (MemoryMeasure).
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

// collectHeap runs a garbage collection and reports what it found.
//
// A collection counts as live what was reachable when it began, plus
// everything allocated while it marked, garbage or not. Beside a heap of a
// few hundred megabytes, a program that makes garbage quickly had tens to
// hundreds of megabytes counted this way, by an amount that varied between
// collections by more than a small limit, and was stopped while holding
// nothing. What was allocated while the collection marked is no more than
// what was allocated while it ran, from start to end, which is known: what
// is counted as live minus that amount is certainly held.
//
// That alone shows nothing held for a program on one processor that
// allocates gigabytes while a collection waits its turn. The collection is
// therefore run with the heap given no room to grow (a GC percent of 0):
// whoever allocates while it runs must then do its marking first, and
// allocates little. The percent is process-wide, so one collection runs at a
// time, and each restores the setting after it has read what is live; a
// collection of the runtime's own that ended first would be read instead.
//
// Many long-running transforms ask at once, and each collection covers the
// whole heap. A watch that waited its turn while a collection began and
// ended is given that collection's result, which dates from after it asked,
// instead of running another. One that was already running when the watch
// asked is not used: it may count what has died since, before the watch
// began.
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
