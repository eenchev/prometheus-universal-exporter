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

// Register makes a request visible before anything is known about it, so a
// configured target has its series from the first scrape of /self-metrics
// rather than only after it has been collected once. An already tracked
// request keeps the outcome it has, because registering is not a scrape: a
// response served from the collector cache, for instance, must not overwrite
// the status and timestamp of the scrape that filled it.
func (t *requestTracker) Register(key requestKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.outcomes[key]; exists {
		return
	}
	if len(t.outcomes) >= VerboseRequestSeriesLimit {
		t.capReached = true
		return
	}
	t.outcomes[key] = requestOutcome{}
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
// failed before a response arrived. It is called only when the exporter
// actually went to the target: a response served from the collector cache
// registers the request instead, so the reported status and timestamp keep
// describing a real scrape.
func (s *Server) recordRequest(collector, labelURL, method string, statusCode int, duration time.Duration) {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return
	}
	s.requests.Record(
		requestKey{Collector: collector, URL: labelURL, Method: method},
		requestOutcome{StatusCode: statusCode, Scraped: time.Now(), Duration: duration},
	)
}

// registerRequest makes a request's series exist without claiming a scrape.
func (s *Server) registerRequest(collector, labelURL, method string) {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return
	}
	s.requests.Register(requestKey{Collector: collector, URL: labelURL, Method: method})
}

// seedScheduledRequests registers every scheduled target, so the targets the
// configuration names are visible before their first collection and remain
// visible across a reload that adds one. A request driven by /probe cannot be
// seeded this way: its URL comes from the probe's own target parameter, so it
// appears the first time it is asked for.
func (s *Server) seedScheduledRequests() {
	cfg := s.manager.Get()
	for _, target := range s.manager.Targets() {
		c := collectorByName(cfg, target.Collector)
		if c == nil {
			continue
		}
		overrides := target.overrides()
		resolved, err := resolveRequestURL(target.Target, c, overrides)
		if err != nil {
			continue
		}
		s.registerRequest(c.Name, requestLabelURL(resolved), requestMethod(c, overrides))
	}
}

// verboseRequestMetrics renders the per-request series. It returns nothing when
// verbose self-metrics are off, and clears anything tracked earlier so a
// reload that turns verbose off does not leave stale series behind.
func (s *Server) verboseRequestMetrics() []Metric {
	if !s.verboseSelfMetrics() {
		s.requests.Reset()
		return nil
	}
	s.seedScheduledRequests()
	samples, capped := s.requests.Snapshot()
	out := make([]Metric, 0, len(samples)*3+2)
	cappedValue := 0.0
	if capped {
		cappedValue = 1
	}
	out = append(out,
		Metric{
			Name:  "http_exporter_request_series_capped",
			Help:  "Whether verbose per-request self-metrics have reached their series limit.",
			Type:  GaugeMetricType,
			Value: cappedValue,
		},
		// Without this, a verbose exporter that has not been asked for a probe
		// yet reports only the cap gauge, which reads as though the feature is
		// doing nothing rather than as an empty set.
		Metric{
			Name:  "http_exporter_request_series_tracked",
			Help:  "Number of collector, URL and method combinations the verbose self-metrics track.",
			Type:  GaugeMetricType,
			Value: float64(len(samples)),
		},
	)
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
				Help:   "Unix timestamp of the last scrape of this request, or 0 when it has not been scraped yet.",
				Type:   GaugeMetricType,
				Value:  scrapeTimestamp(sample.Outcome.Scraped),
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

// scrapeTimestamp reports a registered but never scraped request as 0 rather
// than as the zero instant, which would otherwise appear as a timestamp far in
// the past and read as a very stale scrape.
func scrapeTimestamp(at time.Time) float64 {
	if at.IsZero() {
		return 0
	}
	return float64(at.UnixNano()) / float64(time.Second)
}
