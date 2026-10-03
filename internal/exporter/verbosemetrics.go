package exporter

import (
	"sort"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// Two families of self-metrics are only built with verbose self-metrics
// (web.self_metrics.verbose), alongside the per-request series:
//
//   - http_exporter_collector_scrape_duration_seconds, a histogram per collector
//     of how long a trip to the target took — request, decode, transform and
//     validation — so "this collector got slow" can be alerted on, where
//     http_exporter_scrape_duration_seconds only holds the last probe's
//     duration. Probes answered from the cache or by sharing another probe's
//     request are not trips to the target and are not observed; static
//     target scrapes are.
//   - the Python worker families, per collector that runs Python: how many
//     workers are starting, idle and busy, how many have started or failed to,
//     why workers stopped, and how runs ended.
//   - the same for the Python execution pool as a whole, without a collector
//     label. These are always published in verbose mode, Python collectors or
//     not, so the pool's status can be read and alerted on without knowing
//     which collectors use it.
//
// Both are bounded: fixed buckets per collector, and fixed sets of states,
// stop reasons and run outcomes. The histogram is only recorded while verbose
// self-metrics are on; the worker counters are kept regardless, since the pool
// counts them anyway, and only published when verbose.

// scrapeDurationBuckets are the histogram's upper bounds in seconds, from a
// fast local endpoint to the slowest scrape a Prometheus timeout allows.
var scrapeDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// durationHistogram is one collector's histogram. It is part of the
// collector's statistics (serverStats), whose lock guards it, and not kept by
// the collector's name: a trip records its duration where it records its
// counters, so a trip that ends after a reload removed its collector, and
// dropped those statistics, cannot be observed in the histogram of a collector
// brought back under the name, which would then hold an observation older
// than its creation time (selfcreated.go).
type durationHistogram struct {
	counts []uint64 // per bucket, not cumulative; made by the first observation
	sum    float64
	count  uint64
}

func (h *durationHistogram) observe(elapsed time.Duration) {
	seconds := elapsed.Seconds()
	if h.counts == nil {
		h.counts = make([]uint64, len(scrapeDurationBuckets))
	}
	h.sum += seconds
	h.count++
	for i, bound := range scrapeDurationBuckets {
		if seconds <= bound {
			h.counts[i]++
			return
		}
	}
}

// histogram returns the histogram with cumulative buckets; a collector not
// scraped yet has an empty one.
func (h *durationHistogram) histogram() *model.Histogram {
	out := &model.Histogram{Sum: h.sum, Count: h.count}
	var cumulative uint64
	for i, bound := range scrapeDurationBuckets {
		if h.counts != nil {
			cumulative += h.counts[i]
		}
		out.Buckets = append(out.Buckets, model.Bucket{UpperBound: bound, CumulativeCount: cumulative})
	}
	return out
}

// tripHistogram is the scrape-time histogram of a collector's statistics.
func (s *serverStats) tripHistogram() *model.Histogram {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.durations.histogram()
}

// observeTargetScrape records a trip to the target in the statistics the trip
// counts in, when verbose self-metrics are on.
func (s *Server) observeTargetScrape(stats *serverStats, elapsed time.Duration) {
	if stats == nil || !s.verboseSelfMetrics() {
		return
	}
	stats.mu.Lock()
	stats.durations.observe(elapsed)
	stats.mu.Unlock()
}

const (
	targetScrapeDurationHelp = "Duration of the trips this collector made to its target — request, decode, transform and validation — in seconds. Cache hits and shared probes are not trips."
	pythonWorkersHelp        = "Python workers of this collector, by state: starting, idle or busy."
	pythonWorkerStartsHelp   = "Python workers this collector started."
	pythonStartFailuresHelp  = "Python workers of this collector that failed to start."
	pythonWorkerStopsHelp    = "Python workers of this collector that stopped, by reason: timeout, deadline, crash, output_limit, cancelled, retired, surplus, idle, reload or evicted."
	pythonRunsHelp           = "Python script runs of this collector, by outcome: ok, script_error, timeout, deadline, output_limit or failed."

	pythonPoolWorkersHelp       = "Python workers in the execution pool, across all collectors, by state: starting, idle or busy."
	pythonPoolStartsHelp        = "Python workers the execution pool started, across all collectors."
	pythonPoolStartFailuresHelp = "Python workers the execution pool failed to start, across all collectors."
	pythonPoolStopsHelp         = "Python workers the execution pool stopped, across all collectors, by reason: timeout, deadline, crash, output_limit, cancelled, retired, surplus, idle, reload or evicted."
	pythonPoolRunsHelp          = "Python script runs in the execution pool, across all collectors, by outcome: ok, script_error, timeout, deadline, output_limit or failed."
	pythonPoolWaitingHelp       = "Python script runs waiting for a worker, because --python.max-workers workers are busy or starting."
	tripsWaitingHelp            = "Static target scrapes waiting in line for a trip slot, because their collector is at max_concurrent_probes or the exporter at --probe.max-concurrent."
)

// pythonPoolMetrics builds the pool-wide Python families.
func pythonPoolMetrics() []model.Metric {
	snap := transform.PythonWorkers().PoolSnapshot()
	var out []model.Metric
	for _, state := range []struct {
		name  string
		value int
	}{{"starting", snap.Starting}, {"idle", snap.Idle}, {"busy", snap.Busy}} {
		out = append(out, model.Metric{Name: "http_exporter_python_pool_workers", Help: pythonPoolWorkersHelp, Type: model.GaugeMetricType, Labels: map[string]string{"state": state.name}, Value: float64(state.value)})
	}
	out = append(out,
		model.Metric{Name: "http_exporter_python_pool_worker_starts_total", Help: pythonPoolStartsHelp, Type: model.CounterMetricType, Labels: map[string]string{}, Value: float64(snap.Starts)},
		model.Metric{Name: "http_exporter_python_pool_worker_start_failures_total", Help: pythonPoolStartFailuresHelp, Type: model.CounterMetricType, Labels: map[string]string{}, Value: float64(snap.StartFailures)},
		model.Metric{Name: "http_exporter_python_pool_runs_waiting", Help: pythonPoolWaitingHelp, Type: model.GaugeMetricType, Labels: map[string]string{}, Value: float64(snap.Waiting)},
	)
	for _, reason := range transform.PythonStopReasons {
		out = append(out, model.Metric{Name: "http_exporter_python_pool_worker_stops_total", Help: pythonPoolStopsHelp, Type: model.CounterMetricType, Labels: map[string]string{"reason": reason}, Value: float64(snap.Stops[reason])})
	}
	for _, outcome := range transform.PythonRunOutcomes {
		out = append(out, model.Metric{Name: "http_exporter_python_pool_runs_total", Help: pythonPoolRunsHelp, Type: model.CounterMetricType, Labels: map[string]string{"outcome": outcome}, Value: float64(snap.Runs[outcome])})
	}
	return out
}

// verboseCollectorMetrics builds the verbose-only families above for the
// configured collectors, whose statistics are stats: a collector's histogram
// is part of those, and counts since they do (selfcreated.go). It returns
// nothing unless verbose self-metrics are on.
func (s *Server) verboseCollectorMetrics(stats map[string]*serverStats) []model.Metric {
	if !s.verboseSelfMetrics() {
		return nil
	}
	collectors := s.manager.Get().Collectors
	names := make([]string, 0, len(collectors))
	python := map[string]bool{}
	for _, c := range collectors {
		names = append(names, c.Name)
		if c.Transform.Type == "python" || c.Transform.PreScript != "" {
			python[c.Name] = true
		}
	}
	sort.Strings(names)
	var out []model.Metric
	for _, name := range names {
		// The configuration is read again here, and a reload may have added a
		// collector since stats were taken: its statistics are made now, as
		// its first probe would make them, so its histogram has at once the
		// creation time it keeps.
		collector := stats[name]
		if collector == nil {
			collector = s.statsFor(name)
		}
		out = append(out, model.Metric{
			Name: "http_exporter_collector_scrape_duration_seconds", Help: targetScrapeDurationHelp, Type: model.HistogramMetricType,
			Labels: map[string]string{"collector": name}, Histogram: collector.tripHistogram(), Created: createdMillis(collector.snapshot().created),
		})
	}
	var workers, starts, failures, stops, runs []model.Metric
	for _, name := range names {
		if !python[name] {
			continue
		}
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
	// Grouped by family, so each family's HELP and TYPE come once, before its
	// series.
	// The worker pool keeps a collector's counts, and its own, for the life
	// of the process, also across a reload that removes the collector, so
	// these count since the exporter started.
	for _, family := range [][]model.Metric{workers, starts, failures, stops, runs, pythonPoolMetrics()} {
		out = append(out, countingSince(family, exporterStart())...)
	}
	return append(out, model.Metric{Name: "http_exporter_trips_waiting", Help: tripsWaitingHelp, Type: model.GaugeMetricType, Labels: map[string]string{}, Value: float64(s.trips.waitingCount())})
}
