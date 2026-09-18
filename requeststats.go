package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
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
		resolved, err := resolveRequestURL(target.Target, c, overrides)
		if err != nil {
			continue
		}
		s.registerRequest(c.Name, requestLabelURL(resolved), requestMethod(c, overrides))
	}
}

// verboseRequestSeriesNames are the self-metric families the verbose mode
// republishes per request. They are the per-collector families minus
// http_exporter_cache_entries, which counts the entries a collector's cache
// holds and belongs to no single request.
func verboseRequestSeriesNames() []string {
	names := make([]string, 0, len(selfMetricNames))
	for _, name := range selfMetricNames {
		if name == "http_exporter_cache_entries" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// requestLabels builds the label set of one tracked request. The method label
// is http_method rather than method so it reads unambiguously beside a metric
// family that also has a url.
func requestLabels(key requestKey) map[string]string {
	return map[string]string{"collector": key.Collector, "http_method": key.Method, "url": key.URL}
}

// verboseRequestMetrics renders the per-request series. It returns nothing when
// verbose self-metrics are off, and clears anything tracked earlier so a reload
// that turns verbose off does not leave stale series behind.
func (s *Server) verboseRequestMetrics() []Metric {
	if !s.verboseSelfMetrics() {
		s.requests.Reset()
		return nil
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
		labels := requestLabels(sample.Key)
		for _, series := range statsSeries(sample.Values) {
			out = append(out, Metric{
				Name:   series.Name,
				Type:   series.Type,
				Value:  series.Value,
				Labels: cloneLabels(labels),
			})
		}
		out = append(out, Metric{
			Name:   "http_exporter_request_last_scrape_timestamp_seconds",
			Help:   "Unix timestamp of the last scrape of this request, or 0 when it has not been scraped yet.",
			Type:   GaugeMetricType,
			Value:  scrapeTimestamp(sample.Values.lastScrape),
			Labels: cloneLabels(labels),
		})
	}
	return out
}

// renderVerboseRequestMetrics writes the per-request series into the
// self-metrics text. A family the per-collector block has already declared gets
// no second HELP or TYPE line: repeating the metadata of a metric family makes
// the exposition invalid, so the labelled series join the family that is
// already open rather than starting a new one.
func (s *Server) renderVerboseRequestMetrics(b *strings.Builder, declared map[string]bool) {
	seen := map[string]bool{}
	for _, m := range s.verboseRequestMetrics() {
		if !declared[m.Name] && !seen[m.Name] {
			if m.Help != "" {
				fmt.Fprintf(b, "# HELP %s %s\n", m.Name, m.Help)
			}
			fmt.Fprintf(b, "# TYPE %s %s\n", m.Name, m.Type)
			seen[m.Name] = true
		}
		fmt.Fprintf(b, "%s%s %s\n", m.Name, formatLabels(m.Labels), strconv.FormatFloat(m.Value, 'g', -1, 64))
	}
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
