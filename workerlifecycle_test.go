package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Script durations, idle worker reaping and reloads (pythonworker.go), and
// error kinds (errkinds.go).

// http_exporter_script_duration_seconds is the time the probe's Python took,
// on the collector and, in verbose mode, on the request.
func TestScriptDurationIsRecorded(t *testing.T) {
	requirePython(t)
	target := textTarget(t, "value=42\n")
	slow := pythonCollector("timed_script", "import time\ntime.sleep(0.05)\nmetric(name=\"v\", value=1)\n")
	server := verboseServer(t, true, slow, testCollector("no_script", "text"))
	probe := func(collector string) {
		if recorder := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil); recorder.Code != 200 {
			t.Fatalf("%s: %d %s", collector, recorder.Code, recorder.Body)
		}
	}
	probe("timed_script")
	probe("no_script")
	exposition := selfMetrics(t, server)
	got := seriesValue(t, exposition, `http_exporter_script_duration_seconds{collector="timed_script"}`)
	if got < 0.05 || got > 2 {
		t.Fatalf("script duration %v, want at least the 50ms the script sleeps", got)
	}
	if got := seriesValue(t, exposition, `http_exporter_script_duration_seconds{collector="no_script"}`); got != 0 {
		t.Fatalf("a collector without Python reports %v", got)
	}
	perRequest := fmt.Sprintf(`http_exporter_script_duration_seconds{collector="timed_script",http_method="GET",url=%q}`, target.URL)
	if got := seriesValue(t, exposition, perRequest); got < 0.05 {
		t.Fatalf("per-request script duration %v", got)
	}
}

// The timer adds up the scripts a probe runs and says whether any ran.
func TestScriptTimerSumsTheRuns(t *testing.T) {
	ctx, timer := withScriptTimer(context.Background())
	if _, ran := timer.seconds(); ran {
		t.Fatal("a fresh timer says a script ran")
	}
	scriptTimerFrom(ctx).add(30 * time.Millisecond)
	scriptTimerFrom(ctx).add(20 * time.Millisecond)
	if seconds, ran := timer.seconds(); !ran || seconds < 0.049 || seconds > 0.051 {
		t.Fatalf("seconds=%v ran=%v", seconds, ran)
	}
	if scriptTimerFrom(context.Background()) != nil {
		t.Fatal("a context without a timer has one")
	}
}

// idleWorkers counts a collector's idle workers.
func idleWorkers(collector string) int { return pythonWorkers.snapshot(collector).idle }

// ageIdleWorkers makes a collector's idle workers look idle for longer than
// the idle timeout.
func ageIdleWorkers(collector string) {
	pythonWorkers.mu.Lock()
	defer pythonWorkers.mu.Unlock()
	for _, workers := range pythonWorkers.idle {
		for _, worker := range workers {
			if worker.collector == collector {
				worker.idleSince = time.Now().Add(-pythonWorkerIdleTimeout - time.Minute)
			}
		}
	}
}

// Idle workers are stopped on a timer, though nothing asks for a worker again.
func TestIdleWorkersAreReapedOnATimer(t *testing.T) {
	requirePython(t)
	c := workerCollector("reaped_on_timer", `metric(name="v", value=1)`)
	if _, err := runWorkerScript(t, c); err != nil {
		t.Fatal(err)
	}
	if idleWorkers(c.Name) != 1 {
		t.Fatalf("idle=%d before reaping", idleWorkers(c.Name))
	}
	before := pythonWorkers.snapshot(c.Name).stops[pythonStopIdle]
	ageIdleWorkers(c.Name)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pythonWorkers.reapLoop(ctx, 10*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for idleWorkers(c.Name) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the idle worker was never reaped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := pythonWorkers.snapshot(c.Name).stops[pythonStopIdle]; got != before+1 {
		t.Fatalf("idle stops %d, want %d", got, before+1)
	}
}

// A reload stops the idle workers of a script it changed at once, and a worker
// busy with it when it finishes; workers of scripts it kept are untouched.
func TestAReloadStopsTheWorkersOfChangedScripts(t *testing.T) {
	requirePython(t)
	target := textTarget(t, "value=42\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// The pool is shared by every test, so the names are this run's own.
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	changedName, keptName, busyName := "reload_changed_"+suffix, "reload_kept_"+suffix, "reload_busy_"+suffix
	document := func(changed, slow string) string {
		return `collectors:
  - name: ` + changedName + `
    request: {type: http}
    transform:
      type: python
      script: |
        ` + changed + `
  - name: ` + keptName + `
    request: {type: http}
    transform:
      type: python
      script: |
        metric(name="kept", value=1)
  - name: ` + busyName + `
    request: {type: http}
    limits: {script_timeout: 5s}
    transform:
      type: python
      script: |
        ` + slow + `
`
	}
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(document(`metric(name="old", value=1)`, `import time; time.sleep(0.4); metric(name="busy", value=1)`))
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, path, quietLogger(t))
	manager.SetPythonPath("python3")
	server := NewServer(manager, "python3", quietLogger(t))
	probe := func(collector string) {
		if recorder := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil); recorder.Code != 200 {
			t.Errorf("%s: %d %s", collector, recorder.Code, recorder.Body)
		}
	}
	probe(changedName)
	probe(keptName)
	stopsBefore := pythonWorkers.snapshot(changedName).stops[pythonStopReload]
	busyBefore := pythonWorkers.snapshot(busyName).stops[pythonStopReload]

	// A probe of the slow script is running while the reload lands.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		probe(busyName)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for pythonWorkers.snapshot(busyName).busy == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slow script never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	write(document(`metric(name="new", value=1)`, `metric(name="busy", value=2)`))
	manager.lastMod = time.Time{}
	manager.reloadConfig()
	if manager.Get() == cfg {
		t.Fatal("the reload was not applied")
	}
	if got := idleWorkers(changedName); got != 0 {
		t.Errorf("the changed script still has %d idle workers", got)
	}
	if got := pythonWorkers.snapshot(changedName).stops[pythonStopReload]; got != stopsBefore+1 {
		t.Errorf("reload stops %d, want %d", got, stopsBefore+1)
	}
	if got := idleWorkers(keptName); got != 1 {
		t.Errorf("the unchanged script lost its idle worker: %d", got)
	}

	<-finished
	if got := idleWorkers(busyName); got != 0 {
		t.Errorf("the worker busy during the reload went back idle: %d", got)
	}
	if got := pythonWorkers.snapshot(busyName).stops[pythonStopReload]; got != busyBefore+1 {
		t.Errorf("busy reload stops %d, want %d", got, busyBefore+1)
	}
	// The new script runs in a new worker, which stays.
	probe(busyName)
	if got := idleWorkers(busyName); got != 1 {
		t.Errorf("the new script's worker is not kept: %d", got)
	}
}

// Which counter an error raises depends on its kind, whatever its message.
func TestErrorKindsAreMarkedNotGuessedFromText(t *testing.T) {
	marked := markError(errors.New("anything at all"), errMissingValue)
	wrapped := fmt.Errorf("outer: %w", &MetricFailure{Collector: "c", Metric: "m", Err: marked})
	if !errors.Is(wrapped, errMissingValue) || errors.Is(wrapped, errScriptFailed) || errors.Is(wrapped, errLimitExceeded) {
		t.Fatal("the kind does not survive wrapping, or leaks into another")
	}
	if wrapped.Error() != "outer: anything at all" {
		t.Fatalf("marking changed the message: %q", wrapped)
	}
	if markError(nil, errMissingValue) != nil {
		t.Fatal("marking nil made an error")
	}
	if errors.Is(errors.New("value is missing; response size exceeds limit; python failed"), errMissingValue) {
		t.Fatal("an unmarked error was classified by its text")
	}
}

// A Python error that mentions "missing" and "response size" is a script
// error and nothing else; a missing value and an oversized response are each
// counted as what they are.
func TestFailuresAreCountedByKind(t *testing.T) {
	requirePython(t)
	target := textTarget(t, "value=42\n")
	misleading := pythonCollector("kind_script", `raise ValueError("value is missing and the response size exceeds limit")`)
	missing := testCollector("kind_missing", "text")
	missing.Metrics[0].Expression = `absent=(\d+)`
	missing.Metrics[0].ErrorMode = ErrorModeFail
	large := testCollector("kind_large", "text")
	large.Limits.MaxResponseBytes = 4
	server := verboseServer(t, false, misleading, missing, large)
	for _, collector := range []string{"kind_script", "kind_missing", "kind_large"} {
		probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil)
	}
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_script_errors_total{collector="kind_script"}`:          1,
		`http_exporter_missing_keys_total{collector="kind_script"}`:           0,
		`http_exporter_series_limit_exceeded_total{collector="kind_script"}`:  0,
		`http_exporter_missing_keys_total{collector="kind_missing"}`:          1,
		`http_exporter_script_errors_total{collector="kind_missing"}`:         0,
		`http_exporter_series_limit_exceeded_total{collector="kind_large"}`:   1,
		`http_exporter_transform_errors_total{collector="kind_large"}`:        0,
		`http_exporter_series_limit_exceeded_total{collector="kind_missing"}`: 0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if strings.Contains(exposition, "kind_unused") {
		t.Fatal("unexpected collector")
	}
}
