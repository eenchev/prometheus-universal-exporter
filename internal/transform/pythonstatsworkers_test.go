package transform

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A worker belongs to the statistics it was started for (PythonStats,
// PythonPool.acquire): the tests here hold the pool to it over reloads that
// remove a collector and bring it back under its name with the script it
// had, when the workers of the removed collector and of the one added again
// are kept for one script. The pool is driven by hand with workers that have
// no process (fakeWorker), in the order the exporter drives it: the
// configuration manager's Retain of the scripts in use, then the server's
// Retire of the names a reload removed, and Stats when a trip of a collector
// that is there takes its worker statistics.

// stopsOf is how many workers a snapshot counts as stopped, for whatever
// reason.
func stopsOf(snap pythonWorkerSnapshot) (total uint64) {
	for _, n := range snap.Stops {
		total += n
	}
	return total
}

// balanced says what is wrong with a snapshot whose workers do not add up,
// and nothing for one whose do: the workers started less those stopped are
// the idle and the busy ones, and none is counted below zero.
func balanced(snap pythonWorkerSnapshot) string {
	alive := int64(snap.Starts) - int64(stopsOf(snap))
	if alive != int64(snap.Idle+snap.Busy) || snap.Idle < 0 || snap.Busy < 0 || snap.Starting < 0 {
		return fmt.Sprintf("%d workers started and %d stopped (%v), which leaves %d, and %d are shown idle, %d busy and %d starting", snap.Starts, stopsOf(snap), snap.Stops, alive, snap.Idle, snap.Busy, snap.Starting)
	}
	return ""
}

// wentBack names a counter of now that is less than it was, and nothing
// when none is.
func wentBack(now, was pythonWorkerSnapshot) string {
	if now.Starts < was.Starts {
		return "the workers started"
	}
	if now.StartFailures < was.StartFailures {
		return "the starts that failed"
	}
	for reason, n := range was.Stops {
		if now.Stops[reason] < n {
			return "the workers stopped for " + reason
		}
	}
	for outcome, n := range was.Runs {
		if now.Runs[outcome] < n {
			return "the runs that ended " + outcome
		}
	}
	return ""
}

// A collector is removed while its script runs and is back, as it was,
// before the script ends: two reloads during one run. The script is in use
// again when the run ends, so the worker is not one of a script a reload
// removed; it is one of a collector a reload removed, and is stopped when
// its run ends, for the reload, under no collector's name. The collector
// added again is not given it: its first run starts a worker, and whatever
// stops that one is counted for the collector that started it, whose series
// add up at every step. Before a worker belonged to its statistics, the
// collector added again was given the removed collector's worker, showed it
// idle with no worker started, and counted its stop.
func TestAWorkerOfARemovedCollectorIsStoppedWhenItsRunEndsAndGivenToNoOther(t *testing.T) {
	for _, reason := range []string{pythonStopIdle, pythonStopTimeout, pythonStopReload, pythonStopRetired, pythonStopEvicted, pythonStopCrash} {
		t.Run(reason, func(t *testing.T) {
			usePythonPool(t)
			pool := PythonWorkers()
			pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) { return fakeWorker(spec), nil }
			removed := pythonSpec{Path: "python3", Collector: "gone", Scripts: "same"}
			removed.stats = pool.Stats("gone")
			worker, _, err := pool.acquire(context.Background(), removed)
			if err != nil {
				t.Fatal(err)
			}
			// The script runs. A reload removes the collector, and the next
			// brings it back as it was.
			pool.Retain(nil)
			pool.Retire(map[string]bool{"gone": true})
			pool.Retain(map[string]bool{removed.key(): true})
			again := pythonSpec{Path: "python3", Collector: "gone", Scripts: "same"}
			again.stats = pool.Stats("gone")
			// The removed collector's run ends.
			pool.release(removed, worker)
			pool.recordRun(removed.stats, pythonRunOK)
			noWorkerStatistics(t, "when the removed collector's run has ended", pool, "gone")
			want := pythonWorkerSnapshot{Starts: 1, Stops: map[string]uint64{pythonStopReload: 1}, Runs: map[string]uint64{pythonRunOK: 1}}
			if got := pool.PoolSnapshot(); !reflect.DeepEqual(got, want) {
				t.Fatalf("the pool as a whole counts %+v when the removed collector's run has ended, want %+v: its worker stopped for the reload", got, want)
			}
			// A trip of the collector added again runs its script.
			taken, reused, err := pool.acquire(context.Background(), again)
			if err != nil || reused || taken == worker {
				t.Fatalf("the collector added again was given the removed collector's worker (reused %v), or none: %v", reused, err)
			}
			if got, want := pool.Snapshot("gone"), (pythonWorkerSnapshot{Starts: 1, Busy: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{}}); !reflect.DeepEqual(got, want) {
				t.Errorf("the collector added again counts %+v while its first script runs, want %+v", got, want)
			}
			switch reason {
			case pythonStopIdle:
				pool.release(again, taken)
				pool.reapIdle(time.Now().Add(pythonWorkerIdleTimeout + time.Hour))
			case pythonStopTimeout:
				pool.discard(taken, pythonStopTimeout)
			case pythonStopReload:
				// A reload that changes the collector's script, which keeps
				// its counters.
				pool.release(again, taken)
				pool.Retain(nil)
			case pythonStopRetired:
				taken.runs = pythonWorkerMaxRuns - 1
				pool.release(again, taken)
			case pythonStopEvicted:
				pool.release(again, taken)
				pool.SetMaxWorkers(1)
				if _, _, err := pool.acquire(context.Background(), pythonSpec{Path: "python3", Collector: "other"}); err != nil {
					t.Fatal(err)
				}
			case pythonStopCrash:
				pool.release(again, taken)
				close(taken.exited)
				// Its next run finds the worker dead, and starts another.
				next, reused, err := pool.acquire(context.Background(), again)
				if err != nil || reused {
					t.Fatalf("the collector added again was given its dead worker (%v), or none: %v", reused, err)
				}
				pool.discard(next, pythonStopCancelled)
			}
			got := pool.Snapshot("gone")
			if got.Stops[reason] == 0 || stopsOf(got) != got.Starts || got.Idle != 0 || got.Busy != 0 {
				t.Errorf("the collector added again counts %+v, want every worker it started stopped, the first for %s, and none left", got, reason)
			}
			if wrong := balanced(got); wrong != "" {
				t.Errorf("the collector added again counts %s", wrong)
			}
		})
	}
}

// The other way round: a trip of the removed collector that is still under
// way when the collector is back, with a second script to run (a pre-script
// and a python transform, or the next file of a directory), is not given the
// idle worker the collector added again started: it starts one, which is
// stopped when its script ends, and the worker of the collector added again
// stays idle in that one's series whatever becomes of the other run. Before,
// the run took the worker, which then counted for no collector, and the
// collector added again was left with a worker started, none stopped and
// none alive, for good.
func TestAWorkerOfTheCollectorAddedAgainIsNotGivenToARunOfTheRemovedOne(t *testing.T) {
	for _, took := range []string{"the collector's statistics, retired since", "the pool's departed"} {
		for _, ends := range []string{"well", "with its script timing out"} {
			t.Run(took+", ending "+ends, func(t *testing.T) {
				usePythonPool(t)
				pool := PythonWorkers()
				pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) { return fakeWorker(spec), nil }
				removed := pythonSpec{Path: "python3", Collector: "gone", Scripts: "same"}
				// The trip of the collector takes its statistics, for both
				// its scripts, or reads a collector removed before it took
				// any.
				removed.stats = pool.Stats("gone")
				pool.Retain(nil)
				pool.Retire(map[string]bool{"gone": true})
				if took == "the pool's departed" {
					removed.stats = pool.Departed()
				}
				pool.Retain(map[string]bool{removed.key(): true})
				// The collector added again runs its script once, in a worker
				// it starts.
				again := pythonSpec{Path: "python3", Collector: "gone", Scripts: "same"}
				again.stats = pool.Stats("gone")
				own, reused, err := pool.acquire(context.Background(), again)
				if err != nil || reused {
					t.Fatalf("reused %v, %v", reused, err)
				}
				pool.release(again, own)
				pool.recordRun(again.stats, pythonRunOK)
				want := pythonWorkerSnapshot{Starts: 1, Idle: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{pythonRunOK: 1}}
				if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
					t.Fatalf("the collector added again counts %+v, want %+v", got, want)
				}
				// The removed collector's trip runs its script.
				taken, reused, err := pool.acquire(context.Background(), removed)
				if err != nil || reused || taken == own {
					t.Fatalf("the removed collector's run was given the worker of the collector added again (reused %v), or none: %v", reused, err)
				}
				if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
					t.Errorf("the collector added again counts %+v while the removed collector's script runs, want %+v", got, want)
				}
				whole := pythonWorkerSnapshot{Starts: 2, Stops: map[string]uint64{}, Runs: map[string]uint64{pythonRunOK: 1}}
				if ends == "well" {
					pool.release(removed, taken)
					pool.recordRun(removed.stats, pythonRunOK)
					whole.Stops[pythonStopReload], whole.Runs[pythonRunOK] = 1, 2
				} else {
					pool.discard(taken, pythonStopTimeout)
					pool.recordRun(removed.stats, pythonRunTimeout)
					whole.Stops[pythonStopTimeout], whole.Runs[pythonRunTimeout] = 1, 1
				}
				if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
					t.Errorf("the collector added again counts %+v when the removed collector's script has ended, want %+v", got, want)
				}
				whole.Idle = 1
				if got := pool.PoolSnapshot(); !reflect.DeepEqual(got, whole) {
					t.Errorf("the pool as a whole counts %+v, want %+v: the removed collector's run in a worker of its own, stopped when it ended", got, whole)
				}
				// The next run of the collector added again takes its worker.
				if next, reused, err := pool.acquire(context.Background(), again); err != nil || !reused || next != own {
					t.Errorf("the collector added again was not given its idle worker: reused %v, %v", reused, err)
				}
			})
		}
	}
}

// --python.max-workers holds for the workers of a removed collector as for
// any other. A run of a removed collector that is under way holds its place:
// the first run of the collector added again, with the pool at its limit,
// waits in line for it. When the removed collector's run ends its worker is
// stopped, which frees the place and wakes the run that waits, as a worker
// going idle would have: that run starts the collector's own worker, and
// counts it. A run of a removed collector that itself waits is let through
// by the collector's worker going idle, which it evicts for one of its own.
func TestAWorkerOfARemovedCollectorHoldsItsPlaceUnderTheLimitUntilItsRunEnds(t *testing.T) {
	usePythonPool(t)
	pool := PythonWorkers()
	workers := scriptWorkers(pool)
	pool.SetMaxWorkers(1)
	key := pythonWorkerSpec("python3", workerCollector("gone", `metric(name="v", value=1)`)).key()
	workers.holdRuns.Store(true)
	removed := runScripted("gone")
	held := <-workers.runs
	pool.Retain(nil)
	pool.Retire(map[string]bool{"gone": true})
	pool.Retain(map[string]bool{key: true})
	again := runScripted("gone")
	testutil.WaitFor(t, "the run of the collector added again to wait for a worker", func() bool { return pool.PoolSnapshot().Waiting == 1 })
	if got, want := pool.Snapshot("gone"), (pythonWorkerSnapshot{Stops: map[string]uint64{}, Runs: map[string]uint64{}}); !reflect.DeepEqual(got, want) {
		t.Errorf("the collector added again counts %+v while its run waits, want nothing", got)
	}
	held <- scriptedOK
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	// The run that waited goes on in a worker of its own.
	own := <-workers.runs
	want := pythonWorkerSnapshot{Starts: 1, Busy: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{}}
	if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
		t.Errorf("the collector added again counts %+v while its script runs, want %+v", got, want)
	}
	// A second script of the removed collector's trip waits in its turn.
	workers.holdRuns.Store(false)
	late := make(chan error, 1)
	go func() {
		spec := pythonSpec{Path: "python3", Collector: "gone", Scripts: "late", stats: pool.Departed()}
		worker, reused, err := pool.acquire(context.Background(), spec)
		if err == nil && reused {
			err = errors.New("the removed collector's run was given a worker another had started")
		}
		if err == nil {
			pool.release(spec, worker)
		}
		late <- err
	}()
	testutil.WaitFor(t, "the run of the removed collector to wait for a worker", func() bool { return pool.PoolSnapshot().Waiting == 1 })
	own <- scriptedOK
	if err := <-again; err != nil {
		t.Fatal(err)
	}
	if err := <-late; err != nil {
		t.Fatal(err)
	}
	want = pythonWorkerSnapshot{Starts: 1, Stops: map[string]uint64{pythonStopEvicted: 1}, Runs: map[string]uint64{pythonRunOK: 1}}
	if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
		t.Errorf("the collector added again counts %+v, want %+v: its worker evicted for the removed collector's run", got, want)
	}
	whole := pythonWorkerSnapshot{Starts: 3, Stops: map[string]uint64{pythonStopReload: 2, pythonStopEvicted: 1}, Runs: map[string]uint64{pythonRunOK: 2}}
	if got := pool.PoolSnapshot(); !reflect.DeepEqual(got, whole) {
		t.Errorf("the pool as a whole counts %+v, want %+v", got, whole)
	}
}

// reloadTrip is a trip of a collector as the exporter makes it: it took the
// statistics its scripts count in once, and runs a script now and then, in
// worker while one runs.
type reloadTrip struct {
	spec   pythonSpec
	worker *pythonWorker
}

// What a collector's series say holds together whatever the reloads do, and
// whenever: over generated sequences of events, after every one, the workers
// a collector has started less those stopped under its name are the workers
// its state series show, idle and busy, and none is below zero; so it is for
// the pool as a whole, no counter of which ever goes back; no worker is idle
// whose statistics are retired; and the pool keeps statistics for no more
// collectors than there are. When every run has ended and a reload has
// removed every script, each collector has stopped every worker it started.
//
// The events are those of the exporter, each on its own: a trip that takes
// its collector's statistics and runs a script, the one in use or the one a
// reload has replaced since the trip read it; a trip that began earlier and
// runs its next script, its collector removed since or not; a trip of a
// collector removed before the trip took any statistics; scripts that end
// well, or with their worker stopped, or at their worker's thousandth run;
// workers that fail to start; the configuration manager putting in force a
// configuration without a collector, with it again, or with another script
// for it (Retain); the server following that, some events later or at once,
// and retiring the statistics of the collectors that are gone (Retire); the
// idle timeout; an idle worker dying; --python.max-workers changing; and
// bursts that leave surplus workers.
func TestACollectorsWorkerSeriesAddUpWhateverTheReloadsDo(t *testing.T) {
	names, scripts := []string{"first", "second", "third"}, []string{"one", "two"}
	specOf := func(name, script string) pythonSpec {
		return pythonSpec{Path: "python3", Collector: name, Scripts: script}
	}
	bad := []struct{ reason, outcome string }{
		{pythonStopTimeout, pythonRunTimeout}, {pythonStopDeadline, pythonRunDeadline}, {pythonStopCrash, pythonRunFailed},
		{pythonStopOutputLimit, pythonRunOutputLimit}, {pythonStopCancelled, pythonRunFailed},
	}
	events := map[string]int{}
	for seed := range uint64(60) {
		rng := rand.New(rand.NewPCG(seed, 42))
		pool := newPythonPool()
		fails := false
		pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) {
			if fails {
				return nil, errors.New("the interpreter did not start")
			}
			return fakeWorker(spec), nil
		}
		// inUse is the script each collector has in the configuration in
		// force, and followed the collectors of the configuration the
		// server follows, whose statistics are their own.
		inUse, followed := map[string]string{}, map[string]bool{}
		for _, name := range names {
			inUse[name], followed[name] = "one", true
		}
		var trips []*reloadTrip
		var trace []string
		var was pythonWorkerSnapshot
		failf := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("seed %d: %s, after:\n  %s", seed, fmt.Sprintf(format, args...), strings.Join(trace, "\n  "))
		}
		check := func(event string) {
			t.Helper()
			events[event]++
			trace = append(trace, event)
			whole := pool.PoolSnapshot()
			if wrong := balanced(whole); wrong != "" || whole.Starting != 0 {
				failf("the pool as a whole counts %s%+v", wrong, whole)
			}
			if less := wentBack(whole, was); less != "" {
				failf("%s of the pool as a whole went back: %+v, was %+v", less, whole, was)
			}
			was = whole
			there := 0
			for _, name := range names {
				got := pool.Snapshot(name)
				if wrong := balanced(got); wrong != "" {
					failf("collector %s counts %s", name, wrong)
				}
				if followed[name] {
					there++
				} else if !reflect.DeepEqual(got, pythonWorkerSnapshot{Stops: map[string]uint64{}, Runs: map[string]uint64{}}) {
					failf("collector %s, which is not there, counts %+v", name, got)
				}
			}
			if kept := pool.StatsKept(); kept > there {
				failf("the pool keeps the statistics of %d collectors, and there are %d", kept, there)
			}
			pool.mu.Lock()
			defer pool.mu.Unlock()
			idle := 0
			for _, workers := range pool.idle {
				idle += len(workers)
				for _, worker := range workers {
					if worker.stats.retired {
						failf("a worker of %s is idle, whose statistics are retired", worker.collector)
					}
				}
			}
			if pool.live != idle+whole.Busy {
				failf("the pool holds %d places for %d idle and %d busy workers", pool.live, idle, whole.Busy)
			}
		}
		// room says whether a run that begins now is given a worker without
		// waiting: fewer are busy than the limit allows alive.
		room := func() bool {
			pool.mu.Lock()
			defer pool.mu.Unlock()
			busy := pool.live
			for _, workers := range pool.idle {
				busy -= len(workers)
			}
			return pool.maxWorkers <= 0 || busy < pool.maxWorkers
		}
		retired := func(st *PythonStats) bool {
			pool.mu.Lock()
			defer pool.mu.Unlock()
			return st.retired
		}
		retain := func() {
			keys := map[string]bool{}
			for name, script := range inUse {
				keys[specOf(name, script).key()] = true
			}
			pool.Retain(keys)
		}
		// run begins a script of trip, and says whether a worker started.
		run := func(trip *reloadTrip) bool {
			fails = rng.IntN(8) == 0
			if retired(trip.spec.stats) {
				pool.mu.Lock()
				for _, worker := range pool.idle[trip.spec.key()] {
					if !worker.stats.retired {
						events["a script of a removed collector beginning where the collector added again has a worker idle"]++
						break
					}
				}
				pool.mu.Unlock()
			}
			worker, _, err := pool.acquire(context.Background(), trip.spec)
			fails = false
			if err != nil {
				pool.recordRun(trip.spec.stats, pythonRunFailed)
				return false
			}
			trip.worker = worker
			return true
		}
		running := func(is bool) (out []*reloadTrip) {
			for _, trip := range trips {
				if (trip.worker != nil) == is {
					out = append(out, trip)
				}
			}
			return out
		}
		// over ends a trip one time in two when a script of it has ended.
		over := func(trip *reloadTrip) {
			trip.worker = nil
			if rng.IntN(2) == 0 {
				for i, other := range trips {
					if other == trip {
						trips = append(trips[:i:i], trips[i+1:]...)
					}
				}
			}
		}
		for range 120 {
			name := names[rng.IntN(len(names))]
			switch choice := rng.IntN(21); {
			case choice < 4:
				if !room() {
					continue
				}
				trip := &reloadTrip{spec: specOf(name, scripts[rng.IntN(len(scripts))])}
				event := "a trip of a collector that is there running a script"
				if followed[name] {
					trip.spec.stats = pool.Stats(name)
				} else {
					trip.spec.stats = pool.Departed()
					event = "a trip of a collector removed before it took statistics running a script"
				}
				if !run(trip) {
					check("a worker failing to start")
					continue
				}
				trips = append(trips, trip)
				check(event + ", of " + name)
				events[event]++
			case choice < 6:
				between := running(false)
				if len(between) == 0 || !room() {
					continue
				}
				trip := between[rng.IntN(len(between))]
				event := "a trip that began earlier running its next script"
				if retired(trip.spec.stats) {
					event = "a trip whose collector was removed since running its next script"
				}
				if !run(trip) {
					check("a worker failing to start")
					continue
				}
				check(event + ", of " + trip.spec.Collector)
				events[event]++
			case choice < 10:
				under := running(true)
				if len(under) == 0 {
					continue
				}
				trip := under[rng.IntN(len(under))]
				event := "a script ending"
				if retired(trip.spec.stats) {
					event = "a script of a removed collector ending"
					if inUse[trip.spec.Collector] == trip.spec.Scripts {
						events["a script of a removed collector ending when the script is in use again"]++
					}
				}
				if rng.IntN(12) == 0 {
					trip.worker.runs = pythonWorkerMaxRuns - 1
					event = "a worker's thousandth run"
				}
				pool.release(trip.spec, trip.worker)
				pool.recordRun(trip.spec.stats, pythonRunOK)
				over(trip)
				check(event + ", of " + trip.spec.Collector)
				events[event]++
			case choice < 11:
				under := running(true)
				if len(under) == 0 {
					continue
				}
				trip, how := under[rng.IntN(len(under))], bad[rng.IntN(len(bad))]
				pool.discard(trip.worker, how.reason)
				pool.recordRun(trip.spec.stats, how.outcome)
				over(trip)
				check("a worker of " + trip.spec.Collector + " stopped for " + how.reason)
			case choice < 13:
				// The configuration manager puts another configuration in
				// force, and the pool keeps the scripts it uses.
				script, there := inUse[name]
				switch {
				case !there:
					inUse[name] = scripts[rng.IntN(len(scripts))]
					retain()
					check("a configuration in force with " + name + " again")
				case rng.IntN(3) == 0:
					inUse[name] = scripts[(1+rng.IntN(len(scripts)-1)+indexOf(scripts, script))%len(scripts)]
					retain()
					check("a configuration in force with another script for " + name)
				default:
					delete(inUse, name)
					retain()
					check("a configuration in force without " + name)
				}
				// The server follows it at once, as it does when the reload
				// tells it, two times in three.
				if rng.IntN(3) == 0 {
					continue
				}
				fallthrough
			case choice < 15:
				// The server follows the configuration in force: the
				// statistics of the collectors that are gone are retired.
				gone := map[string]bool{}
				for _, name := range names {
					_, there := inUse[name]
					if followed[name] && !there {
						gone[name] = true
					}
					followed[name] = there
				}
				if len(gone) == 0 {
					continue
				}
				pool.Retire(gone)
				check(fmt.Sprintf("the statistics of %v retired", model.SortedKeys(gone)))
				events["statistics retired"]++
			case choice < 16:
				pool.reapIdle(time.Now().Add(pythonWorkerIdleTimeout + time.Hour))
				check("the idle timeout")
			case choice < 17:
				pool.SetMaxWorkers([]int{0, 2, 3, 6}[rng.IntN(4)])
				check("another limit")
			case choice < 19:
				// An idle worker dies: it is found dead when a run of its
				// statistics is next given it.
				pool.mu.Lock()
				if keys := model.SortedKeys(pool.idle); len(keys) > 0 {
					if workers := pool.idle[keys[rng.IntN(len(keys))]]; len(workers) > 0 {
						worker := workers[rng.IntN(len(workers))]
						select {
						case <-worker.exited:
						default:
							close(worker.exited)
						}
					}
				}
				pool.mu.Unlock()
				check("an idle worker dying")
			default:
				// A burst: more runs of one script at once than the pool
				// keeps workers idle for, which all end well.
				pool.mu.Lock()
				unbounded := pool.maxWorkers <= 0
				pool.mu.Unlock()
				if !unbounded || !followed[name] {
					continue
				}
				spec := specOf(name, scripts[rng.IntN(len(scripts))])
				spec.stats = pool.Stats(name)
				var burst []*pythonWorker
				for range pythonWorkerMaxIdle + 2 {
					worker, _, err := pool.acquire(context.Background(), spec)
					if err != nil {
						t.Fatal(err)
					}
					burst = append(burst, worker)
				}
				for _, worker := range burst {
					pool.release(spec, worker)
					pool.recordRun(spec.stats, pythonRunOK)
				}
				check("a burst of " + name + " that leaves surplus workers")
				events["a burst that leaves surplus workers"]++
			}
		}
		// Every run ends, and a reload removes every script.
		for _, trip := range running(true) {
			pool.release(trip.spec, trip.worker)
			pool.recordRun(trip.spec.stats, pythonRunOK)
			trip.worker = nil
		}
		check("every script ending")
		pool.Retain(nil)
		check("a configuration in force with no script")
		if whole := pool.PoolSnapshot(); whole.Idle != 0 || whole.Busy != 0 || stopsOf(whole) != whole.Starts {
			failf("the pool as a whole counts %+v, want every worker it started stopped", whole)
		}
		for _, name := range names {
			if got := pool.Snapshot(name); got.Idle != 0 || got.Busy != 0 || stopsOf(got) != got.Starts {
				failf("collector %s counts %+v, want every worker it started stopped", name, got)
			}
		}
	}
	// The sequences reached what a worker that belongs to no statistics
	// would have got wrong, and every kind of event.
	var missing []string
	for _, event := range []string{
		"a script of a removed collector ending when the script is in use again",
		"a script of a removed collector beginning where the collector added again has a worker idle",
		"a trip of a collector that is there running a script", "a trip of a collector removed before it took statistics running a script",
		"a trip that began earlier running its next script", "a trip whose collector was removed since running its next script",
		"a script ending", "a script of a removed collector ending", "a worker's thousandth run", "a worker failing to start",
		"statistics retired", "the idle timeout", "another limit", "an idle worker dying", "a burst that leaves surplus workers",
	} {
		if events[event] == 0 {
			missing = append(missing, event)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the sequences never had %v", missing)
	}
}

// indexOf is where value is in values.
func indexOf(values []string, value string) int {
	for i, v := range values {
		if v == value {
			return i
		}
	}
	return 0
}

// Outside what a worker belonging to its statistics is about, the pool
// counts as a whole what it counted before (oraclePool), which retired
// nothing and gave a worker to whatever run asked for its script: after
// every event of generated sequences in which collectors are removed, their
// statistics retired, and added again, the pool as it is and the pool as it
// was count the same for the pool as a whole, worker for worker and run for
// run, the same for every collector that was never removed, and no counter
// goes back. What the two differ in is left out of the sequences, and held
// by the tests above: a collector is added again only when no script of the
// collector removed under its name is still running, and a removed collector
// begins no script. The workers of a removed collector that are busy at the
// reload are then stopped when their runs end by both, for the reload, and
// the idle ones by the reload itself.
//
// The events are runs that begin, of the script in use or of the one a
// reload has just replaced, finding an idle worker or starting one, which
// may fail; runs that end well, their worker kept, retired at its thousandth
// run or surplus; runs that end badly, for each reason there is; the idle
// timeout; a --python.max-workers that changes, with the evictions it makes;
// and reloads that change a script, remove a collector or add one again.
func TestThePoolAsAWholeCountsAsItDidWhereNoWorkerOutlivesItsCollector(t *testing.T) {
	names, scripts := []string{"first", "second", "third"}, []string{"one", "two"}
	specOf := func(name, script string) pythonSpec {
		return pythonSpec{Path: "python3", Collector: name, Scripts: script}
	}
	bad := []struct{ reason, outcome string }{
		{pythonStopTimeout, pythonRunTimeout}, {pythonStopDeadline, pythonRunDeadline}, {pythonStopCrash, pythonRunFailed},
		{pythonStopOutputLimit, pythonRunOutputLimit}, {pythonStopCancelled, pythonRunFailed},
	}
	base := time.Now()
	events := map[string]int{}
	for seed := range uint64(40) {
		rng := rand.New(rand.NewPCG(seed, 43))
		old, pool := newOraclePool(), newPythonPool()
		fails := false
		start := func(_ context.Context, spec pythonSpec) (*pythonWorker, error) {
			if fails {
				return nil, errors.New("the interpreter did not start")
			}
			return fakeWorker(spec), nil
		}
		old.start, pool.start = start, start
		// A run, in the pool as it was and in the pool as it is; removed
		// says that a reload has removed its collector since it began.
		type run struct {
			spec    pythonSpec
			was, is *pythonWorker
			removed bool
		}
		var runs []*run
		inUse, everRemoved := map[string]string{}, map[string]bool{}
		for _, name := range names {
			inUse[name] = "one"
		}
		var trace []string
		var before pythonWorkerSnapshot
		tick := 0
		compare := func(event string) {
			t.Helper()
			events[event]++
			trace = append(trace, event)
			was, got := old.PoolSnapshot(), pool.PoolSnapshot()
			if !reflect.DeepEqual(got, was) {
				t.Fatalf("seed %d: the pool as a whole counts %+v, and the pool as it was %+v, after:\n  %s", seed, got, was, strings.Join(trace, "\n  "))
			}
			if less := wentBack(got, before); less != "" {
				t.Fatalf("seed %d: %s of the pool as a whole went back: %+v, was %+v", seed, less, got, before)
			}
			before = got
			for _, name := range names {
				if everRemoved[name] {
					continue
				}
				if got, was := pool.Snapshot(name), old.Snapshot(name); !reflect.DeepEqual(got, was) {
					t.Fatalf("seed %d: the pool counts %+v for %s, which no reload removed, and the pool as it was %+v, after:\n  %s", seed, got, name, was, strings.Join(trace, "\n  "))
				}
			}
		}
		retain := func() {
			keys := map[string]bool{}
			for name, script := range inUse {
				keys[specOf(name, script).key()] = true
			}
			old.Retain(keys)
			pool.Retain(keys)
		}
		// ended takes a run from those under way.
		ended := func() *run {
			which := rng.IntN(len(runs))
			r := runs[which]
			runs = append(runs[:which:which], runs[which+1:]...)
			return r
		}
		for range 250 {
			name := names[rng.IntN(len(names))]
			switch choice := rng.IntN(18); {
			case choice < 6:
				script, there := inUse[name]
				if !there {
					continue
				}
				if rng.IntN(4) == 0 {
					// A trip that read the script before a reload changed
					// it.
					script = scripts[(1+indexOf(scripts, script))%len(scripts)]
				}
				// A run that might wait for a worker is not begun.
				old.mu.Lock()
				busy := old.live
				for _, workers := range old.idle {
					busy -= len(workers)
				}
				room := old.maxWorkers <= 0 || busy < old.maxWorkers
				old.mu.Unlock()
				if !room {
					continue
				}
				spec := specOf(name, script)
				given := spec
				given.stats = pool.Stats(name)
				fails = rng.IntN(8) == 0
				was, wasReused, wasErr := old.acquire(context.Background(), spec)
				is, reused, err := pool.acquire(context.Background(), given)
				fails = false
				if reused != wasReused || (err == nil) != (wasErr == nil) {
					t.Fatalf("seed %d: the pool gave a run of %s a worker reused %v, %v, and the pool as it was reused %v, %v, after:\n  %s", seed, name, reused, err, wasReused, wasErr, strings.Join(trace, "\n  "))
				}
				switch {
				case err != nil:
					old.recordRun(name, pythonRunFailed)
					pool.recordRun(given.stats, pythonRunFailed)
					compare("a worker failing to start")
				case reused:
					runs = append(runs, &run{spec: given, was: was, is: is})
					compare("a run taking an idle worker")
				default:
					runs = append(runs, &run{spec: given, was: was, is: is})
					compare("a run starting a worker")
				}
			case choice < 11:
				if len(runs) == 0 {
					continue
				}
				r := ended()
				plain := r.spec
				plain.stats = nil
				event := "a run ending"
				if r.removed {
					event = "a run of a removed collector ending"
				}
				if rng.IntN(12) == 0 {
					r.was.runs, r.is.runs = pythonWorkerMaxRuns-1, pythonWorkerMaxRuns-1
					event = "a worker's thousandth run"
				}
				tick++
				old.release(plain, r.was)
				old.recordRun(plain.Collector, pythonRunOK)
				pool.release(r.spec, r.is)
				pool.recordRun(r.spec.stats, pythonRunOK)
				// Workers go idle at times apart, the same in both pools, so
				// the one idle for longest is the same.
				old.mu.Lock()
				r.was.idleSince = base.Add(time.Duration(tick))
				old.mu.Unlock()
				pool.mu.Lock()
				r.is.idleSince = base.Add(time.Duration(tick))
				pool.mu.Unlock()
				compare(event)
			case choice < 12:
				if len(runs) == 0 {
					continue
				}
				r, how := ended(), bad[rng.IntN(len(bad))]
				old.discard(r.was, how.reason)
				old.recordRun(r.spec.Collector, how.outcome)
				pool.discard(r.is, how.reason)
				pool.recordRun(r.spec.stats, how.outcome)
				compare("a worker stopped for " + how.reason)
			case choice < 13:
				at := base.Add(pythonWorkerIdleTimeout + time.Hour)
				old.reapIdle(at)
				pool.reapIdle(at)
				compare("the idle timeout")
			case choice < 14:
				limit := []int{0, 2, 3, 6}[rng.IntN(4)]
				old.SetMaxWorkers(limit)
				pool.SetMaxWorkers(limit)
				compare("another limit")
			case choice < 15:
				script, there := inUse[name]
				if !there {
					continue
				}
				inUse[name] = scripts[(1+indexOf(scripts, script))%len(scripts)]
				retain()
				compare("a reload changing a script")
			case choice < 17:
				if _, there := inUse[name]; !there {
					continue
				}
				// The reload removes the collector: the scripts in use are
				// kept, and its statistics retired, which the pool as it was
				// never did.
				delete(inUse, name)
				retain()
				pool.Retire(map[string]bool{name: true})
				everRemoved[name] = true
				for _, r := range runs {
					if r.spec.Collector == name {
						r.removed = true
					}
				}
				compare("a reload removing a collector")
			default:
				if _, there := inUse[name]; there {
					continue
				}
				// Left to the tests above: a collector added again while a
				// script of the removed one still runs.
				under := false
				for _, r := range runs {
					under = under || r.spec.Collector == name
				}
				if under {
					continue
				}
				inUse[name] = scripts[rng.IntN(len(scripts))]
				retain()
				compare("a reload adding a collector again")
			}
		}
	}
	var missing []string
	for _, event := range []string{
		"a worker failing to start", "a run taking an idle worker", "a run starting a worker", "a run ending", "a run of a removed collector ending",
		"a worker's thousandth run", "the idle timeout", "another limit", "a reload changing a script", "a reload removing a collector",
		"a reload adding a collector again",
		"a worker stopped for " + pythonStopTimeout, "a worker stopped for " + pythonStopDeadline, "a worker stopped for " + pythonStopCrash,
		"a worker stopped for " + pythonStopOutputLimit, "a worker stopped for " + pythonStopCancelled,
	} {
		if events[event] == 0 {
			missing = append(missing, event)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the sequences never had %v", missing)
	}
}

// A reader finds a collector's statistics without making any (Kept), and
// reads what they count from them (SnapshotOf), whatever is kept under the
// name by then: statistics retired since the reader found them show what
// they had counted, and not what a collector added again under the name has
// counted since. No statistics, and those of another pool, have counted
// nothing. Read by the name, they are what the name's are now (Snapshot).
func TestAReaderReadsTheStatisticsItFoundAndMakesNone(t *testing.T) {
	usePythonPool(t)
	former := PythonWorkers()
	foreign := former.Stats("gone")
	usePythonPool(t)
	pool := PythonWorkers()
	pool.start = func(_ context.Context, spec pythonSpec) (*pythonWorker, error) { return fakeWorker(spec), nil }
	if got := pool.Kept("gone"); got != nil || pool.StatsKept() != 0 {
		t.Fatalf("asking for the statistics of a collector that has run nothing gave %v, and the pool keeps those of %d collectors", got, pool.StatsKept())
	}
	nothing := pythonWorkerSnapshot{Stops: map[string]uint64{}, Runs: map[string]uint64{}}
	for what, st := range map[string]*PythonStats{"no statistics": nil, "statistics of another pool": foreign} {
		if got := pool.SnapshotOf(st); !reflect.DeepEqual(got, nothing) {
			t.Errorf("%s count %+v, want nothing", what, got)
		}
	}
	spec := pythonSpec{Path: "python3", Collector: "gone"}
	spec.stats = pool.Stats("gone")
	worker, _, err := pool.acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	pool.release(spec, worker)
	pool.recordRun(spec.stats, pythonRunOK)
	found := pool.Kept("gone")
	want := pythonWorkerSnapshot{Starts: 1, Idle: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{pythonRunOK: 1}}
	if got := pool.SnapshotOf(found); found != spec.stats || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(pool.Snapshot("gone"), want) {
		t.Fatalf("the statistics the reader found count %+v, want %+v, those of the collector", got, want)
	}
	// The collector is removed and added again, and runs its script twice.
	pool.Retain(nil)
	pool.Retire(map[string]bool{"gone": true})
	again := pythonSpec{Path: "python3", Collector: "gone"}
	again.stats = pool.Stats("gone")
	for range 2 {
		worker, _, err := pool.acquire(context.Background(), again)
		if err != nil {
			t.Fatal(err)
		}
		pool.release(again, worker)
		pool.recordRun(again.stats, pythonRunOK)
	}
	want = pythonWorkerSnapshot{Starts: 1, Stops: map[string]uint64{pythonStopReload: 1}, Runs: map[string]uint64{pythonRunOK: 1}}
	if got := pool.SnapshotOf(found); !reflect.DeepEqual(got, want) {
		t.Errorf("the statistics the reader found count %+v once they are retired, want %+v, what the removed collector had counted", got, want)
	}
	want = pythonWorkerSnapshot{Starts: 1, Idle: 1, Stops: map[string]uint64{}, Runs: map[string]uint64{pythonRunOK: 2}}
	if got := pool.Snapshot("gone"); !reflect.DeepEqual(got, want) {
		t.Errorf("the collector added again counts %+v, want %+v", got, want)
	}
}
