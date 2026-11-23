package expr

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/itchyny/gojq"
)

// The watch believes only a collection it asked for, and of that only what
// it shows certainly held. What the runtime's last collection found live
// is of before the watch began, and may be hundreds of megabytes that have
// died since; and a collection counts live what was made while it ran,
// which differs from one to the next by more than a small limit. So a
// program that holds nothing is left to run, whatever the collections
// count live, however large the heap grows and however often it is looked
// at: it is stopped when a collection shows certainly held more than the
// limit over what the first counted live.
func TestTheMemoryWatchBelievesOnlyWhatACollectionShowsHeld(t *testing.T) {
	objects, collect := heapObjects, heapHeld
	t.Cleanup(func() { heapObjects, heapHeld = objects, collect })
	const mib = 1 << 20
	for name, tc := range map[string]struct {
		// heap is the heap at each look, the last repeated, and shows what
		// each collection asked for finds, the last repeated.
		heap    []uint64
		shows   []collection
		stopped bool
	}{
		"garbage alone: counted live by tens of megabytes more each time, held no more": {[]uint64{600 * mib, 700 * mib, 900 * mib, 1200 * mib},
			[]collection{{live: 230 * mib, held: 5 * mib}, {live: 420 * mib, held: 190 * mib}, {live: 285 * mib}, {live: 350 * mib, held: 180 * mib}}, false},
		"held a little under the limit":               {[]uint64{100 * mib, 200 * mib, 300 * mib}, []collection{{live: 10 * mib, held: 9 * mib}, {live: 40 * mib, held: 25 * mib}}, false},
		"held past the limit":                         {[]uint64{100 * mib, 200 * mib, 300 * mib}, []collection{{live: 10 * mib, held: 9 * mib}, {live: 40 * mib, held: 27 * mib}}, true},
		"held past the limit at the first collection": {[]uint64{100 * mib, 200 * mib}, []collection{{live: 150 * mib, held: 117 * mib}}, true},
	} {
		looks, collections := 0, 0
		heapObjects = func() uint64 { looks++; return tc.heap[min(looks-1, len(tc.heap)-1)] }
		heapHeld = func() collection {
			collections++
			found := tc.shows[min(collections-1, len(tc.shows)-1)]
			found.heap = tc.heap[min(looks-1, len(tc.heap)-1)]
			return found
		}
		ended, done := make(chan struct{}), make(chan struct{})
		var cause error
		go func() {
			watchMemory(ended, 16*mib, func(err error) { cause = err }, nil, 0)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(15 * memoryLook):
			close(ended)
			<-done
		}
		if (cause != nil) != tc.stopped {
			t.Errorf("%s: stopped with %v after %d looks and %d collections, want stopped %v", name, cause, looks, collections, tc.stopped)
		}
		if collections == 0 {
			t.Errorf("%s: no collection was asked for in %d looks", name, looks)
		}
	}
}

// A collection that ran while much was being made counts it live, and
// shows held no more than was: beside a heap that takes a while to mark and
// goroutines that make garbage without pause, on one processor and on
// several, every collection shows held at most what the test holds, and
// counts live no less. The GC percent it ran with is the process's again
// after it, when several were asked for at once too.
func TestWhatACollectionShowsHeldLeavesOutWhatWasMadeMeanwhile(t *testing.T) {
	type node struct {
		next *node
		_    [6]uintptr
	}
	nodes := 3000000
	if alloctest.RaceDetector {
		nodes = 300000
	}
	var kept *node
	for range nodes {
		kept = &node{next: kept}
	}
	percent := debug.SetGCPercent(73)
	t.Cleanup(func() { debug.SetGCPercent(percent) })
	for _, processors := range []int{1, 4} {
		was := runtime.GOMAXPROCS(processors)
		runtime.GC()
		before := readHeap("/gc/heap/live:bytes")
		stop := make(chan struct{})
		var making, collecting sync.WaitGroup
		for range 4 {
			making.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
						garbage := make([]*int, 0, 64)
						for i := range 64 {
							garbage = append(garbage, &i)
						}
						_ = garbage
					}
				}
			})
		}
		for range 3 {
			collecting.Go(func() {
				for range 2 {
					if found := collectHeap(); found.held > before+(1<<20) || found.live < found.held {
						t.Errorf("on %d processors a collection counts %d bytes live and shows %d held, of a test that held %d before it made garbage", processors, found.live, found.held, before)
					}
				}
			})
		}
		collecting.Wait()
		close(stop)
		making.Wait()
		runtime.GOMAXPROCS(was)
	}
	if now := debug.SetGCPercent(73); now != 73 {
		t.Errorf("the GC percent is %d after the collections, want the 73 it was", now)
	}
	runtime.KeepAlive(kept)
}

// One value larger than the limit is past it whatever else is held, and
// stops its program when it is made, without a look at the heap: a text
// or an array a function gives, by whichever operator or builtin, as a
// program that doubles one at every step makes within milliseconds. A
// value of the limit exactly is within it, and with no limit nothing is
// measured.
func TestAValueLargerThanTheMemoryLimitStopsItsProgramWhenItIsMade(t *testing.T) {
	const limit = 1 << 20
	// told runs a program with the interpreter told to say when a value
	// passes limit bytes, and no watch of the heap, which may stop a
	// program too: it says how often the interpreter said so, and what
	// the program gave.
	told := func(program string) (times int, value any) {
		t.Helper()
		code, err := CompileJQ(program)
		if err != nil {
			t.Fatalf("%s: %v", program, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = gojq.WithMaxValueBytes(ctx, limit, func() { times++; cancel() })
		value, _ = code.RunWithContext(ctx, nil, JQBindings{}.Values(nil)...).Next()
		return times, value
	}
	for _, grows := range []string{
		`reduce range(40) as $i ("x"; . + .)`,
		`reduce range(40) as $i ("x"; . * 2)`,
		`reduce range(40) as $i ("x"; "\(.)\(.)")`,
		`reduce range(40) as $i ("x"; [., .] | join(""))`,
		`reduce range(40) as $i ("x"; [., .] | add)`,
		`reduce range(40) as $i ("x"; [., .] | tojson)`,
		`reduce range(40) as $i ("x"; [., .] | @text)`,
		`reduce range(40) as $i ("x"; ascii_downcase + ascii_upcase)`,
		`reduce range(40) as $i ({a: "x"}; .a += .a)`,
		`reduce range(40) as $i ([1]; . + .)`,
		`reduce range(40) as $i ([1]; [., .] | flatten)`,
		fmt.Sprintf(`"x" * %d`, limit+1),
		fmt.Sprintf(`[range(%d)] | . + [1]`, limit/16),
		fmt.Sprintf(`[range(%d) | {key: tostring, value: .}] | from_entries`, limit/48+1),
	} {
		if times, value := told(grows); times != 1 || !errors.Is(asError(value), context.Canceled) {
			t.Errorf("%s: a value past %d bytes was told of %d times, and the program gave %.60v", grows, limit, times, value)
		}
	}
	for _, within := range []string{
		fmt.Sprintf(`"x" * %d | length`, limit),
		fmt.Sprintf(`[range(%d)] | . + [] | length`, limit/16),
		fmt.Sprintf(`[range(%d) | {key: tostring, value: .}] | from_entries | length`, limit/48),
		`[range(100000) | tostring] | map(. + .) | length`,
	} {
		if times, value := told(within); times != 0 || asError(value) != nil || value == nil {
			t.Errorf("%s: told of a value past %d bytes %d times, and the program gave %.60v", within, limit, times, value)
		}
	}

	// A watch of a program's memory tells the interpreter its limit, and
	// one with no limit does not.
	run := func(program string, limit model.ByteSize) (value any, exceeded *MemoryExceeded) {
		t.Helper()
		code, err := CompileJQ(program)
		if err != nil {
			t.Fatalf("%s: %v", program, err)
		}
		ctx, ended := WatchMemory(context.Background(), limit)
		value, _ = code.RunWithContext(ctx, nil, JQBindings{}.Values(nil)...).Next()
		return value, ended()
	}
	if value, exceeded := run(fmt.Sprintf(`"x" * %d | length`, limit+1), limit); asError(value) == nil || exceeded == nil || exceeded.Limit != limit {
		t.Errorf("a text of a byte more than a watch's limit: gave %.60v, stopped for %v", value, exceeded)
	}
	if value, exceeded := run(`reduce range(22) as $i ("x"; . + .) | length`, 0); value != 1<<22 || exceeded != nil {
		t.Errorf("with no limit: gave %.60v, stopped for %v", value, exceeded)
	}
}

// A value a function was given is none it made, however large: a program
// that reads a text, a list or an object of megabytes out of its input, by a key, a
// path, a slice, max, a function that gives its input back, is not told of
// it, where reading a field of a large answer by a key in a variable
// stopped the transform. A copy of it is a value made, as large as it is.
func TestAValueLargerThanTheMemoryLimitThatWasGivenIsNoneMade(t *testing.T) {
	const limit = 1 << 20
	text := strings.Repeat("x", 2*limit)
	items := make([]any, limit/8)
	for i := range items {
		items[i] = i
	}
	object := map[string]any{}
	for i := range limit / 24 {
		object["k"+strconv.Itoa(i)] = i
	}
	input := map[string]any{"text": text, "items": items, "nested": map[string]any{"text": text}, "texts": []any{"a", text}, "key": "text",
		"object": object, "objects": []any{object}, "name": "object"}
	told := func(program string) (times int, value any) {
		t.Helper()
		code, err := CompileJQ(program)
		if err != nil {
			t.Fatalf("%s: %v", program, err)
		}
		ctx := gojq.WithMaxValueBytes(context.Background(), limit, func() { times++ })
		value, _ = code.RunWithContext(ctx, input, JQBindings{}.Values(input)...).Next()
		return times, value
	}
	for _, given := range []string{
		`.text`, `.items`, `.nested.text`, `.[.key]`, `.nested[.key]`, `.texts[1]`, `.texts[-1:][0]`, `.text[10:]`, `.items[5:]`,
		`getpath(["nested", "text"])`, `.texts | max`, `.texts | max_by(length)`, `.texts | last`, `[.text] | first`,
		`.text | debug`, `.text | tostring`, `.text | @text`, `.text | ltrimstr("y")`, `.text | rtrimstr("y")`, `.text | sub("^y"; "")`,
		`.items | toarray`, `.text as $t | $t`, `.text | values`, `to_entries | map(.value) | .[] | strings | select(length > 100)`,
		`{a: .text}.a`, `[.items][0]`, `.text | ascii_downcase`,
		`.object`, `.[.name]`, `.object | debug`, `getpath(["object"])`, `.objects | first`, `.objects | max`, `.object | select(true)`, `{a: .object} | .a`,
	} {
		if times, value := told(given + ` | length`); times != 0 || asError(value) != nil {
			t.Errorf("%s: told of a value made %d times, and the program gave %.60v", given, times, value)
		}
	}
	for _, made := range []string{
		`.texts | add`, `.text + "y"`, `.text | tojson`, `.text | ascii_upcase`, `.items | sort`, `.items | reverse`, `.items | .[0] = 5`, `.items + [1]`, `.text | explode`,
		`.object + {a: 1}`, `.object | .a = 1`, `.object | del(.k1)`, `.object | map_values(.)`, `.object | with_entries(.)`,
		fmt.Sprintf(`[range(%d) | {key: tostring, value: .}] | from_entries`, limit/24),
	} {
		if times, value := told(made + ` | length`); times == 0 || asError(value) != nil {
			t.Errorf("%s: told of no value made, and the program gave %.60v", made, value)
		}
	}
}

// asError is a value a program gave as the error it is, nil when it is
// none.
func asError(value any) error {
	err, _ := value.(error)
	return err
}

// Watches that ask for a collection while one is being run are given what
// the next one finds, all of them: eight that waited have one collection
// run for them, where each ran its own, of the whole heap, one after
// another. A watch that asks when none is being run has its own.
func TestWatchesThatAskAtOnceShareACollection(t *testing.T) {
	forced := func() uint64 { return readHeap("/gc/cycles/forced:gc-cycles") }
	before := forced()
	collecting.Lock()
	found := make(chan collection, 8)
	for range 8 {
		go func() { found <- collectHeap() }()
	}
	// Each has asked, and waits.
	time.Sleep(50 * time.Millisecond)
	collecting.Unlock()
	first := <-found
	for range 7 {
		if next := <-found; next != first {
			t.Errorf("a watch that waited was given %+v, and another %+v", next, first)
		}
	}
	if ran := forced() - before; ran != 1 {
		t.Errorf("%d collections were run for eight watches that asked at once, want 1", ran)
	}
	collectHeap()
	collectHeap()
	if ran := forced() - before; ran != 3 {
		t.Errorf("%d collections were run in all, want one more for each watch that asked alone", ran)
	}
}

// What ends a watch may be called on every way out of the code that began
// it, and so more than once: it says each time what it said first, and a
// watch with no limit is none.
func TestAMemoryWatchIsEndedOnceHoweverOftenItIsAskedTo(t *testing.T) {
	for _, limit := range []model.ByteSize{0, 1 << 30} {
		ctx, ended := WatchMemory(context.Background(), limit)
		if first, again := ended(), ended(); first != nil || again != nil || limit == 0 && ctx != context.Background() {
			t.Errorf("limit %d: ended with %v, then %v", limit, first, again)
		}
		if limit > 0 && context.Cause(ctx) == nil {
			t.Errorf("limit %d: the context of a watch that ended is not done", limit)
		}
	}
	if _, ended := WatchMemoryOf(WithMemoryLimit(context.Background(), 0)); ended() != nil {
		t.Error("a context given no limit is watched")
	}
}
