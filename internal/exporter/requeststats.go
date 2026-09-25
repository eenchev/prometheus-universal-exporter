package exporter

import (
	"sort"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// VerboseRequestSeriesLimit bounds how many collector/URL/method combinations
// the verbose self-metrics track. Section 22 warns against unbounded
// self-metric labels, and a request URL is exactly that: a large or churning
// target set would otherwise grow exporter memory and Prometheus cardinality
// without end. Each combination carries the whole self-metric family, so the
// limit is what keeps the endpoint bounded. Reaching it is reported rather than
// silent.
const VerboseRequestSeriesLimit = 1000

// VerboseRequestIdleExpiry is how long a tracked request is kept after the
// last probe or scrape that asked for it. A probe's URL is whatever its caller
// named, so without an expiry a target probed once, or renamed at the scraping
// Prometheus, would hold one of the VerboseRequestSeriesLimit slots, and publish
// a frozen timestamp, until the exporter restarts. An hour is well beyond any
// scrape interval Prometheus allows a target to be considered live at, while
// still freeing a slot the same day. The requests of configured static targets
// do not expire: they are dropped when a reload removes their target instead.
const VerboseRequestIdleExpiry = time.Hour

// requestKey identifies one tracked request. The URL carries no credentials and
// no query string: fetch leaves both out of the request label.
type requestKey struct {
	Collector string
	URL       string
	Method    string
}

// trackedRequest is one request's statistics and when a probe or scrape last
// asked for them, which is what VerboseRequestIdleExpiry measures from.
type trackedRequest struct {
	stats *serverStats
	used  time.Time
}

// requestTracker holds the statistics of individual requests, which the verbose
// self-metrics publish beside the per-collector totals. It is only written to
// while verbose self-metrics are configured, so switching verbose off stops the
// growth and switching it on starts from what happens next.
type requestTracker struct {
	mu    sync.Mutex
	stats map[requestKey]*trackedRequest
	// static are the requests of the configured static targets, as last
	// seeded (setStatic): they never expire, and leave when their target
	// leaves the configuration.
	static     map[requestKey]bool
	capReached bool
	// now is the clock expiry reads; tests replace it.
	now func() time.Time
}

func newRequestTracker() *requestTracker {
	return &requestTracker{stats: map[requestKey]*trackedRequest{}, static: map[requestKey]bool{}, now: time.Now}
}

// statsFor returns the statistics of one request, creating them on first use.
// It returns nil once the limit is reached, so a caller updating a new request
// simply records nothing while requests already tracked keep updating.
func (t *requestTracker) statsFor(key requestKey) *serverStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.adoptLocked(key, &serverStats{})
}

// existing returns the statistics of a request already tracked, marking it
// used, or nil when the request is not tracked.
func (t *requestTracker) existing(key requestKey) *serverStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked := t.stats[key]
	if tracked == nil {
		return nil
	}
	tracked.used = t.now()
	return tracked.stats
}

// adopt starts tracking a request with the statistics a probe gathered for it
// while it was not yet known whether the target policy would refuse it. When a
// concurrent probe of the same request got there first, the two are merged.
// Past the limit nothing is adopted, and the limit is reported as reached.
func (t *requestTracker) adopt(key requestKey, staged *serverStats) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if kept := t.adoptLocked(key, staged); kept != nil && kept != staged {
		staged.mu.Lock()
		values := staged.statsValues
		staged.mu.Unlock()
		kept.mu.Lock()
		kept.absorb(values)
		kept.mu.Unlock()
	}
}

// adoptLocked returns the statistics of key, tracking created for it when it
// is not tracked yet, or nil past the limit.
func (t *requestTracker) adoptLocked(key requestKey, created *serverStats) *serverStats {
	now := t.now()
	if tracked := t.stats[key]; tracked != nil {
		tracked.used = now
		return tracked.stats
	}
	if len(t.stats) >= VerboseRequestSeriesLimit {
		// Idle requests make room before a new one is turned away.
		t.expireLocked(now)
	}
	if len(t.stats) >= VerboseRequestSeriesLimit {
		t.capReached = true
		return nil
	}
	t.stats[key] = &trackedRequest{stats: created, used: now}
	return created
}

// setStatic makes the configured static targets' requests exist, and drops the
// requests of static targets the configuration no longer has: after a reload
// changed a target's URL, the old URL's series would otherwise stay, with a
// timestamp that never moves again.
func (t *requestTracker) setStatic(keys map[requestKey]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.static {
		if !keys[key] {
			delete(t.stats, key)
		}
	}
	t.static = keys
	for key := range keys {
		if t.stats[key] == nil {
			t.adoptLocked(key, &serverStats{})
		}
	}
	t.settleCapLocked()
}

// expire drops the requests no probe or scrape has asked for within
// VerboseRequestIdleExpiry, other than the static targets'.
func (t *requestTracker) expire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(t.now())
}

func (t *requestTracker) expireLocked(now time.Time) {
	for key, tracked := range t.stats {
		if !t.static[key] && now.Sub(tracked.used) > VerboseRequestIdleExpiry {
			delete(t.stats, key)
		}
	}
	t.settleCapLocked()
}

// settleCapLocked clears the limit indicator once requests have left and
// there is room again.
func (t *requestTracker) settleCapLocked() {
	if len(t.stats) < VerboseRequestSeriesLimit {
		t.capReached = false
	}
}

// Snapshot returns a value copy of every tracked request in a stable order, and
// whether the limit has been reached.
func (t *requestTracker) Snapshot() ([]requestSample, bool) {
	t.mu.Lock()
	keys := make([]requestKey, 0, len(t.stats))
	byKey := make(map[requestKey]*serverStats, len(t.stats))
	for key, tracked := range t.stats {
		keys = append(keys, key)
		byKey[key] = tracked.stats
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
	t.stats = map[requestKey]*trackedRequest{}
	t.static = map[requestKey]bool{}
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
	// pending is set while request is a probe's statistics for a request
	// not tracked yet, which commit adopts once the probe is known not to
	// have been refused by the target policy.
	pending *pendingRequest
}

type pendingRequest struct {
	tracker *requestTracker
	key     requestKey
}

// recorderFor pairs a collector's statistics with those of one request. The
// request side is absent while verbose self-metrics are off, when the URL could
// not be resolved, and once the series limit has been reached. It is for the
// requests the configuration names, static targets: they take their slot at
// once.
func (s *Server) recorderFor(collector *serverStats, name, labelURL, method string) statsRecorder {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return statsRecorder{collector: collector}
	}
	return statsRecorder{
		collector: collector,
		request:   s.requests.statsFor(requestKey{Collector: name, URL: labelURL, Method: method}),
	}
}

// probeRecorderFor is recorderFor for a probe, whose URL is whatever its
// caller named. A request already tracked is updated as it goes; a new one is
// gathered aside and only takes one of the VerboseRequestSeriesLimit slots when
// commit finds its target was not refused by allowed_targets or
// denied_targets, so probes of targets the collector may not reach cannot fill
// the slots and keep a legitimate target from being tracked.
func (s *Server) probeRecorderFor(collector *serverStats, name, labelURL, method string) statsRecorder {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return statsRecorder{collector: collector}
	}
	key := requestKey{Collector: name, URL: labelURL, Method: method}
	if existing := s.requests.existing(key); existing != nil {
		return statsRecorder{collector: collector, request: existing}
	}
	return statsRecorder{collector: collector, request: &serverStats{}, pending: &pendingRequest{tracker: s.requests, key: key}}
}

// commit ends a probe: a new request whose target was not refused starts
// being tracked with what the probe recorded; a refused one is forgotten.
// Updates made after commit are not guaranteed to reach the request.
func (r statsRecorder) commit(refused bool) {
	if r.pending == nil || refused {
		return
	}
	r.pending.tracker.adopt(r.pending.key, r.request)
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

// absorb adds what another probe of the same request recorded: its counters
// are added, and its latest values, of a trip that reached the target, replace
// these. It is only needed when two probes of a request not tracked yet ran at
// once (requestTracker.adopt).
func (v *statsValues) absorb(o statsValues) {
	for _, pair := range [][2]*uint64{
		{&v.probes, &o.probes}, {&v.success, &o.success}, {&v.decodeOK, &o.decodeOK},
		{&v.parseErrors, &o.parseErrors}, {&v.transformErrors, &o.transformErrors},
		{&v.missing, &o.missing}, {&v.scriptErrors, &o.scriptErrors}, {&v.limitErrors, &o.limitErrors},
		{&v.emitted, &o.emitted}, {&v.cacheHits, &o.cacheHits}, {&v.cacheMisses, &o.cacheMisses},
		{&v.coalesced, &o.coalesced}, {&v.rejected, &o.rejected}, {&v.rejectedByExporter, &o.rejectedByExporter},
		{&v.refused, &o.refused}, {&v.staleServed, &o.staleServed}, {&v.invalidUTF8, &o.invalidUTF8},
		{&v.seriesLeftOut, &o.seriesLeftOut}, {&v.linesSkipped, &o.linesSkipped},
	} {
		*pair[0] += *pair[1]
	}
	v.lastDuration = o.lastDuration
	if o.lastScrape.After(v.lastScrape) {
		v.lastScrape = o.lastScrape
		v.lastStatus, v.lastBytes, v.lastScriptDuration = o.lastStatus, o.lastBytes, o.lastScriptDuration
		v.grpcCode, v.grpcCalled = o.grpcCode, o.grpcCalled
	}
}

// seedStaticRequests registers every static target, so the targets the
// configuration names are visible before their first collection and remain
// visible across a reload that adds one, and drops those of static targets a
// reload removed or pointed elsewhere. A request driven by /probe cannot be
// seeded this way: its URL comes from the probe's own target parameter, so it
// appears the first time it is asked for, and expires when it is no longer
// asked for (VerboseRequestIdleExpiry).
func (s *Server) seedStaticRequests() {
	cfg := s.manager.Get()
	keys := map[requestKey]bool{}
	for _, target := range s.manager.StaticTargets() {
		c := model.CollectorByName(cfg, target.Collector)
		if c == nil {
			continue
		}
		overrides := fetch.TargetOverrides(&target)
		label, err := fetch.RequestLabelFor(target.Target, c, overrides)
		if err != nil {
			continue
		}
		keys[requestKey{Collector: c.Name, URL: label, Method: fetch.RequestMethodFor(c, overrides)}] = true
	}
	s.requests.setStatic(keys)
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
func (s *Server) verboseRequests() ([]requestSample, []model.Metric) {
	if !s.verboseSelfMetrics() {
		s.requests.Reset()
		return nil, nil
	}
	s.seedStaticRequests()
	s.requests.expire()
	samples, capped := s.requests.Snapshot()
	cappedValue := 0.0
	if capped {
		cappedValue = 1
	}
	out := []model.Metric{
		{
			Name:  "http_exporter_request_series_capped",
			Help:  "Whether verbose per-request self-metrics have reached their series limit.",
			Type:  model.GaugeMetricType,
			Value: cappedValue,
		},
		// Without this, a verbose exporter that has not been asked for a probe
		// yet reports only the cap gauge, which reads as though the feature is
		// doing nothing rather than as an empty set.
		{
			Name:  "http_exporter_request_series_tracked",
			Help:  "Number of collector, URL and method combinations the verbose self-metrics track.",
			Type:  model.GaugeMetricType,
			Value: float64(len(samples)),
		},
	}
	for _, sample := range samples {
		out = append(out, model.Metric{
			Name:   "http_exporter_request_last_scrape_timestamp_seconds",
			Help:   "Unix timestamp of the last scrape of this request, or 0 when it has not been scraped yet.",
			Type:   model.GaugeMetricType,
			Value:  model.ScrapeTimestamp(sample.Values.lastScrape),
			Labels: requestLabels(sample.Key),
		})
	}
	return samples, out
}
