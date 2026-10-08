//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The statistics of a collector's Python workers follow a reload as the
// collector's own do (pythonstats.go). The tests here make the reloads, by
// the manager as the exporter makes them, and read the self-metrics; how a
// run that is under way at a reload counts, held in its worker's start and
// in its script, is shown where a test can hold it there
// (transform/pythonstats_test.go).

// pythonScripted is a collector with a python transform, as an item of a
// configuration file's collectors; more is what else its definition has, a
// line of YAML, or nothing.
func pythonScripted(name, script, more string) string {
	item := "  - name: " + name + "\n    request: {type: http}\n    limits: {script_timeout: 1m}\n"
	if more != "" {
		item += "    " + more + "\n"
	}
	return item + "    transform:\n      type: python\n      script: |\n        " + strings.ReplaceAll(script, "\n", "\n        ") + "\n"
}

// pythonDocument is a configuration file with verbose self-metrics, created
// timestamps and the collectors given, each an item of the list.
func pythonDocument(collectors ...string) string {
	return "web:\n  self_metrics:\n    verbose: true\n    created_timestamps: true\ncollectors:\n" + strings.Join(collectors, "")
}

// pythonSeriesOf is the lines of a self-metrics answer that are of the
// Python workers of the collector.
func pythonSeriesOf(answer, collector string) []string {
	var lines []string
	for _, line := range shownOf(answer, collector) {
		if strings.HasPrefix(line, "http_exporter_python_") {
			lines = append(lines, line)
		}
	}
	return lines
}

// pythonSeriesFromZero fails the test unless the collector of the answer has
// every series of its Python workers, each at want, which names the series
// that are not at zero, and every creation time of them is back or later.
func pythonSeriesFromZero(t *testing.T, where, answer, collector string, back int64, want map[string]float64) {
	t.Helper()
	lines := pythonSeriesOf(answer, collector)
	// Three states, a start and a start failure counter, ten reasons and six
	// outcomes, and a creation time for each counter.
	if len(lines) != 3+2*(2+len(transform.PythonStopReasons)+len(transform.PythonRunOutcomes)) {
		t.Errorf("%s: the collector %s has %d series of its Python workers:\n%s", where, collector, len(lines), strings.Join(lines, "\n"))
	}
	times := createdTimes(t, answer)
	var early []string
	for _, line := range lines {
		name, _, _ := strings.Cut(line, " ")
		if strings.Contains(name, "_created{") {
			if at := times[name]; at < back {
				early = append(early, fmt.Sprintf("%s at %d", name, at))
			}
			continue
		}
		if got := seriesValue(t, answer, name); got != want[name] {
			t.Errorf("%s: %s is %v, want %v", where, name, got, want[name])
		}
	}
	if len(early) > 0 {
		t.Errorf("%s: %d series of the Python workers of %s are created earlier than %d, when the collector came back, the first: %s", where, len(early), collector, back, early[0])
	}
	for name := range want {
		if !strings.Contains(answer, "\n"+name+" ") {
			t.Errorf("%s: the answer has no %s", where, name)
		}
	}
}

// pythonFamiliesAsTheyWere is the families of the Python workers as
// verboseCollectorMetrics built them before a collector's worker statistics
// followed a reload, for the collectors named, in order: the per-collector
// families and the pool's, all counting since the exporter started.
func pythonFamiliesAsTheyWere(names []string) []model.Metric {
	var out, workers, starts, failures, stops, runs []model.Metric
	for _, name := range names {
		snap := transform.PythonWorkers().Snapshot(name)
		labels := func(extra ...string) map[string]string {
			l := map[string]string{"collector": name}
			for i := 0; i+1 < len(extra); i += 2 {
				l[extra[i]] = extra[i+1]
			}
			return l
		}
		for _, state := range []struct {
			name  string
			value int
		}{{"starting", snap.Starting}, {"idle", snap.Idle}, {"busy", snap.Busy}} {
			workers = append(workers, model.Metric{Name: "http_exporter_python_workers", Help: pythonWorkersHelp, Type: model.GaugeMetricType, Labels: labels("state", state.name), Value: float64(state.value)})
		}
		starts = append(starts, model.Metric{Name: "http_exporter_python_worker_starts_total", Help: pythonWorkerStartsHelp, Type: model.CounterMetricType, Labels: labels(), Value: float64(snap.Starts)})
		failures = append(failures, model.Metric{Name: "http_exporter_python_worker_start_failures_total", Help: pythonStartFailuresHelp, Type: model.CounterMetricType, Labels: labels(), Value: float64(snap.StartFailures)})
		for _, reason := range transform.PythonStopReasons {
			stops = append(stops, model.Metric{Name: "http_exporter_python_worker_stops_total", Help: pythonWorkerStopsHelp, Type: model.CounterMetricType, Labels: labels("reason", reason), Value: float64(snap.Stops[reason])})
		}
		for _, outcome := range transform.PythonRunOutcomes {
			runs = append(runs, model.Metric{Name: "http_exporter_python_runs_total", Help: pythonRunsHelp, Type: model.CounterMetricType, Labels: labels("outcome", outcome), Value: float64(snap.Runs[outcome])})
		}
	}
	for _, family := range [][]model.Metric{workers, starts, failures, stops, runs, pythonPoolMetrics()} {
		out = append(out, countingSince(family, exporterStart())...)
	}
	return out
}

// pythonFamiliesOf is the families of the Python workers in the server's own
// metrics, which are those it serves and those it exports over OTLP.
func pythonFamiliesOf(server *Server) []model.Metric {
	var out []model.Metric
	for _, m := range server.selfMetricSet().Metrics {
		if strings.HasPrefix(m.Name, "http_exporter_python_") {
			out = append(out, m)
		}
	}
	return out
}

// The statistics of a collector's Python workers follow a reload. Before
// any reload the worker families are, series for series, what they were
// before they did. A reload that removes a collector drops them: the
// collector has no series left, in what the exporter serves and in what it
// exports over OTLP alike, and the stop of its worker for the reload is
// counted for the pool as a whole alone, which has lost nothing of what the
// collector counted. A collector whose script the reload changed keeps its
// counters, the stop of its worker for the reload among them; one whose
// definition changed otherwise keeps them and its worker; an unchanged one
// keeps everything. The collector added again under the name has every
// series from zero, created no earlier than its return, and counts its
// first probe as a first.
func TestACollectorsPythonWorkerStatisticsFollowAReload(t *testing.T) {
	requirePython(t)
	good, bad := textTarget(t, "value=42\n"), textTarget(t, "bad\n")
	const script = "if \"bad\" in data:\n    fail(\"refused\")\nmetric(name=\"v\", value=1)"
	document := func(gone bool, changed, retuned string) string {
		collectors := []string{pythonScripted("changed", changed, ""), pythonScripted("retuned", script, retuned), pythonScripted("kept", script, "")}
		if gone {
			collectors = append(collectors, pythonScripted("gone", script, ""))
		}
		return pythonDocument(collectors...)
	}
	r := newReloadable(t, document(true, script, ""), "")
	read := func() string {
		_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
		return body
	}
	probe := func(collector, target string, want int) {
		t.Helper()
		if got := probeOnce(t, r.server, probePath(collector, target, ""), nil); got.Code != want {
			t.Fatalf("the probe of %s was answered %d, want %d: %s", collector, got.Code, want, got.Body)
		}
	}
	for _, collector := range []string{"gone", "changed", "retuned", "kept"} {
		probe(collector, good.URL, http.StatusOK)
	}
	probe("gone", bad.URL, http.StatusBadGateway)
	series := func(family, collector, label string) string {
		if label != "" {
			label = "," + label
		}
		return family + `{collector="` + collector + `"` + label + `}`
	}
	started := read()
	if got, want := pythonFamiliesOf(r.server), pythonFamiliesAsTheyWere([]string{"changed", "gone", "kept", "retuned"}); !reflect.DeepEqual(got, want) {
		t.Errorf("before any reload the families of the Python workers are\n%+v\nand they were\n%+v", got, want)
	}
	for name, want := range map[string]float64{
		series("http_exporter_python_worker_starts_total", "gone", ""):                  1,
		series("http_exporter_python_runs_total", "gone", `outcome="ok"`):               1,
		series("http_exporter_python_runs_total", "gone", `outcome="script_error"`):     1,
		series("http_exporter_python_workers", "gone", `state="idle"`):                  1,
		"http_exporter_python_pool_worker_starts_total":                                 4,
		`http_exporter_python_pool_runs_total{outcome="ok"}`:                            4,
		`http_exporter_python_pool_runs_total{outcome="script_error"}`:                  1,
		`http_exporter_python_pool_workers{state="idle"}`:                               4,
		`http_exporter_python_pool_worker_stops_total{reason="reload"}`:                 0,
		series("http_exporter_python_worker_stops_total", "changed", `reason="reload"`): 0,
	} {
		if got := seriesValue(t, started, name); got != want {
			t.Fatalf("before any reload %s is %v, want %v", name, got, want)
		}
	}

	const changedScript = "metric(name=\"changed\", value=2)"
	r.reloadTo(document(false, changedScript, "max_concurrent_probes: 3"))
	removed := read()
	if shown := shownOf(removed, "gone"); len(shown) > 0 {
		t.Errorf("the removed collector is shown:\n%s", strings.Join(shown, "\n"))
	}
	for _, m := range r.server.selfMetricSet().Metrics {
		if m.Labels["collector"] == "gone" {
			t.Errorf("the removed collector is in what the exporter exports: %s%v", m.Name, m.Labels)
		}
	}
	for name, want := range map[string]float64{
		// What the removed collector counted, and its worker's stop, stay
		// counted for the pool.
		"http_exporter_python_pool_worker_starts_total":                                 4,
		`http_exporter_python_pool_runs_total{outcome="ok"}`:                            4,
		`http_exporter_python_pool_runs_total{outcome="script_error"}`:                  1,
		`http_exporter_python_pool_worker_stops_total{reason="reload"}`:                 2,
		`http_exporter_python_pool_workers{state="idle"}`:                               2,
		series("http_exporter_python_worker_stops_total", "changed", `reason="reload"`): 1,
		series("http_exporter_python_worker_starts_total", "changed", ""):               1,
		series("http_exporter_python_runs_total", "changed", `outcome="ok"`):            1,
		series("http_exporter_python_workers", "changed", `state="idle"`):               0,
	} {
		if got := seriesValue(t, removed, name); got != want {
			t.Errorf("after the reload that removed a collector %s is %v, want %v", name, got, want)
		}
	}
	// Its counters are created when they were: the collector stayed.
	was, now := createdTimes(t, started), createdTimes(t, removed)
	for _, name := range pythonSeriesOf(removed, "changed") {
		if name, _, _ = strings.Cut(name, " "); strings.Contains(name, "_created{") && now[name] != was[name] {
			t.Errorf("%s is %d after the reload that changed the collector's script, and was %d", name, now[name], was[name])
		}
	}
	for _, collector := range []string{"retuned", "kept"} {
		if got, want := pythonSeriesOf(removed, collector), pythonSeriesOf(started, collector); !reflect.DeepEqual(got, want) || len(got) == 0 {
			t.Errorf("the series of the Python workers of %s after the reload:\n%s\nwere:\n%s", collector, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	time.Sleep(2 * time.Millisecond)
	back := time.Now().UnixMilli()
	r.reloadTo(document(true, changedScript, "max_concurrent_probes: 3"))
	returned := read()
	pythonSeriesFromZero(t, "when the collector is back", returned, "gone", back, nil)
	exported, carried := 0, 0
	for _, m := range pythonFamiliesOf(r.server) {
		if m.Labels["collector"] != "gone" {
			continue
		}
		exported++
		if m.Value != 0 || m.Type == model.CounterMetricType && m.Created < back {
			carried++
		}
	}
	if carried > 0 || exported != 3+2+len(transform.PythonStopReasons)+len(transform.PythonRunOutcomes) {
		t.Errorf("of the %d series of the Python workers of the collector added again that the exporter exports, %d are not at zero or are created earlier than %d, when it came back", exported, carried, back)
	}
	for _, collector := range []string{"changed", "retuned", "kept"} {
		if got, want := pythonSeriesOf(returned, collector), pythonSeriesOf(removed, collector); !reflect.DeepEqual(got, want) {
			t.Errorf("the series of the Python workers of %s after the reload that added another collector:\n%s\nwere:\n%s", collector, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	probe("gone", good.URL, http.StatusOK)
	probed := read()
	pythonSeriesFromZero(t, "after the first probe of the collector that is back", probed, "gone", back, map[string]float64{
		series("http_exporter_python_worker_starts_total", "gone", ""):    1,
		series("http_exporter_python_runs_total", "gone", `outcome="ok"`): 1,
		series("http_exporter_python_workers", "gone", `state="idle"`):    1,
	})
	if got := seriesValue(t, probed, "http_exporter_python_pool_worker_starts_total"); got != 5 {
		t.Errorf("the pool has started %v workers, want the 5 there were", got)
	}
	strictlyRead(t, started, probed)
}

// noInterpreter is a stand-in for python3 that runs no script: asked to
// check a configuration's scripts, it finds nothing wrong with them, and
// started as a worker it exits at once, so that a script's run fails with
// its worker's start. A test that only counts what the pool counted, and
// under which name, reloads and probes with it at the cost of a shell.
func noInterpreter(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	path := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\necho '{\"problems\": []}'\n"), 0o700); err != nil { //nolint:gosec // a script has to be executable
		t.Fatal(err)
	}
	return path
}

// newUninterpreted is newReloadable with noInterpreter for its Python and a
// worker pool of its own: every run of a script fails, and is counted as a
// worker that failed to start and a run that failed.
func newUninterpreted(t *testing.T, document, targets string) *reloadable {
	t.Helper()
	usePythonPool(t)
	interpreter := noInterpreter(t)
	r := newReloadable(t, document, targets)
	r.manager.SetPythonPath(interpreter)
	r.server.pythonPath = interpreter
	return r
}

// startFailures is the two series a run counts in for a collector whose
// worker fails to start, each at runs.
func startFailures(collector string, runs float64) map[string]float64 {
	if runs == 0 {
		return nil
	}
	return map[string]float64{
		`http_exporter_python_worker_start_failures_total{collector="` + collector + `"}`: runs,
		`http_exporter_python_runs_total{collector="` + collector + `",outcome="failed"}`: runs,
	}
}

// A probe that read its collector before a reload removed it runs its
// script afterwards, when its target has answered: the run is counted under
// no collector's name, whether the probe had taken the collector's
// statistics when they were dropped or only read the configuration, and
// whether it goes on while the collector is gone or when it is back. The
// collector added again has every series of its Python workers at zero,
// nothing is kept under the name meanwhile, and the run is counted for the
// pool as a whole. A debug probe, whose scripts run in the collector's
// workers too, is held to the same.
func TestAProbeOfARemovedCollectorRunsItsScriptUnderNoName(t *testing.T) {
	for _, held := range []string{"when it had read the configuration", "waiting for its target", "a debug probe waiting for its target"} {
		for _, goesOnLate := range []bool{false, true} {
			where := fmt.Sprintf("a probe held %s, going on after the collector is back %v", held, goesOnLate)
			both := pythonDocument(pythonScripted("kept", "metric(name=\"v\", value=1)", ""), pythonScripted("gone", "metric(name=\"v\", value=1)", ""))
			r := newUninterpreted(t, both, "")
			r.server.SetProbeDebug(true)
			read := func() string {
				_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
				return body
			}
			var old <-chan probeOutcome
			var goOn func()
			switch held {
			case "when it had read the configuration":
				target := textTarget(t, "value=42\n")
				reached, resume := holdFirstProbeOf(t, "gone")
				old, goOn = probeAsync(context.Background(), r.server, probePath("gone", target.URL, ""), nil), resume
				<-reached
			case "waiting for its target":
				target, arrived, release := slowFirstTarget(t)
				old, goOn = probeAsync(context.Background(), r.server, probePath("gone", target.URL, ""), nil), release
				<-arrived
			default:
				target, arrived, release := slowFirstTarget(t)
				old, goOn = probeAsync(context.Background(), r.server, probePath("gone", target.URL, "&debug=true"), nil), release
				<-arrived
			}
			r.reloadTo(pythonDocument(pythonScripted("kept", "metric(name=\"v\", value=1)", "")))
			time.Sleep(2 * time.Millisecond)
			back := time.Now().UnixMilli()
			if goesOnLate {
				r.reloadTo(both)
			}
			goOn()
			outcome := <-old
			if !strings.Contains(outcome.body, "the interpreter did not start") {
				t.Fatalf("%s: the probe did not run its script: %d %s", where, outcome.code, outcome.body)
			}
			if got := transform.PythonWorkers().Snapshot("gone"); got.StartFailures != 0 || len(got.Runs) != 0 {
				t.Errorf("%s: the run of the removed collector is counted under its name: %+v", where, got)
			}
			if !goesOnLate {
				if shown := shownOf(read(), "gone"); len(shown) > 0 {
					t.Errorf("%s: the removed collector is shown:\n%s", where, strings.Join(shown, "\n"))
				}
				r.reloadTo(both)
			}
			answer := read()
			pythonSeriesFromZero(t, where, answer, "gone", back, nil)
			for _, name := range []string{"http_exporter_python_pool_worker_start_failures_total", `http_exporter_python_pool_runs_total{outcome="failed"}`} {
				if got := seriesValue(t, answer, name); got != 1 {
					t.Errorf("%s: %s is %v, want the run of the removed collector", where, name, got)
				}
			}
			probeConfigReadHook.Store(nil)
		}
	}
}

// A static target's scrape runs its collector's script in the same workers,
// and counts in the same statistics, as a probe: its run is counted for the
// collector; a reload that removes the collector with its target drops the
// worker series, and the collector added again starts from zero; a scrape
// that read the collector before it was removed, and runs its script when
// the collector is back, is counted under no collector's name; and the
// first scrape of the collector that is back is counted as a first.
func TestAStaticTargetsCollectorHasItsPythonWorkerStatisticsFollowAReload(t *testing.T) {
	target, other := textTarget(t, "value=42\n"), textTarget(t, "value=1\n")
	kept := pythonScripted("kept", "metric(name=\"v\", value=1)", "")
	both := pythonDocument(kept, pythonScripted("gone", "metric(name=\"v\", value=1)", ""))
	r := newUninterpreted(t, both, staticDocument("scraped", "gone", target.URL))
	read := func() string {
		_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
		return body
	}
	scrape := func(as *followedConfig) {
		r.server.scrapeTargetSince(context.Background(), as.config, as.generation, as.targets.Targets[0])
	}
	first := r.server.followedInForce()
	scrape(first)
	pythonSeriesFromZero(t, "after the target's first scrape", read(), "gone", 0, startFailures("gone", 1))

	r.reloadBoth(pythonDocument(kept), staticDocument("stays", "kept", other.URL))
	if shown := shownOf(read(), "gone"); len(shown) > 0 {
		t.Errorf("the removed collector is shown:\n%s", strings.Join(shown, "\n"))
	}
	time.Sleep(2 * time.Millisecond)
	back := time.Now().UnixMilli()
	r.reloadBoth(both, staticDocument("scraped", "gone", target.URL))
	pythonSeriesFromZero(t, "when the collector is back", read(), "gone", back, nil)
	// The scrape that read the collector before it was removed.
	scrape(first)
	late := read()
	pythonSeriesFromZero(t, "after the scrape that had read the removed collector", late, "gone", back, nil)
	if got := seriesValue(t, late, "http_exporter_python_pool_worker_start_failures_total"); got != 2 {
		t.Errorf("the pool counts %v workers that failed to start, want the two scrapes'", got)
	}
	scrape(r.server.followedInForce())
	pythonSeriesFromZero(t, "after the first scrape of the collector that is back", read(), "gone", back, startFailures("gone", 1))
}

// Probes and reloads at once, under the race detector: probes of a
// collector that stays and of one that reloads remove and add again run
// their scripts while the reloads come. Every script that ran is counted
// once for the pool as a whole; the collector that stays has counted every
// one of its own, through every reload; the one added again no more than
// the probes of its name; and when the probes have ended and one more
// reload has removed and added it, it is at zero in every series.
func TestProbesAndReloadsTogetherCountEveryPythonRunOnce(t *testing.T) {
	kept := pythonScripted("kept", "metric(name=\"v\", value=1)", "")
	both := pythonDocument(kept, pythonScripted("gone", "metric(name=\"v\", value=1)", ""))
	r := newUninterpreted(t, both, "")
	target := textTarget(t, "value=42\n")
	const probers, each = 3, 8
	var ran [2]atomic.Int64
	var probing sync.WaitGroup
	var asked atomic.Int64
	for range probers {
		probing.Add(1)
		go func() {
			defer probing.Done()
			for range each {
				for i, collector := range []string{"kept", "gone"} {
					// Each a request of its own, which shares no trip.
					path := probePath(collector, fmt.Sprintf("%s/?probe=%d", target.URL, asked.Add(1)), "")
					outcome := <-probeAsync(context.Background(), r.server, path, nil)
					switch {
					case strings.Contains(outcome.body, "the interpreter did not start"):
						ran[i].Add(1)
					case collector == "kept" || outcome.code != http.StatusBadRequest:
						t.Errorf("the probe of %s was answered %d: %s", collector, outcome.code, outcome.body)
					}
				}
			}
		}()
	}
	stop, reloaded := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(reloaded)
		for {
			for _, document := range []string{pythonDocument(kept), both} {
				select {
				case <-stop:
					return
				default:
				}
				r.reloadTo(document)
				_ = selfMetrics(t, r.server)
			}
		}
	}()
	probing.Wait()
	close(stop)
	<-reloaded
	r.reloadTo(both)
	answer := selfMetrics(t, r.server)
	runs := float64(ran[0].Load() + ran[1].Load())
	if got := seriesValue(t, answer, "http_exporter_python_pool_worker_start_failures_total"); got != runs || ran[0].Load() != probers*each {
		t.Errorf("the pool counts %v scripts that ran, want the %v of the probes, %d of them of the collector that stayed", got, runs, ran[0].Load())
	}
	if got := seriesValue(t, answer, `http_exporter_python_worker_start_failures_total{collector="kept"}`); got != probers*each {
		t.Errorf("the collector that stayed counts %v scripts that ran, want its %d", got, probers*each)
	}
	if got := seriesValue(t, answer, `http_exporter_python_worker_start_failures_total{collector="gone"}`); got > float64(ran[1].Load()) {
		t.Errorf("the collector added again counts %v scripts that ran, more than the %d of its name", got, ran[1].Load())
	}
	for _, state := range []string{"starting", "busy", "idle"} {
		if got := seriesValue(t, answer, `http_exporter_python_pool_workers{state="`+state+`"}`); got != 0 {
			t.Errorf("the pool has %v workers %s when every probe has ended", got, state)
		}
	}
	r.reloadTo(pythonDocument(kept))
	r.reloadTo(both)
	_, last := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
	pythonSeriesFromZero(t, "when the probes have ended and the collector was removed and added again", last, "gone", 0, nil)
	if got := seriesValue(t, last, `http_exporter_python_worker_start_failures_total{collector="kept"}`); got != probers*each {
		t.Errorf("the collector that stayed counts %v scripts that ran after the last reloads, want its %d", got, probers*each)
	}
}
