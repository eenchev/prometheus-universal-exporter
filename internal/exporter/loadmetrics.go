package exporter

import (
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// loadCounters add up the time the exporter spends on static target scrapes
// and on probes, for autoscaling (docs/SELF-METRICS.md#autoscaling). Their
// rate is the average number of scrapes or probes in progress, which a
// HorizontalPodAutoscaler can average over the pods; the gauges of what is in
// progress (http_exporter_trips_in_flight) are only what a scrape happens to
// catch. They are two numbers for the whole exporter, never labelled by
// collector or target, so they cost the same however many targets there are,
// and kept in nanoseconds so they add up without a lock.
type loadCounters struct {
	staticNanos atomic.Int64
	probeNanos  atomic.Int64
}

// addStaticScrape counts the time one static target scrape took.
func (l *loadCounters) addStaticScrape(elapsed time.Duration) {
	if elapsed > 0 {
		l.staticNanos.Add(int64(elapsed))
	}
}

// addProbe counts the time one probe took.
func (l *loadCounters) addProbe(elapsed time.Duration) {
	if elapsed > 0 {
		l.probeNanos.Add(int64(elapsed))
	}
}

// loadMetrics are http_exporter_static_targets_scrape_seconds_total and
// http_exporter_probes_seconds_total. Both are there from the start, at 0,
// whether or not the exporter has static targets or is probed, so a rule that
// adds the two up never finds one missing.
func (s *Server) loadMetrics() []model.Metric {
	return []model.Metric{
		{Name: "http_exporter_static_targets_scrape_seconds_total", Help: exporterMetricHelp["http_exporter_static_targets_scrape_seconds_total"], Type: model.CounterMetricType, Value: time.Duration(s.load.staticNanos.Load()).Seconds()},
		{Name: "http_exporter_probes_seconds_total", Help: exporterMetricHelp["http_exporter_probes_seconds_total"], Type: model.CounterMetricType, Value: time.Duration(s.load.probeNanos.Load()).Seconds()},
	}
}
