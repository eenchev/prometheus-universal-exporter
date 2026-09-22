package main

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Python scripts run in long-lived workers (pythonworker.go). These tests pin
// reuse, isolation, timeouts, crashes, output limits and the sandbox.

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
}

func workerCollector(name, script string) *Collector {
	return &Collector{
		Name: name, Request: RequestConfig{Type: RequestTypeHTTP},
		Transform: TransformConfig{Type: "python", Script: script},
		Limits:    Limits{ScriptTimeout: Duration(2 * time.Second), MaxOutputBytes: 1 << 20},
	}
}

func runWorkerScript(t *testing.T, c *Collector) (*MetricSet, error) {
	t.Helper()
	r := &HTTPResponse{StatusCode: 200, Body: []byte("value=7"), Headers: http.Header{}}
	return executePython(context.Background(), "python3", c.Transform.Script, &Decoded{Kind: "text", Data: "value=7", Raw: r.Body}, r, c)
}

func workerMetricValue(t *testing.T, set *MetricSet, name string) float64 {
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
	before := pythonWorkers.started.Load()
	for i := 0; i < 5; i++ {
		set, err := runWorkerScript(t, c)
		if err != nil {
			t.Fatal(err)
		}
		if workerMetricValue(t, set, "v") != 1 {
			t.Fatal("wrong value")
		}
	}
	if started := pythonWorkers.started.Load() - before; started != 1 {
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
func TestPythonWorkerTimeoutKillsTheWorker(t *testing.T) {
	requirePython(t)
	c := workerCollector("timeout", `
if data == "value=7":
    while True:
        pass
metric(name="v", value=1)
`)
	c.Limits.ScriptTimeout = Duration(200 * time.Millisecond)
	start := time.Now()
	_, err := runWorkerScript(t, c)
	if err == nil || !strings.Contains(err.Error(), "python transform timed out after 200ms") {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the timeout took %s", elapsed)
	}
	c.Transform.Script = `metric(name="v", value=1)`
	set, err := runWorkerScript(t, c)
	if err != nil || workerMetricValue(t, set, "v") != 1 {
		t.Fatalf("after a timeout: set=%+v err=%v", set, err)
	}
}

// script_timeout bounds the script, not starting the interpreter, so a cold
// start does not count against a small budget.
func TestPythonWorkerStartupIsNotCountedAgainstTheScript(t *testing.T) {
	requirePython(t)
	c := workerCollector("cold_start", `metric(name="v", value=1)`)
	c.Limits.ScriptTimeout = Duration(25 * time.Millisecond)
	if _, err := runWorkerScript(t, c); err != nil {
		t.Fatalf("a cold start with a 25ms budget failed: %v", err)
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
	before := pythonWorkers.started.Load()
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
	if started := pythonWorkers.started.Load() - before; started != 3 {
		t.Fatalf("started %d interpreters, want 3", started)
	}
}

// A worker that dies is discarded, and the next scrape starts another.
func TestPythonWorkerCrashIsReplaced(t *testing.T) {
	requirePython(t)
	c := workerCollector("crash", `import os; os._exit(1)`)
	before := pythonWorkers.started.Load()
	for run := 0; run < 2; run++ {
		if _, err := runWorkerScript(t, c); err == nil || !strings.Contains(err.Error(), "python transform failed: the interpreter exited") {
			t.Fatalf("run %d: err=%v", run, err)
		}
	}
	if started := pythonWorkers.started.Load() - before; started != 2 {
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

// A declared library is imported before the sandbox, so one that imports a
// blocked module itself still loads, and its import time is not the script's.
func TestPythonWorkerPreloadsDeclaredLibraries(t *testing.T) {
	requirePython(t)
	if exec.Command("python3", "-c", "import dateutil.parser").Run() != nil {
		t.Skip("python-dateutil is not installed")
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
	pythonWorkers.mu.Lock()
	idle := pythonWorkers.idle[key]
	if len(idle) != 1 {
		pythonWorkers.mu.Unlock()
		t.Fatalf("%d idle workers, want 1", len(idle))
	}
	idle[0].idleSince = time.Now().Add(-pythonWorkerIdleTimeout - time.Second)
	pythonWorkers.reapLocked(time.Now())
	_, stillThere := pythonWorkers.idle[key]
	pythonWorkers.mu.Unlock()
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
	pythonWorkers.mu.Lock()
	idle := len(pythonWorkers.idle[key])
	pythonWorkers.mu.Unlock()
	if idle < 1 || idle > pythonWorkerMaxIdle {
		t.Fatalf("%d idle workers after a burst, want 1..%d", idle, pythonWorkerMaxIdle)
	}
}
