package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Server answers the exporter's HTTP endpoints — probes, its own metrics,
// health and readiness, and reloads — and keeps the state they share: the
// cache, the statistics and the OTLP queue.
type Server struct {
	manager         *config.Manager
	pythonPath      string
	logger          *slog.Logger
	selfMetricsPath string
	// staticTargetsPath is where the static targets are served; staticResults
	// is each target's latest result, by name (statictargetsendpoint.go).
	staticTargetsPath string
	staticMu          sync.Mutex
	staticResults     map[string]model.MetricSet
	staticLastSuccess map[string]time.Time
	// staticClashes are the targets' metrics the last read of the endpoint
	// left out for their type (statictargetsendpoint.go).
	staticClashMu sync.Mutex
	staticClashes map[string]staticClash
	// timeoutOffset is how much of Prometheus's scrape timeout a probe leaves
	// unused (scrapetimeout.go).
	timeoutOffset time.Duration
	// defaultProbeTimeout bounds a probe that names no deadline
	// (scrapetimeout.go).
	defaultProbeTimeout time.Duration
	statsMu             sync.Mutex
	stats               map[string]*serverStats
	otlpMu              sync.Mutex
	otlpPending         map[string]*otlpBatch
	// otlpPoints counts the pending data points, and otlpSeq and
	// otlpRequeueSeq order them by age for otlp.max_pending_points: queued
	// points count up from 1, points queued again after a failed export count
	// down from 0, so they are always the oldest.
	otlpPoints     int
	otlpSeq        int64
	otlpRequeueSeq int64
	// otlpStarts remembers when each cumulative series exported over OTLP
	// started (otlpstart.go).
	otlpStarts *otlpStartTimes
	cache      *responseCache
	requests   *requestTracker
	flights    *probeFlights
	durations  *scrapeDurations
	// trips bounds each collector's trips to its targets (triplimit.go).
	trips *tripLimiter
	// lifecycle enables POST /-/reload (lifecycle.go).
	lifecycle bool
	// otlp is how exports are going (otlpstatus.go).
	otlp *otlpStatus
	// failures keeps repeated failures from flooding the log (failurelog.go).
	failures *failureLog
	// scrapesCtx is what static target scrapes run under, until
	// AbortStaticScrapes cancels it (statictargetschedule.go).
	scrapesOnce  sync.Once
	scrapesCtx   context.Context
	abortScrapes context.CancelCauseFunc
	// fingerprints remembers each collector's fingerprint for the cache
	// keys of the current configuration (fingerprint.go).
	fingerprints *fingerprintMemo
	// stopping is set when a shutdown begins; /ready answers 503 from then.
	stopping atomic.Bool
	// seenConfig is the configuration the per-collector state was last
	// reconciled with (reconcile.go).
	seenConfig atomic.Pointer[model.Config]
}

// NewServer returns a server using the configuration m holds and running
// Python scripts with the interpreter at p.
func NewServer(m *config.Manager, p string, l *slog.Logger) *Server {
	s := &Server{manager: m, pythonPath: p, logger: l, stats: map[string]*serverStats{}, otlpPending: map[string]*otlpBatch{}, cache: newResponseCache(), requests: newRequestTracker(), flights: newProbeFlights(), durations: newScrapeDurations(), trips: newTripLimiter(), otlp: &otlpStatus{}, otlpStarts: newOTLPStartTimes(), failures: newFailureLog(), fingerprints: &fingerprintMemo{}, timeoutOffset: DefaultTimeoutOffset, defaultProbeTimeout: DefaultProbeTimeout}
	s.seenConfig.Store(m.Get())
	return s
}

// DefaultSelfMetricsPath is where the exporter serves its own metrics unless
// --web.self-metrics-path says otherwise.
const DefaultSelfMetricsPath = "/self-metrics"

// endpointPathRE, for the self-metrics and static targets paths, is a path of
// plain segments: no query, fragment, trailing slash or ServeMux wildcard, any
// of which would make the endpoint something other than one fixed path, and no
// segment of dots alone. ServeMux cleans a request's path before routing it,
// so /metrics/.. is asked for as / and an endpoint registered there is never
// reached; a segment needs a character other than a dot.
var endpointPathRE = regexp.MustCompile(`^(/[A-Za-z0-9._~-]*[A-Za-z0-9_~-][A-Za-z0-9._~-]*)+$`)

// SelfMetricsPath checks --web.self-metrics-path and returns it with its
// leading slash. The exporter serves its own metrics there and nowhere else,
// so a path another endpoint uses is refused rather than moved aside.
func SelfMetricsPath(path string) (string, error) {
	return endpointPath("--web.self-metrics-path", path, "/self-metrics or /metrics")
}

// endpointPath checks the path flag gives an endpoint: one fixed path, and not
// one of the exporter's fixed endpoints. example is suggested in the error.
func endpointPath(flag, path, example string) (string, error) {
	if path != "" && path[0] != '/' {
		path = "/" + path
	}
	if !endpointPathRE.MatchString(path) {
		return "", fmt.Errorf("%s %q must be a path of letters, digits and . _ ~ - segments, each with a character other than a dot, such as %s", flag, path, example)
	}
	switch path {
	case "/probe", "/health", "/ready", "/collectors", "/-/reload":
		return "", fmt.Errorf("%s %q is the exporter's %s endpoint; choose another path, such as %s", flag, path, path, example)
	}
	return path, nil
}

// SetSelfMetricsPath serves the exporter's own metrics at path, which
// SelfMetricsPath has checked.
func (s *Server) SetSelfMetricsPath(path string) { s.selfMetricsPath = path }

// selfMetricsEndpoint is the path the exporter's own metrics are served at.
func (s *Server) selfMetricsEndpoint() string {
	if s.selfMetricsPath == "" {
		return DefaultSelfMetricsPath
	}
	return s.selfMetricsPath
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

// Handler routes the exporter's endpoints: the landing page at /, the
// collectors page at /collectors, /probe, the self-metrics path, the static
// targets path,
// /health, /ready and /-/reload.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ready", s.readyHandler)
	protected := func(handler http.HandlerFunc) http.HandlerFunc { return s.basicAuthMiddleware(handler) }
	// The answers Prometheus scrapes are gzipped when it asks (compression.go).
	mux.HandleFunc(s.selfMetricsEndpoint(), compressed(protected(s.metricsHandler)))
	mux.HandleFunc(s.staticTargetsEndpoint(), compressed(protected(s.staticTargetsHandler)))
	mux.HandleFunc("/probe", compressed(protected(s.probeHandler)))
	mux.HandleFunc("/-/reload", protected(s.reloadHandler))
	// Only / itself: any other unknown path is still a 404.
	mux.HandleFunc("GET /{$}", protected(s.landingHandler))
	mux.HandleFunc("GET /collectors", protected(s.collectorsHandler))
	return mux
}

func (s *Server) probeHandler(w http.ResponseWriter, r *http.Request) {
	// A probe only reads; HEAD runs it in full, as net/http answers HEAD with
	// GET's status and headers. Anything else is refused before the target
	// is contacted or the probe counted.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "use GET or HEAD to probe a target", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	target := r.URL.Query().Get("target")
	name := r.URL.Query().Get("collector")
	if name == "" {
		http.Error(w, "the collector parameter is required: /probe?collector=<name>&target=<target>", http.StatusBadRequest)
		return
	}
	s.reconcile()
	cfg := s.manager.Get()
	var c *model.Collector
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
	if err := fetch.CheckTarget(c, target, false); err != nil {
		if errors.Is(err, fetch.ErrMissingTarget) {
			http.Error(w, fmt.Sprintf("the target parameter is required for collector %q, whose request.type is %s", name, c.Request.Type), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	logTarget := fetch.DisplayTarget(c, target)
	// The request parameters are resolved before anything is counted, so every
	// counter this probe raises lands on the individual request as well as on
	// the collector rather than only on the collector.
	overrides, err := fetch.ParseRequestOverrides(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Each request type accepts its own probe parameters; one that belongs to
	// another type is a mistake, reported before anything else happens.
	if err := fetch.CheckOverrideParams(c, r.URL.Query()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A path parameter the scrape did not supply and that has no default is the
	// caller's mistake, reported before the target is contacted or anything is
	// counted against it.
	if err := fetch.CheckPathParams(c, overrides); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var requestURL string
	method := fetch.RequestMethodFor(c, overrides)
	if label, labelErr := fetch.RequestLabelFor(target, c, overrides); labelErr == nil {
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
	key := s.probeCacheKey(cfg, c, target, r.URL.Query(), forwarded)
	var cacheKey string
	if model.UsesCache(c) {
		cacheKey = key
	}
	if model.CacheTTL(c) > 0 {
		if cached, fetched, ok := s.cache.Get(cacheKey, time.Now()); ok {
			rec.update(func(x *serverStats) {
				x.cacheHits++
				x.emitted += uint64(len(cached.Metrics))
			})
			// The names were checked before the result was stored.
			answer, _ := withFreshness(cached, c, false, fetched, time.Now())
			finish(true)
			writeMetricSet(w, &answer)
			s.queueProbeOTLP(answer, name, logTarget)
			return
		}
		rec.update(func(x *serverStats) { x.cacheMisses++ })
	}
	budget, budgetSource := s.probeDeadline(r.Header, overrides)
	upstream := func(ctx context.Context) *probeResult {
		return s.probeUpstream(ctx, upstreamProbe{
			collector: c, target: target, logTarget: logTarget, overrides: overrides,
			forwarded: forwarded, rec: rec, cacheKey: cacheKey,
			budget: budget, budgetSource: budgetSource,
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
	collector *model.Collector
	target    string
	logTarget string
	overrides fetch.RequestOverrides
	forwarded http.Header
	rec       statsRecorder
	cacheKey  string
	// budget bounds the trip (scrapetimeout.go); budgetSource says where it
	// came from.
	budget       time.Duration
	budgetSource string
}

// probeUpstream goes to the target, decodes, transforms and validates, and
// records the answer instead of writing it, so identical probes waiting on it
// can each be given a copy. Its self-metrics, logs, cache entry and OTLP
// export happen once, however many probes share it. When the trip fails and
// the collector has cache.stale_if_error, the last good result answers
// instead of the error (stalecache.go).
func (s *Server) probeUpstream(ctx context.Context, p upstreamProbe) *probeResult {
	c, name := p.collector, p.collector.Name
	result := s.probeTrip(ctx, p)
	if model.StaleIfError(c) <= 0 || p.cacheKey == "" {
		return result
	}
	staleKey := failureKey(name, p.logTarget, "\x00stale")
	if result.ok {
		s.failures.recovered(s.logger, staleKey, "probe answered with a fresh result again", "collector", name, "target", p.logTarget)
		return result
	}
	now := time.Now()
	cached, fetched, found := s.cache.GetStale(p.cacheKey, now)
	if !found {
		return result
	}
	answer, err := withFreshness(cached, c, true, fetched, now)
	if err != nil {
		return result
	}
	p.rec.update(func(x *serverStats) {
		x.staleServed++
		x.emitted += uint64(len(cached.Metrics))
	})
	s.failures.failed(s.logger, slog.LevelWarn, staleKey, "probe failed; answered with the last successful result (cache.stale_if_error)", "stale", nil, "collector", name, "target", p.logTarget, "result_age", now.Sub(fetched).Round(time.Second).String())
	out := newProbeRecorder()
	writeMetricSet(out, &answer)
	s.queueProbeOTLP(answer, name, p.logTarget)
	// A stale answer is not a success: the trip failed.
	return out.result(false)
}

// probeTrip is one trip to the target, or the fresh cache entry a probe that
// finished while this one waited to start left behind.
func (s *Server) probeTrip(ctx context.Context, p upstreamProbe) *probeResult {
	c, name, rec, logTarget := p.collector, p.collector.Name, p.rec, p.logTarget
	out := newProbeRecorder()
	// A probe that finished while this one was waiting to start may have just
	// filled the cache.
	if p.cacheKey != "" && model.CacheTTL(c) > 0 {
		if cached, fetched, ok := s.cache.Get(p.cacheKey, time.Now()); ok {
			rec.update(func(x *serverStats) { x.emitted += uint64(len(cached.Metrics)) })
			answer, _ := withFreshness(cached, c, false, fetched, time.Now())
			writeMetricSet(out, &answer)
			return out.result(true)
		}
	}
	// Answered at once when the collector's backend already has all it may
	// get, rather than queued behind the probes in progress (triplimit.go).
	limit := maxConcurrentProbes(c)
	if !s.trips.tryAcquire(name, limit) {
		rec.update(func(x *serverStats) { x.rejected++ })
		s.failures.failed(s.logger, slog.LevelWarn, failureKey(name, logTarget, ""), "probe rejected: the collector has too many probes in progress", "concurrency", nil, "collector", name, "target", logTarget, "max_concurrent_probes", limit)
		http.Error(out, fmt.Sprintf("collector %s already has %d probes to its targets in progress, its max_concurrent_probes; this one was not sent", name, limit), http.StatusServiceUnavailable)
		return out.result(false)
	}
	defer s.trips.release(name)
	if p.budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.budget)
		defer cancel()
	}
	result := s.collect(ctx, collectJob{
		collector: c, target: p.target, overrides: p.overrides, headers: p.forwarded,
		rec: rec, display: logTarget, cacheKey: p.cacheKey, budget: p.budget, budgetSource: p.budgetSource,
		log: collectLog{
			key:    failureKey(name, logTarget, ""),
			failed: "probe failed", continuing: "probe stage failed; continuing", recovery: "probe recovered",
			attrs: []any{"collector", name, "target", logTarget},
		},
	})
	switch {
	case result.metric != "":
		writeProbeError(out, http.StatusBadGateway, probeError{
			Stage:     result.stage,
			Collector: name,
			Metric:    result.metric,
			Target:    logTarget,
			Error:     result.err.Error(),
		})
		return out.result(false)
	case result.failed():
		http.Error(out, fmt.Sprintf("collector %s %s failed: %v", name, result.stage, result.err), http.StatusBadGateway)
		return out.result(false)
	case result.carriedOn:
		// Answered 200 with nothing: the stage's policy said to carry on.
		return out.result(true)
	}
	writeMetricSet(out, &result.answer)
	s.queueProbeOTLP(result.answer, name, logTarget)
	return out.result(true)
}

// unforwardableHeaders are never forwarded, whatever request.forward_headers
// lists: Authorization has request.forward_authorization of its own, and the
// rest describe the connection to the exporter, not the request to the target.
var unforwardableHeaders = map[string]bool{
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

// forwardableHeaders is request.forward_headers as they are forwarded:
// canonical, without the ones never forwarded, each once, sorted.
func forwardableHeaders(request model.RequestConfig) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range request.ForwardHeaders {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name != "" && !unforwardableHeaders[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// forwardedHeaders extracts only explicitly allowed headers from the probe
// request. Prometheus Operator monitor params use the header_<name> convention
// for static, non-secret target headers. Authorization is handled separately so
// a Secret-backed monitor credential can be forwarded without putting it in a
// URL parameter.
func forwardedHeaders(r *http.Request, request model.RequestConfig) http.Header {
	out := make(http.Header)
	allowed := map[string]bool{}
	for _, name := range forwardableHeaders(request) {
		allowed[name] = true
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
		if name == "" || !allowed[name] {
			continue
		}
		// An empty value is left out, as an empty param_ value is: a form
		// field left blank is a header not given, not one sent empty.
		for _, value := range values {
			if value != "" {
				out.Add(name, value)
			}
		}
	}
	return out
}

// sanitizeUTF8 repairs a transform's output, counting and logging what it
// changed, so the one scrape still reaches Prometheus and the problem is still
// seen.
func (s *Server) sanitizeUTF8(set *model.MetricSet, rec statsRecorder, c *model.Collector, target string) {
	key := failureKey(c.Name, target, "\x00utf8")
	changed, first := model.SanitizeUTF8(set)
	if changed == 0 {
		s.failures.recovered(s.logger, key, "output is valid UTF-8 again", "collector", c.Name, "target", target)
		return
	}
	rec.update(func(x *serverStats) { x.invalidUTF8 += changed })
	s.failures.failed(s.logger, slog.LevelWarn, key, "label values or help text were not valid UTF-8; the invalid bytes were replaced with U+FFFD. If the target uses another encoding without declaring it, set response.charset", "utf8", nil, "collector", c.Name, "target", target, "values", changed, "first_metric", first)
}
