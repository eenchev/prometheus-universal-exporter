package transform

import (
	"context"
	"errors"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// greedy is a jq program that holds some hundred megabytes before it ends.
const greedy = "[range(5000000)] | length"

// A jq or yq transform that takes the exporter's memory past the collector's
// limits.max_expression_memory is stopped, wherever in a rule the program
// is, and whatever the rule's error mode: the transform fails as a whole,
// as a limit exceeded, naming the rule and the limit, and nothing is counted
// or logged against the rule. Without the limit the same program runs to
// its end.
func TestATransformPastItsExpressionMemoryIsStopped(t *testing.T) {
	const limit = 8 << 20
	for name, rule := range map[string]model.MetricRule{
		"value":        {Name: "greedy", Expression: greedy},
		"label":        {Name: "greedy", Expression: ".a", Labels: []model.LabelRule{{Name: "l", Expression: greedy}}},
		"items":        {Name: "greedy", Items: "[range(5000000)] | .[0]", Expression: "."},
		"item's value": {Name: "greedy", Items: ".a", Expression: greedy},
		"item's label": {Name: "greedy", Items: ".a", Expression: ".", Labels: []model.LabelRule{{Name: "l", Expression: greedy}}},
	} {
		for _, transform := range []string{"jq", "yq"} {
			for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeFail} {
				if transform == "yq" && (name != "value" || mode != model.ErrorModeFail) {
					continue
				}
				t.Run(name+" of "+transform+" under "+mode, func(t *testing.T) {
					logs := testutil.CaptureLogs(t)
					rule.ErrorMode = mode
					c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: transform},
						Limits:  model.Limits{MaxExpressionMemory: limit},
						Metrics: []model.MetricRule{{Name: "first", Expression: ".a"}, rule}}
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					set, failures, err := transformWith(ctx, t, c, "application/json", `{"a":1}`)
					if set != nil || !errors.Is(err, model.ErrLimitExceeded) {
						t.Fatalf("series %+v, err %v; want a limit exceeded and no series", set, err)
					}
					if want := `the transform was stopped at metric "greedy": the exporter's memory grew by more than limits.max_expression_memory, 8388608 bytes, while it ran; make the expression keep less at once, or raise the limit`; err.Error() != want {
						t.Fatalf("err %q, want %q", err, want)
					}
					var failure *MetricFailure
					if errors.As(err, &failure) || len(failures) != 0 || logs.Len() != 0 {
						t.Fatalf("counted against the rule: %v, %+v; logged: %s", failure, failures, logs)
					}
				})
			}
		}
	}

	// The limit is what a transform may hold, not what it may make: one that
	// makes several times the limit of garbage, holding none of it, runs to
	// its end. So does the greedy one whose collector has no limit.
	// Under the race detector, where a program runs ten times as long, the
	// two are a tenth of what they are.
	garbage, unbounded := "last(range(5000000))", greedy
	if raceDetector {
		garbage, unbounded = "last(range(500000))", "[range(500000)] | length"
	}
	for name, c := range map[string]model.Collector{
		"garbage":  {Limits: model.Limits{MaxExpressionMemory: 16 << 20}, Metrics: []model.MetricRule{{Name: "n", Expression: garbage}}},
		"no limit": {Metrics: []model.MetricRule{{Name: "n", Expression: unbounded}}},
	} {
		c.Name, c.Decoder.Type, c.Transform.Type = "c", "json", "jq"
		set, failures, err := transformWith(context.Background(), t, c, "application/json", `{"a":1}`)
		if err != nil || len(failures) != 0 || set == nil || len(set.Metrics) != 1 {
			t.Errorf("%s: series %+v, failures %+v, err %v; want the one series", name, set, failures, err)
		}
	}
}

// A transform that ends before its memory was looked at is left alone, and
// so is one whose time ran out: that is still the deadline's failure.
func TestTheExpressionMemoryWatchLeavesOtherTransformsAsTheyWere(t *testing.T) {
	c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Limits: model.Limits{MaxExpressionMemory: 1 << 20}, Metrics: []model.MetricRule{{Name: "n", Expression: ".a"}}}
	for range 50 {
		if set, _, err := transformWith(context.Background(), t, c, "application/json", `{"a":1}`); err != nil || set == nil {
			t.Fatalf("series %+v, err %v", set, err)
		}
	}
	c.Metrics[0].Expression = endless
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.Limits.MaxExpressionMemory = 1 << 30
	if _, _, err := transformWith(ctx, t, c, "application/json", `{"a":1}`); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("err %v, want the deadline's", err)
	}
}

// The watch believes only a collection it asked for, and of that only what
// it shows certainly held. What the runtime's last collection found live
// is of before the watch began, and may be hundreds of megabytes that have
// died since; and a collection counts live what was made while it ran,
// which differs from one to the next by more than a small limit. So a
// transform that holds nothing is left to run, whatever the collections
// count live, however large the heap grows and however often it is looked
// at: it is stopped when a collection shows certainly held more than the
// limit over what the first counted live.
func TestTheExpressionMemoryWatchBelievesOnlyWhatACollectionShowsHeld(t *testing.T) {
	objects, collect := heapObjects, heapHeld
	t.Cleanup(func() { heapObjects, heapHeld = objects, collect })
	const mib = 1 << 20
	type shown struct{ live, held uint64 }
	for name, tc := range map[string]struct {
		// heap is the heap at each look, the last repeated, and shows what
		// each collection asked for counts live and shows held, the last
		// repeated.
		heap    []uint64
		shows   []shown
		stopped bool
	}{
		"garbage alone: counted live by tens of megabytes more each time, held no more": {[]uint64{600 * mib, 700 * mib, 900 * mib, 1200 * mib}, []shown{{230 * mib, 5 * mib}, {420 * mib, 190 * mib}, {285 * mib, 0}, {350 * mib, 180 * mib}}, false},
		"held a little under the limit":               {[]uint64{100 * mib, 200 * mib, 300 * mib}, []shown{{10 * mib, 9 * mib}, {40 * mib, 25 * mib}}, false},
		"held past the limit":                         {[]uint64{100 * mib, 200 * mib, 300 * mib}, []shown{{10 * mib, 9 * mib}, {40 * mib, 27 * mib}}, true},
		"held past the limit at the first collection": {[]uint64{100 * mib, 200 * mib}, []shown{{150 * mib, 117 * mib}}, true},
	} {
		looks, collections := 0, 0
		heapObjects = func() uint64 { looks++; return tc.heap[min(looks-1, len(tc.heap)-1)] }
		heapHeld = func() (uint64, uint64, uint64) {
			collections++
			show := tc.shows[min(collections-1, len(tc.shows)-1)]
			return tc.heap[min(looks-1, len(tc.heap)-1)], show.live, show.held
		}
		ended, done := make(chan struct{}), make(chan struct{})
		var cause error
		go func() {
			watchExpressionMemory(ended, 16*mib, func(err error) { cause = err })
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(15 * expressionMemoryLook):
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
	if raceDetector {
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
					if _, live, held := collectHeap(); held > before+(1<<20) || live < held {
						t.Errorf("on %d processors a collection counts %d bytes live and shows %d held, of a test that held %d before it made garbage", processors, live, held, before)
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

// On one processor a program makes gigabytes while a collection waits its
// turn, which is more than it holds: so the collection is run with the
// heap given no room to grow, and a transform that builds without end is
// stopped there as it is on several.
func TestATransformPastItsExpressionMemoryIsStoppedOnOneProcessor(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	endless := "[range(30000000)] | length"
	if raceDetector {
		endless = "[range(5000000)] | length"
	}
	c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Limits: model.Limits{MaxExpressionMemory: 16 << 20}, Metrics: []model.MetricRule{{Name: "n", Expression: endless}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if set, _, err := transformWith(ctx, t, c, "application/json", `{"a":1}`); set != nil || !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("series %+v, err %v; want a limit exceeded and no series", set, err)
	}
}
