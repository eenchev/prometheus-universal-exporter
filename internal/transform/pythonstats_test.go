package transform

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A collector's worker statistics follow a reload (PythonStats, Retire): the
// tests here hold the pool to it with workers that have no interpreter, so
// that a run is held where the test says, at its worker's start or in its
// script, for as long as the test says, and released by the test. The
// exporter's own tests make the reloads (exporter/pythonstats_http_test.go).

// fakeWorker is a worker with no process, which a pool driven by hand starts,
// lends, takes back and stops as it does any other: nothing is ever run in
// it.
func fakeWorker(spec pythonSpec) *pythonWorker {
	return &pythonWorker{collector: spec.Collector, key: spec.key(), cmd: &exec.Cmd{}, lines: make(chan pythonLine, 1), exited: make(chan struct{}), stderr: &tailBuffer{max: pythonStderrTail}}
}

// scriptedOK is the answer of a script that ran and emitted nothing.
const scriptedOK = `{"ok": true, "metrics": []}`

// scriptedWorkers stand in for the interpreters of a pool. A worker takes a
// request as an interpreter does, says it has it, and answers that the
// script emitted nothing. While holdStarts is set, a start says on starts
// that it has begun and goes on when it is told how it ends; while holdRuns
// is set, a run says on runs that its script is running and ends when it is
// given the answer.
type scriptedWorkers struct {
	holdStarts, holdRuns atomic.Bool
	starts               chan chan error
	runs                 chan chan string
}

// scriptWorkers gives the pool stand-ins for its interpreters.
func scriptWorkers(pool *PythonPool) *scriptedWorkers {
	s := &scriptedWorkers{starts: make(chan chan error, 64), runs: make(chan chan string, 64)}
	pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) {
		if s.holdStarts.Load() {
			ends := make(chan error)
			s.starts <- ends
			if err := <-ends; err != nil {
				return nil, err
			}
		}
		requestRead, requestWrite, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		worker := fakeWorker(spec)
		worker.requests = requestWrite
		go func() {
			// Stopping the worker closes its requests, which ends this.
			defer closeFiles(requestRead)
			requests := bufio.NewReader(requestRead)
			for {
				if _, err := requests.ReadString('\n'); err != nil {
					return
				}
				worker.lines <- pythonLine{data: []byte(pythonRequestTaken)}
				answer := scriptedOK
				if s.holdRuns.Load() {
					held := make(chan string)
					s.runs <- held
					answer = <-held
				}
				worker.lines <- pythonLine{data: []byte(answer)}
			}
		}()
		return worker, nil
	}
	return s
}

// runScripted runs the collector's transform script in the pool in place, in
// a goroutine of its own, and returns where its error arrives when it ends.
func runScripted(name string) <-chan error {
	done := make(chan error, 1)
	go func() {
		c := workerCollector(name, `metric(name="v", value=1)`)
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
		_, err := executePython(context.Background(), "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}, r, c)
		done <- err
	}()
	return done
}

// noWorkerStatistics fails the test unless nothing is counted under the
// collector's name: no worker starting, busy or idle, none started, failed
// to start or stopped, and no run.
func noWorkerStatistics(t *testing.T, where string, pool *PythonPool, collector string) {
	t.Helper()
	got := pool.Snapshot(collector)
	if got.Starting != 0 || got.Busy != 0 || got.Idle != 0 || got.Starts != 0 || got.StartFailures != 0 || len(got.Stops) != 0 || len(got.Runs) != 0 {
		t.Errorf("%s: %+v is counted under %s, want nothing", where, got, collector)
	}
}

// A run that is under way when a reload removes its collector, and the
// collector's statistics are retired, counts under no collector's name,
// wherever it was then: waiting for a worker under --python.max-workers,
// starting its worker, or running its script. It leaves no statistics under
// the name, and where the collector has been added again by the time the run
// goes on, that one's are still at zero when the run has ended: its worker
// started, the worker busy, and the run's outcome are counted for the pool as
// a whole and for nothing else. The run leaves no worker idle: its worker is
// stopped when the run ends, for the reload, under no collector's name,
// whether the script is in use again by then or not, and the collector added
// again starts a worker of its own and counts from zero.
//
// The pool is called as the exporter calls it: a reload that removes the
// collector makes the scripts still in use the ones kept (Retain, by the
// configuration manager) and then retires the collector's statistics (Retire,
// where the server drops its own), and the reload that brings the collector
// back makes its script one of those kept again.
func TestARunUnderWayWhenItsStatisticsAreRetiredCountsUnderNoName(t *testing.T) {
	for _, state := range []string{"waiting for a worker", "starting its worker", "running its script"} {
		for _, back := range []bool{false, true} {
			where := fmt.Sprintf("a run %s, the collector added again %v", state, back)
			t.Run(where, func(t *testing.T) {
				usePythonPool(t)
				pool := PythonWorkers()
				workers := scriptWorkers(pool)
				// What the pool has counted for another collector when the
				// run begins, which stays that collector's, and the scripts
				// in use when the collector is removed.
				others := 0
				gone := pythonWorkerSpec("python3", workerCollector("gone", `metric(name="v", value=1)`)).key()
				inUse := map[string]bool{pythonWorkerSpec("python3", workerCollector("other", `metric(name="v", value=1)`)).key(): true}
				var ended <-chan error
				goOn := func() {}
				switch state {
				case "waiting for a worker":
					pool.SetMaxWorkers(1)
					workers.holdRuns.Store(true)
					other := runScripted("other")
					held := <-workers.runs
					ended = runScripted("gone")
					testutil.WaitFor(t, "the run to wait for a worker", func() bool { return pool.PoolSnapshot().Waiting == 1 })
					others = 1
					goOn = func() {
						held <- scriptedOK
						if err := <-other; err != nil {
							t.Fatal(err)
						}
						(<-workers.runs) <- scriptedOK
					}
				case "starting its worker":
					workers.holdStarts.Store(true)
					ended = runScripted("gone")
					starts := <-workers.starts
					goOn = func() { starts <- nil }
				case "running its script":
					workers.holdRuns.Store(true)
					ended = runScripted("gone")
					held := <-workers.runs
					goOn = func() { held <- scriptedOK }
				}
				before := pool.PoolSnapshot()
				pool.Retain(inUse)
				pool.Retire(map[string]bool{"gone": true})
				if got := pool.PoolSnapshot(); !reflect.DeepEqual(got, before) {
					t.Errorf("retiring a collector's statistics changed what the pool counts as a whole: %+v, was %+v", got, before)
				}
				noWorkerStatistics(t, "while the run is held", pool, "gone")
				kept := others
				if back {
					// The reload that adds the collector again keeps its
					// script, and the exporter makes statistics for it when
					// a trip of it first asks.
					inUse[gone] = true
					pool.Retain(inUse)
					pool.Stats("gone")
					kept++
				}
				goOn()
				if err := <-ended; err != nil {
					t.Fatal(err)
				}
				noWorkerStatistics(t, "when the run has ended", pool, "gone")
				if got := pool.StatsKept(); got != kept {
					t.Errorf("the pool keeps the statistics of %d collectors, want %d", got, kept)
				}
				// The run's worker was stopped when the run ended, for the
				// reload. The run that waited for a worker started its own
				// in the place of the other collector's, which had gone
				// idle and was evicted for it.
				want := pythonWorkerSnapshot{Starts: uint64(1 + others), Stops: map[string]uint64{pythonStopReload: 1}, Runs: map[string]uint64{pythonRunOK: uint64(1 + others)}}
				if others > 0 {
					want.Stops[pythonStopEvicted] = uint64(others)
				}
				if got := pool.PoolSnapshot(); !reflect.DeepEqual(got, want) {
					t.Errorf("the pool as a whole counts %+v, want %+v: the run's worker started, and stopped for the reload when the run ended", got, want)
				}
				if !back {
					return
				}
				workers.holdStarts.Store(false)
				workers.holdRuns.Store(false)
				pool.SetMaxWorkers(0)
				if err := <-runScripted("gone"); err != nil {
					t.Fatal(err)
				}
				want = pythonWorkerSnapshot{Idle: 1, Starts: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{pythonRunOK: 1}}
				if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
					t.Errorf("the collector added again counts %+v after its first run, want %+v", got, want)
				}
			})
		}
	}
}

// A run that has given its worker back when its collector's statistics are
// retired, and has not yet said how it ended, says it for the pool as a
// whole alone: the outcome is counted under no collector's name, and makes
// no statistics under the name. The worker it gave back, idle when the
// statistics are retired, is stopped with them, for the reload.
func TestARunEndingWhenItsStatisticsAreRetiredCountsItsOutcomeUnderNoName(t *testing.T) {
	usePythonPool(t)
	pool := PythonWorkers()
	pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) { return fakeWorker(spec), nil }
	spec := pythonSpec{Path: "python3", Collector: "gone"}
	spec.stats = pool.statsOf(nil, "gone")
	worker, _, err := pool.acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	pool.release(spec, worker)
	pool.Retire(map[string]bool{"gone": true})
	pool.recordRun(spec.stats, pythonRunScriptError)
	noWorkerStatistics(t, "when the run has ended", pool, "gone")
	if got := pool.StatsKept(); got != 0 {
		t.Errorf("the pool keeps the statistics of %d collectors, want none", got)
	}
	want := pythonWorkerSnapshot{Starts: 1, Stops: map[string]uint64{pythonStopReload: 1}, Runs: map[string]uint64{pythonRunScriptError: 1}}
	if got := pool.PoolSnapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("the pool as a whole counts %+v, want %+v: the worker started, and stopped for the reload, and the run ended in a script error", got, want)
	}
}

// A run that asks the pool for a worker only after its statistics were
// retired counts under no collector's name either, though the collector may
// have been added again by then: the second script of a trip whose
// collector was removed while its first ran, which counts in the statistics
// the trip took, and a run of a trip that read a collector removed before
// the trip took any, which is handed the pool's departed. Its worker's
// start, a start that fails, the worker stopped for a script that timed out
// and the run's outcome are counted for the pool as a whole, and the
// collector added again is still at zero. The run starts a worker of its
// own, though the script may no longer be in use, and leaves none idle: the
// worker is stopped when the run ends, for the reload that removed its
// collector, rather than kept until the idle timeout for a script nothing
// runs any more.
func TestARunBegunAfterItsStatisticsWereRetiredCountsUnderNoName(t *testing.T) {
	for _, took := range []string{"the collector's statistics, retired since", "the pool's departed"} {
		for _, ends := range []string{"well", "with its worker failing to start", "with its script timing out"} {
			t.Run(took+", ending "+ends, func(t *testing.T) {
				usePythonPool(t)
				pool := PythonWorkers()
				fails := ends == "with its worker failing to start"
				pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) {
					if fails {
						return nil, errors.New("the interpreter did not start")
					}
					return fakeWorker(spec), nil
				}
				spec := pythonSpec{Path: "python3", Collector: "gone"}
				spec.stats = pool.Departed()
				if took != "the pool's departed" {
					spec.stats = pool.statsOf(nil, "gone")
					pool.Retire(map[string]bool{"gone": true})
				}
				pool.Stats("gone")
				want := pythonWorkerSnapshot{Stops: map[string]uint64{}, Runs: map[string]uint64{}}
				worker, _, err := pool.acquire(context.Background(), spec)
				switch ends {
				case "well":
					want.Starts, want.Stops[pythonStopReload], want.Runs[pythonRunOK] = 1, 1, 1
					pool.release(spec, worker)
					pool.recordRun(spec.stats, pythonRunOK)
				case "with its worker failing to start":
					if err == nil {
						t.Fatal("the worker started")
					}
					want.StartFailures, want.Runs[pythonRunFailed] = 1, 1
					pool.recordRun(spec.stats, pythonRunFailed)
				case "with its script timing out":
					want.Starts, want.Stops[pythonStopTimeout], want.Runs[pythonRunTimeout] = 1, 1, 1
					pool.discard(worker, pythonStopTimeout)
					pool.recordRun(spec.stats, pythonRunTimeout)
				}
				noWorkerStatistics(t, "when the run has ended", pool, "gone")
				if got := pool.PoolSnapshot(); !reflect.DeepEqual(got, want) {
					t.Errorf("the pool as a whole counts %+v, want %+v", got, want)
				}
				if got := pool.StatsKept(); got != 1 {
					t.Errorf("the pool keeps the statistics of %d collectors, want those of the collector added again", got)
				}
			})
		}
	}
}

// A script's run counts in the statistics its trip carries with its script
// timer, and asks for none by its collector's name: the runs of a trip that
// carries the pool's departed, as the trip of a collector a reload removed
// does, count under no name and leave nothing under the collector's, each in
// a worker it starts and leaves stopped, and those of a trip that carries a
// collector's statistics count there, under whatever name the run's own
// collector has, in a worker of those statistics. A run under a timer that
// carries none, or under no timer, counts under its collector's name, as
// every run did, in a worker of its collector's: it is not given the idle
// worker of the script that counts in the statistics the other trip carried.
func TestARunCountsInTheStatisticsItsTripCarries(t *testing.T) {
	usePythonPool(t)
	pool := PythonWorkers()
	scriptWorkers(pool)
	c := workerCollector("gone", `metric(name="v", value=1)`)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
	run := func(ctx context.Context) {
		t.Helper()
		if _, err := executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}, r, c); err != nil {
			t.Fatal(err)
		}
	}
	departed, timer := WithScriptTimer(context.Background(), pool.Departed())
	run(departed)
	run(departed)
	noWorkerStatistics(t, "after the runs of a trip that carries the pool's departed", pool, "gone")
	if got := pool.StatsKept(); got != 0 {
		t.Errorf("the pool keeps the statistics of %d collectors after the runs of a removed collector, want none", got)
	}
	if got := pool.PoolSnapshot(); got.Starts != 2 || got.Runs[pythonRunOK] != 2 || got.Idle != 0 || got.Stops[pythonStopReload] != 2 {
		t.Errorf("the pool as a whole counts %+v, want two runs, each in a worker started and stopped", got)
	}
	if _, ran := timer.Seconds(); !ran {
		t.Error("the timer that carries statistics did not time the runs")
	}
	carried, _ := WithScriptTimer(context.Background(), pool.Stats("carried"))
	run(carried)
	noWorkerStatistics(t, "after the run of a trip that carries another's statistics", pool, "gone")
	if got := pool.Snapshot("carried"); got.Runs[pythonRunOK] != 1 || got.Idle != 1 || got.Starts != 1 {
		t.Errorf("the statistics the trip carries count %+v, want the run and the worker it started, idle", got)
	}
	bare, _ := WithScriptTimer(context.Background(), nil)
	run(bare)
	run(context.Background())
	if got := pool.Snapshot("gone"); got.Runs[pythonRunOK] != 2 || got.Idle != 1 || got.Starts != 1 {
		t.Errorf("the collector counts %+v after two runs that carried no statistics, want both, in one worker it started, idle", got)
	}
	if got := pool.Snapshot("carried"); got.Idle != 1 || got.Starts != 1 {
		t.Errorf("the statistics the trip carried count %+v once the collector's own runs have ended, want their worker idle still", got)
	}
}

// A worker that is idle when its collector's statistics are retired is
// stopped under no collector's name, for the reload that removed the
// collector: by the reload itself, which stops the workers of the scripts no
// longer in use before the statistics are retired, and its stop is then
// shown under no collector afterwards; or, where the reload left it, a run of
// the removed collector having given it back since, with the statistics. The
// stop is counted for the pool as a whole. The collector added again under
// the name, with the same script, has counted no stop and is given no worker
// of the removed collector: its first run starts one. The idle worker of a
// collector that stays is left as it is.
func TestTheStopOfARemovedCollectorsWorkerIsCountedForThePoolAlone(t *testing.T) {
	for _, stopped := range []string{"by the reload, before the statistics are retired", "with the statistics, where the reload left it"} {
		t.Run(stopped, func(t *testing.T) {
			usePythonPool(t)
			pool := PythonWorkers()
			pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) { return fakeWorker(spec), nil }
			spec, stays := pythonSpec{Path: "python3", Collector: "gone"}, pythonSpec{Path: "python3", Collector: "stays"}
			var worker *pythonWorker
			for _, of := range []*pythonSpec{&spec, &stays} {
				of.stats = pool.statsOf(nil, of.Collector)
				started, _, err := pool.acquire(context.Background(), *of)
				if err != nil {
					t.Fatal(err)
				}
				pool.release(*of, started)
				if of == &spec {
					worker = started
				}
			}
			if got := pool.Snapshot("gone"); got.Idle != 1 || got.Starts != 1 {
				t.Fatalf("the collector counts %+v before it is removed, want a worker started and idle", got)
			}
			if stopped == "by the reload, before the statistics are retired" {
				pool.Retain(map[string]bool{stays.key(): true})
				if got := pool.Snapshot("gone").Stops[pythonStopReload]; got != 1 {
					t.Fatalf("the collector counts %d stops for the reload while it has its statistics, want 1", got)
				}
			}
			pool.Retire(map[string]bool{"gone": true})
			if got := pool.PoolSnapshot(); got.Stops[pythonStopReload] != 1 || len(got.Stops) != 1 || got.Idle != 1 {
				t.Errorf("the pool as a whole counts %+v, want the removed collector's worker stopped for the reload, and the other collector's idle", got)
			}
			// The reload that adds the collector again, as it was.
			pool.Retain(map[string]bool{stays.key(): true, spec.key(): true})
			again := pythonSpec{Path: "python3", Collector: "gone"}
			again.stats = pool.Stats("gone")
			noWorkerStatistics(t, "when the collector has been added again", pool, "gone")
			taken, reused, err := pool.acquire(context.Background(), again)
			if err != nil || reused || taken == worker {
				t.Fatalf("the collector added again was given the removed collector's worker (reused %v), or none: %v", reused, err)
			}
			want := pythonWorkerSnapshot{Starts: 1, Busy: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{}}
			if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
				t.Errorf("the collector added again counts %+v, want %+v", got, want)
			}
			want = pythonWorkerSnapshot{Starts: 1, Idle: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{}}
			if got := pool.Snapshot("stays"); !reflect.DeepEqual(got, want) {
				t.Errorf("the collector that stayed counts %+v, want %+v", got, want)
			}
			if got := pool.PoolSnapshot(); got.Stops[pythonStopReload] != 1 || len(got.Stops) != 1 {
				t.Errorf("the pool as a whole counts the stops %v, want the one for the reload", got.Stops)
			}
		})
	}
}

// The pool keeps statistics for the collectors there are and no others: over
// reloads that each give the one collector another name, the statistics of
// every name it had are dropped, so what is kept does not grow with the
// reloads. What every one of them counted stays counted for the pool as a
// whole, which never counts less than it did. Asking what is counted under a
// name nothing is kept for keeps nothing for it either.
func TestTheStatisticsKeptDoNotGrowWithTheCollectorsRemoved(t *testing.T) {
	usePythonPool(t)
	pool := PythonWorkers()
	pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) { return fakeWorker(spec), nil }
	const renames = 200
	var was pythonWorkerSnapshot
	for i := range renames {
		name := fmt.Sprintf("renamed_%d", i)
		spec := pythonSpec{Path: "python3", Collector: name}
		spec.stats = pool.statsOf(nil, name)
		worker, _, err := pool.acquire(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		pool.release(spec, worker)
		pool.recordRun(spec.stats, pythonRunOK)
		if got := pool.StatsKept(); got != 1 {
			t.Fatalf("the pool keeps the statistics of %d collectors while it has one, after %d renames", got, i)
		}
		// The reload: the script's workers are stopped, and the statistics
		// of the name the collector had are retired.
		pool.Retain(nil)
		pool.Retire(map[string]bool{name: true})
		noWorkerStatistics(t, "when the collector has another name", pool, name)
		whole := pool.PoolSnapshot()
		if whole.Starts < was.Starts || whole.Runs[pythonRunOK] < was.Runs[pythonRunOK] || whole.Stops[pythonStopReload] < was.Stops[pythonStopReload] {
			t.Fatalf("the pool as a whole counts %+v after %d renames, less than the %+v it counted", whole, i, was)
		}
		was = whole
	}
	if got := pool.StatsKept(); got != 0 {
		t.Errorf("the pool keeps the statistics of %d collectors after %d renames, want none", got, renames)
	}
	if was.Starts != renames || was.Runs[pythonRunOK] != renames || was.Stops[pythonStopReload] != renames || was.Idle != 0 || was.Busy != 0 {
		t.Errorf("the pool as a whole counts %+v, want %d workers started and stopped for the reload, and %d runs", was, renames, renames)
	}
}

// A run counts in the statistics its trip was handed only when they are the
// pool's it runs in: a test may have put another pool in place since, and
// the run then counts in those kept under its collector's name there, as a
// run does that was handed none.
func TestStatisticsOfAnotherPoolAreNotCountedIn(t *testing.T) {
	usePythonPool(t)
	former := PythonWorkers()
	carried := former.Stats("moved")
	usePythonPool(t)
	pool := PythonWorkers()
	if got := pool.statsOf(carried, "moved"); got == carried || got != pool.Stats("moved") {
		t.Error("a run counts in the statistics of a pool it does not run in")
	}
	own := pool.Stats("moved")
	if got := pool.statsOf(own, "other"); got != own {
		t.Error("a run does not count in the statistics its trip was handed")
	}
	if got := pool.statsOf(pool.Departed(), "moved"); got != pool.Departed() {
		t.Error("a run of a collector removed before it began does not count in the pool's departed")
	}
}

// drivenPool is a pool a test drives by hand, a run at a time: the pool as
// it is, or as it was (oraclePool). runs are the runs under way, in the
// order they began.
type drivenPool struct {
	what    string
	acquire func(spec pythonSpec) (*pythonWorker, bool, error)
	release func(spec pythonSpec, worker *pythonWorker)
	discard func(worker *pythonWorker, reason string)
	record  func(spec pythonSpec, outcome string)
	reap    func(now time.Time)
	retain  func(keys map[string]bool)
	limit   func(workers int)
	one     func(collector string) pythonWorkerSnapshot
	whole   func() pythonWorkerSnapshot
	// locked runs do with the pool's lock held and its idle workers.
	locked func(do func(idle map[string][]*pythonWorker, live, limit int))
	// given gives a run the statistics it counts in, as the pool it is run
	// in has it take them.
	given     func(spec pythonSpec) pythonSpec
	failStart *bool
	runs      []drivenRun
}

type drivenRun struct {
	spec   pythonSpec
	worker *pythonWorker
}

// drive makes a pool of each kind, with workers that have no process: the
// pool as it was; the pool as it is, each run taking its statistics once
// when it begins, as a script's run does; and the pool as it is for a
// caller that hands a run none.
func drive() []*drivenPool {
	starter := func(fail *bool) func(context.Context, pythonSpec) (*pythonWorker, error) {
		return func(_ context.Context, spec pythonSpec) (*pythonWorker, error) {
			if *fail {
				return nil, errors.New("the interpreter did not start")
			}
			return fakeWorker(spec), nil
		}
	}
	old := newOraclePool()
	oldFails := new(bool)
	old.start = starter(oldFails)
	pools := []*drivenPool{{
		what:      "the pool as it was",
		acquire:   func(spec pythonSpec) (*pythonWorker, bool, error) { return old.acquire(context.Background(), spec) },
		release:   old.release,
		discard:   old.discard,
		record:    func(spec pythonSpec, outcome string) { old.recordRun(spec.Collector, outcome) },
		reap:      old.reapIdle,
		retain:    old.Retain,
		limit:     old.SetMaxWorkers,
		one:       old.Snapshot,
		whole:     old.PoolSnapshot,
		given:     func(spec pythonSpec) pythonSpec { return spec },
		failStart: oldFails,
		locked: func(do func(map[string][]*pythonWorker, int, int)) {
			old.mu.Lock()
			defer old.mu.Unlock()
			do(old.idle, old.live, old.maxWorkers)
		},
	}}
	for _, handed := range []bool{true, false} {
		pool := newPythonPool()
		fails := new(bool)
		pool.start = starter(fails)
		driven := &drivenPool{
			what:      "the pool, its runs handed no statistics",
			acquire:   func(spec pythonSpec) (*pythonWorker, bool, error) { return pool.acquire(context.Background(), spec) },
			release:   pool.release,
			discard:   pool.discard,
			record:    func(spec pythonSpec, outcome string) { pool.recordRun(pool.Stats(spec.Collector), outcome) },
			reap:      pool.reapIdle,
			retain:    pool.Retain,
			limit:     pool.SetMaxWorkers,
			one:       pool.Snapshot,
			whole:     pool.PoolSnapshot,
			given:     func(spec pythonSpec) pythonSpec { return spec },
			failStart: fails,
			locked: func(do func(map[string][]*pythonWorker, int, int)) {
				pool.mu.Lock()
				defer pool.mu.Unlock()
				do(pool.idle, pool.live, pool.maxWorkers)
			},
		}
		if handed {
			driven.what = "the pool, each run taking its statistics when it begins"
			driven.given = func(spec pythonSpec) pythonSpec {
				spec.stats = pool.statsOf(nil, spec.Collector)
				return spec
			}
			driven.record = func(spec pythonSpec, outcome string) { pool.recordRun(spec.stats, outcome) }
		}
		pools = append(pools, driven)
	}
	return pools
}

// While no statistics are retired, which only a reload that removes a
// collector does, the pool counts what it counted before a collector's
// statistics followed a reload: over generated sequences of everything that
// counts, the statistics of every collector and of the pool as a whole are,
// after each event, those of the pool as it was (oraclePool), for a run that
// takes its statistics once when it begins, as every script's run now does,
// and for one that is handed none. The events are runs that begin, finding
// an idle worker or starting one, which may fail; runs that end well, their
// worker kept, retired at its thousandth run or surplus; runs that end
// badly, their worker stopped for each reason there is; idle workers that
// die, are reaped, are evicted for another script under a limit that
// changes, or are stopped by a reload that keeps some scripts and not
// others; and bursts of runs of one script that leave more workers than the
// pool keeps idle.
func TestThePoolCountsAsItDidWhileNoStatisticsAreRetired(t *testing.T) {
	collectors := []string{"first", "second", "third"}
	var specs []pythonSpec
	for _, name := range collectors {
		for _, scripts := range []string{"one", "two"} {
			specs = append(specs, pythonSpec{Path: "python3", Collector: name, Scripts: scripts})
		}
	}
	bad := []struct{ reason, outcome string }{
		{pythonStopTimeout, pythonRunTimeout}, {pythonStopDeadline, pythonRunDeadline}, {pythonStopCrash, pythonRunFailed},
		{pythonStopOutputLimit, pythonRunOutputLimit}, {pythonStopCancelled, pythonRunFailed},
	}
	base := time.Now()
	events := map[string]int{}
	for seed := range uint64(alloctest.UnlessRaced(20, 6)) {
		rng := rand.New(rand.NewPCG(seed, 39))
		pools := drive()
		oracle := pools[0]
		tick := 0
		compare := func(event string) {
			t.Helper()
			events[event]++
			was := oracle.whole()
			for _, pool := range pools[1:] {
				if got := pool.whole(); !reflect.DeepEqual(got, was) {
					t.Fatalf("seed %d, after %s: %s counts as a whole %+v, and the pool as it was %+v", seed, event, pool.what, got, was)
				}
				for _, name := range collectors {
					if got, want := pool.one(name), oracle.one(name); !reflect.DeepEqual(got, want) {
						t.Fatalf("seed %d, after %s: %s counts for %s %+v, and the pool as it was %+v", seed, event, pool.what, name, got, want)
					}
				}
			}
		}
		for range 300 {
			switch choice := rng.IntN(20); {
			case choice < 8:
				spec := specs[rng.IntN(len(specs))]
				fails := rng.IntN(8) == 0
				// A run that might wait for a worker is not begun: nothing
				// counts while it waits. None waits while fewer workers are
				// busy than the limit allows alive, whatever is idle, which
				// the run takes, finds dead or evicts.
				room := true
				oracle.locked(func(idle map[string][]*pythonWorker, live, limit int) {
					busy := live
					for _, workers := range idle {
						busy -= len(workers)
					}
					room = limit <= 0 || busy < limit
				})
				if !room {
					continue
				}
				var first bool
				var failed error
				for i, pool := range pools {
					*pool.failStart = fails
					given := pool.given(spec)
					worker, reused, err := pool.acquire(given)
					if i == 0 {
						first, failed = reused, err
					} else if reused != first || (err == nil) != (failed == nil) {
						t.Fatalf("seed %d: %s gave a run of %s a worker reused %v, %v, and the pool as it was reused %v, %v", seed, pool.what, spec.Collector, reused, err, first, failed)
					}
					if err == nil {
						pool.runs = append(pool.runs, drivenRun{spec: given, worker: worker})
					}
				}
				switch {
				case failed != nil:
					compare("a worker failing to start")
				case first:
					compare("a run taking an idle worker")
				default:
					compare("a run starting a worker")
				}
			case choice < 14:
				if len(oracle.runs) == 0 {
					continue
				}
				which := rng.IntN(len(oracle.runs))
				outcome := []string{pythonRunOK, pythonRunOK, pythonRunScriptError}[rng.IntN(3)]
				last := rng.IntN(12) == 0
				tick++
				for _, pool := range pools {
					run := pool.runs[which]
					pool.runs = append(pool.runs[:which:which], pool.runs[which+1:]...)
					if last {
						run.worker.runs = pythonWorkerMaxRuns - 1
					}
					pool.release(run.spec, run.worker)
					pool.record(run.spec, outcome)
					// Workers go idle at times apart, in the same order in
					// every pool, so the one idle for longest is the same.
					pool.locked(func(map[string][]*pythonWorker, int, int) { run.worker.idleSince = base.Add(time.Duration(tick)) })
				}
				if last {
					compare("a worker's thousandth run")
				} else {
					compare("a run ending " + outcome)
				}
			case choice < 16:
				if len(oracle.runs) == 0 {
					continue
				}
				which, how := rng.IntN(len(oracle.runs)), bad[rng.IntN(len(bad))]
				for _, pool := range pools {
					run := pool.runs[which]
					pool.runs = append(pool.runs[:which:which], pool.runs[which+1:]...)
					pool.discard(run.worker, how.reason)
					pool.record(run.spec, how.outcome)
				}
				compare("a worker stopped for " + how.reason)
			case choice < 17:
				// An idle worker dies: the same one in every pool, by the
				// script it is of and its place among that script's.
				key, place := "", rng.IntN(pythonWorkerMaxIdle)
				oracle.locked(func(idle map[string][]*pythonWorker, _, _ int) {
					keys := model.SortedKeys(idle)
					if len(keys) > 0 {
						key = keys[rng.IntN(len(keys))]
					}
				})
				for _, pool := range pools {
					pool.locked(func(idle map[string][]*pythonWorker, _, _ int) {
						if workers := idle[key]; place < len(workers) {
							select {
							case <-workers[place].exited:
							default:
								close(workers[place].exited)
							}
						}
					})
				}
				compare("an idle worker dying")
			case choice < 18:
				for _, pool := range pools {
					pool.reap(base.Add(pythonWorkerIdleTimeout + time.Hour))
				}
				compare("the idle timeout")
			case choice < 19 && rng.IntN(2) == 0:
				keys := map[string]bool{}
				for _, spec := range specs {
					if rng.IntN(3) > 0 {
						keys[spec.key()] = true
					}
				}
				for _, pool := range pools {
					pool.retain(keys)
				}
				compare("a reload")
			case choice < 19:
				limit := []int{0, 2, 3, 6}[rng.IntN(4)]
				for _, pool := range pools {
					pool.limit(limit)
				}
				compare("another limit")
			default:
				// A burst: more runs of one script at once than the pool
				// keeps workers idle for, which all end well.
				spec := specs[rng.IntN(len(specs))]
				unbounded := false
				oracle.locked(func(_ map[string][]*pythonWorker, _, limit int) { unbounded = limit <= 0 })
				if !unbounded {
					continue
				}
				for _, pool := range pools {
					*pool.failStart = false
					var burst []drivenRun
					for range pythonWorkerMaxIdle + 2 {
						given := pool.given(spec)
						worker, _, err := pool.acquire(given)
						if err != nil {
							t.Fatal(err)
						}
						burst = append(burst, drivenRun{spec: given, worker: worker})
					}
					for i, run := range burst {
						pool.release(run.spec, run.worker)
						pool.record(run.spec, pythonRunOK)
						pool.locked(func(map[string][]*pythonWorker, int, int) {
							run.worker.idleSince = base.Add(time.Duration(tick + 1 + i))
						})
					}
				}
				tick += pythonWorkerMaxIdle + 2
				compare("a burst that leaves surplus workers")
			}
		}
	}
	// The sequences reached every way a worker stops and a run ends.
	var missing []string
	for _, event := range []string{
		"a worker failing to start", "a run taking an idle worker", "a run starting a worker", "a worker's thousandth run",
		"a run ending " + pythonRunOK, "a run ending " + pythonRunScriptError, "an idle worker dying", "the idle timeout", "a reload", "another limit",
		"a burst that leaves surplus workers",
		"a worker stopped for " + pythonStopTimeout, "a worker stopped for " + pythonStopDeadline, "a worker stopped for " + pythonStopCrash,
		"a worker stopped for " + pythonStopOutputLimit, "a worker stopped for " + pythonStopCancelled,
	} {
		if events[event] == 0 {
			missing = append(missing, event)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the sequences never had %v", missing)
	}
}

// Runs and reloads at once, under the race detector: runs of a collector
// that stays and of one that reloads remove and add again go on while the
// second's statistics are retired and made anew, and its script's workers
// stopped. Every run is counted once, for the pool as a whole; the collector
// that stays has counted every one of its own; and when all have ended no
// worker is counted as starting or busy.
func TestRunsAndRetiringTogetherCountEveryRunOnce(t *testing.T) {
	usePythonPool(t)
	pool := PythonWorkers()
	scriptWorkers(pool)
	const runners, each = 4, 25
	var running sync.WaitGroup
	failures := make(chan error, 2*runners*each)
	for range runners {
		for _, name := range []string{"kept", "gone"} {
			running.Add(1)
			go func() {
				defer running.Done()
				for range each {
					if err := <-runScripted(name); err != nil {
						failures <- err
					}
				}
			}()
		}
	}
	stop, reloaded := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(reloaded)
		gone := pythonWorkerSpec("python3", workerCollector("gone", `metric(name="v", value=1)`)).key()
		kept := pythonWorkerSpec("python3", workerCollector("kept", `metric(name="v", value=1)`)).key()
		for {
			select {
			case <-stop:
				return
			default:
			}
			pool.Retain(map[string]bool{kept: true})
			pool.Retire(map[string]bool{"gone": true})
			pool.Retain(map[string]bool{kept: true, gone: true})
			pool.Stats("gone")
			pool.PoolSnapshot()
			pool.Snapshot("gone")
		}
	}()
	running.Wait()
	close(stop)
	<-reloaded
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	whole := pool.PoolSnapshot()
	if got := whole.Runs[pythonRunOK]; got != 2*runners*each || len(whole.Runs) != 1 {
		t.Errorf("the pool as a whole counts the runs %v, want %d that ended ok", whole.Runs, 2*runners*each)
	}
	if whole.Starting != 0 || whole.Busy != 0 {
		t.Errorf("the pool counts %d workers starting and %d busy when every run has ended", whole.Starting, whole.Busy)
	}
	if got := pool.Snapshot("kept").Runs[pythonRunOK]; got != runners*each {
		t.Errorf("the collector that stayed counts %d runs, want its %d", got, runners*each)
	}
	if got := pool.Snapshot("gone"); got.Runs[pythonRunOK] > runners*each || got.Starting != 0 || got.Busy != 0 {
		t.Errorf("the collector added again counts %+v, want no more runs than the %d of its name and no worker starting or busy", got, runners*each)
	}
	if got := pool.StatsKept(); got != 2 {
		t.Errorf("the pool keeps the statistics of %d collectors, want the two there are", got)
	}
}
