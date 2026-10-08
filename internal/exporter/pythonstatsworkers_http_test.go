//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A worker is of the statistics it was started for (transform.PythonStats),
// and a trip's scripts count in the statistics of the collector the trip
// read (pythonstats.go). The tests here hold the exporter to both through
// its own reloads, probes, debug probes and answers; the pool is held to the
// first over every order of events where a test can drive it by hand
// (transform/pythonstatsworkers_test.go).

const workerScript, otherWorkerScript = `metric(name="v", value=1)`, `metric(name="other", value=2)`

// workerMetrics is the self-metrics answer of r's server, with the creation
// times.
func workerMetrics(t *testing.T, r *reloadable) string {
	t.Helper()
	_, body := answerOf(t, r.server, "/self-metrics", prometheus2Accept)
	return body
}

// failedStartsOf is how many of the collector's workers the answer counts as
// having failed to start: its runs, where no interpreter starts
// (newUninterpreted).
func failedStartsOf(t *testing.T, answer, collector string) float64 {
	t.Helper()
	return seriesValue(t, answer, `http_exporter_python_worker_start_failures_total{collector="`+collector+`"}`)
}

// workersOf is what the answer shows of the collector's workers: how many
// it started, how many were stopped under its name, for whatever reason, and
// how many it shows idle or busy.
func workersOf(t *testing.T, answer, collector string) (starts, stops, alive float64) {
	t.Helper()
	series := func(family, label string) float64 {
		return seriesValue(t, answer, family+`{collector="`+collector+`"`+label+`}`)
	}
	starts = series("http_exporter_python_worker_starts_total", "")
	for _, reason := range transform.PythonStopReasons {
		stops += series("http_exporter_python_worker_stops_total", `,reason="`+reason+`"`)
	}
	return starts, stops, series("http_exporter_python_workers", `,state="idle"`) + series("http_exporter_python_workers", `,state="busy"`)
}

// With python3 itself, by the manager's reloads and the server's probes: a
// probe of a collector waits for its target while a reload removes the
// collector, and goes on when the next has brought the collector back as it
// was, or while it is still gone. Its script then runs in a worker that is
// counted for the pool alone and stopped when the script ends, for the
// reload: no worker is left idle, whether the script is in use again or the
// run began when it no longer was, where the worker used to be kept until
// the idle timeout. The collector added again has every series at zero, and
// its first probe starts a worker of its own, which is the one its series
// show from then on: idle after the probe, and stopped under its name by a
// reload that changes its script. Before a worker belonged to its
// statistics, the collector added again was given the removed collector's
// worker: it showed one idle with none started, and counted its stop.
func TestAProbeOfARemovedCollectorLeavesNoWorkerToTheCollectorAddedAgain(t *testing.T) {
	for _, back := range []bool{true, false} {
		t.Run(fmt.Sprintf("the collector back when the probe goes on %v", back), func(t *testing.T) {
			requirePython(t)
			kept := pythonScripted("kept", workerScript, "")
			both := pythonDocument(kept, pythonScripted("gone", workerScript, ""))
			r := newReloadable(t, both, "")
			pool := func(when, answer string, want map[string]float64) {
				t.Helper()
				for name, value := range want {
					if got := seriesValue(t, answer, name); got != value {
						t.Errorf("%s %s is %v, want %v", when, name, got, value)
					}
				}
			}
			// A probe of the collector waits for its target.
			slow, arrived, release := slowFirstTarget(t)
			old := probeAsync(context.Background(), r.server, probePath("gone", slow.URL, ""), nil)
			<-arrived
			r.reloadTo(pythonDocument(kept))
			if back {
				r.reloadTo(both)
			}
			release()
			if outcome := <-old; outcome.code != http.StatusOK {
				t.Fatalf("the probe of the removed collector was answered %d: %s", outcome.code, outcome.body)
			}
			ran := workerMetrics(t, r)
			if back {
				pythonSeriesFromZero(t, "when the removed collector's probe has run its script", ran, "gone", 0, nil)
			} else if shown := shownOf(ran, "gone"); len(shown) > 0 {
				t.Errorf("the removed collector is shown:\n%s", strings.Join(shown, "\n"))
			}
			pool("when the removed collector's probe has run its script", ran, map[string]float64{
				"http_exporter_python_pool_worker_starts_total":                 1,
				`http_exporter_python_pool_runs_total{outcome="ok"}`:            1,
				`http_exporter_python_pool_worker_stops_total{reason="reload"}`: 1,
				`http_exporter_python_pool_workers{state="idle"}`:               0,
				`http_exporter_python_pool_workers{state="busy"}`:               0,
			})
			if !back {
				r.reloadTo(both)
			}

			// The first probe of the collector that is back.
			target := textTarget(t, "value=1\n")
			if got := probeOnce(t, r.server, probePath("gone", target.URL, ""), nil); got.Code != http.StatusOK {
				t.Fatalf("the probe of the collector added again was answered %d: %s", got.Code, got.Body)
			}
			probed := workerMetrics(t, r)
			if starts, stops, alive := workersOf(t, probed, "gone"); starts != 1 || stops != 0 || alive != 1 {
				t.Errorf("after its first probe the collector added again has started %v workers, stopped %v and shows %v, want one of its own, idle", starts, stops, alive)
			}
			pool("after the first probe of the collector added again", probed, map[string]float64{
				"http_exporter_python_pool_worker_starts_total":   2,
				`http_exporter_python_pool_workers{state="idle"}`: 1,
			})

			// A reload changes the collector's script, which keeps its
			// counters and stops the worker of the script that changed.
			r.reloadTo(pythonDocument(kept, pythonScripted("gone", otherWorkerScript, "")))
			changed := workerMetrics(t, r)
			if starts, stops, alive := workersOf(t, changed, "gone"); starts != 1 || stops != 1 || alive != 0 {
				t.Errorf("after the reload that changed its script the collector added again has started %v workers, stopped %v and shows %v, want its one stopped", starts, stops, alive)
			}
			pool("after the reload that changed the script", changed, map[string]float64{
				`http_exporter_python_worker_stops_total{collector="gone",reason="reload"}`: 1,
				`http_exporter_python_pool_worker_stops_total{reason="reload"}`:             2,
				`http_exporter_python_pool_workers{state="idle"}`:                           0,
			})
		})
	}
}

// /static-targets?debug=<name> reads its target and collector with the
// generation of the configuration they are in, before it reads the target's
// credential file. A reload that comes while it reads the file, and leaves
// the target's collector as it is, takes nothing from the collector: the
// scrape's script, which runs in the collector's worker, is counted for the
// collector, as a scheduled scrape's is. Asked for after the file was read,
// the configuration's generation was none by then, and the run was counted
// under no collector.
//
// The credential file is a named pipe, so the test knows the scrape is
// reading it: opening the pipe for writing returns when the scrape has
// opened it for reading. Nothing waits for time.
func TestADebugScrapeOfAStaticTargetCountsForItsCollectorThroughAReloadThatKeepsIt(t *testing.T) {
	target := textTarget(t, "value=42\n")
	credential := filepath.Join(t.TempDir(), "token")
	if err := syscall.Mkfifo(credential, 0o600); err != nil {
		t.Skip("no named pipe:", err)
	}
	kept := pythonScripted("kept", workerScript, "")
	targets := "interval: 1m\ntargets:\n  - name: scraped\n    collector: kept\n    target: " + target.URL + "\n    request:\n      bearer_token_file: " + credential + "\n"
	r := newUninterpreted(t, pythonDocument(kept), targets)
	r.server.SetProbeDebug(true)
	scrape := func(between func()) {
		t.Helper()
		debug := probeAsync(context.Background(), r.server, "/static-targets?debug=scraped", nil)
		// The scrape has read the configuration and reads its credential.
		pipe, err := os.OpenFile(credential, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		between()
		if _, err := pipe.WriteString("secret\n"); err != nil {
			t.Fatal(err)
		}
		if err := pipe.Close(); err != nil {
			t.Fatal(err)
		}
		if outcome := <-debug; !strings.Contains(outcome.body, "the interpreter did not start") {
			t.Fatalf("the debug scrape did not run its script: %d %s", outcome.code, outcome.body)
		}
	}
	// With no reload the run is counted for the collector.
	scrape(func() {})
	if got := failedStartsOf(t, workerMetrics(t, r), "kept"); got != 1 {
		t.Fatalf("the collector counts %v runs after a debug scrape of its static target, want 1", got)
	}
	// A reload adds another collector, and leaves this one as it is.
	scrape(func() {
		r.reloadTo(pythonDocument(kept, pythonScripted("added", otherWorkerScript, "")))
	})
	answer := workerMetrics(t, r)
	if got := failedStartsOf(t, answer, "kept"); got != 2 {
		t.Errorf("the collector, which no reload removed or changed, counts %v runs after two debug scrapes of its static target, want 2: the run of the scrape a reload came into is counted under no collector", got)
	}
	if got := seriesValue(t, answer, "http_exporter_python_pool_worker_start_failures_total"); got != 2 {
		t.Errorf("the pool counts %v runs, want the 2 there were", got)
	}
	// A reload that removes the collector while the scrape reads the file
	// does take the run from it, as it does a scheduled scrape's.
	scrape(func() {
		r.reloadBoth(pythonDocument(pythonScripted("added", otherWorkerScript, "")), staticDocument("other", "added", target.URL))
		r.reloadBoth(pythonDocument(kept, pythonScripted("added", otherWorkerScript, "")), targets)
	})
	answer = workerMetrics(t, r)
	pythonSeriesFromZero(t, "after a debug scrape that read the collector before it was removed", answer, "kept", 0, nil)
	if got := seriesValue(t, answer, "http_exporter_python_pool_worker_start_failures_total"); got != 3 {
		t.Errorf("the pool counts %v runs, want the 3 there were", got)
	}
}

// A debug probe, and the debug scrape of a static target, are counted in no
// self-metric of their own: not as a scrape of the collector, in no
// per-request series. Their scripts run in the collector's workers, which
// are the pool's real workers, and the Python worker series count what the
// workers did: the runs are counted for the collector while it is there, as
// a probe's are. With a debug probe always handed the pool's departed, or
// the debug scrape of a static target naming no generation, the runs would
// be counted under no collector, and the worker one of them started would be
// the collector's idle worker with no start counted for it.
func TestADebugProbeOfACollectorThatIsThereCountsItsScriptsForItAndNothingElse(t *testing.T) {
	target := textTarget(t, "value=42\n")
	r := newUninterpreted(t, pythonDocument(pythonScripted("scripted", workerScript, "")), staticDocument("scraped", "scripted", target.URL))
	r.server.SetProbeDebug(true)
	if got := probeOnce(t, r.server, probePath("scripted", target.URL, "&debug=true"), nil); !strings.Contains(got.Body.String(), "the interpreter did not start") {
		t.Fatalf("the debug probe did not run its script: %d %s", got.Code, got.Body)
	}
	if got := failedStartsOf(t, workerMetrics(t, r), "scripted"); got != 1 {
		t.Errorf("the collector counts %v runs after a debug probe of it, want 1", got)
	}
	if got := probeOnce(t, r.server, "/static-targets?debug=scraped", nil); !strings.Contains(got.Body.String(), "the interpreter did not start") {
		t.Fatalf("the debug scrape did not run its script: %d %s", got.Code, got.Body)
	}
	answer := workerMetrics(t, r)
	if got := failedStartsOf(t, answer, "scripted"); got != 2 {
		t.Errorf("the collector counts %v runs after a debug probe and a debug scrape of its static target, want 2", got)
	}
	if got := seriesValue(t, answer, `http_exporter_python_runs_total{collector="scripted",outcome="failed"}`); got != 2 {
		t.Errorf("the collector counts %v runs that failed after a debug probe and a debug scrape, want 2", got)
	}
	if got := seriesValue(t, answer, `http_exporter_scrapes_total{collector="scripted"}`); got != 0 {
		t.Errorf("the debug probe and scrape are counted as %v scrapes of the collector", got)
	}
}

// One answer of the self-metrics reads a collector's statistics first and
// what its Python workers counted later. Two reloads between the two, one
// that removes the collector and one that adds it again, and probes of the
// collector added again, do not make the answer show what that one's workers
// counted with the creation time of the collector it read: the answer shows
// the collector it read, with no worker counts, as it shows one a reload has
// just removed. The next answer shows the collector added again, its counts
// with its own creation time.
func TestAnAnswerShowsNoWorkerCountsOfACollectorAddedAgainWhileItWasMade(t *testing.T) {
	kept := pythonScripted("kept", workerScript, "")
	both := pythonDocument(kept, pythonScripted("gone", workerScript, ""))
	r := newUninterpreted(t, both, "")
	target := textTarget(t, "value=42\n")
	probe := func() {
		t.Helper()
		if got := probeOnce(t, r.server, probePath("gone", target.URL, ""), nil); !strings.Contains(got.Body.String(), "the interpreter did not start") {
			t.Fatalf("the probe did not run its script: %d %s", got.Code, got.Body)
		}
	}
	created := func(answer string) int64 {
		t.Helper()
		for name, at := range createdTimes(t, answer) {
			if strings.HasPrefix(name, "http_exporter_python_worker_start_failures") && strings.Contains(name, `collector="gone"`) {
				return at
			}
		}
		t.Fatalf("the answer has no creation time of the collector's start failures:\n%s", strings.Join(pythonSeriesOf(answer, "gone"), "\n"))
		return 0
	}
	probe()
	before := workerMetrics(t, r)
	was := created(before)
	if got := failedStartsOf(t, before, "gone"); got != 1 {
		t.Fatalf("the collector counts %v runs after its probe, want 1", got)
	}
	// Creation times are whole milliseconds: the collector added again is
	// made in a later one.
	time.Sleep(2 * time.Millisecond)
	var once sync.Once
	hook := func(held bool) {
		if held {
			return
		}
		// The answer has read the collectors' statistics.
		once.Do(func() {
			r.reloadTo(pythonDocument(kept))
			r.reloadTo(both)
			probe()
			probe()
		})
	}
	statsReadHook.Store(&hook)
	t.Cleanup(func() { statsReadHook.Store(nil) })
	during := workerMetrics(t, r)
	statsReadHook.Store(nil)
	if got := seriesValue(t, during, `http_exporter_scrapes_total{collector="gone"}`); got != 1 {
		t.Fatalf("the answer shows %v scrapes of the collector, want the 1 of the collector it read", got)
	}
	if got, at := failedStartsOf(t, during, "gone"), created(during); got != 0 || at != was {
		t.Errorf("the answer that read the removed collector shows %v runs of its workers, created at %d: want none, at the %d of the collector it read; the collector added again has counted 2", got, at, was)
	}
	after := workerMetrics(t, r)
	if got, at := failedStartsOf(t, after, "gone"), created(after); got != 2 || at <= was {
		t.Errorf("the next answer shows %v runs of the workers of the collector added again, created at %d: want its 2, created later than %d", got, at, was)
	}
}
