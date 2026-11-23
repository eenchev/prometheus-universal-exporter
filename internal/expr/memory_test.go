package expr

import (
	"context"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The watch believes only a collection it asked for, and of that only what
// it shows certainly held. What the runtime's last collection found live
// is of before the watch began, and may be hundreds of megabytes that have
// died since; and a collection counts live what was made while it ran,
// which differs from one to the next by more than a small limit. So a
// program that holds nothing is left to run, whatever the collections
// count live, however large the heap grows and however often it is looked
// at: it is stopped when a collection shows certainly held more than the
// limit over what the first counted live, or counts that much live and
// shows that one value larger than the limit was made while it ran, as of
// a text that is doubled at every step, which is each time as much as
// all the collection counts live.
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
		"a text doubled: nothing certainly held, one value larger than the limit made":  {[]uint64{100 * mib, 400 * mib, 800 * mib}, []collection{{live: 10 * mib, held: 9 * mib}, {live: 193 * mib, held: 1 * mib, largest: 96 * mib}}, true},
		"garbage in values under the limit each, counted live past it":                  {[]uint64{100 * mib, 400 * mib, 800 * mib}, []collection{{live: 10 * mib, held: 9 * mib}, {live: 193 * mib, held: 1 * mib, largest: 16 * mib}}, false},
		"one value larger than the limit made and let go: counted live under the limit": {[]uint64{100 * mib, 400 * mib, 800 * mib}, []collection{{live: 10 * mib, held: 9 * mib}, {live: 26 * mib, held: 0, largest: 96 * mib}}, false},
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
			watchMemory(ended, 16*mib, func(err error) { cause = err })
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
					if found := collectHeap(); found.held > before+(1<<20) || found.live < found.held || found.largest > 1<<20 {
						t.Errorf("on %d processors a collection counts %d bytes live and shows %d held and a value of %d made, of a test that held %d before it made garbage in small values", processors, found.live, found.held, found.largest, before)
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

// A collection shows the least that the largest value made while it ran
// was: beside goroutines that make garbage in small values, one that makes
// texts of 24 MiB and lets each go is shown to have made a value of more
// than 16 MiB, in most collections that it made one in at all, and none is
// shown to have made a larger one than it did.
func TestACollectionShowsTheLargestValueMadeWhileItRan(t *testing.T) {
	const large = 24 << 20
	stop := make(chan struct{})
	var making sync.WaitGroup
	for i := range 3 {
		making.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if i == 0 {
					sink.Store(new(make([]byte, large)))
					continue
				}
				garbage := make([]*int, 0, 64)
				for i := range 64 {
					garbage = append(garbage, &i)
				}
				_ = garbage
			}
		})
	}
	shown := 0
	for range 12 {
		found := collectHeap()
		if found.largest > large+(1<<20) {
			t.Errorf("a collection shows a value of %d bytes made, and none was larger than %d", found.largest, large)
		}
		if found.largest > 16<<20 {
			shown++
		}
	}
	close(stop)
	making.Wait()
	if shown < 4 {
		t.Errorf("%d of 12 collections showed a value of more than 16 MiB made, beside a goroutine that makes them of 24 MiB without pause", shown)
	}
}

// sink keeps the last large value a test made, so that making it is not
// compiled away.
var sink atomic.Pointer[[]byte]

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
