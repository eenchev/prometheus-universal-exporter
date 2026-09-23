package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// statsValues are the exporter's own counters for one collector, or for one
// request when verbose self-metrics are configured. They are separated from the
// mutex so a consistent copy can be taken under the lock and rendered outside
// it.
type statsValues struct {
	probes, success, decodeOK, parseErrors, transformErrors, missing, scriptErrors, limitErrors, emitted, cacheHits, cacheMisses uint64
	// coalesced counts probes answered by sharing another probe's request.
	coalesced uint64
	// rejected counts probes turned away by max_concurrent_probes.
	rejected uint64
	// lastScriptDuration is how long the Python of the last probe that ran any
	// took, in seconds.
	lastScriptDuration float64
	lastStatus         int
	lastBytes          int64
	lastDuration       float64
	// lastScrape is only kept per request; the per-collector series has no
	// timestamp of its own.
	lastScrape time.Time
}

type serverStats struct {
	mu sync.Mutex
	statsValues
}

func (s *serverStats) snapshot() statsValues {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsValues
}

// selfMetricDescriptors are the exporter's per-collector metric families, in
// the order they are exposed: each one's name, type and help text, and how its
// value is read from a collector's counters. They are the single source of all
// three, for the text exposition and for OTLP alike (selfMetricSet), so a
// family cannot be declared a gauge in one and a counter in the other. A
// shared placeholder help would make the endpoint self-documenting in name
// only: HELP is what a reader sees in Grafana's metric browser or `curl`.
var selfMetricDescriptors = []selfMetricDescriptor{
	{"http_exporter_scrapes_total", CounterMetricType, "Probes served for this collector, including those answered from the response cache.", func(v statsValues) float64 { return float64(v.probes) }},
	{"http_exporter_scrape_success_total", CounterMetricType, "Probes for this collector that completed without a fatal error.", func(v statsValues) float64 { return float64(v.success) }},
	{"http_exporter_scrape_duration_seconds", GaugeMetricType, "Duration of the most recent probe of this collector, in seconds.", func(v statsValues) float64 { return v.lastDuration }},
	{"http_exporter_scrape_http_status_code", GaugeMetricType, "HTTP status the target returned on the most recent scrape, or 0 when the request failed before a response arrived.", func(v statsValues) float64 { return float64(v.lastStatus) }},
	{"http_exporter_scrape_response_bytes", GaugeMetricType, "Size of the most recent response body for this collector, in bytes.", func(v statsValues) float64 { return float64(v.lastBytes) }},
	{"http_exporter_decode_success_total", CounterMetricType, "Responses this collector decoded into its configured format.", func(v statsValues) float64 { return float64(v.decodeOK) }},
	{"http_exporter_parse_errors_total", CounterMetricType, "Responses this collector's decoder could not parse.", func(v statsValues) float64 { return float64(v.parseErrors) }},
	{"http_exporter_transform_errors_total", CounterMetricType, "Transforms that failed for this collector.", func(v statsValues) float64 { return float64(v.transformErrors) }},
	{"http_exporter_missing_keys_total", CounterMetricType, "Transform failures caused by a key or field the response did not contain.", func(v statsValues) float64 { return float64(v.missing) }},
	{"http_exporter_script_errors_total", CounterMetricType, "Python script failures during this collector's transform.", func(v statsValues) float64 { return float64(v.scriptErrors) }},
	{"http_exporter_script_duration_seconds", GaugeMetricType, "Duration of the most recent Python script run for this collector, in seconds.", func(v statsValues) float64 { return v.lastScriptDuration }},
	{"http_exporter_metrics_emitted_total", CounterMetricType, "Metrics this collector has produced across its scrapes.", func(v statsValues) float64 { return float64(v.emitted) }},
	{"http_exporter_series_limit_exceeded_total", CounterMetricType, "Scrapes rejected for exceeding this collector's response size or series limits.", func(v statsValues) float64 { return float64(v.limitErrors) }},
	{"http_exporter_cache_hits_total", CounterMetricType, "Probes answered from this collector's response cache.", func(v statsValues) float64 { return float64(v.cacheHits) }},
	{"http_exporter_cache_misses_total", CounterMetricType, "Probes that found no usable cache entry and went to the target.", func(v statsValues) float64 { return float64(v.cacheMisses) }},
	// What a collector's cache holds belongs to the collector, not to any one
	// request, so it has no per-request value.
	{"http_exporter_cache_entries", GaugeMetricType, "Entries currently held in this collector's response cache.", nil},
	{"http_exporter_probes_in_flight", GaugeMetricType, "Trips to this collector's targets in progress, which max_concurrent_probes bounds.", nil},
	{"http_exporter_probes_rejected_total", CounterMetricType, "Probes answered 503 because this collector already had max_concurrent_probes trips to its targets in progress.", func(v statsValues) float64 { return float64(v.rejected) }},
	{"http_exporter_probes_coalesced_total", CounterMetricType, "Probes answered by sharing an identical probe already in flight instead of going to the target.", func(v statsValues) float64 { return float64(v.coalesced) }},
}

// selfMetricDescriptor describes one per-collector self-metric family. Value
// is nil for a family that belongs to the collector rather than to a request.
type selfMetricDescriptor struct {
	Name  string
	Type  MetricType
	Help  string
	Value func(statsValues) float64
}

// exporterMetricHelp describes the exporter-wide families, which carry no
// collector's counters.
var exporterMetricHelp = map[string]string{
	"http_exporter_collector_config_valid":                       "Whether the collector configuration is valid.",
	"http_exporter_scheduled_targets":                            "Scheduled targets configured for OTLP delivery.",
	"http_exporter_config_last_reload_successful":                "Whether the last load of this configuration file, at startup or on reload, succeeded.",
	"http_exporter_config_last_reload_success_timestamp_seconds": "Unix time this configuration file was last loaded successfully, at startup or on reload.",
	"http_exporter_config_reloads_total":                         "Reloads of this configuration file after startup, by result: success or failure.",
	"http_exporter_otlp_exports_total":                           "OTLP exports, each a delivery of everything pending with its retries, by result: success or failure.",
	"http_exporter_otlp_export_retries_total":                    "OTLP export attempts repeated after a network error, 429, 502, 503 or 504.",
	"http_exporter_otlp_points_dropped_total":                    "Data points given up on because the OTLP endpoint rejected them with a status that is not retried.",
	"http_exporter_otlp_export_duration_seconds":                 "Duration of the most recent OTLP export, its retries included.",
	"http_exporter_otlp_last_export_success_timestamp_seconds":   "Unix time of the last OTLP export that got through; 0 before the first.",
}

// selfMetricHelp indexes the help of every family the self-metrics carry
// outside the verbose and runtime sets.
var selfMetricHelp = func() map[string]string {
	out := make(map[string]string, len(selfMetricDescriptors)+len(exporterMetricHelp))
	for _, d := range selfMetricDescriptors {
		out[d.Name] = d.Help
	}
	for name, help := range exporterMetricHelp {
		out[name] = help
	}
	return out
}()

// selfMetricNames lists the per-collector families in exposition order.
func selfMetricNames() []string {
	names := make([]string, 0, len(selfMetricDescriptors))
	for _, d := range selfMetricDescriptors {
		names = append(names, d.Name)
	}
	return names
}

type Server struct {
	manager         *ConfigManager
	pythonPath      string
	logger          *slog.Logger
	selfMetricsPath string
	// timeoutOffset is how much of Prometheus's scrape timeout a probe leaves
	// unused (scrapetimeout.go).
	timeoutOffset time.Duration
	statsMu       sync.Mutex
	stats         map[string]*serverStats
	otlpMu        sync.Mutex
	otlpPending   map[string]*otlpBatch
	cache         *responseCache
	requests      *requestTracker
	flights       *probeFlights
	durations     *scrapeDurations
	// trips bounds each collector's trips to its targets (triplimit.go).
	trips *tripLimiter
	// lifecycle enables POST /-/reload (lifecycle.go).
	lifecycle bool
	// otlp is how exports are going (otlpstatus.go).
	otlp *otlpStatus
}

func NewServer(m *ConfigManager, p string, l *slog.Logger) *Server {
	s := &Server{manager: m, pythonPath: p, logger: l, stats: map[string]*serverStats{}, otlpPending: map[string]*otlpBatch{}, cache: newResponseCache(), requests: newRequestTracker(), flights: newProbeFlights(), durations: newScrapeDurations(), trips: newTripLimiter(), otlp: &otlpStatus{}, timeoutOffset: DefaultTimeoutOffset}
	return s
}

// otlpBatch holds the metrics pending export for one OTLP resource.
type otlpBatch struct {
	identity otlpResourceIdentity
	metrics  map[string]Metric
}

// queueOTLP stages metrics under the exporter-wide OTLP resource.
func (s *Server) queueOTLP(set MetricSet) {
	s.queueOTLPResource(set, defaultResourceIdentity(s.manager.Get().OTLP))
}

// queueOTLPResource stages metrics under a specific resource, so a scheduled
// target's own service name and resource attributes survive to the exporter.
func (s *Server) queueOTLPResource(set MetricSet, identity otlpResourceIdentity) {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" || len(set.Metrics) == 0 {
		return
	}
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	key := identity.key()
	batch := s.otlpPending[key]
	if batch == nil {
		batch = &otlpBatch{identity: identity, metrics: map[string]Metric{}}
		s.otlpPending[key] = batch
	}
	for _, metric := range set.Metrics {
		batch.metrics[otlpMetricKey(metric)] = cloneMetric(metric)
	}
}

func (s *Server) drainOTLP() []otlpResourceSet {
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	keys := make([]string, 0, len(s.otlpPending))
	for key := range s.otlpPending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]otlpResourceSet, 0, len(keys))
	for _, key := range keys {
		batch := s.otlpPending[key]
		set := MetricSet{Metrics: make([]Metric, 0, len(batch.metrics))}
		for _, metricKey := range sortedMetricKeys(batch.metrics) {
			set.Metrics = append(set.Metrics, batch.metrics[metricKey])
		}
		out = append(out, otlpResourceSet{Identity: batch.identity, Set: set})
	}
	s.otlpPending = make(map[string]*otlpBatch)
	return out
}

func sortedMetricKeys(in map[string]Metric) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// appendToResource adds metrics to the matching resource in resources, creating
// the entry when the identity is not present yet.
func appendToResource(resources []otlpResourceSet, identity otlpResourceIdentity, set MetricSet) []otlpResourceSet {
	if len(set.Metrics) == 0 {
		return resources
	}
	key := identity.key()
	for i := range resources {
		if resources[i].Identity.key() == key {
			resources[i].Set.Metrics = append(resources[i].Set.Metrics, set.Metrics...)
			return resources
		}
	}
	return append(resources, otlpResourceSet{Identity: identity, Set: set})
}

// OTLPExportLoop scrapes the scheduled targets and exports everything pending
// every otlp.interval until ctx ends. It returns without a last export, which
// is FlushOTLP's to make once the HTTP server has finished its probes.
func (s *Server) OTLPExportLoop(ctx context.Context) {
	for {
		interval := time.Duration(s.manager.Get().OTLP.Interval)
		if interval <= 0 {
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		cfg := s.manager.Get().OTLP
		if !cfg.Enabled || cfg.Endpoint == "" {
			_ = s.drainOTLP()
			continue
		}
		s.scrapeScheduledTargets(ctx, interval)
		// An export may retry for up to an interval, so it never runs into
		// the next one.
		s.exportOTLP(ctx, interval)
	}
}

// FlushOTLP makes the last export at shutdown: the metrics probes queued since
// the last export, and a final self-metric snapshot, within otlp.timeout.
// Scheduled targets are not scraped again.
func (s *Server) FlushOTLP() {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" {
		return
	}
	s.logger.Info("sending the last OTLP export before exiting")
	s.exportOTLP(context.Background(), time.Duration(cfg.Timeout))
}

// exportOTLP sends everything pending, with a self-metric snapshot, retrying
// within budget. Metrics that could not be delivered for a reason worth
// retrying — a network error, 429, 502, 503, 504, or the export being cut short
// by shutdown — are queued again for the next export, unless a newer value of
// the same series has been queued since; the self-metrics are not, since the
// next export takes a new snapshot. Metrics the endpoint refused outright are
// dropped and counted, since sending them again would be refused again.
func (s *Server) exportOTLP(ctx context.Context, budget time.Duration) {
	cfg := s.manager.Get().OTLP
	pending := s.drainOTLP()
	if !cfg.Enabled || cfg.Endpoint == "" {
		return
	}
	// The copy keeps the self-metrics out of pending, which may be queued again.
	resources := appendToResource(append([]otlpResourceSet(nil), pending...), defaultResourceIdentity(cfg), s.selfMetricSet())
	start := time.Now()
	retries, err := s.pushOTLP(ctx, cfg, resources, budget)
	if err != nil && ctx.Err() != nil {
		// Shutting down: the last export sends these.
		s.requeueOTLP(pending)
		return
	}
	s.otlp.record(time.Since(start), retries, err == nil)
	if err == nil {
		return
	}
	var refused *otlpRefusedError
	if errors.As(err, &refused) {
		points := countPoints(pending)
		s.otlp.drop(points)
		s.logger.Warn("OTLP endpoint refused an export; its data points are dropped", "status", refused.status, "dropped_points", points, "retries", retries)
		return
	}
	s.requeueOTLP(pending)
	s.logger.Warn("OTLP export failed; its data points are kept for the next export", "error", err, "retries", retries)
}

// requeueOTLP queues metrics that were not delivered again, each unless a
// newer value of its series has been queued since it was drained.
func (s *Server) requeueOTLP(resources []otlpResourceSet) {
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	for _, resource := range resources {
		key := resource.Identity.key()
		batch := s.otlpPending[key]
		if batch == nil {
			batch = &otlpBatch{identity: resource.Identity, metrics: map[string]Metric{}}
			s.otlpPending[key] = batch
		}
		for _, metric := range resource.Set.Metrics {
			if _, newer := batch.metrics[otlpMetricKey(metric)]; !newer {
				batch.metrics[otlpMetricKey(metric)] = metric
			}
		}
	}
}

func countPoints(resources []otlpResourceSet) int {
	n := 0
	for _, resource := range resources {
		n += len(resource.Set.Metrics)
	}
	return n
}

func otlpMetricKey(metric Metric) string {
	keys := make([]string, 0, len(metric.Labels))
	for key := range metric.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(metric.Name)
	b.WriteByte(0)
	b.WriteString(string(metric.Type))
	for _, key := range keys {
		b.WriteByte(0)
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(metric.Labels[key])
	}
	return b.String()
}
func (s *Server) SetSelfMetricsPath(path string) {
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	switch path {
	case "/probe", "/health", "/ready":
		path = "/self-metrics"
	}
	s.selfMetricsPath = path
}
func (s *Server) statsFor(name string) *serverStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if x := s.stats[name]; x != nil {
		return x
	}
	x := &serverStats{}
	s.stats[name] = x
	return x
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ready", s.readyHandler)
	path := s.selfMetricsPath
	if path == "" {
		path = "/self-metrics"
	}
	protected := func(handler http.HandlerFunc) http.HandlerFunc { return s.basicAuthMiddleware(handler) }
	if path != "/metrics" {
		mux.HandleFunc(path, protected(s.metricsHandler))
	}
	mux.HandleFunc("/metrics", protected(s.metricsHandler))
	mux.HandleFunc("/probe", protected(s.probeHandler))
	mux.HandleFunc("/-/reload", protected(s.reloadHandler))
	return mux
}
func (s *Server) basicAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		credentials := s.manager.Get().Web.BasicAuth
		if credentials == nil || !credentials.Enabled {
			next(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(username), []byte(credentials.Username)) != 1 || subtle.ConstantTimeCompare([]byte(password), []byte(credentials.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="prometheus-universal-exporter"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) probeHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	target := r.URL.Query().Get("target")
	name := r.URL.Query().Get("collector")
	if name == "" {
		http.Error(w, "target and collector are required", http.StatusBadRequest)
		return
	}
	cfg := s.manager.Get()
	var c *Collector
	for i := range cfg.Collectors {
		if cfg.Collectors[i].Name == name {
			c = &cfg.Collectors[i]
			break
		}
	}
	if c == nil {
		http.Error(w, fmt.Sprintf("unknown collector %q", name), http.StatusBadRequest)
		return
	}
	// Whether a target is needed, and what it may be, is the request type's
	// to say: http needs a URL, localfile a file under its root, or nothing.
	if err := checkTarget(c, target, false); err != nil {
		if errors.Is(err, errMissingTarget) {
			http.Error(w, "target and collector are required", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	logTarget := displayTarget(c, target)
	// The request parameters are resolved before anything is counted, so every
	// counter this probe raises lands on the individual request as well as on
	// the collector rather than only on the collector.
	overrides, err := parseRequestOverrides(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Each request type accepts its own probe parameters; one that belongs to
	// another type is a mistake, reported before anything else happens.
	if err := checkOverrideParams(c, r.URL.Query()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A path parameter the scrape did not supply and that has no default is the
	// caller's mistake, reported before the target is contacted or anything is
	// counted against it.
	if err := checkPathParams(c, overrides); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var requestURL string
	method := requestMethodFor(c, overrides)
	if label, labelErr := requestLabelFor(target, c, overrides); labelErr == nil {
		requestURL = label
	}
	st := s.statsFor(name)
	rec := s.recorderFor(st, name, requestURL, method)
	rec.update(func(x *serverStats) { x.probes++ })
	finish := func(ok bool) {
		duration := time.Since(start)
		rec.update(func(x *serverStats) {
			if ok {
				x.success++
			}
			x.lastDuration = duration.Seconds()
		})
	}
	forwarded := forwardedHeaders(r, c.Request)
	cacheTTL := time.Duration(c.Cache)
	key := probeCacheKey(c, target, r.URL.Query(), forwarded)
	var cacheKey string
	if cacheTTL > 0 {
		cacheKey = key
		if cached, ok := s.cache.Get(cacheKey, time.Now()); ok {
			rec.update(func(x *serverStats) {
				x.cacheHits++
				x.emitted += uint64(len(cached.Metrics))
			})
			finish(true)
			writeMetricSet(w, &cached)
			s.queueOTLP(cached)
			return
		}
		rec.update(func(x *serverStats) { x.cacheMisses++ })
	}
	upstream := func(ctx context.Context) *probeResult {
		return s.probeUpstream(ctx, upstreamProbe{
			collector: c, target: target, logTarget: logTarget, overrides: overrides,
			forwarded: forwarded, rec: rec, cacheKey: cacheKey, cacheTTL: cacheTTL,
			budget: probeBudget(r.Header, s.timeoutOffset),
		})
	}
	if !coalesceProbes(c) {
		result := upstream(r.Context())
		finish(result.ok)
		result.writeTo(w)
		return
	}
	result, shared, err := s.flights.do(r.Context(), key, upstream)
	if err != nil {
		// This caller went away while it waited; there is nobody to answer.
		finish(false)
		return
	}
	if shared {
		rec.update(func(x *serverStats) { x.coalesced++ })
		s.logger.Debug("probe shared an identical probe in flight", "collector", name, "target", logTarget)
	}
	finish(result.ok)
	result.writeTo(w)
}

// upstreamProbe is what one trip to the target needs.
type upstreamProbe struct {
	collector *Collector
	target    string
	logTarget string
	overrides RequestOverrides
	forwarded http.Header
	rec       statsRecorder
	cacheKey  string
	cacheTTL  time.Duration
	// budget bounds the trip when Prometheus said how long it will wait.
	budget time.Duration
}

// probeUpstream goes to the target, decodes, transforms and validates, and
// records the answer instead of writing it, so identical probes waiting on it
// can each be given a copy. Its self-metrics, logs, cache entry and OTLP
// export happen once, however many probes share it.
func (s *Server) probeUpstream(ctx context.Context, p upstreamProbe) *probeResult {
	c, name, rec, logTarget := p.collector, p.collector.Name, p.rec, p.logTarget
	out := newProbeRecorder()
	// A probe that finished while this one was waiting to start may have just
	// filled the cache.
	if p.cacheKey != "" {
		if cached, ok := s.cache.Get(p.cacheKey, time.Now()); ok {
			rec.update(func(x *serverStats) { x.emitted += uint64(len(cached.Metrics)) })
			writeMetricSet(out, &cached)
			return out.result(true)
		}
	}
	// Answered at once when the collector's backend already has all it may
	// get, rather than queued behind the probes in progress (triplimit.go).
	limit := maxConcurrentProbes(c)
	if !s.trips.tryAcquire(name, limit) {
		rec.update(func(x *serverStats) { x.rejected++ })
		s.logger.Warn("probe rejected: the collector has too many probes in progress", "collector", name, "target", logTarget, "max_concurrent_probes", limit)
		http.Error(out, fmt.Sprintf("collector %s already has %d probes to its targets in progress, its max_concurrent_probes; this one was not sent", name, limit), http.StatusServiceUnavailable)
		return out.result(false)
	}
	defer s.trips.release(name)
	if p.budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.budget)
		defer cancel()
	}
	scraped := false
	tripStart := time.Now()
	defer func() {
		// The last-scrape timestamp and the duration histogram describe a trip
		// to the target, not a probe answered from the cache.
		if scraped {
			rec.scraped(time.Now())
			s.observeTargetScrape(name, time.Since(tripStart))
		}
	}()
	// failStage applies a stage's error policy. It reports whether the probe
	// carries on; when it does not, the error response is already recorded.
	failStage := func(stage string, err error, policy string) bool {
		err = explainBudget(ctx, p.budget, err)
		switch policy {
		case ErrorPolicyLog, errorPolicyWarn:
			s.logger.Warn("probe stage failed; continuing", "collector", name, "target", logTarget, "stage", stage, "error", err)
			return true
		case ErrorPolicyIgnore:
			s.logger.Debug("probe stage failed; continuing", "collector", name, "target", logTarget, "stage", stage, "error", err)
			return true
		}
		s.logger.Error("probe failed", "collector", name, "target", logTarget, "stage", stage, "error", err)
		http.Error(out, fmt.Sprintf("collector %s %s failed: %v", name, stage, err), http.StatusBadGateway)
		return false
	}
	resp, err := fetchCollector(ctx, p.target, c, p.overrides, p.forwarded)
	scraped = true
	if err != nil {
		if errors.Is(err, errLimitExceeded) {
			rec.update(func(x *serverStats) { x.limitErrors++ })
		}
		return out.result(failStage(fetchStage(c), err, c.ErrorHandling.OnFetchError))
	}
	rec.update(func(x *serverStats) {
		x.lastStatus = resp.StatusCode
		x.lastBytes = int64(len(resp.Body))
	})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out.result(failStage("http_status", fmt.Errorf("received HTTP status %d", resp.StatusCode), c.ErrorHandling.OnFetchError))
	}
	var ms *MetricSet
	if resp.Directory != nil {
		// A directory: each file is decoded and transformed on its own, and
		// one that fails is left out rather than failing the probe.
		ms = s.collectDirectory(ctx, resp.Directory, c, rec, logTarget)
	} else {
		d, err := decode(resp, c)
		if err != nil {
			rec.update(func(x *serverStats) { x.parseErrors++ })
			return out.result(failStage("decode", err, c.ErrorHandling.OnDecodeError))
		}
		rec.update(func(x *serverStats) { x.decodeOK++ })
		scriptCtx, timer := withScriptTimer(ctx)
		ms, err = transform(scriptCtx, d, resp, c, s.pythonPath)
		recordScriptDuration(rec, timer)
		if err != nil {
			rec.update(func(x *serverStats) {
				if errors.Is(err, errMissingValue) {
					x.missing++
				}
				if errors.Is(err, errScriptFailed) {
					x.scriptErrors++
				}
				x.transformErrors++
			})
			// A metric rule with error_mode fail asked for the scrape to fail when it
			// cannot produce its value. That is a statement about this one metric,
			// more specific than the collector's on_transform_error, so it is honoured
			// even where the collector would have carried on after a failed transform.
			var failure *MetricFailure
			if errors.As(err, &failure) {
				s.logger.Error("probe failed", "collector", name, "target", logTarget, "stage", "metric", "metric", failure.Metric, "error", err)
				writeProbeError(out, http.StatusBadGateway, probeError{
					Stage:     "metric",
					Collector: name,
					Metric:    failure.Metric,
					Target:    logTarget,
					Error:     err.Error(),
				})
				return out.result(false)
			}
			return out.result(failStage("transform", err, c.ErrorHandling.OnTransformError))
		}
	}
	if err = ms.Validate(c.Limits); err != nil {
		rec.update(func(x *serverStats) { x.limitErrors++ })
		failStage("validation", err, ErrorPolicyFail)
		return out.result(false)
	}
	rec.update(func(x *serverStats) { x.emitted += uint64(len(ms.Metrics)) })
	s.cache.Put(p.cacheKey, c.Name, *ms, p.cacheTTL, c.Limits.MaxCacheEntries, time.Now())
	writeMetricSet(out, ms)
	s.queueOTLP(*ms)
	return out.result(true)
}

// safeTarget renders a target for logs and error bodies with any credentials
// redacted. It must never fail: it runs on the error paths, including for a
// target that is not a URL at all.
func safeTarget(raw string) string {
	u, err := url.Parse(normalizeTarget(raw))
	if err != nil {
		// Unparseable, so the credentials cannot be located to redact them;
		// the raw text is withheld rather than risk echoing a password.
		return "<invalid target>"
	}
	if u.User != nil {
		u.User = url.UserPassword("redacted", "redacted")
	}
	return u.String()
}

// normalizeTarget gives a target without a scheme the default http:// one.
//
// Prometheus service discovery hands over __address__, which is host:port with
// no scheme, and the chart's monitors pass it straight through as target. It
// cannot simply be parsed and then checked for an empty scheme: url.Parse
// rejects 10.0.0.5:8080 outright ("first path segment cannot contain colon")
// and reads legacy.example:8080 as the scheme "legacy.example". So the
// decision is made on the text, before parsing: no "://" means no scheme.
// A target that wants https says so explicitly.
func normalizeTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	return "http://" + raw
}

// forwardedHeaders extracts only explicitly allowed headers from the probe
// request. Prometheus Operator monitor params use the header_<name> convention
// for static, non-secret target headers. Authorization is handled separately so
// a Secret-backed monitor credential can be forwarded without putting it in a
// URL parameter.
func forwardedHeaders(r *http.Request, request RequestConfig) http.Header {
	out := make(http.Header)
	blocked := map[string]bool{
		"Authorization":       true,
		"Connection":          true,
		"Content-Length":      true,
		"Host":                true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailer":             true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
	}
	allowed := make(map[string]bool, len(request.ForwardHeaders))
	for _, name := range request.ForwardHeaders {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name != "" && !blocked[name] {
			allowed[name] = true
		}
	}
	if request.ForwardAuthorization {
		if value := r.Header.Get("Authorization"); value != "" {
			out.Set("Authorization", value)
		}
	}
	for name := range allowed {
		if values := r.Header.Values(name); len(values) > 0 {
			out[name] = append([]string(nil), values...)
		}
	}
	for key, values := range r.URL.Query() {
		if len(key) <= len("header_") || !strings.EqualFold(key[:len("header_")], "header_") {
			continue
		}
		name := http.CanonicalHeaderKey(strings.TrimSpace(key[len("header_"):]))
		if name == "" || !allowed[name] || blocked[name] {
			continue
		}
		for _, value := range values {
			out.Add(name, value)
		}
	}
	return out
}

// metricsHandler serves the self-metrics. The text is rendered from the same
// set OTLP exports, by the same code as collector output, so the two cannot
// disagree about a family's type or help, and every family is one contiguous
// block with one HELP and one TYPE line.
func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	set := s.selfMetricSet()
	writeMetricSet(w, &set)
}

// collectorStats returns every configured collector's counters, and those of
// collectors served before a reload removed them, sorted by name.
func (s *Server) collectorStats() (names []string, values map[string]statsValues) {
	s.statsMu.Lock()
	for _, c := range s.manager.Get().Collectors {
		if s.stats[c.Name] == nil {
			s.stats[c.Name] = &serverStats{}
		}
	}
	stats := make(map[string]*serverStats, len(s.stats))
	for name, st := range s.stats {
		stats[name] = st
		names = append(names, name)
	}
	s.statsMu.Unlock()
	sort.Strings(names)
	values = make(map[string]statsValues, len(names))
	for _, name := range names {
		values[name] = stats[name].snapshot()
	}
	return names, values
}

// selfMetricSet is every self-metric, grouped by family: each per-collector
// family with the collectors' series and then, in verbose mode, the
// per-request ones; then the exporter-wide families; then the verbose-only and
// runtime families.
func (s *Server) selfMetricSet() MetricSet {
	names, values := s.collectorStats()
	cacheEntries := s.cache.Stats(time.Now())
	requests, requestFamilies := s.verboseRequests()
	// The families that belong to a collector rather than to a request.
	collectorOnly := map[string]func(string) float64{
		"http_exporter_cache_entries":    func(name string) float64 { return float64(cacheEntries[name]) },
		"http_exporter_probes_in_flight": func(name string) float64 { return float64(s.trips.count(name)) },
	}
	var out []Metric
	for _, d := range selfMetricDescriptors {
		for _, name := range names {
			var value float64
			if d.Value != nil {
				value = d.Value(values[name])
			} else {
				value = collectorOnly[d.Name](name)
			}
			out = append(out, Metric{Name: d.Name, Help: d.Help, Type: d.Type, Value: value, Labels: map[string]string{"collector": name}})
		}
		if d.Value == nil {
			continue
		}
		for _, sample := range requests {
			out = append(out, Metric{Name: d.Name, Help: d.Help, Type: d.Type, Value: d.Value(sample.Values), Labels: requestLabels(sample.Key)})
		}
	}
	for _, c := range s.manager.Get().Collectors {
		out = append(out, Metric{Name: "http_exporter_collector_config_valid", Help: exporterMetricHelp["http_exporter_collector_config_valid"], Type: GaugeMetricType, Value: 1, Labels: map[string]string{"collector": c.Name}})
	}
	out = append(out, Metric{Name: "http_exporter_scheduled_targets", Help: exporterMetricHelp["http_exporter_scheduled_targets"], Type: GaugeMetricType, Value: float64(len(s.manager.Targets()))})
	out = append(out, s.manager.reloadMetrics()...)
	out = append(out, s.otlpStatusMetrics()...)
	out = append(out, requestFamilies...)
	out = append(out, s.verboseCollectorMetrics()...)
	out = append(out, s.runtimeMetrics()...)
	return MetricSet{Metrics: out}
}

// probeError is the body of a probe that failed because a metric rule with
// error_mode fail could not produce its value. Prometheus only looks at the
// status code, which already fails the scrape; the body is for the person who
// runs the probe by hand to find out why, so it names the collector, the rule
// and the error rather than leaving them to be dug out of the exporter's log.
type probeError struct {
	Status    string `json:"status"`
	Stage     string `json:"stage"`
	Collector string `json:"collector"`
	Metric    string `json:"metric,omitempty"`
	Target    string `json:"target,omitempty"`
	Error     string `json:"error"`
}

// writeProbeError writes a probe failure as a JSON object. Status is always
// "error", so a client can test one field without knowing the others.
func writeProbeError(w http.ResponseWriter, code int, body probeError) {
	body.Status = "error"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeMetricSet(w http.ResponseWriter, s *MetricSet) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	renderMetricSet(&b, s)
	_, _ = w.Write([]byte(b.String()))
}

// renderMetricSet writes the exposition text for a metric set, so the verbose
// self-metrics are rendered by exactly the same code as collector output.
func renderMetricSet(b *strings.Builder, s *MetricSet) {
	help := map[string]bool{}
	for _, m := range s.Metrics {
		if !help[m.Name] {
			if m.Help != "" {
				fmt.Fprintf(b, "# HELP %s %s\n", m.Name, strings.ReplaceAll(strings.ReplaceAll(m.Help, "\\", "\\\\"), "\n", "\\n"))
			}
			fmt.Fprintf(b, "# TYPE %s %s\n", m.Name, m.Type)
			help[m.Name] = true
		}
		if m.Histogram != nil {
			writeHistogram(b, m)
			continue
		}
		if m.Summary != nil {
			writeSummary(b, m)
			continue
		}
		fmt.Fprintf(b, "%s%s %s", m.Name, formatLabels(m.Labels), strconv.FormatFloat(m.Value, 'g', -1, 64))
		if m.Timestamp != nil {
			fmt.Fprintf(b, " %d", *m.Timestamp)
		}
		b.WriteByte('\n')
	}
}
func formatLabels(ls map[string]string) string {
	if len(ls) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ls))
	for k := range ls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=\"%s\"", k, promQuote(ls[k]))
	}
	b.WriteByte('}')
	return b.String()
}
func promQuote(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "\n", "\\n"), "\"", "\\\"")
}
func writeHistogram(b *strings.Builder, m Metric) {
	for _, x := range m.Histogram.Buckets {
		// The +Inf bucket is written below from the count. A histogram decoded
		// from a Prometheus source carries its own +Inf bucket, which written
		// here as well would be a duplicate series.
		if math.IsInf(x.UpperBound, 1) {
			continue
		}
		ls := cloneLabels(m.Labels)
		ls["le"] = strconv.FormatFloat(x.UpperBound, 'g', -1, 64)
		fmt.Fprintf(b, "%s_bucket%s %d\n", m.Name, formatLabels(ls), x.CumulativeCount)
	}
	ls := cloneLabels(m.Labels)
	ls["le"] = "+Inf"
	fmt.Fprintf(b, "%s_bucket%s %d\n%s_sum%s %s\n%s_count%s %d\n", m.Name, formatLabels(ls), m.Histogram.Count, m.Name, formatLabels(m.Labels), strconv.FormatFloat(m.Histogram.Sum, 'g', -1, 64), m.Name, formatLabels(m.Labels), m.Histogram.Count)
}
func writeSummary(b *strings.Builder, m Metric) {
	for _, x := range m.Summary.Quantiles {
		ls := cloneLabels(m.Labels)
		ls["quantile"] = strconv.FormatFloat(x.Quantile, 'g', -1, 64)
		fmt.Fprintf(b, "%s%s %s\n", m.Name, formatLabels(ls), strconv.FormatFloat(x.Value, 'g', -1, 64))
	}
	fmt.Fprintf(b, "%s_sum%s %s\n%s_count%s %d\n", m.Name, formatLabels(m.Labels), strconv.FormatFloat(m.Summary.Sum, 'g', -1, 64), m.Name, formatLabels(m.Labels), m.Summary.Count)
}
func cloneLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
