package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// statsValues are the exporter's own counters for one collector, or for one
// request when verbose self-metrics are configured. They are separated from the
// mutex so a consistent copy can be taken under the lock and rendered outside
// it.
type statsValues struct {
	probes, success, decodeOK, parseErrors, transformErrors, missing, scriptErrors, limitErrors, emitted, cacheHits, cacheMisses uint64
	lastStatus                                                                                                                   int
	lastBytes                                                                                                                    int64
	lastDuration                                                                                                                 float64
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

// selfMetricDescriptors are the exporter's own metric families, in the order
// they are exposed, each with the help text it is published with. A shared
// placeholder would make the endpoint self-documenting in name only: HELP is
// what a reader sees in Grafana's metric browser or `curl`, and sixteen
// identical lines tell them nothing about which counter to reach for.
var selfMetricDescriptors = []selfMetricDescriptor{
	{"http_exporter_scrapes_total", "Probes served for this collector, including those answered from the response cache."},
	{"http_exporter_scrape_success", "Probes for this collector that completed without a fatal error."},
	{"http_exporter_scrape_duration_seconds", "Duration of the most recent probe of this collector, in seconds."},
	{"http_exporter_scrape_http_status_code", "HTTP status the target returned on the most recent scrape, or 0 when the request failed before a response arrived."},
	{"http_exporter_scrape_response_bytes", "Size of the most recent response body for this collector, in bytes."},
	{"http_exporter_decode_success", "Responses this collector decoded into its configured format."},
	{"http_exporter_parse_errors_total", "Responses this collector's decoder could not parse."},
	{"http_exporter_transform_errors_total", "Transforms that failed for this collector."},
	{"http_exporter_missing_keys_total", "Transform failures caused by a key or field the response did not contain."},
	{"http_exporter_script_errors_total", "Python script failures during this collector's transform."},
	{"http_exporter_script_duration_seconds", "Duration of the most recent Python script run for this collector, in seconds."},
	{"http_exporter_metrics_emitted", "Metrics this collector has produced across its scrapes."},
	{"http_exporter_series_limit_exceeded", "Scrapes rejected for exceeding this collector's response size or series limits."},
	{"http_exporter_cache_hits_total", "Probes answered from this collector's response cache."},
	{"http_exporter_cache_misses_total", "Probes that found no usable cache entry and went to the target."},
	{"http_exporter_cache_entries", "Entries currently held in this collector's response cache."},
}

// selfMetricDescriptor names one of the exporter's own metric families and the
// help text it is published with.
type selfMetricDescriptor struct {
	Name string
	Help string
}

// selfMetricHelp indexes the descriptors, so the self-metrics served as a
// MetricSet carry the same help as the ones rendered as text.
var selfMetricHelp = func() map[string]string {
	out := make(map[string]string, len(selfMetricDescriptors))
	for _, d := range selfMetricDescriptors {
		out[d.Name] = d.Help
	}
	return out
}()

// selfMetricNames lists the families in exposition order.
func selfMetricNames() []string {
	names := make([]string, 0, len(selfMetricDescriptors))
	for _, d := range selfMetricDescriptors {
		names = append(names, d.Name)
	}
	return names
}

type selfSeries struct {
	Name  string
	Type  MetricType
	Value float64
}

// statsSeries turns one set of counters into the exporter's own series, in the
// order of selfMetricNames. http_exporter_cache_entries is not here: it counts
// what a collector's cache holds, which belongs to the collector rather than to
// any one request, so the per-collector rendering appends it itself.
func statsSeries(v statsValues) []selfSeries {
	return []selfSeries{
		{"http_exporter_scrapes_total", CounterMetricType, float64(v.probes)},
		{"http_exporter_scrape_success", GaugeMetricType, float64(v.success)},
		{"http_exporter_scrape_duration_seconds", GaugeMetricType, v.lastDuration},
		{"http_exporter_scrape_http_status_code", GaugeMetricType, float64(v.lastStatus)},
		{"http_exporter_scrape_response_bytes", GaugeMetricType, float64(v.lastBytes)},
		{"http_exporter_decode_success", GaugeMetricType, float64(v.decodeOK)},
		{"http_exporter_parse_errors_total", CounterMetricType, float64(v.parseErrors)},
		{"http_exporter_transform_errors_total", CounterMetricType, float64(v.transformErrors)},
		{"http_exporter_missing_keys_total", CounterMetricType, float64(v.missing)},
		{"http_exporter_script_errors_total", CounterMetricType, float64(v.scriptErrors)},
		{"http_exporter_script_duration_seconds", GaugeMetricType, 0},
		{"http_exporter_metrics_emitted", GaugeMetricType, float64(v.emitted)},
		{"http_exporter_series_limit_exceeded", CounterMetricType, float64(v.limitErrors)},
		{"http_exporter_cache_hits_total", CounterMetricType, float64(v.cacheHits)},
		{"http_exporter_cache_misses_total", CounterMetricType, float64(v.cacheMisses)},
	}
}

type Server struct {
	manager         *ConfigManager
	pythonPath      string
	logger          *slog.Logger
	selfMetricsPath string
	statsMu         sync.Mutex
	stats           map[string]*serverStats
	otlpMu          sync.Mutex
	otlpPending     map[string]*otlpBatch
	cache           *responseCache
	requests        *requestTracker
	ready           atomic.Bool
}

func NewServer(m *ConfigManager, p string, l *slog.Logger) *Server {
	s := &Server{manager: m, pythonPath: p, logger: l, stats: map[string]*serverStats{}, otlpPending: map[string]*otlpBatch{}, cache: newResponseCache(), requests: newRequestTracker()}
	s.ready.Store(true)
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

func (s *Server) OTLPExportLoop(ctx context.Context) {
	for {
		interval := time.Duration(s.manager.Get().OTLP.Interval)
		if interval <= 0 {
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		cfg := s.manager.Get().OTLP
		if !cfg.Enabled || cfg.Endpoint == "" {
			_ = s.drainOTLP()
			continue
		}
		s.scrapeScheduledTargets(ctx, interval)
		resources := appendToResource(s.drainOTLP(), defaultResourceIdentity(cfg), s.selfMetricSet())
		if len(resources) > 0 {
			s.pushOTLP(resources)
		}
	}
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
	return mux
}
func (s *Server) readyHandler(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
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
	if target == "" || name == "" {
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
	logTarget := safeTarget(target)
	// The request parameters are resolved before anything is counted, so every
	// counter this probe raises lands on the individual request as well as on
	// the collector rather than only on the collector.
	overrides, err := parseRequestOverrides(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var requestURL string
	method := requestMethod(c, overrides)
	if resolved, resolveErr := resolveRequestURL(target, c, overrides); resolveErr == nil {
		requestURL = requestLabelURL(resolved)
	}
	st := s.statsFor(name)
	rec := s.recorderFor(st, name, requestURL, method)
	rec.update(func(x *serverStats) { x.probes++ })
	// scraped stays false until the exporter has actually gone to the target,
	// so a response served from the collector cache leaves the last-scrape
	// timestamp describing the scrape that filled it.
	scraped := false
	finish := func(ok bool) {
		duration := time.Since(start)
		rec.update(func(x *serverStats) {
			if ok {
				x.success++
			}
			x.lastDuration = duration.Seconds()
		})
		if scraped {
			rec.scraped(time.Now())
		}
	}
	failStage := func(stage string, err error, policy string) bool {
		if policy == "warn" || policy == "ignore" {
			s.logger.Warn("probe stage failed; continuing", "collector", name, "target", logTarget, "stage", stage, "error", err)
			return true
		}
		s.logger.Error("probe failed", "collector", name, "target", logTarget, "stage", stage, "error", err)
		finish(false)
		http.Error(w, fmt.Sprintf("collector %s %s failed: %v", name, stage, err), http.StatusBadGateway)
		return false
	}
	ctx := r.Context()
	forwarded := forwardedHeaders(r, c.Request)
	cacheTTL := time.Duration(c.Cache)
	var cacheKey string
	if cacheTTL > 0 {
		cacheKey = probeCacheKey(c, target, r.URL.Query(), forwarded)
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
	resp, err := fetch(ctx, target, c, overrides, forwarded)
	scraped = true
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "response size") {
			rec.update(func(x *serverStats) { x.limitErrors++ })
		}
		if !failStage("http", err, c.ErrorHandling.OnHTTPError) {
			return
		}
		finish(true)
		return
	}
	rec.update(func(x *serverStats) {
		x.lastStatus = resp.StatusCode
		x.lastBytes = int64(len(resp.Body))
	})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !failStage("http_status", fmt.Errorf("received HTTP status %d", resp.StatusCode), c.ErrorHandling.OnHTTPError) {
			return
		}
		finish(true)
		return
	}
	d, err := decode(resp, c)
	if err != nil {
		rec.update(func(x *serverStats) { x.parseErrors++ })
		if !failStage("decode", err, c.ErrorHandling.OnDecodeError) {
			return
		}
		finish(true)
		return
	}
	rec.update(func(x *serverStats) { x.decodeOK++ })
	ms, err := transform(ctx, d, resp, c, s.pythonPath)
	if err != nil {
		rec.update(func(x *serverStats) {
			if strings.Contains(strings.ToLower(err.Error()), "missing") {
				x.missing++
			}
			if strings.Contains(strings.ToLower(err.Error()), "python") {
				x.scriptErrors++
			}
			x.transformErrors++
		})
		if !failStage("transform", err, c.ErrorHandling.OnTransformError) {
			return
		}
		finish(true)
		return
	}
	if err = ms.Validate(c.Limits); err != nil {
		rec.update(func(x *serverStats) { x.limitErrors++ })
		if !failStage("validation", err, "fail") {
			return
		}
		return
	}
	rec.update(func(x *serverStats) { x.emitted += uint64(len(ms.Metrics)) })
	finish(true)
	s.cache.Put(cacheKey, c.Name, *ms, cacheTTL, c.Limits.MaxCacheEntries, time.Now())
	writeMetricSet(w, ms)
	s.queueOTLP(*ms)
}

func safeTarget(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.User != nil {
		u.User = url.UserPassword("redacted", "redacted")
	}
	return u.String()
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

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	s.statsMu.Lock()
	for _, c := range s.manager.Get().Collectors {
		if s.stats[c.Name] == nil {
			s.stats[c.Name] = &serverStats{}
		}
	}
	names := make([]string, 0, len(s.stats))
	for n := range s.stats {
		names = append(names, n)
	}
	sort.Strings(names)
	ss := make([]struct {
		name string
		v    *serverStats
	}, 0, len(names))
	for _, n := range names {
		ss = append(ss, struct {
			name string
			v    *serverStats
		}{n, s.stats[n]})
	}
	s.statsMu.Unlock()
	cacheEntries := s.cache.Stats(time.Now())
	var b strings.Builder
	declared := map[string]bool{}
	for _, d := range selfMetricDescriptors {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", d.Name, d.Help, d.Name)
		declared[d.Name] = true
	}
	b.WriteString("# HELP http_exporter_collector_config_valid Whether the collector configuration is valid.\n# TYPE http_exporter_collector_config_valid gauge\n")
	for _, x := range names {
		fmt.Fprintf(&b, "http_exporter_collector_config_valid{collector=%q} 1\n", x)
	}
	fmt.Fprintf(&b, "# HELP http_exporter_scheduled_targets Scheduled targets configured for OTLP delivery.\n# TYPE http_exporter_scheduled_targets gauge\nhttp_exporter_scheduled_targets %d\n", len(s.manager.Targets()))
	for _, x := range ss {
		x.v.mu.Lock()
		p, ok, d, pe, te, m, se, le, em, status, bytes, dur, hits, misses := x.v.probes, x.v.success, x.v.decodeOK, x.v.parseErrors, x.v.transformErrors, x.v.missing, x.v.scriptErrors, x.v.limitErrors, x.v.emitted, x.v.lastStatus, x.v.lastBytes, x.v.lastDuration, x.v.cacheHits, x.v.cacheMisses
		x.v.mu.Unlock()
		label := fmt.Sprintf("{collector=%q}", x.name)
		fmt.Fprintf(&b, "http_exporter_scrapes_total%s %d\nhttp_exporter_scrape_success%s %d\nhttp_exporter_scrape_duration_seconds%s %s\nhttp_exporter_scrape_http_status_code%s %d\nhttp_exporter_scrape_response_bytes%s %d\nhttp_exporter_decode_success%s %d\nhttp_exporter_parse_errors_total%s %d\nhttp_exporter_transform_errors_total%s %d\nhttp_exporter_missing_keys_total%s %d\nhttp_exporter_script_errors_total%s %d\nhttp_exporter_script_duration_seconds%s 0\nhttp_exporter_metrics_emitted%s %d\nhttp_exporter_series_limit_exceeded%s %d\n", label, p, label, ok, label, strconv.FormatFloat(dur, 'f', -1, 64), label, status, label, bytes, label, d, label, pe, label, te, label, m, label, se, label, label, em, label, le)
		fmt.Fprintf(&b, "http_exporter_cache_hits_total%s %d\nhttp_exporter_cache_misses_total%s %d\nhttp_exporter_cache_entries%s %d\n", label, hits, label, misses, label, cacheEntries[x.name])
	}
	s.renderVerboseRequestMetrics(&b, declared)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) selfMetricSet() MetricSet {
	cacheEntries := s.cache.Stats(time.Now())
	s.statsMu.Lock()
	for _, c := range s.manager.Get().Collectors {
		if s.stats[c.Name] == nil {
			s.stats[c.Name] = &serverStats{}
		}
	}
	ss := make([]struct {
		name string
		v    *serverStats
	}, 0, len(s.stats))
	for name, st := range s.stats {
		ss = append(ss, struct {
			name string
			v    *serverStats
		}{name, st})
	}
	s.statsMu.Unlock()
	out := MetricSet{}
	for _, x := range ss {
		x.v.mu.Lock()
		labels := map[string]string{"collector": x.name}
		add := func(name string, typ MetricType, value float64) {
			out.Metrics = append(out.Metrics, Metric{Name: name, Help: selfMetricHelp[name], Type: typ, Value: value, Labels: cloneLabels(labels)})
		}
		add("http_exporter_scrapes_total", CounterMetricType, float64(x.v.probes))
		add("http_exporter_scrape_success", GaugeMetricType, float64(x.v.success))
		add("http_exporter_scrape_duration_seconds", GaugeMetricType, x.v.lastDuration)
		add("http_exporter_scrape_http_status_code", GaugeMetricType, float64(x.v.lastStatus))
		add("http_exporter_scrape_response_bytes", GaugeMetricType, float64(x.v.lastBytes))
		add("http_exporter_decode_success", GaugeMetricType, float64(x.v.decodeOK))
		add("http_exporter_parse_errors_total", CounterMetricType, float64(x.v.parseErrors))
		add("http_exporter_transform_errors_total", CounterMetricType, float64(x.v.transformErrors))
		add("http_exporter_missing_keys_total", CounterMetricType, float64(x.v.missing))
		add("http_exporter_script_errors_total", CounterMetricType, float64(x.v.scriptErrors))
		add("http_exporter_script_duration_seconds", GaugeMetricType, 0)
		add("http_exporter_metrics_emitted", GaugeMetricType, float64(x.v.emitted))
		add("http_exporter_series_limit_exceeded", CounterMetricType, float64(x.v.limitErrors))
		add("http_exporter_cache_hits_total", CounterMetricType, float64(x.v.cacheHits))
		add("http_exporter_cache_misses_total", CounterMetricType, float64(x.v.cacheMisses))
		add("http_exporter_cache_entries", GaugeMetricType, float64(cacheEntries[x.name]))
		x.v.mu.Unlock()
	}
	for _, c := range s.manager.Get().Collectors {
		out.Metrics = append(out.Metrics, Metric{Name: "http_exporter_collector_config_valid", Type: GaugeMetricType, Value: 1, Labels: map[string]string{"collector": c.Name}})
	}
	out.Metrics = append(out.Metrics, Metric{Name: "http_exporter_scheduled_targets", Help: "Scheduled targets configured for OTLP delivery.", Type: GaugeMetricType, Value: float64(len(s.manager.Targets()))})
	out.Metrics = append(out.Metrics, s.verboseRequestMetrics()...)
	return out
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
