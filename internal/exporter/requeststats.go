package exporter

import (
	"sort"
	"sync"
	"sync/atomic"
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
// last probe or scrape that asked for it. A probe's URL is whatever its
// caller named. Without an expiry, a target probed once, or renamed at the
// scraping Prometheus, would hold one of the VerboseRequestSeriesLimit slots
// and publish a frozen timestamp until the exporter restarts. An hour is far
// longer than any scrape interval at which Prometheus still considers a
// target live, and still frees a slot the same day. The requests of
// configured static targets do not expire. Instead, they are dropped when a
// reload removes their target.
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
	// latestDropped is the latest creation time a request had that is no
	// longer tracked (dropLocked): a request tracked from now on, which may be
	// that one again, counts since a later time (adoptLocked).
	latestDropped time.Time
	// idleFloor is a time before which no request other than a static
	// target's was last used (markUsedLocked, expireLocked). While none of
	// them can have been idle for VerboseRequestIdleExpiry by that time,
	// expireLocked has nothing to drop and skips the walk over the requests.
	// This matters past the limit: each read of the self-metrics offers the
	// tracker every static target's request it has no room for (setStatic).
	// Each offer used to walk every tracked request, which took 55 ms or more
	// per read for 2,000 static targets, and more than half a second for
	// 10,000. idleFloor moves later only when expireLocked walks the
	// requests. It moves earlier when a request is used at an earlier time,
	// which a test's clock can cause. It starts at the zero time.
	idleFloor time.Time
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
	return t.adoptLocked(key, newServerStats(t.now()))
}

// statsOf is statsFor for a request of the collector whose statistics are
// collector. Once a reload has removed that collector, it returns nil and
// the caller records nothing for the request. The collector's requests were
// forgotten with it, and a collector brought back under the same name is
// not where a scrape of the removed one should count. It is read under the
// lock, as adopt reads it.
func (t *requestTracker) statsOf(key requestKey, collector *serverStats) *serverStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	if collector != nil && collector.retired.Load() {
		return nil
	}
	return t.adoptLocked(key, newServerStats(t.now()))
}

// newStats returns statistics for a request that is not tracked yet,
// counting from now. That is what its _created sample says once it is
// tracked with them (adoptLocked). A request that is dropped and asked for
// again gets new statistics, and so a later time.
func (t *requestTracker) newStats() *serverStats { return newServerStats(t.now()) }

// existing returns the statistics of a request already tracked, marking it
// used, or nil when the request is not tracked.
func (t *requestTracker) existing(key requestKey) *serverStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.existingLocked(key)
}

func (t *requestTracker) existingLocked(key requestKey) *serverStats {
	tracked := t.stats[key]
	if tracked == nil {
		return nil
	}
	t.markUsedLocked(tracked, t.now())
	return tracked.stats
}

// markUsedLocked records that a probe or scrape asked for a tracked request
// at now, keeping idleFloor not after it.
func (t *requestTracker) markUsedLocked(tracked *trackedRequest, now time.Time) {
	tracked.used = now
	if now.Before(t.idleFloor) {
		t.idleFloor = now
	}
}

// existingOf is existing for a request of the collector whose statistics are
// collector. gone reports that a reload has removed that collector. A
// request tracked under its name then belongs to a collector brought back,
// and this caller must not count in it. It is read under the lock, as adopt
// reads it.
func (t *requestTracker) existingOf(key requestKey, collector *serverStats) (stats *serverStats, gone bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if collector != nil && collector.retired.Load() {
		return nil, true
	}
	return t.existingLocked(key), false
}

// adopt starts tracking a request with the statistics a probe gathered for it
// while it was not yet known whether the target policy would refuse it. If a
// concurrent probe of the same request got there first, the two are merged.
// Past the limit nothing is adopted, and the limit is reported as reached.
//
// collector is the statistics of the request's collector that the probe
// counted in. Once a reload has removed that collector, they are retired and
// nothing is adopted. The collector's requests were forgotten with it
// (reconcile.go). Otherwise a probe that began before then would show a
// request of a collector that is gone, and would add to the requests of a
// collector brought back under the same name counts it did not make under
// that collector. adopt reads
// under the lock that the forgetting takes, after the statistics are
// retired, so a probe that ends in between is either forgotten or not
// adopted.
func (t *requestTracker) adopt(key requestKey, staged, collector *serverStats) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if collector != nil && collector.retired.Load() {
		return
	}
	if kept := t.adoptLocked(key, staged); kept != nil && kept != staged {
		staged.mu.Lock()
		values := staged.statsValues
		staged.mu.Unlock()
		kept.mu.Lock()
		kept.absorb(values)
		kept.mu.Unlock()
	}
}

// adoptLocked returns the statistics of key. If key is not tracked yet, it
// starts tracking it with created. Past the limit it returns nil.
//
// A request's creation time (selfcreated.go) is set here, when tracking
// starts, and never changes afterwards. So its _created sample is the same
// at every scrape, and over OTLP its points keep their start time; a moving
// start time would read as a series that restarted. The time is when the
// statistics it is tracked with were made, which for a probe's statistics is
// when the probe began. The series counts what is added to the tracked
// statistics, at the moment it is added. A probe of a request not tracked
// yet gathers its counts aside, and they join the series when the probe is
// committed (adopt). So:
//
//   - The time is not later than anything the series has counted. The probe
//     committed first brings a time before its own counts. Another probe adds
//     its counts (absorb) after the request was tracked, however early that
//     probe began. Its earlier time is not taken, because that would move a
//     creation time that was already shown.
//   - A request that is dropped and tracked again gets a later time than
//     before. The first probe committed after the drop may have begun before
//     the dropped request was tracked and run all along. Its counts join the
//     series now, so the series counts from now, not from when the probe
//     began. The dropped requests are not kept, only the latest time any of
//     them had (latestDropped). A probe that began after that time also began
//     after its own request's time, and keeps its own time.
func (t *requestTracker) adoptLocked(key requestKey, created *serverStats) *serverStats {
	now := t.now()
	if tracked := t.stats[key]; tracked != nil {
		t.markUsedLocked(tracked, now)
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
	created.mu.Lock()
	if !created.created.After(t.latestDropped) {
		created.created = now
	}
	created.mu.Unlock()
	tracked := &trackedRequest{stats: created}
	t.markUsedLocked(tracked, now)
	t.stats[key] = tracked
	return created
}

// dropLocked stops tracking a request, and remembers how late a dropped
// request's creation time has been (adoptLocked).
func (t *requestTracker) dropLocked(key requestKey) {
	tracked := t.stats[key]
	if tracked == nil {
		return
	}
	if created := tracked.stats.snapshot().created; created.After(t.latestDropped) {
		t.latestDropped = created
	}
	delete(t.stats, key)
}

// setStatic makes the configured static targets' requests exist, and drops
// the requests of static targets the configuration no longer has. Otherwise,
// after a reload changes a target's URL, the old URL's series would stay,
// with a timestamp that never moves again.
func (t *requestTracker) setStatic(keys map[requestKey]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.static {
		if !keys[key] {
			t.dropLocked(key)
		}
	}
	t.static = keys
	for key := range keys {
		if t.stats[key] == nil {
			t.adoptLocked(key, newServerStats(t.now()))
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

// expireLocked is expire under the lock, at now. It walks the requests only
// when one of them may be idle (idleFloor), and sets idleFloor to the
// earliest time a remaining request was last used. A request that a static
// target no longer has is dropped (setStatic), not turned into one that can
// expire. So another walk is needed only once that time is
// VerboseRequestIdleExpiry old.
func (t *requestTracker) expireLocked(now time.Time) {
	if now.Sub(t.idleFloor) > VerboseRequestIdleExpiry {
		if hook := requestsScannedHook.Load(); hook != nil {
			(*hook)()
		}
		floor := now
		for key, tracked := range t.stats {
			switch {
			case t.static[key]:
			case now.Sub(tracked.used) > VerboseRequestIdleExpiry:
				t.dropLocked(key)
			case tracked.used.Before(floor):
				floor = tracked.used
			}
		}
		t.idleFloor = floor
	}
	t.settleCapLocked()
}

// requestsScannedHook, set by tests, is called whenever expireLocked goes
// through the tracked requests, so a test can count how often a read of the
// verbose self-metrics does. Nothing in the exporter sets it.
var requestsScannedHook atomic.Pointer[func()]

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
	for key := range t.stats {
		t.dropLocked(key)
	}
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
	// pending is set for a request not tracked yet, while request holds a
	// probe's statistics or those of the trip it started (forTrip). commit
	// adopts it once the probe is known not to have been refused by the
	// target policy.
	pending *pendingRequest
	// read is the configuration in which the probe or scrape read its
	// collector. It says whether what it writes under the collector's name
	// when it ends still belongs to that collector (configRead). The caller
	// sets it; if it is unset, the collector always stands.
	read configRead
}

type pendingRequest struct {
	tracker *requestTracker
	key     requestKey
}

// recorderFor pairs a collector's statistics with those of one request. The
// request side is absent in these cases:
//   - verbose self-metrics are off;
//   - the URL could not be resolved;
//   - the series limit has been reached;
//   - a reload has removed the scrape's collector (requestTracker.statsOf).
//
// It is used for the requests the configuration names (static targets),
// which take their slot at once.
func (s *Server) recorderFor(collector *serverStats, name, labelURL, method string) statsRecorder {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return statsRecorder{collector: collector}
	}
	return statsRecorder{
		collector: collector,
		request:   s.requests.statsOf(requestKey{Collector: name, URL: labelURL, Method: method}, collector),
	}
}

// probeRecorderFor is recorderFor for a probe, whose URL is whatever its
// caller named. A request already tracked is updated as the probe goes. A
// new one is gathered aside. It takes one of the VerboseRequestSeriesLimit
// slots only when commit finds that allowed_targets or denied_targets did not
// refuse its target. So probes of targets the collector may not reach cannot
// fill the slots and keep a legitimate target from being tracked. A probe
// whose collector a reload has removed has no request side: by then, what is
// tracked under its request's name belongs to the collector brought back.
func (s *Server) probeRecorderFor(collector *serverStats, name, labelURL, method string) statsRecorder {
	if labelURL == "" || !s.verboseSelfMetrics() {
		return statsRecorder{collector: collector}
	}
	key := requestKey{Collector: name, URL: labelURL, Method: method}
	existing, gone := s.requests.existingOf(key, collector)
	if gone {
		return statsRecorder{collector: collector}
	}
	if existing != nil {
		return statsRecorder{collector: collector, request: existing}
	}
	return statsRecorder{collector: collector, request: s.requests.newStats(), pending: &pendingRequest{tracker: s.requests, key: key}}
}

// forTrip returns the recorder of the trip that r's probe starts for every
// identical probe in flight (probeflight.go). Such a trip belongs to none of
// the probes sharing it, and neither do its counts. For a request already
// tracked, they are counted on the request as they happen, like r's own
// counts. For a new request, they are gathered apart from r's and committed
// when the trip ends, whichever probe started the trip and whichever left
// before the answer. If they were gathered with the starter's own counts,
// they would be forgotten when the starter left, while the trip went on to
// answer the probes that joined it. The trip is committed before the probe
// that started it, and its counts carry that probe's time: a request it
// starts tracking counts from when the probe began.
func (r statsRecorder) forTrip() statsRecorder {
	if r.pending == nil {
		return r
	}
	return statsRecorder{collector: r.collector, request: newServerStats(r.request.snapshot().created), pending: &pendingRequest{tracker: r.pending.tracker, key: r.pending.key}, read: r.read}
}

// commit ends a probe or a shared trip. A new request whose target was not
// refused starts being tracked with what was recorded. Otherwise it is
// forgotten: when it was refused, when its probe or trip was abandoned
// (which the caller passes as refused), or when a reload removed its
// collector meanwhile (adopt). Updates made after commit are not guaranteed
// to reach the request.
func (r statsRecorder) commit(refused bool) {
	if r.pending == nil || refused {
		return
	}
	r.pending.tracker.adopt(r.pending.key, r.request, r.collector)
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

// absorb adds what another probe of the same request, or a shared trip to
// its target, recorded. Its counters are added, and its latest values, from
// a trip that reached the target, replace these. The duration belongs to a
// probe: a trip gathered apart from the probes sharing it (forTrip) has none
// to give. absorb is needed when two probes of an untracked request ran at
// once, and when a probe joins the trip that answered it
// (requestTracker.adopt). The creation time stays these counters' own,
// whichever of the two began earlier: what is absorbed counts from now
// (adoptLocked).
func (v *statsValues) absorb(o statsValues) {
	for _, pair := range [][2]*uint64{
		{&v.probes, &o.probes}, {&v.success, &o.success}, {&v.decodeOK, &o.decodeOK},
		{&v.parseErrors, &o.parseErrors}, {&v.transformErrors, &o.transformErrors},
		{&v.missing, &o.missing}, {&v.scriptErrors, &o.scriptErrors}, {&v.limitErrors, &o.limitErrors},
		{&v.emitted, &o.emitted}, {&v.cacheHits, &o.cacheHits}, {&v.cacheMisses, &o.cacheMisses},
		{&v.coalesced, &o.coalesced}, {&v.rejected, &o.rejected}, {&v.rejectedByExporter, &o.rejectedByExporter},
		{&v.refused, &o.refused}, {&v.staleServed, &o.staleServed}, {&v.backoffSkipped, &o.backoffSkipped}, {&v.invalidUTF8, &o.invalidUTF8},
		{&v.seriesLeftOut, &o.seriesLeftOut}, {&v.linesSkipped, &o.linesSkipped},
		{&v.pagesFetched, &o.pagesFetched}, {&v.pagesStopped, &o.pagesStopped}, {&v.memoryStops, &o.memoryStops},
		{&v.revalidatedNotModified, &o.revalidatedNotModified}, {&v.revalidatedModified, &o.revalidatedModified},
	} {
		*pair[0] += *pair[1]
	}
	if o.probes > 0 {
		v.lastDuration = o.lastDuration
	}
	if o.lastScrape.After(v.lastScrape) {
		v.lastScrape = o.lastScrape
		v.lastStatus, v.lastBytes, v.lastScriptDuration = o.lastStatus, o.lastBytes, o.lastScriptDuration
		v.grpcCode, v.grpcCalled = o.grpcCode, o.grpcCalled
	}
}

// seedStaticRequests registers every static target, so the targets the
// configuration names are visible before their first collection and stay
// visible across a reload that adds one. It also drops the requests of
// static targets that a reload removed or pointed elsewhere. A request
// driven by /probe cannot be seeded this way. Its URL comes from the probe's
// own target parameter, so it appears the first time it is asked for, and
// expires when it is no longer asked for (VerboseRequestIdleExpiry).
func (s *Server) seedStaticRequests() {
	s.requests.setStatic(s.staticRequestKeys())
}

// staticRequestKeys returns the request of every static target in force, as
// the verbose self-metrics name it. It covers the targets whose collector
// the configuration has and whose request can be named. Each target's
// collector is looked up by name in the place the followed configuration
// keeps (followedConfig.collectorOf), instead of walking the collectors.
// That walk once ran for every static target at every read of the verbose
// self-metrics. For 2,000 targets of 2,000 collectors it took as long as
// the rest of finding the requests, and for 10,000 of 10,000 more than ten
// times as long. The place is used only while the configuration read here
// is exactly the followed one. That is the case for a read of the
// self-metrics, which follows what is in force before it gets here
// (collectorStats). For a configuration that a reload put in force since,
// the collectors are walked as before until that reload has followed it.
func (s *Server) staticRequestKeys() map[requestKey]bool {
	// Read together, so every target finds the collector it was checked
	// against.
	cfg, file := s.manager.InForce()
	followed := s.followed.Load()
	keys := map[requestKey]bool{}
	for _, target := range staticTargetsOf(file) {
		c := followed.collectorOf(cfg, target.Collector)
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
	return keys
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
