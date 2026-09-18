package main

import (
	"sort"
	"sync"
	"time"
)

// VerboseRequestSeriesLimit bounds how many collector/URL/method combinations
// the verbose self-metrics track. Section 22 warns against unbounded self-metric
// labels, and a request URL is exactly that: a large or churning target set
// would otherwise grow exporter memory and Prometheus cardinality without end.
// Reaching the limit is reported rather than silent.
const VerboseRequestSeriesLimit = 1000

// requestKey identifies one tracked request. The URL carries no credentials and
// no query string; requestLabelURL strips both.
type requestKey struct {
	Collector string
	URL       string
	Method    string
}

// requestOutcome is the last result observed for one request.
type requestOutcome struct {
	StatusCode int
	Scraped    time.Time
	Duration   time.Duration
}

// requestTracker holds the verbose per-request self-metrics. It is only written
// to while verbose self-metrics are configured, so switching verbose off stops
// the growth and switching it on starts from what happens next.
type requestTracker struct {
	mu         sync.Mutex
	outcomes   map[requestKey]requestOutcome
	capReached bool
}

func newRequestTracker() *requestTracker {
	return &requestTracker{outcomes: map[requestKey]requestOutcome{}}
}

// Record stores the outcome of one scrape. A request already being tracked is
// always updated; a new one is admitted only while there is room under the
// limit, so the series set cannot grow without bound.
func (t *requestTracker) Record(key requestKey, outcome requestOutcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.outcomes[key]; !exists && len(t.outcomes) >= VerboseRequestSeriesLimit {
		t.capReached = true
		return
	}
	t.outcomes[key] = outcome
}

// Snapshot returns the tracked outcomes in a stable order, and whether the
// limit has been reached.
func (t *requestTracker) Snapshot() ([]requestSample, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	samples := make([]requestSample, 0, len(t.outcomes))
	for key, outcome := range t.outcomes {
		samples = append(samples, requestSample{Key: key, Outcome: outcome})
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Key.Collector != samples[j].Key.Collector {
			return samples[i].Key.Collector < samples[j].Key.Collector
		}
		if samples[i].Key.URL != samples[j].Key.URL {
			return samples[i].Key.URL < samples[j].Key.URL
		}
		return samples[i].Key.Method < samples[j].Key.Method
	})
	return samples, t.capReached
}

type requestSample struct {
	Key     requestKey
	Outcome requestOutcome
}

// Reset drops everything tracked so far. Turning verbose off through a
// configuration reload stops the exporter publishing stale request series.
func (t *requestTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.outcomes = map[requestKey]requestOutcome{}
	t.capReached = false
}

// verboseSelfMetrics reports whether the running configuration asks for the
// per-request series.
func (s *Server) verboseSelfMetrics() bool {
	return s.manager.Get().Web.SelfMetrics.Verbose
}

// recordRequest stores one scrape outcome when verbose self-metrics are on. The
// status is the HTTP status code the target returned, or zero when the request
// failed before a response arrived.
func (s *Server) recordRequest(collector, labelURL, method string, statusCode int, duration time.Duration) {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return
	}
	s.requests.Record(
		requestKey{Collector: collector, URL: labelURL, Method: method},
		requestOutcome{StatusCode: statusCode, Scraped: time.Now(), Duration: duration},
	)
}

// verboseRequestMetrics renders the per-request series. It returns nothing when
// verbose self-metrics are off, and clears anything tracked earlier so a
// reload that turns verbose off does not leave stale series behind.
func (s *Server) verboseRequestMetrics() []Metric {
	if !s.verboseSelfMetrics() {
		s.requests.Reset()
		return nil
	}
	samples, capped := s.requests.Snapshot()
	out := make([]Metric, 0, len(samples)*3+1)
	cappedValue := 0.0
	if capped {
		cappedValue = 1
	}
	out = append(out, Metric{
		Name:  "http_exporter_request_series_capped",
		Help:  "Whether verbose per-request self-metrics have reached their series limit.",
		Type:  GaugeMetricType,
		Value: cappedValue,
	})
	for _, sample := range samples {
		labels := map[string]string{
			"collector": sample.Key.Collector,
			"url":       sample.Key.URL,
			"method":    sample.Key.Method,
		}
		out = append(out,
			Metric{
				Name:   "http_exporter_request_last_status_code",
				Help:   "HTTP status code of the last scrape of this request, or 0 when it failed before a response.",
				Type:   GaugeMetricType,
				Value:  float64(sample.Outcome.StatusCode),
				Labels: cloneLabels(labels),
			},
			Metric{
				Name:   "http_exporter_request_last_scrape_timestamp_seconds",
				Help:   "Unix timestamp of the last scrape of this request.",
				Type:   GaugeMetricType,
				Value:  float64(sample.Outcome.Scraped.UnixNano()) / float64(time.Second),
				Labels: cloneLabels(labels),
			},
			Metric{
				Name:   "http_exporter_request_last_duration_seconds",
				Help:   "Duration of the last scrape of this request in seconds.",
				Type:   GaugeMetricType,
				Value:  sample.Outcome.Duration.Seconds(),
				Labels: cloneLabels(labels),
			},
		)
	}
	return out
}
