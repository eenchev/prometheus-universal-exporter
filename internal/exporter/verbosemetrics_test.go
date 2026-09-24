package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// With verbose self-metrics, the exporter publishes a histogram of trips to
// the target per collector, and the state of each collector's Python workers
// (verbosemetrics.go). Without verbose, neither exists: see verboseOnlyNames.

func textTarget(t *testing.T, body string) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(target.Close)
	return target
}

func TestScrapeDurationHistogramBuckets(t *testing.T) {
	d := newScrapeDurations()
	for _, elapsed := range []time.Duration{3 * time.Millisecond, 5 * time.Millisecond, 70 * time.Millisecond, 2 * time.Second, 90 * time.Second} {
		d.observe("c", elapsed)
	}
	h := d.histogram("c")
	cumulative := map[float64]uint64{}
	for _, b := range h.Buckets {
		cumulative[b.UpperBound] = b.CumulativeCount
	}
	// A value on a boundary lands in that bucket; one above the last bound
	// only in +Inf, which is the count.
	for bound, want := range map[float64]uint64{0.005: 2, 0.05: 2, 0.1: 3, 1: 3, 2.5: 4, 60: 4} {
		if cumulative[bound] != want {
			t.Errorf("le=%v: %d, want %d", bound, cumulative[bound], want)
		}
	}
	if h.Count != 5 || h.Sum < 92.07 || h.Sum > 92.08 {
		t.Fatalf("count=%d sum=%v", h.Count, h.Sum)
	}
	if empty := d.histogram("never"); empty.Count != 0 || len(empty.Buckets) != len(scrapeDurationBuckets) {
		t.Fatalf("an unscraped collector: %+v", empty)
	}
}

func TestTargetScrapeHistogramIsPublishedWhenVerbose(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	server := verboseServer(t, true, testutil.Collector("timed", "text"), testutil.Collector("idle", "text"))
	for i := 0; i < 2; i++ {
		probeOnce(t, server, "/probe?collector=timed&target="+url.QueryEscape(target.URL), nil)
	}
	exposition := selfMetrics(t, server)
	for _, want := range []string{
		"# TYPE http_exporter_collector_scrape_duration_seconds histogram\n",
		`http_exporter_collector_scrape_duration_seconds_bucket{collector="timed",le="60"} 2` + "\n",
		`http_exporter_collector_scrape_duration_seconds_bucket{collector="timed",le="+Inf"} 2` + "\n",
		`http_exporter_collector_scrape_duration_seconds_count{collector="timed"} 2` + "\n",
		// A collector not scraped yet has its series, at zero.
		`http_exporter_collector_scrape_duration_seconds_count{collector="idle"} 0` + "\n",
	} {
		if !strings.Contains(exposition, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(exposition, "# TYPE http_exporter_collector_scrape_duration_seconds "); n != 1 {
		t.Errorf("the family is declared %d times", n)
	}
	// The same series go out over OTLP.
	found := false
	for _, m := range server.selfMetricSet().Metrics {
		if m.Name == "http_exporter_collector_scrape_duration_seconds" && m.Labels["collector"] == "timed" && m.Histogram != nil && m.Histogram.Count == 2 {
			found = true
		}
	}
	if !found {
		t.Fatal("the histogram is missing from the OTLP self-metric set")
	}
}

// Only trips to the target are observed: not cache hits, not probes that
// shared another's request; static target scrapes are.
func TestOnlyTripsToTheTargetAreObserved(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	c := testutil.Collector("cached_timed", "text")
	c.Cache.TTL = model.Duration(time.Minute)
	server := verboseServer(t, true, c)
	for i := 0; i < 3; i++ {
		probeOnce(t, server, "/probe?collector=cached_timed&target="+url.QueryEscape(target.URL), nil)
	}
	if n := server.durations.histogram("cached_timed").Count; n != 1 {
		t.Fatalf("observed %d trips for one trip and two cache hits", n)
	}

	gated := newGatedTarget(t, http.StatusOK, "value=42\n")
	shared := verboseServer(t, true, testutil.Collector("shared_timed", "text"))
	var outcomes []<-chan probeOutcome
	for i := 0; i < 3; i++ {
		outcomes = append(outcomes, probeAsync(context.Background(), shared, probePath("shared_timed", gated.URL, ""), nil))
	}
	waitForWaiters(t, shared, 3)
	gated.open()
	for _, outcome := range outcomes {
		<-outcome
	}
	if n := shared.durations.histogram("shared_timed").Count; n != 1 {
		t.Fatalf("observed %d trips for three probes sharing one", n)
	}

	scheduled := testutil.Collector("static_timed", "text")
	cfg := &model.Config{Collectors: []model.Collector{scheduled}, OTLP: otlpConfig("http://collector.invalid/v1/metrics"), Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}}}
	server = newStaticServer(t, cfg, &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "eu", Collector: "static_timed", Target: target.URL}}})
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	if n := server.durations.histogram("static_timed").Count; n != 1 {
		t.Fatalf("observed %d trips for one static target scrape", n)
	}
}

// Without verbose, nothing is recorded, so the histogram costs nothing.
func TestTheHistogramIsNotRecordedWhenNotVerbose(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	server := verboseServer(t, false, testutil.Collector("untimed", "text"))
	probeOnce(t, server, "/probe?collector=untimed&target="+url.QueryEscape(target.URL), nil)
	if n := server.durations.histogram("untimed").Count; n != 0 {
		t.Fatalf("recorded %d observations with verbose off", n)
	}
	for _, m := range server.selfMetricSet().Metrics {
		if strings.HasPrefix(m.Name, "http_exporter_target_scrape_duration") || strings.HasPrefix(m.Name, "http_exporter_python_") {
			t.Fatalf("%s is in the OTLP self-metric set with verbose off", m.Name)
		}
	}
}

func pythonCollector(name, script string) model.Collector {
	return model.Collector{
		Name: name, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "text"},
		Transform: model.TransformConfig{Type: "python", Script: script},
		Limits:    model.Limits{ScriptTimeout: model.Duration(2 * time.Second)},
	}
}

func seriesValue(t *testing.T, exposition, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(exposition, "\n") {
		if value, found := strings.CutPrefix(line, series+" "); found {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("no series %s", series)
	return 0
}

func TestPythonWorkerMetrics(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	good := pythonCollector("py_metrics_good", `metric(name="v", value=1)`)
	failing := pythonCollector("py_metrics_error", `raise ValueError("bad data")`)
	slow := pythonCollector("py_metrics_slow", "while True:\n    pass\n")
	slow.Limits.ScriptTimeout = model.Duration(100 * time.Millisecond)
	crashing := pythonCollector("py_metrics_crash", "import os\nos._exit(1)\n")
	server := verboseServer(t, true, good, failing, slow, crashing, testutil.Collector("py_metrics_none", "text"))

	probe := func(collector string) {
		probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil)
	}
	probe("py_metrics_good")
	probe("py_metrics_good")
	probe("py_metrics_error")
	probe("py_metrics_error")
	probe("py_metrics_slow")
	probe("py_metrics_crash")

	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		// Two runs in one worker, now idle.
		`http_exporter_python_workers{collector="py_metrics_good",state="idle"}`:     1,
		`http_exporter_python_workers{collector="py_metrics_good",state="busy"}`:     0,
		`http_exporter_python_workers{collector="py_metrics_good",state="starting"}`: 0,
		`http_exporter_python_worker_starts_total{collector="py_metrics_good"}`:      1,
		`http_exporter_python_runs_total{collector="py_metrics_good",outcome="ok"}`:  2,
		// A script error keeps its worker.
		`http_exporter_python_runs_total{collector="py_metrics_error",outcome="script_error"}`: 2,
		`http_exporter_python_worker_starts_total{collector="py_metrics_error"}`:               1,
		`http_exporter_python_workers{collector="py_metrics_error",state="idle"}`:              1,
		// A timeout kills its worker.
		`http_exporter_python_runs_total{collector="py_metrics_slow",outcome="timeout"}`:        1,
		`http_exporter_python_worker_stops_total{collector="py_metrics_slow",reason="timeout"}`: 1,
		`http_exporter_python_workers{collector="py_metrics_slow",state="idle"}`:                0,
		// So does a crash.
		`http_exporter_python_runs_total{collector="py_metrics_crash",outcome="failed"}`:       1,
		`http_exporter_python_worker_stops_total{collector="py_metrics_crash",reason="crash"}`: 1,
		`http_exporter_python_worker_start_failures_total{collector="py_metrics_crash"}`:       0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	// Every reason and outcome has a series, so rates work from the start.
	for _, reason := range transform.PythonStopReasons {
		seriesValue(t, exposition, `http_exporter_python_worker_stops_total{collector="py_metrics_good",reason="`+reason+`"}`)
	}
	// Each family is declared once, however many collectors it has series for.
	for _, family := range []string{"http_exporter_python_workers", "http_exporter_python_worker_starts_total", "http_exporter_python_worker_start_failures_total", "http_exporter_python_worker_stops_total", "http_exporter_python_runs_total"} {
		for _, line := range []string{"# HELP " + family + " ", "# TYPE " + family + " "} {
			if n := strings.Count(exposition, line); n != 1 {
				t.Errorf("%q appears %d times, want 1", line, n)
			}
		}
	}
	// A collector without Python has no worker series.
	if strings.Contains(exposition, `http_exporter_python_workers{collector="py_metrics_none"`) {
		t.Fatal("a collector without Python has worker series")
	}
}

// A histogram passed through from a Prometheus source keeps one +Inf bucket.
// It used to be written twice: once from the source's own +Inf bucket and once
// from the count.
func TestAPassedThroughHistogramHasOneInfBucket(t *testing.T) {
	testutil.CaptureLogs(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# TYPE latency_seconds histogram\nlatency_seconds_bucket{le=\"1\"} 2\nlatency_seconds_bucket{le=\"+Inf\"} 3\nlatency_seconds_sum 1.5\nlatency_seconds_count 3\n"))
	}))
	defer upstream.Close()
	server := verboseServer(t, false, model.Collector{Name: "passthrough", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "prometheus"}})
	body := probeOnce(t, server, "/probe?collector=passthrough&target="+url.QueryEscape(upstream.URL), nil).Body.String()
	if n := strings.Count(body, `latency_seconds_bucket{le="+Inf"}`); n != 1 {
		t.Fatalf("the +Inf bucket appears %d times:\n%s", n, body)
	}
}

// The pool-wide families are published in verbose mode even when no collector
// runs Python, and not at all otherwise.
func TestPythonPoolMetricsWithoutPythonCollectors(t *testing.T) {
	exposition := selfMetrics(t, verboseServer(t, true, testutil.Collector("pool_jq_only", "text")))
	for _, series := range []string{
		`http_exporter_python_pool_workers{state="starting"}`,
		`http_exporter_python_pool_workers{state="idle"}`,
		`http_exporter_python_pool_workers{state="busy"}`,
		`http_exporter_python_pool_worker_starts_total`,
		`http_exporter_python_pool_worker_start_failures_total`,
	} {
		seriesValue(t, exposition, series)
	}
	for _, reason := range transform.PythonStopReasons {
		seriesValue(t, exposition, `http_exporter_python_pool_worker_stops_total{reason="`+reason+`"}`)
	}
	for _, outcome := range transform.PythonRunOutcomes {
		seriesValue(t, exposition, `http_exporter_python_pool_runs_total{outcome="`+outcome+`"}`)
	}
	for _, family := range []string{"http_exporter_python_pool_workers", "http_exporter_python_pool_worker_starts_total", "http_exporter_python_pool_worker_start_failures_total", "http_exporter_python_pool_worker_stops_total", "http_exporter_python_pool_runs_total"} {
		for _, line := range []string{"# HELP " + family + " ", "# TYPE " + family + " "} {
			if n := strings.Count(exposition, line); n != 1 {
				t.Errorf("%q appears %d times, want 1", line, n)
			}
		}
	}
	if strings.Contains(exposition, "http_exporter_python_workers{") {
		t.Fatal("a jq-only exporter has per-collector worker series")
	}
	if strings.Contains(selfMetrics(t, verboseServer(t, false, testutil.Collector("pool_jq_only", "text"))), "http_exporter_python_pool_") {
		t.Fatal("pool series without verbose self-metrics")
	}
}

// The pool-wide values are the sum over every collector, and keep counting
// runs of collectors that are no longer configured.
func TestPythonPoolMetricsSumTheCollectors(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	first := pythonCollector("pool_sum_a", `metric(name="v", value=1)`)
	second := pythonCollector("pool_sum_b", `raise ValueError("bad data")`)
	server := verboseServer(t, true, first, second)
	before := transform.PythonWorkers().PoolSnapshot()

	probe := func(collector string) {
		probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil)
	}
	probe("pool_sum_a")
	probe("pool_sum_a")
	probe("pool_sum_b")

	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_python_pool_runs_total{outcome="ok"}`:           float64(before.Runs["ok"] + 2),
		`http_exporter_python_pool_runs_total{outcome="script_error"}`: float64(before.Runs["script_error"] + 1),
		`http_exporter_python_pool_worker_starts_total`:                float64(before.Starts + 2),
		`http_exporter_python_pool_workers{state="idle"}`:              float64(before.Idle + 2),
		`http_exporter_python_pool_workers{state="busy"}`:              0,
		`http_exporter_python_pool_workers{state="starting"}`:          0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	// A server that no longer has these collectors still counts their runs.
	later := selfMetrics(t, verboseServer(t, true, testutil.Collector("pool_sum_none", "text")))
	if got := seriesValue(t, later, `http_exporter_python_pool_runs_total{outcome="ok"}`); got != float64(before.Runs["ok"]+2) {
		t.Errorf("after the collectors went, ok runs = %v", got)
	}
}

// A family name means one thing everywhere it is delivered. The scheduled
// target health series go over OTLP beside the self-metrics, so none of the
// verbose families may reuse one of their names with another type.
func TestVerboseFamiliesDoNotReuseStaticHealthNames(t *testing.T) {
	server := verboseServer(t, true, pythonCollector("names_python", `metric(name="v", value=1)`))
	types := map[string]model.MetricType{}
	for _, m := range server.verboseCollectorMetrics() {
		types[m.Name] = m.Type
	}
	c := testutil.Collector("names_text", "text")
	for _, m := range staticTargetHealthMetrics(model.StaticTarget{Name: "t", Target: "http://a.example"}, &c, 1, 0.1, time.Now()).Metrics {
		if other, clash := types[m.Name]; clash && other != m.Type {
			t.Errorf("%s is a %s in the verbose self-metrics and a %s in the static target health series", m.Name, other, m.Type)
		}
		if _, clash := types[m.Name]; clash {
			t.Errorf("%s is both a verbose self-metric and a static target health series", m.Name)
		}
	}
}
