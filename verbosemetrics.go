package main

import (
	"sort"
	"sync"
	"time"
)

// Two families of self-metrics are only built with verbose self-metrics
// (web.self_metrics.verbose), alongside the per-request series:
//
//   - http_exporter_target_scrape_duration_seconds, a histogram per collector
//     of how long a trip to the target took — request, decode, transform and
//     validation — so "this collector got slow" can be alerted on, where
//     http_exporter_scrape_duration_seconds only holds the last probe's
//     duration. Probes answered from the cache or by sharing another probe's
//     request are not trips to the target and are not observed; scheduled
//     target scrapes are.
//   - the Python worker families, per collector that runs Python: how many
//     workers are starting, idle and busy, how many have started or failed to,
//     why workers stopped, and how runs ended.
//
// Both are bounded: fixed buckets per collector, and fixed sets of states,
// stop reasons and run outcomes. The histogram is only recorded while verbose
// self-metrics are on; the worker counters are kept regardless, since the pool
// counts them anyway, and only published when verbose.

// scrapeDurationBuckets are the histogram's upper bounds in seconds, from a
// fast local endpoint to the slowest scrape a Prometheus timeout allows.
var scrapeDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type durationHistogram struct {
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

type scrapeDurations struct {
	mu         sync.Mutex
	collectors map[string]*durationHistogram
}

func newScrapeDurations() *scrapeDurations {
	return &scrapeDurations{collectors: map[string]*durationHistogram{}}
}

func (d *scrapeDurations) observe(collector string, elapsed time.Duration) {
	seconds := elapsed.Seconds()
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.collectors[collector]
	if h == nil {
		h = &durationHistogram{counts: make([]uint64, len(scrapeDurationBuckets))}
		d.collectors[collector] = h
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

// histogram returns a collector's histogram with cumulative buckets; a
// collector not scraped yet has an empty one.
func (d *scrapeDurations) histogram(collector string) *Histogram {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := &Histogram{}
	h := d.collectors[collector]
	var cumulative uint64
	for i, bound := range scrapeDurationBuckets {
		if h != nil {
			cumulative += h.counts[i]
		}
		out.Buckets = append(out.Buckets, Bucket{UpperBound: bound, CumulativeCount: cumulative})
	}
	if h != nil {
		out.Sum, out.Count = h.sum, h.count
	}
	return out
}

// observeTargetScrape records a trip to the target, when verbose self-metrics
// are on.
func (s *Server) observeTargetScrape(collector string, elapsed time.Duration) {
	if s.verboseSelfMetrics() {
		s.durations.observe(collector, elapsed)
	}
}

const (
	targetScrapeDurationHelp = "Duration of the trips this collector made to its target — request, decode, transform and validation — in seconds. Cache hits and shared probes are not trips."
	pythonWorkersHelp        = "Python workers of this collector, by state: starting, idle or busy."
	pythonWorkerStartsHelp   = "Python workers this collector started."
	pythonStartFailuresHelp  = "Python workers of this collector that failed to start."
	pythonWorkerStopsHelp    = "Python workers of this collector that stopped, by reason: timeout, crash, output_limit, cancelled, retired, surplus or idle."
	pythonRunsHelp           = "Python script runs of this collector, by outcome: ok, script_error, timeout, output_limit or failed."
)

// verboseCollectorMetrics builds the verbose-only families above for the
// configured collectors. It returns nothing unless verbose self-metrics are on.
func (s *Server) verboseCollectorMetrics() []Metric {
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
	var out []Metric
	for _, name := range names {
		out = append(out, Metric{
			Name: "http_exporter_target_scrape_duration_seconds", Help: targetScrapeDurationHelp, Type: HistogramMetricType,
			Labels: map[string]string{"collector": name}, Histogram: s.durations.histogram(name),
		})
	}
	var workers, starts, failures, stops, runs []Metric
	for _, name := range names {
		if !python[name] {
			continue
		}
		snap := pythonWorkers.snapshot(name)
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
		}{{"starting", snap.starting}, {"idle", snap.idle}, {"busy", snap.busy}} {
			workers = append(workers, Metric{Name: "http_exporter_python_workers", Help: pythonWorkersHelp, Type: GaugeMetricType, Labels: labels("state", state.name), Value: float64(state.value)})
		}
		starts = append(starts, Metric{Name: "http_exporter_python_worker_starts_total", Help: pythonWorkerStartsHelp, Type: CounterMetricType, Labels: labels(), Value: float64(snap.starts)})
		failures = append(failures, Metric{Name: "http_exporter_python_worker_start_failures_total", Help: pythonStartFailuresHelp, Type: CounterMetricType, Labels: labels(), Value: float64(snap.startFailures)})
		for _, reason := range pythonStopReasons {
			stops = append(stops, Metric{Name: "http_exporter_python_worker_stops_total", Help: pythonWorkerStopsHelp, Type: CounterMetricType, Labels: labels("reason", reason), Value: float64(snap.stops[reason])})
		}
		for _, outcome := range pythonRunOutcomes {
			runs = append(runs, Metric{Name: "http_exporter_python_runs_total", Help: pythonRunsHelp, Type: CounterMetricType, Labels: labels("outcome", outcome), Value: float64(snap.runs[outcome])})
		}
	}
	// Grouped by family, so each family's HELP and TYPE come once, before its
	// series.
	for _, family := range [][]Metric{workers, starts, failures, stops, runs} {
		out = append(out, family...)
	}
	return out
}
