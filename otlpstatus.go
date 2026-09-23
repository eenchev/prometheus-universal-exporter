package main

import (
	"sync"
	"time"
)

// OTLP export is best-effort: a failed export never fails a probe. Best-effort
// is not the same as silent, though. An endpoint that went away, a token that
// expired or a collector that answers 429 for hours would otherwise show only
// as warnings in the log and as data missing from the backend, where nobody
// looks for a cause. These self-metrics say how exports are going, for as long
// as OTLP is enabled:
//
//	http_exporter_otlp_exports_total{result}
//	http_exporter_otlp_export_retries_total
//	http_exporter_otlp_points_dropped_total
//	http_exporter_otlp_export_duration_seconds
//	http_exporter_otlp_last_export_success_timestamp_seconds
//
// An export is one delivery of everything pending, its retries included, so a
// retried export that got through is one success, and one that ran out of
// retries is one failure. They are exported over OTLP too, so the backend
// learns of a failed export at the next one that gets through.
//
// With otlp.unready_after_failures set, consecutive failures also make the
// exporter unready (readiness.go). They are counted per endpoint: a reload
// that points OTLP somewhere else starts the count again, so a new endpoint
// is not held responsible for the old one's failures.

type otlpStatus struct {
	mu                  sync.Mutex
	successes, failures uint64
	retries, dropped    int
	lastDuration        time.Duration
	lastSuccess         time.Time
	// consecutiveFailures counts the exports to endpoint since the last
	// success.
	consecutiveFailures int
	endpoint            string
}

// record records an export to endpoint and how many retries it took.
func (o *otlpStatus) record(endpoint string, duration time.Duration, retries int, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if endpoint != o.endpoint {
		o.endpoint, o.consecutiveFailures = endpoint, 0
	}
	o.lastDuration = duration
	o.retries += retries
	if ok {
		o.successes++
		o.lastSuccess = time.Now()
		o.consecutiveFailures = 0
		return
	}
	o.failures++
	o.consecutiveFailures++
}

// drop counts data points given up on.
func (o *otlpStatus) drop(points int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropped += points
}

// failing reports the exports to endpoint that have failed since the last
// success; none when the last exports went elsewhere.
func (o *otlpStatus) failing(endpoint string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if endpoint != o.endpoint {
		return 0
	}
	return o.consecutiveFailures
}

// otlpStatusMetrics renders the export status while OTLP is enabled, one
// family at a time.
func (s *Server) otlpStatusMetrics() []Metric {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" {
		return nil
	}
	o := s.otlp
	o.mu.Lock()
	defer o.mu.Unlock()
	metric := func(name string, typ MetricType, value float64, labels map[string]string) Metric {
		return Metric{Name: name, Help: exporterMetricHelp[name], Type: typ, Value: value, Labels: labels}
	}
	return []Metric{
		metric("http_exporter_otlp_exports_total", CounterMetricType, float64(o.successes), map[string]string{"result": "success"}),
		metric("http_exporter_otlp_exports_total", CounterMetricType, float64(o.failures), map[string]string{"result": "failure"}),
		metric("http_exporter_otlp_export_retries_total", CounterMetricType, float64(o.retries), nil),
		metric("http_exporter_otlp_points_dropped_total", CounterMetricType, float64(o.dropped), nil),
		metric("http_exporter_otlp_export_duration_seconds", GaugeMetricType, o.lastDuration.Seconds(), nil),
		metric("http_exporter_otlp_last_export_success_timestamp_seconds", GaugeMetricType, scrapeTimestamp(o.lastSuccess), nil),
	}
}
