package exporter

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// Script durations, idle worker reaping and reloads (transform/pythonworker.go), and
// error kinds (model/errkinds.go).

// http_exporter_script_duration_seconds is the time the probe's Python took,
// on the collector and, in verbose mode, on the request.
func TestScriptDurationIsRecorded(t *testing.T) {
	requirePython(t)
	target := textTarget(t, "value=42\n")
	slow := pythonCollector("timed_script", "import time\ntime.sleep(0.05)\nmetric(name=\"v\", value=1)\n")
	server := verboseServer(t, true, slow, testutil.Collector("no_script", "text"))
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

// A reload stops the idle workers of a script it changed at once, and a worker
// busy with it when it finishes; workers of scripts it kept are untouched.
func TestAReloadStopsTheWorkersOfChangedScripts(t *testing.T) {
	requirePython(t)
	target := textTarget(t, "value=42\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	changedName, keptName, busyName := "reload_changed", "reload_kept", "reload_busy"
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
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, path, testutil.QuietLogger(t))
	manager.SetPythonPath("python3")
	server := NewServer(manager, "python3", testutil.QuietLogger(t))
	probe := func(collector string) {
		if recorder := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil); recorder.Code != 200 {
			t.Errorf("%s: %d %s", collector, recorder.Code, recorder.Body)
		}
	}
	probe(changedName)
	probe(keptName)
	stopsBefore := transform.PythonWorkers().Snapshot(changedName).Stops["reload"]
	busyBefore := transform.PythonWorkers().Snapshot(busyName).Stops["reload"]

	// A probe of the slow script is running while the reload lands.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		probe(busyName)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for transform.PythonWorkers().Snapshot(busyName).Busy == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slow script never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	write(document(`metric(name="new", value=1)`, `metric(name="busy", value=2)`))
	if err := manager.Reload(config.ReloadTriggerSignal); err != nil {
		t.Fatal(err)
	}
	if manager.Get() == cfg {
		t.Fatal("the reload was not applied")
	}
	if got := idleWorkers(changedName); got != 0 {
		t.Errorf("the changed script still has %d idle workers", got)
	}
	if got := transform.PythonWorkers().Snapshot(changedName).Stops["reload"]; got != stopsBefore+1 {
		t.Errorf("reload stops %d, want %d", got, stopsBefore+1)
	}
	if got := idleWorkers(keptName); got != 1 {
		t.Errorf("the unchanged script lost its idle worker: %d", got)
	}

	<-finished
	if got := idleWorkers(busyName); got != 0 {
		t.Errorf("the worker busy during the reload went back idle: %d", got)
	}
	if got := transform.PythonWorkers().Snapshot(busyName).Stops["reload"]; got != busyBefore+1 {
		t.Errorf("busy reload stops %d, want %d", got, busyBefore+1)
	}
	// The new script runs in a new worker, which stays.
	probe(busyName)
	if got := idleWorkers(busyName); got != 1 {
		t.Errorf("the new script's worker is not kept: %d", got)
	}
}

// A Python error that mentions "missing" and "response size" is a script
// error and nothing else; a missing value and an oversized response are each
// counted as what they are.
func TestFailuresAreCountedByKind(t *testing.T) {
	requirePython(t)
	target := textTarget(t, "value=42\n")
	misleading := pythonCollector("kind_script", `raise ValueError("value is missing and the response size exceeds limit")`)
	missing := testutil.Collector("kind_missing", "text")
	missing.Metrics[0].Expression = `absent=(\d+)`
	missing.Metrics[0].ErrorMode = model.ErrorModeFail
	large := testutil.Collector("kind_large", "text")
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
