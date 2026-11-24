package exporter

import (
	"math"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// OpenMetrics lets a counter, a histogram and a summary say since when they
// have been counting, in a _created sample. A scraper can then tell a
// series that started again from one that merely grew slowly. The exporter
// does not know this for what it reads from targets. It does know it for
// its own series, and selfMetricSet puts their creation time in
// model.Metric.Created:
//
//   - The per-collector counters, a collector's rule failures and its
//     scrape-time histogram count since the collector's statistics were
//     made (serverStats). For a collector of the configuration the exporter
//     started with, that is the exporter's start. For a collector a reload
//     added, or removed and brought back, it is when the collector was
//     first probed or shown. The histogram is part of the statistics and is
//     dropped with them (reconcile.go), and a trip is observed in the
//     statistics it began with (verbosemetrics.go). So a trip that ends
//     after its collector was removed is not observed in the histogram of a
//     collector brought back under the name. The counters of a collector's
//     Python workers count since then too: the worker pool keeps them as
//     long as those statistics are kept, and a run counts in the ones its
//     trip took by them (pythonstats.go).
//   - The per-request counters of verbose mode count since the request
//     began to be tracked: since the start of the probe that got it
//     tracked, which is the first to end of several first probes at once.
//     A request that is dropped, for not being asked for or with its
//     collector, starts again from zero at a later time when it comes back.
//     The time is settled when the request starts being tracked and does
//     not change while it is tracked (requestTracker.adoptLocked).
//   - The reload and OTLP counters, the counters of the Python execution
//     pool as a whole, and the go_ and process_ counters and summary count
//     since the exporter started. The worker pool keeps the pool counters
//     for the life of the process, whatever a reload does.
//
// The _created samples are written only in an OpenMetrics answer of the
// self-metrics endpoint, and only with web.self_metrics.created_timestamps
// (metricsHandler), because without Prometheus' created-timestamp feature
// each one is stored as a series of its own. OTLP has a field for the same
// time, and it is always filled in (otlpMetrics).

// loadedAt is as early as the exporter's own clock goes.
var loadedAt = time.Now()

// exporterStart is when the exporter started: the start of its process, as
// process_start_time_seconds reports it, where /proc says, and otherwise the
// moment the program was loaded. It is one value for the life of the process,
// so a series counting since the start has the same _created at every scrape.
var exporterStart = sync.OnceValue(func() time.Time {
	if seconds, ok := processStartSeconds(); ok {
		if started := time.UnixMilli(int64(math.Round(seconds * 1000))); !started.After(loadedAt) {
			return started
		}
	}
	return loadedAt
})

// createdMillis is a creation time as model.Metric.Created holds it.
func createdMillis(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}

// countingSince gives the counters, histograms and summaries among metrics
// the creation time at, and returns metrics. Gauges have none.
func countingSince(metrics []model.Metric, at time.Time) []model.Metric {
	created := createdMillis(at)
	for i := range metrics {
		switch metrics[i].Type {
		case model.CounterMetricType, model.HistogramMetricType, model.SummaryMetricType:
			metrics[i].Created = created
		}
	}
	return metrics
}

// withoutCreated takes the creation times off a set, which is then written
// without _created samples.
func withoutCreated(set *model.MetricSet) {
	for i := range set.Metrics {
		set.Metrics[i].Created = 0
	}
}
