package transform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Python scripts run in long-lived workers (pythonworker.go). These tests pin
// reuse, isolation, timeouts, crashes, output limits and the sandbox.

// requirePython skips a test without python3, and gives it a worker pool of
// its own (usePythonPool).
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
	usePythonPool(t)
}

// usePythonPool runs the test against a fresh worker pool and restores the
// previous one afterwards (IsolatePythonWorkers). Every count a test reads
// from the pool is then its own, so the tests pass under -count=N and
// -shuffle=on. Tests do not run in parallel, so swapping the pool is safe.
//
// The pool leaves every script at least a minute, whatever its
// limits.script_timeout: a script that takes a millisecond has been seen to
// take more than the default 100ms on a machine with every CPU busy
// elsewhere, and one that fills 200 MiB more than two seconds. The minute
// bounds a script that never ends; a test of the timeout itself calls
// holdScriptsToTheirTimeout.
//
// An interpreter has a minute to start in too, where the exporter gives it
// ten seconds, which one start on a busy machine has overrun. That is kept
// by a test of the script's timeout, which starts an interpreter as any
// other does; a test of the start's own limit sets a short one
// (SetStartTimeout) and starts what never says it is ready.
func usePythonPool(t *testing.T) {
	t.Helper()
	t.Cleanup(IsolatePythonWorkers())
	PythonWorkers().SetLeastScriptTimeout(time.Minute)
	PythonWorkers().SetStartTimeout(time.Minute)
}

// holdScriptsToTheirTimeout makes limits.script_timeout what ends a script
// in the test's pool, as it is in the exporter: for the tests of the timeout,
// whose scripts are stopped by it. It is called after requirePython or
// usePythonPool.
func holdScriptsToTheirTimeout(t *testing.T) {
	t.Helper()
	PythonWorkers().SetLeastScriptTimeout(0)
}

func workerCollector(name, script string) *model.Collector {
	return &model.Collector{
		Name: name, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Transform: model.TransformConfig{Type: "python", Script: script},
		Limits:    model.Limits{ScriptTimeout: model.Duration(2 * time.Second), MaxOutputBytes: 1 << 20},
	}
}

func runWorkerScript(t *testing.T, c *model.Collector) (*model.MetricSet, error) {
	t.Helper()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("value=7"), Headers: http.Header{}}
	return executePython(context.Background(), "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "value=7", Raw: r.Body}, r, c)
}

func workerMetricValue(t *testing.T, set *model.MetricSet, name string) float64 {
	t.Helper()
	for _, m := range set.Metrics {
		if m.Name == name {
			return m.Value
		}
	}
	t.Fatalf("no %s in %+v", name, set.Metrics)
	return 0
}

func TestPythonWorkerIsReused(t *testing.T) {
	requirePython(t)
	c := workerCollector("reuse", `metric(name="v", value=1)`)
	before := PythonWorkers().started.Load()
	for i := 0; i < 5; i++ {
		set, err := runWorkerScript(t, c)
		if err != nil {
			t.Fatal(err)
		}
		if workerMetricValue(t, set, "v") != 1 {
			t.Fatal("wrong value")
		}
	}
	if started := PythonWorkers().started.Load() - before; started != 1 {
		t.Fatalf("five sequential runs started %d interpreters, want 1", started)
	}
}

// Each run gets fresh globals, and a worker only ever runs one collector's
// scripts, so module state one collector leaves behind is invisible to another.
func TestPythonWorkerIsolation(t *testing.T) {
	requirePython(t)
	counter := workerCollector("isolation_a", `
try:
    seen
    metric(name="seen_before", value=1)
except NameError:
    metric(name="seen_before", value=0)
seen = True
import json
json._left_behind = True
`)
	for i := 0; i < 2; i++ {
		set, err := runWorkerScript(t, counter)
		if err != nil {
			t.Fatal(err)
		}
		if workerMetricValue(t, set, "seen_before") != 0 {
			t.Fatal("a global from the previous run survived into this one")
		}
	}
	other := workerCollector("isolation_b", `
import json
metric(name="leaked", value=1 if hasattr(json, "_left_behind") else 0)
`)
	set, err := runWorkerScript(t, other)
	if err != nil {
		t.Fatal(err)
	}
	if workerMetricValue(t, set, "leaked") != 0 {
		t.Fatal("one collector saw module state another left behind")
	}
}

// A script that overruns fails with a timeout; its worker is killed and the
// next scrape gets a working one.
//
// The script never ends, so the timeout is what ends the run, however slow
// the machine; the run after it has a minute, so a slow machine does not
// fail it by the timeout it is not about.
func TestPythonWorkerTimeoutKillsTheWorker(t *testing.T) {
	requirePython(t)
	holdScriptsToTheirTimeout(t)
	c := workerCollector("timeout", `
if data == "value=7":
    while True:
        pass
metric(name="v", value=1)
`)
	c.Limits.ScriptTimeout = model.Duration(200 * time.Millisecond)
	_, err := runWorkerScript(t, c)
	if err == nil || !strings.Contains(err.Error(), "python transform timed out after 200ms") {
		t.Fatalf("err=%v", err)
	}
	if snap := PythonWorkers().Snapshot(c.Name); snap.Stops[pythonStopTimeout] != 1 || snap.Idle != 0 || snap.Busy != 0 {
		t.Fatalf("after the timeout: stops %v, %d workers idle and %d busy, want the one worker stopped for it", snap.Stops, snap.Idle, snap.Busy)
	}
	c.Transform.Script = `metric(name="v", value=1)`
	c.Limits.ScriptTimeout = model.Duration(time.Minute)
	set, err := runWorkerScript(t, c)
	if err != nil || workerMetricValue(t, set, "v") != 1 {
		t.Fatalf("after a timeout: set=%+v err=%v", set, err)
	}
	if snap := PythonWorkers().Snapshot(c.Name); snap.Starts != 2 {
		t.Fatalf("%d workers were started, want the one the timeout killed and another for the next run", snap.Starts)
	}
}

// A pool told the least time to give a script gives every script that long,
// whatever its limits.script_timeout, and says so when a script overruns it;
// a limit above it stands; and a pool told 0, as the exporter's own is, holds
// each script to its limit. The tests run scripts under the first, so that a
// machine too busy to run a script within its limit does not fail a test
// that is not about the limit (usePythonPool).
//
// The workers are stand-ins and the clock the test's own, as below.
func TestAPoolsLeastScriptTimeIsWhatAScriptHas(t *testing.T) {
	const limit = 25 * time.Millisecond
	for _, test := range []struct {
		name          string
		least, limit  time.Duration
		script        time.Duration
		timesOutAfter string
	}{
		{"a script over its limit and within the least time", time.Minute, limit, 30 * time.Second, ""},
		{"a script over the least time", time.Minute, limit, time.Minute + time.Millisecond, "1m0s"},
		{"a script within a limit above the least time", time.Minute, 2 * time.Minute, 90 * time.Second, ""},
		{"a script over a limit above the least time", time.Minute, 2 * time.Minute, 2*time.Minute + time.Millisecond, "2m0s"},
		{"no least time, a script within its limit", 0, limit, limit - time.Millisecond, ""},
		{"no least time, a script over its limit", 0, limit, limit + time.Millisecond, "25ms"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				t.Cleanup(IsolatePythonWorkers())
				if got := time.Duration(PythonWorkers().leastScriptTimeout.Load()); got != 0 {
					t.Fatalf("a fresh pool gives a script at least %s, want its limit alone", got)
				}
				PythonWorkers().SetLeastScriptTimeout(test.least)
				PythonWorkers().start = func(context.Context, pythonSpec) (*pythonWorker, error) {
					return heldWorker(t, heldAnswer{0, pythonRequestTaken}, heldAnswer{test.script, `{"ok": true, "metrics": [{"name": "v", "value": 1}]}`}), nil
				}
				c := workerCollector("least", `metric(name="v", value=1)`)
				c.Limits.ScriptTimeout = model.Duration(test.limit)
				_, err := runWorkerScript(t, c)
				// The stand-in is left the time to end, as below.
				time.Sleep(test.script)
				if test.timesOutAfter == "" {
					if err != nil {
						t.Fatalf("a script of %s under a limit of %s in a pool that gives at least %s: %v", test.script, test.limit, test.least, err)
					}
					return
				}
				if want := "python transform timed out after " + test.timesOutAfter + ": "; err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("a script of %s under a limit of %s in a pool that gives at least %s: %v, want %q", test.script, test.limit, test.least, err, want)
				}
			})
		})
	}
}

// script_timeout bounds the script, not starting the interpreter: a worker
// that takes four hundred times the budget to start, and whose script then
// runs for all but a millisecond of it, does not time out, and the run is
// said to have taken what the script took. One whose script runs a
// millisecond over does, however quickly it started.
//
// The worker is a stand-in and the clock the test's own (testing/synctest),
// so starting and running take exactly the time given here. A real cold
// start against a real 25ms passed or failed by how long the machine left
// the interpreter waiting for a CPU between taking the request and answering.
func TestPythonWorkerStartupIsNotCountedAgainstTheScript(t *testing.T) {
	const budget, startup = 25 * time.Millisecond, 10 * time.Second
	for _, test := range []struct {
		name            string
		startup, script time.Duration
		timesOut        bool
	}{
		{"a slow start and a script within the budget", startup, budget - time.Millisecond, false},
		{"a start in no time and a script over the budget", 0, budget + time.Millisecond, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				usePythonPool(t)
				holdScriptsToTheirTimeout(t)
				PythonWorkers().start = func(context.Context, pythonSpec) (*pythonWorker, error) {
					time.Sleep(test.startup)
					return heldWorker(t, heldAnswer{0, pythonRequestTaken}, heldAnswer{test.script, `{"ok": true, "metrics": [{"name": "v", "value": 1}]}`}), nil
				}
				c := workerCollector("cold_start", `metric(name="v", value=1)`)
				c.Limits.ScriptTimeout = model.Duration(budget)
				ctx, timer := WithScriptTimer(context.Background())
				began := time.Now()
				r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("value=7"), Headers: http.Header{}}
				set, err := executePython(ctx, "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "value=7", Raw: r.Body}, r, c)
				took := time.Since(began)
				// The clock stops when this function returns, so the
				// stand-in is first left the time to end.
				time.Sleep(test.script)
				if test.timesOut {
					if err == nil || !strings.Contains(err.Error(), "python transform timed out after 25ms") || took != budget {
						t.Fatalf("a script that ran %s under a budget of %s: after %s, %v", test.script, budget, took, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("a start of %s and a script of %s under a budget of %s failed after %s: %v", test.startup, test.script, budget, took, err)
				}
				if workerMetricValue(t, set, "v") != 1 || took != test.startup+test.script {
					t.Fatalf("%+v after %s", set.Metrics, took)
				}
				if seconds, ran := timer.Seconds(); !ran || seconds != test.script.Seconds() {
					t.Fatalf("the script is said to have run %vs, %v; it ran %s", seconds, ran, test.script)
				}
				if snap := PythonWorkers().Snapshot(c.Name); snap.Starts != 1 || snap.Runs[pythonRunOK] != 1 {
					t.Fatalf("%d started, runs %v", snap.Starts, snap.Runs)
				}
			})
		})
	}
}

// Printing, and reading stdin, cannot corrupt the protocol: neither is the
// channel the worker answers on.
func TestPythonWorkerStdioCannotCorruptTheProtocol(t *testing.T) {
	requirePython(t)
	c := workerCollector("stdio", `
import sys
print('{"ok": true, "metrics": []}')
print("to stderr", file=sys.stderr)
rest = sys.stdin.read()
metric(name="stdin_bytes", value=len(rest))
`)
	for i := 0; i < 2; i++ {
		set, err := runWorkerScript(t, c)
		if err != nil {
			t.Fatal(err)
		}
		if workerMetricValue(t, set, "stdin_bytes") != 0 {
			t.Fatal("stdin carried data")
		}
	}
}

// A script that raises, or calls sys.exit, fails that run with the Python
// error; the worker survives and is reused.
func TestPythonWorkerSurvivesScriptErrors(t *testing.T) {
	requirePython(t)
	before := PythonWorkers().started.Load()
	for _, test := range []struct{ script, want string }{
		{`raise ValueError("bad vendor data")`, "ValueError: bad vendor data"},
		{`fail("no workers found")`, "RuntimeError: no workers found"},
		{`import sys; sys.exit(3)`, "SystemExit: 3"},
	} {
		c := workerCollector("errors", test.script)
		_, err := runWorkerScript(t, c)
		if err == nil || !strings.Contains(err.Error(), "python transform failed") || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: err=%v", test.script, err)
		}
	}
	// The three scripts differ, so each has its own pool; each started one
	// worker, and a re-run of the last reuses it.
	c := workerCollector("errors", `import sys; sys.exit(3)`)
	_, _ = runWorkerScript(t, c)
	if started := PythonWorkers().started.Load() - before; started != 3 {
		t.Fatalf("started %d interpreters, want 3", started)
	}
}

// A worker that dies is discarded, and the next scrape starts another.
func TestPythonWorkerCrashIsReplaced(t *testing.T) {
	requirePython(t)
	c := workerCollector("crash", `import os; os._exit(1)`)
	before := PythonWorkers().started.Load()
	for run := 0; run < 2; run++ {
		if _, err := runWorkerScript(t, c); err == nil || !strings.Contains(err.Error(), "python transform failed: the interpreter exited") {
			t.Fatalf("run %d: err=%v", run, err)
		}
	}
	if started := PythonWorkers().started.Load() - before; started != 2 {
		t.Fatalf("started %d interpreters for two crashing runs, want 2", started)
	}
}

func TestPythonWorkerOutputLimit(t *testing.T) {
	requirePython(t)
	c := workerCollector("output_limit", `
for i in range(1000):
    metric(name="series_with_a_long_name", value=i, labels={"i": str(i)})
`)
	c.Limits.MaxOutputBytes = 2048
	_, err := runWorkerScript(t, c)
	if err == nil || !strings.Contains(err.Error(), "python transform output exceeds limit") {
		t.Fatalf("err=%v", err)
	}
	c.Transform.Script = `metric(name="v", value=1)`
	if _, err := runWorkerScript(t, c); err != nil {
		t.Fatalf("after an oversized answer: %v", err)
	}
}

// The sandbox is installed once, at start-up, and holds for every run.
func TestPythonWorkerSandbox(t *testing.T) {
	requirePython(t)
	for _, test := range []struct{ script, want string }{
		{`import socket`, "module disabled by exporter"},
		{`import subprocess`, "module disabled by exporter"},
		{`import threading`, "module disabled by exporter"},
		{`open("/etc/passwd")`, "operation disabled by exporter"},
		{`import os; os.system("true")`, "operation disabled by exporter"},
		{`import os; os.read(3, 10)`, "operation disabled by exporter"},
		{`import os; os.write(4, b"x")`, "operation disabled by exporter"},
		{`import os; os.fork()`, "operation disabled by exporter"},
		{`import os; os.kill(os.getpid(), 0)`, "operation disabled by exporter"},
		{`import os; os.remove("/nonexistent/sandbox-test")`, "operation disabled by exporter"},
		{`import os; os.listdir("/")`, "operation disabled by exporter"},
		// The modules beneath the blocked ones, and the ways around the
		// import guard and the replaced functions.
		{`import _socket`, "module disabled by exporter"},
		{`import _posixsubprocess`, "module disabled by exporter"},
		{`import posix`, "module disabled by exporter"},
		{`import _io`, "module disabled by exporter"},
		{`import _thread`, "module disabled by exporter"},
		{`import importlib`, "module disabled by exporter"},
		{`sys.modules["posix"]`, "KeyError"},
		{`import io; io.FileIO("/etc/passwd")`, "operation disabled by exporter"},
		{`sys.modules["_io"].FileIO("/etc/passwd")`, "operation disabled by exporter"},
		{`sys.modules["_io"].open("/etc/passwd")`, "operation disabled by exporter"},
		// The importer's own open reads source and bytecode in binary, and
		// nothing else in binary either.
		{`sys.modules["_io"].open("/etc/passwd", "rb")`, "operation disabled by exporter"},
		{`sys.modules["socket"]`, "KeyError"},
	} {
		c := workerCollector("sandbox", test.script)
		for run := 0; run < 2; run++ {
			_, err := runWorkerScript(t, c)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("%s run %d: err=%v", test.script, run, err)
			}
		}
	}
}

// The sandbox leaves the standard library a script uses alone, including
// modules that import a module scripts may not import themselves.
func TestPythonWorkerSandboxLeavesTheStandardLibrary(t *testing.T) {
	requirePython(t)
	c := workerCollector("stdlib", `
import collections, csv, dataclasses, datetime, decimal, math, random, re, statistics
@dataclasses.dataclass
class Point:
    x: int
metric(name="v", value=Point(statistics.mean([1, 3])).x)
`)
	set, err := runWorkerScript(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if workerMetricValue(t, set, "v") != 2 {
		t.Fatalf("metrics=%+v", set.Metrics)
	}
}

// Where a module has no bytecode cache, or a stale one, and its directory is
// writable, the importer compiles it and writes a cache. The sandbox refuses
// the write with an error the importer does not expect, so the import failed:
// a runner-owned Python whose caches look stale after extraction broke every
// stdlib import. The worker runs with -B, never writing caches.
func TestPythonWorkerImportsModulesWithoutABytecodeCache(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uncached_helper.py"), []byte("VALUE = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := workerCollector("uncached", fmt.Sprintf(`
import sys
sys.path.insert(0, %q)
import uncached_helper
metric(name="v", value=uncached_helper.VALUE)
metric(name="dont_write_bytecode", value=1 if sys.dont_write_bytecode else 0)
`, dir))
	set, err := runWorkerScript(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := workerMetricValue(t, set, "v"); got != 5 {
		t.Fatalf("v = %v, want 5", got)
	}
	if got := workerMetricValue(t, set, "dont_write_bytecode"); got != 1 {
		t.Fatal("the worker would write bytecode caches")
	}
	if _, err := os.Stat(filepath.Join(dir, "__pycache__")); !os.IsNotExist(err) {
		t.Fatalf("the worker wrote a bytecode cache: %v", err)
	}
}

// A declared library is imported before the sandbox, so one that imports a
// blocked module itself still loads, and its import time is not the script's.
func TestPythonWorkerPreloadsDeclaredLibraries(t *testing.T) {
	requirePython(t)
	// -I as the worker starts the interpreter: isolated, without the user's
	// site-packages, where a library the worker cannot import may be.
	if exec.Command("python3", "-I", "-c", "import dateutil.parser").Run() != nil {
		t.Skip("python-dateutil is not installed where the worker's isolated interpreter finds it")
	}
	c := workerCollector("preload", `
from dateutil import parser
metric(name="year", value=parser.parse("2026-09-22T18:00:07Z").year)
`)
	c.Transform.Libraries = []string{"python-dateutil"}
	set, err := runWorkerScript(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if workerMetricValue(t, set, "year") != 2026 {
		t.Fatalf("metrics=%+v", set.Metrics)
	}
}

// Idle workers are stopped after the idle timeout, which also retires the
// workers of a script a reload replaced.
func TestPythonWorkerIdleWorkersAreReaped(t *testing.T) {
	requirePython(t)
	c := workerCollector("idle", `metric(name="v", value=1)`)
	if _, err := runWorkerScript(t, c); err != nil {
		t.Fatal(err)
	}
	key := pythonWorkerSpec("python3", c).key()
	PythonWorkers().mu.Lock()
	idle := PythonWorkers().idle[key]
	if len(idle) != 1 {
		PythonWorkers().mu.Unlock()
		t.Fatalf("%d idle workers, want 1", len(idle))
	}
	idle[0].idleSince = time.Now().Add(-pythonWorkerIdleTimeout - time.Second)
	PythonWorkers().reapLocked(time.Now())
	_, stillThere := PythonWorkers().idle[key]
	PythonWorkers().mu.Unlock()
	if stillThere {
		t.Fatal("an expired idle worker was kept")
	}
}

// A changed script gets its own workers.
func TestPythonWorkerPoolsAreKeyedByScript(t *testing.T) {
	a := workerCollector("keyed", `metric(name="v", value=1)`)
	b := workerCollector("keyed", `metric(name="v", value=2)`)
	if pythonWorkerSpec("python3", a).key() == pythonWorkerSpec("python3", b).key() {
		t.Fatal("two scripts share a pool")
	}
	c := workerCollector("keyed", `metric(name="v", value=1)`)
	if pythonWorkerSpec("python3", a).key() != pythonWorkerSpec("python3", c).key() {
		t.Fatal("the same script does not share a pool")
	}
}

// Concurrent scrapes each get a worker; afterwards at most
// pythonWorkerMaxIdle stay around.
func TestPythonWorkerConcurrentRuns(t *testing.T) {
	requirePython(t)
	c := workerCollector("concurrent", `metric(name="v", value=1)`)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func() {
			_, err := runWorkerScript(t, c)
			errs <- err
		}()
	}
	for i := 0; i < 12; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	key := pythonWorkerSpec("python3", c).key()
	PythonWorkers().mu.Lock()
	idle := len(PythonWorkers().idle[key])
	PythonWorkers().mu.Unlock()
	if idle < 1 || idle > pythonWorkerMaxIdle {
		t.Fatalf("%d idle workers after a burst, want 1..%d", idle, pythonWorkerMaxIdle)
	}
}

// A worker that cannot start is counted as a start failure.
func TestPythonWorkerStartFailuresAreCounted(t *testing.T) {
	usePythonPool(t)
	testutil.CaptureLogs(t)
	c := workerCollector("py_metrics_no_interpreter", `metric(name="v", value=1)`)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{}}
	if _, err := executePython(context.Background(), "/nonexistent/python", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}, r, c); err == nil {
		t.Fatal("a missing interpreter started")
	}
	snap := PythonWorkers().Snapshot("py_metrics_no_interpreter")
	if snap.StartFailures != 1 || snap.Starts != 0 || snap.Starting != 0 || snap.Runs[pythonRunFailed] != 1 {
		t.Fatalf("snapshot=%+v", snap)
	}
}

// --python.max-workers bounds the workers alive at once, of every collector
// together: a burst waits for a worker instead of starting one each, and a
// script with no worker of its own takes the place of the idle worker unused
// for longest.
func TestPythonWorkersAreCappedProcessWide(t *testing.T) {
	requirePython(t)
	pool := PythonWorkers()
	pool.SetMaxWorkers(2)
	stop := make(chan struct{})
	most := make(chan int, 1)
	go func() {
		seen := 0
		for {
			pool.mu.Lock()
			seen = max(seen, pool.live)
			pool.mu.Unlock()
			select {
			case <-stop:
				most <- seen
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	a := workerCollector("capped_a", "import time\ntime.sleep(0.05)\nmetric(name=\"v\", value=1)")
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() {
			_, err := runWorkerScript(t, a)
			errs <- err
		}()
	}
	for i := 0; i < 6; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	b := workerCollector("capped_b", `metric(name="v", value=2)`)
	if _, err := runWorkerScript(t, b); err != nil {
		t.Fatal(err)
	}
	close(stop)
	if seen := <-most; seen > 2 {
		t.Fatalf("%d workers alive at once, want at most 2", seen)
	}
	if evicted := pool.Snapshot("capped_a").Stops[pythonStopEvicted]; evicted != 1 {
		t.Fatalf("%d idle workers evicted, want 1", evicted)
	}
}

// A run that finds every worker busy waits only as long as its own deadline,
// and says why it gave up.
//
// The worker is busy until the test ends its run: with a script that slept
// a second, a machine that took longer than that to get the other run to
// the pool found the worker free.
func TestPythonWorkerWaitEndsWithTheRun(t *testing.T) {
	requirePython(t)
	pool := PythonWorkers()
	pool.SetMaxWorkers(1)
	slow := workerCollector("waited_slow", "import time\ntime.sleep(3600)\nmetric(name=\"v\", value=1)")
	busy, endBusy := context.WithCancel(context.Background())
	defer endBusy()
	done := make(chan error, 1)
	go func() {
		_, err := runWorkerText(busy, slow, "value=7")
		done <- err
	}()
	testutil.WaitFor(t, "the only worker to be busy", func() bool { return pool.Snapshot("waited_slow").Busy == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	other := workerCollector("waited_other", `metric(name="v", value=1)`)
	if _, _, err := pool.run(ctx, pythonWorkerSpec("python3", other), []byte(`{}`), time.Second); err == nil || !strings.Contains(err.Error(), "--python.max-workers") {
		t.Fatalf("err=%v", err)
	}
	endBusy()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the run that kept the worker busy ended with %v, want it ended by the test", err)
	}
	if ValidateMaxWorkers(-1) == nil || ValidateMaxWorkers(0) != nil {
		t.Fatal("validation")
	}
}

// limits.max_script_memory bounds a worker's address space: a script that
// needs more fails saying so, and the worker goes on serving the next run.
func TestPythonWorkerMemoryLimit(t *testing.T) {
	requirePython(t)
	if runtime.GOOS != "linux" {
		t.Skip("RLIMIT_AS is enforced on Linux")
	}
	c := workerCollector("memory", "size = 1 << 30\nblob = bytearray(size)\nmetric(name=\"v\", value=len(blob))")
	c.Limits.MaxScriptMemory = 256 << 20
	if _, err := runWorkerScript(t, c); err == nil || !strings.Contains(err.Error(), "limits.max_script_memory") {
		t.Fatalf("err=%v", err)
	}
	small := workerCollector("memory", "metric(name=\"v\", value=len(bytearray(1 << 20)))")
	small.Limits.MaxScriptMemory = 256 << 20
	set, err := runWorkerScript(t, small)
	if err != nil {
		t.Fatal(err)
	}
	if v := workerMetricValue(t, set, "v"); v != 1<<20 {
		t.Fatalf("v=%v", v)
	}
	unlimited := workerCollector("memory", c.Transform.Script)
	if pythonWorkerSpec("python3", unlimited).key() == pythonWorkerSpec("python3", c).key() {
		t.Fatal("a memory limit does not change the worker a script runs in")
	}
}

// A worker that dies mid-run under a memory limit says the limit may be
// why, since a C library out of memory can end the interpreter instead of
// raising MemoryError; without a limit it says nothing of memory.
func TestPythonWorkerDeathUnderAMemoryLimitNamesIt(t *testing.T) {
	requirePython(t)
	die := "import signal\nsignal.raise_signal(signal.SIGKILL)"
	limited := workerCollector("dies_limited", die)
	limited.Limits.MaxScriptMemory = 256 << 20
	if _, err := runWorkerScript(t, limited); err == nil || !strings.Contains(err.Error(), "limits.max_script_memory, 268435456 bytes") {
		t.Fatalf("err=%v", err)
	}
	if _, err := runWorkerScript(t, workerCollector("dies", die)); err == nil || strings.Contains(err.Error(), "max_script_memory") {
		t.Fatalf("err=%v", err)
	}
}

// Runs waiting for a worker under --python.max-workers are served in line,
// one per worker that frees, and one giving up passes its turn on.
//
// The worker is busy until the test ends its run, when the line is as the
// test wants it: with a script that slept 300ms, a machine that took longer
// than that to form the line found the worker free halfway.
func TestPythonWorkerWaitersAreServedInLine(t *testing.T) {
	requirePython(t)
	pool := PythonWorkers()
	pool.SetMaxWorkers(1)
	slow := workerCollector("line_busy", "import time\ntime.sleep(3600)\nmetric(name=\"v\", value=1)")
	held, free := context.WithCancel(context.Background())
	defer free()
	busy := make(chan error, 1)
	go func() {
		_, err := runWorkerText(held, slow, "value=7")
		busy <- err
	}()
	waitFor := func(n int) {
		t.Helper()
		testutil.WaitFor(t, fmt.Sprintf("%d runs to wait in line for the one worker", n), func() bool {
			pool.mu.Lock()
			defer pool.mu.Unlock()
			return len(pool.waiting) == n && pool.live == 1
		})
	}
	waitFor(0)
	served := make(chan string, 3)
	run := func(name string, ctx context.Context) {
		c := workerCollector(name, `metric(name="v", value=1)`)
		go func() {
			if _, _, err := pool.run(ctx, pythonWorkerSpec("python3", c), []byte(`{"mode":"metrics","script":"metric(name='v', value=1)","data":null,"response":{"status_code":200,"headers":{},"body":"","text":""},"target":"","collector":"`+name+`"}`), time.Minute); err == nil {
				served <- name
			}
		}()
	}
	quitter, quit := context.WithCancel(context.Background())
	run("line_first", context.Background())
	waitFor(1)
	run("line_quitter", quitter)
	waitFor(2)
	run("line_third", context.Background())
	waitFor(3)
	if waiting := pool.PoolSnapshot().Waiting; waiting != 3 {
		t.Fatalf("the pool reports %d waiting runs, want 3", waiting)
	}
	quit()
	waitFor(2)
	free()
	if err := <-busy; !errors.Is(err, context.Canceled) {
		t.Fatalf("the run that kept the worker busy ended with %v, want it ended by the test", err)
	}
	for _, want := range []string{"line_first", "line_third"} {
		select {
		case got := <-served:
			if got != want {
				t.Fatalf("%s was served, want %s", got, want)
			}
		case <-time.After(time.Minute):
			t.Fatalf("%s was never served", want)
		}
	}
}

// response.header(name) is a header's values joined by ", ", as jq's
// $headers has them, whatever the name's case, or the default without one;
// response.headers keeps each header's list.
func TestPythonResponseHeaderJoinsTheValues(t *testing.T) {
	requirePython(t)
	usePythonPool(t)
	c := workerCollector("headers", `
metric(name="joined", value=1, labels={"v": response.header("x-mode")})
metric(name="missing", value=1, labels={"v": response.header("X-None", "none")})
metric(name="listed", value=len(response.headers["X-Mode"]))`)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("x"), Headers: http.Header{"X-Mode": {"a", "b"}}}
	set, err := executePython(context.Background(), "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "x", Raw: r.Body}, r, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range set.Metrics {
		switch m.Name {
		case "joined":
			if m.Labels["v"] != "a, b" {
				t.Errorf("header joined as %q", m.Labels["v"])
			}
		case "missing":
			if m.Labels["v"] != "none" {
				t.Errorf("a missing header gave %q", m.Labels["v"])
			}
		case "listed":
			if m.Value != 2 {
				t.Errorf("response.headers has %v values", m.Value)
			}
		}
	}
	if len(set.Metrics) != 3 {
		t.Fatalf("metrics: %+v", set.Metrics)
	}
}

// A script may read time zone data — zoneinfo and dateutil.tz find named
// zones — and nothing else: another file, or one reached through the zone
// directory by .., is still refused. From Python 3.12 zoneinfo loads
// sysconfig, which imports the blocked threading; zoneinfo still works, and
// threading is not left reachable through sysconfig.
func TestPythonScriptsReadTimeZoneDataOnly(t *testing.T) {
	requirePython(t)
	usePythonPool(t)
	c := workerCollector("tz", `
import zoneinfo, datetime
berlin = zoneinfo.ZoneInfo("Europe/Berlin")
offset = datetime.datetime(2026, 1, 15, tzinfo=berlin).utcoffset().total_seconds()
metric(name="zoneinfo_offset_seconds", value=offset)
try:
    from dateutil import tz
    metric(name="dateutil_offset_seconds", value=datetime.datetime(2026, 7, 15, tzinfo=tz.gettz("Europe/Berlin")).utcoffset().total_seconds())
except ImportError:
    metric(name="dateutil_offset_seconds", value=7200)
refused = 0
for path in ("/etc/passwd", "/usr/share/zoneinfo/../../../etc/passwd"):
    try:
        open(path).read()
    except RuntimeError:
        refused += 1
metric(name="refused", value=refused)
import sysconfig
metric(name="threading_reachable", value=1 if hasattr(sysconfig, "threading") else 0)`)
	c.Limits.ScriptTimeout = model.Duration(10 * time.Second)
	c.Transform.Libraries = []string{"python-dateutil"}
	set, err := runWorkerScript(t, c)
	if err != nil {
		if strings.Contains(err.Error(), "No time zone found") || strings.Contains(err.Error(), "ZoneInfoNotFoundError") {
			t.Skip("no time zone data on this machine")
		}
		t.Fatal(err)
	}
	for name, want := range map[string]float64{"zoneinfo_offset_seconds": 3600, "dateutil_offset_seconds": 7200, "refused": 2, "threading_reachable": 0} {
		if got := workerMetricValue(t, set, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// A script's error shows the script's own innermost frames, the failing line
// among them with its source, and none of the worker's frames: five nested
// calls fail on the innermost one's line, which a traceback of the outermost
// frames would cut off.
func TestPythonWorkerTracebackShowsTheFailingLine(t *testing.T) {
	requirePython(t)
	c := workerCollector("traceback", `def f1():
    return f2()
def f2():
    return f3()
def f3():
    return f4()
def f4():
    return f5()
def f5():
    return {}["missing_key"]
f1()
`)
	_, err := runWorkerScript(t, c)
	if err == nil {
		t.Fatal("the script did not fail")
	}
	text := err.Error()
	for _, want := range []string{
		"Traceback (most recent call last):",
		`File "<collector-python>", line 10, in f5`,
		`return {}["missing_key"]`,
		"KeyError: 'missing_key'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the error lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `File "<string>"`) || strings.Contains(text, "<module>") {
		t.Errorf("the error keeps the worker's or the outermost frames:\n%s", text)
	}
	if n := strings.Count(text, `File "`); n != 5 {
		t.Errorf("the error shows %d frames, want the 5 innermost:\n%s", n, text)
	}

	// An error raised by the exporter's own functions (metric, fail) points at
	// the script's call, not into the worker.
	c = workerCollector("traceback_metric", "x = 1\nmetric(name='v', value='n/a')\n")
	_, err = runWorkerScript(t, c)
	if err == nil || !strings.Contains(err.Error(), `File "<collector-python>", line 2, in <module>`) || strings.Contains(err.Error(), `File "<string>"`) {
		t.Fatalf("err=%v", err)
	}
}
