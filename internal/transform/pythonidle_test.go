package transform

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// --python.max-idle-workers and --python.idle-timeout: how many workers of a
// script stay idle after a burst, and for how long. The workers here have no
// interpreter (fakeWorker), and are taken from and given back to the pool as
// a run does (acquire, release), so a burst is exactly as wide as the test
// makes it.

// fakeWorkerPool is a pool whose workers have no interpreter.
func fakeWorkerPool() *PythonPool {
	pool := newPythonPool()
	pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) { return fakeWorker(spec), nil }
	return pool
}

// idleSpec is the spec of a script of the collector, with its statistics.
func idleSpec(pool *PythonPool, collector string) pythonSpec {
	spec := pythonSpec{Path: "python3", Collector: collector}
	spec.stats = pool.Stats(collector)
	return spec
}

// burst takes n workers of spec's script at once, each started for the burst
// since none is free, and gives them all back.
func burst(t *testing.T, pool *PythonPool, spec pythonSpec, n int) {
	t.Helper()
	workers := make([]*pythonWorker, 0, n)
	for range n {
		worker, reused, err := pool.acquire(context.Background(), spec)
		if err != nil || reused {
			t.Fatalf("a worker of the burst: reused=%v err=%v", reused, err)
		}
		workers = append(workers, worker)
	}
	for _, worker := range workers {
		pool.release(spec, worker)
	}
}

// After a burst of five runs of one script at once, a pool keeps as many of
// its workers idle as --python.max-idle-workers says, one by default and
// none at 0, and stops the rest as surplus; another script keeps its own.
// With none kept the five still run at once, each in a worker of its own.
func TestPythonPoolKeepsAtMostTheMaxIdleWorkersOfAScript(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     bool
		maxIdle int
		kept    int
	}{
		{"the default", false, 0, 1},
		{"none", true, 0, 0},
		{"one", true, 1, 1},
		{"three", true, 3, 3},
		{"more than the burst", true, 8, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := fakeWorkerPool()
			defer pool.shutdown()
			if tc.set {
				pool.SetMaxIdleWorkers(tc.maxIdle)
			}
			a, b := idleSpec(pool, "idle_a"), idleSpec(pool, "idle_b")
			burst(t, pool, a, 5)
			burst(t, pool, b, 5)
			for _, name := range []string{"idle_a", "idle_b"} {
				snap := pool.Snapshot(name)
				if snap.Idle != tc.kept || snap.Starts != 5 || snap.Stops[pythonStopSurplus] != uint64(5-tc.kept) {
					t.Errorf("%s after a burst of five: %d idle, %d started, %d stopped as surplus; want %d idle, 5 started, %d surplus", name, snap.Idle, snap.Starts, snap.Stops[pythonStopSurplus], tc.kept, 5-tc.kept)
				}
			}
		})
	}
	if ValidateMaxIdleWorkers(-1) == nil || ValidateMaxIdleWorkers(0) != nil || ValidateMaxIdleWorkers(1000) != nil {
		t.Error("--python.max-idle-workers: a negative number is refused, 0 and more are taken")
	}
}

// With no worker kept idle, a run under --python.max-workers that waits for
// a place gets the one its predecessor's worker leaves when it stops at the
// end of its run, and no idle worker is evicted for it.
func TestPythonWorkersKeptNoneIdleStillFreeTheirPlace(t *testing.T) {
	pool := fakeWorkerPool()
	defer pool.shutdown()
	pool.SetMaxWorkers(1)
	pool.SetMaxIdleWorkers(0)
	a, b := idleSpec(pool, "freed_a"), idleSpec(pool, "freed_b")
	worker, _, err := pool.acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		other, _, err := pool.acquire(context.Background(), b)
		if err == nil {
			pool.release(b, other)
		}
		got <- err
	}()
	for pool.PoolSnapshot().Waiting != 1 {
		time.Sleep(time.Millisecond)
	}
	pool.release(a, worker)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if snap := pool.PoolSnapshot(); snap.Idle != 0 || snap.Starts != 2 || snap.Stops[pythonStopSurplus] != 2 || snap.Stops[pythonStopEvicted] != 0 {
		t.Errorf("the pool counts %+v, want two workers started and both stopped as surplus, none evicted", snap)
	}
}

// A script run with none kept idle runs as any other, in a worker that
// stops when the run ends.
func TestAScriptRunsWithNoWorkerKeptIdle(t *testing.T) {
	usePythonPool(t)
	pool := PythonWorkers()
	scriptWorkers(pool)
	pool.SetMaxIdleWorkers(0)
	c := workerCollector("none_idle", `metric(name="v", value=1)`)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
	for range 2 {
		if _, err := executePython(context.Background(), "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}, r, c); err != nil {
			t.Fatal(err)
		}
	}
	if snap := pool.Snapshot("none_idle"); snap.Runs[pythonRunOK] != 2 || snap.Starts != 2 || snap.Idle != 0 || snap.Stops[pythonStopSurplus] != 2 {
		t.Errorf("the collector counts %+v, want two runs, each in a worker started for it and stopped when it ended", snap)
	}
}

// An idle worker is stopped once it has been unused for longer than
// --python.idle-timeout, two minutes by default, by the timer alone, which
// checks every minute or every half the timeout when that is shorter; it is
// counted as stopped for being idle. The clock is the test's own
// (testing/synctest).
func TestPythonIdleWorkersStopAfterTheIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name            string
		timeout         time.Duration // 0: the default
		interval        time.Duration
		kept, stoppedBy time.Duration
	}{
		// Checked at one and two minutes, when it is not yet past two, and
		// stopped at three.
		{"the default", 0, time.Minute, 2 * time.Minute, 3 * time.Minute},
		{"thirty seconds", 30 * time.Second, 15 * time.Second, 30 * time.Second, 45 * time.Second},
		{"ten minutes", 10 * time.Minute, time.Minute, 10 * time.Minute, 11 * time.Minute},
		{"a second", time.Second, 500 * time.Millisecond, time.Second, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pool := fakeWorkerPool()
				defer pool.shutdown()
				if tc.timeout != 0 {
					pool.SetIdleTimeout(tc.timeout)
				}
				if got := pool.ReapInterval(); got != tc.interval {
					t.Fatalf("the idle workers are checked every %s, want %s", got, tc.interval)
				}
				spec := idleSpec(pool, "timed_out")
				burst(t, pool, spec, 1)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go pool.ReapLoop(ctx, pool.ReapInterval())
				time.Sleep(tc.kept)
				synctest.Wait()
				if snap := pool.Snapshot("timed_out"); snap.Idle != 1 {
					t.Fatalf("after %s unused: %+v, want the worker idle still", tc.kept, snap)
				}
				time.Sleep(tc.stoppedBy - tc.kept)
				synctest.Wait()
				if snap := pool.Snapshot("timed_out"); snap.Idle != 0 || snap.Stops[pythonStopIdle] != 1 {
					t.Fatalf("after %s unused: %+v, want the worker stopped as idle", tc.stoppedBy, snap)
				}
				cancel()
			})
		})
	}
	for timeout, ok := range map[time.Duration]bool{0: false, -time.Second: false, 999 * time.Millisecond: false, time.Second: true, PythonDefaultIdleTimeout: true, 24 * time.Hour: true} {
		if err := ValidateIdleTimeout(timeout); (err == nil) != ok {
			t.Errorf("--python.idle-timeout=%s: %v, want it taken: %v", timeout, err, ok)
		}
	}
}

// With the defaults, a collector scraped every minute, the longest scrape
// interval in common use, keeps its one worker warm, and so does one scraped
// every minute and a half or with every other scrape missed: ten minutes of
// its scrapes start one interpreter. One scraped every five minutes starts
// one per scrape.
func TestTheDefaultIdleTimeoutKeepsAWorkerOfAScrapeEveryMinuteWarm(t *testing.T) {
	for _, tc := range []struct {
		every  time.Duration
		starts uint64
	}{
		{15 * time.Second, 1},
		{time.Minute, 1},
		{90 * time.Second, 1},
		{5 * time.Minute, 3},
	} {
		t.Run(tc.every.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pool := fakeWorkerPool()
				defer pool.shutdown()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go pool.ReapLoop(ctx, pool.ReapInterval())
				spec := idleSpec(pool, "warm")
				for at := time.Duration(0); at <= 10*time.Minute; at += tc.every {
					burst1(t, pool, spec)
					time.Sleep(tc.every)
				}
				if got := pool.Snapshot("warm").Starts; got != tc.starts {
					t.Errorf("scraped every %s for ten minutes: %d workers started, want %d", tc.every, got, tc.starts)
				}
				cancel()
			})
		})
	}
}

// burst1 is a run of spec's script: it takes a worker, the idle one when
// there is one, and gives it back.
func burst1(t *testing.T, pool *PythonPool, spec pythonSpec) {
	t.Helper()
	worker, _, err := pool.acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	pool.release(spec, worker)
}
