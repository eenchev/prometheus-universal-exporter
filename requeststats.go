package main

import (
	"sort"
	"sync"
	"time"
)

// VerboseRequestSeriesLimit bounds how many collector/URL/method combinations
// the verbose self-metrics track. Section 22 warns against unbounded
// self-metric labels, and a request URL is exactly that: a large or churning
// target set would otherwise grow exporter memory and Prometheus cardinality
// without end. Each combination carries the whole self-metric family, so the
// limit is what keeps the endpoint bounded. Reaching it is reported rather than
// silent.
const VerboseRequestSeriesLimit = 1000

// requestKey identifies one tracked request. The URL carries no credentials and
// no query string; requestLabelURL strips both.
type requestKey struct {
	Collector string
	URL       string
	Method    string
}

// requestTracker holds the statistics of individual requests, which the verbose
// self-metrics publish beside the per-collector totals. It is only written to
// while verbose self-metrics are configured, so switching verbose off stops the
// growth and switching it on starts from what happens next.
type requestTracker struct {
	mu         sync.Mutex
	stats      map[requestKey]*serverStats
	capReached bool
}

func newRequestTracker() *requestTracker {
	return &requestTracker{stats: map[requestKey]*serverStats{}}
}

// statsFor returns the statistics of one request, creating them on first use.
// It returns nil once the limit is reached, so a caller updating a new request
// simply records nothing while requests already tracked keep updating.
func (t *requestTracker) statsFor(key requestKey) *serverStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing := t.stats[key]; existing != nil {
		return existing
	}
	if len(t.stats) >= VerboseRequestSeriesLimit {
		t.capReached = true
		return nil
	}
	created := &serverStats{}
	t.stats[key] = created
	return created
}

// Snapshot returns a value copy of every tracked request in a stable order, and
// whether the limit has been reached.
func (t *requestTracker) Snapshot() ([]requestSample, bool) {
	t.mu.Lock()
	keys := make([]requestKey, 0, len(t.stats))
	byKey := make(map[requestKey]*serverStats, len(t.stats))
	for key, stats := range t.stats {
		keys = append(keys, key)
		byKey[key] = stats
	}
	capReached := t.capReached
	t.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Collector != keys[j].Collector {
			return keys[i].Collector < keys[j].Collector
		}
		if keys[i].URL != keys[j].URL {
			return keys[i].URL < keys[j].URL
		}
		return keys[i].Method < keys[j].Method
	})
	samples := make([]requestSample, 0, len(keys))
	for _, key := range keys {
		samples = append(samples, requestSample{Key: key, Values: byKey[key].snapshot()})
	}
	return samples, capReached
}

type requestSample struct {
	Key    requestKey
	Values statsValues
}

// Reset drops everything tracked so far. Turning verbose off through a
// configuration reload stops the exporter publishing stale request series.
func (t *requestTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats = map[requestKey]*serverStats{}
	t.capReached = false
}

// verboseSelfMetrics reports whether the running configuration asks for the
// per-request series.
func (s *Server) verboseSelfMetrics() bool {
	return s.manager.Get().Web.SelfMetrics.Verbose
}

// statsRecorder applies one update to a collector's own statistics and, when
// verbose self-metrics are configured, to the statistics of the individual
// request as well. Going through it is what keeps the two views from drifting:
// a counter can never be raised on one and not the other.
type statsRecorder struct {
	collector *serverStats
	request   *serverStats
}

// recorderFor pairs a collector's statistics with those of one request. The
// request side is absent while verbose self-metrics are off, when the URL could
// not be resolved, and once the series limit has been reached.
func (s *Server) recorderFor(collector *serverStats, name, labelURL, method string) statsRecorder {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return statsRecorder{collector: collector}
	}
	return statsRecorder{
		collector: collector,
		request:   s.requests.statsFor(requestKey{Collector: name, URL: labelURL, Method: method}),
	}
}

// update applies the same change to both sets of statistics.
func (r statsRecorder) update(apply func(*serverStats)) {
	for _, stats := range [2]*serverStats{r.collector, r.request} {
		if stats == nil {
			continue
		}
		stats.mu.Lock()
		apply(stats)
		stats.mu.Unlock()
	}
}

// scraped stamps the request with the moment it was last collected. It is only
// called when the exporter actually went to the target: a response served from
// the collector cache leaves the stamp describing the scrape that filled it.
func (r statsRecorder) scraped(at time.Time) {
	if r.request == nil {
		return
	}
	r.request.mu.Lock()
	r.request.lastScrape = at
	r.request.mu.Unlock()
}

// registerRequest makes a request's series exist without claiming a scrape, so
// a configured target is visible before it has been collected once.
func (s *Server) registerRequest(collector, labelURL, method string) {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return
	}
	s.requests.statsFor(requestKey{Collector: collector, URL: labelURL, Method: method})
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
		label, err := requestLabelFor(target.Target, c, overrides)
		if err != nil {
			continue
		}
		s.registerRequest(c.Name, label, requestMethodFor(c, overrides))
	}
}

// verboseRequestSeriesNames are the self-metric families the verbose mode
// republishes per request: the per-collector families that have a per-request
// value, which leaves out http_exporter_cache_entries.
func verboseRequestSeriesNames() []string {
	var names []string
	for _, d := range selfMetricDescriptors {
		if d.Value != nil {
			names = append(names, d.Name)
		}
	}
	return names
}

// requestLabels builds the label set of one tracked request. The method label
// is http_method rather than method so it reads unambiguously beside a metric
// family that also has a url.
func requestLabels(key requestKey) map[string]string {
	return map[string]string{"collector": key.Collector, "http_method": key.Method, "url": key.URL}
}

// verboseRequests returns the tracked requests, whose series join the
// per-collector families in selfMetricSet, and the families only verbose mode
// has. Both are empty unless verbose self-metrics are configured, and turning
// verbose off forgets the requests.
func (s *Server) verboseRequests() ([]requestSample, []Metric) {
	if !s.verboseSelfMetrics() {
		s.requests.Reset()
		return nil, nil
	}
	s.seedScheduledRequests()
	samples, capped := s.requests.Snapshot()
	cappedValue := 0.0
	if capped {
		cappedValue = 1
	}
	out := []Metric{
		{
			Name:  "http_exporter_request_series_capped",
			Help:  "Whether verbose per-request self-metrics have reached their series limit.",
			Type:  GaugeMetricType,
			Value: cappedValue,
		},
		// Without this, a verbose exporter that has not been asked for a probe
		// yet reports only the cap gauge, which reads as though the feature is
		// doing nothing rather than as an empty set.
		{
			Name:  "http_exporter_request_series_tracked",
			Help:  "Number of collector, URL and method combinations the verbose self-metrics track.",
			Type:  GaugeMetricType,
			Value: float64(len(samples)),
		},
	}
	for _, sample := range samples {
		out = append(out, Metric{
			Name:   "http_exporter_request_last_scrape_timestamp_seconds",
			Help:   "Unix timestamp of the last scrape of this request, or 0 when it has not been scraped yet.",
			Type:   GaugeMetricType,
			Value:  scrapeTimestamp(sample.Values.lastScrape),
			Labels: requestLabels(sample.Key),
		})
	}
	return samples, out
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
