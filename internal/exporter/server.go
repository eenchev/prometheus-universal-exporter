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
	// is each target's latest result, by name, and staticFetched when the
	// data in it came from the target, for its age (statictargetsendpoint.go).
	staticTargetsPath string
	// probeDebug allows debug probes, --web.enable-probe-debug
	// (probedebug.go).
	probeDebug        bool
	staticMu          sync.Mutex
	staticResults     map[string]model.MetricSet
	staticFetched     map[string]time.Time
	staticLastSuccess map[string]time.Time
	// staticGeneration counts the results published, under staticMu, and
	// staticView is the endpoint's merge of them, rebuilt when a result is
	// published or the targets change (statictargetsendpoint.go).
	staticGeneration uint64
	staticViewMu     sync.Mutex
	staticView       *staticTargetsView
	// staticClashes are the targets' metrics the last read of the endpoint
	// left out for their type (statictargetsendpoint.go).
	staticClashMu sync.Mutex
	staticClashes map[string]staticClash
	// timeoutOffset is how much of Prometheus's scrape timeout a probe leaves
	// unused (scrapetimeout.go).
	timeoutOffset time.Duration
	// defaultProbeTimeout bounds a probe without a scrape timeout
	// (scrapetimeout.go).
	defaultProbeTimeout time.Duration
	statsMu             sync.Mutex
	stats               map[string]*serverStats
	otlpMu              sync.Mutex
	otlpPending         map[string]*otlpBatch
	// otlpPoints counts the pending data points, and otlpSeq orders them by
	// age for otlp.max_pending_points: each point queued takes the next
	// number, and keeps it when a failed export queues it again, so the
	// oldest points are dropped first however many exports have failed.
	otlpPoints int
	otlpSeq    int64
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
	// otlp is how exports are going (otlp.go).
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
	// The answers Prometheus scrapes are gzipped when it asks (middleware.go).
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
	// A debug probe is refused before anything else when it is not enabled,
	// so the refusal does not depend on the rest of the probe being right.
	debug, err := probeDebugRequested(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if debug && !s.probeDebug {
		http.Error(w, errProbeDebugDisabled.Error(), http.StatusForbidden)
		return
	}
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
	if debug {
		// Counted nowhere, cached nowhere, shared with nobody
		// (probedebug.go).
		forwarded := forwardedHeaders(r, c.Request)
		budget, budgetSource := s.probeDeadline(r.Header, overrides)
		p := debugProbe{
			upstreamProbe: upstreamProbe{
				collector: c, target: target, logTarget: logTarget, overrides: overrides,
				forwarded: forwarded, budget: budget, budgetSource: budgetSource,
			},
			requestURL: requestURL, method: method,
		}
		if model.UsesCache(c) {
			// The key the same probe without debug has: debug is no
			// parameter of the request (probeKeyQuery).
			p.staleKey = s.probeCacheKey(cfg, c, target, probeKeyQuery(c, r.URL.Query()), forwarded)
		}
		s.serveDebugProbe(w, r, p)
		return
	}
	st := s.statsFor(name)
	rec := s.probeRecorderFor(st, name, requestURL, method)
	rec.update(func(x *serverStats) { x.probes++ })
	// finish counts the probe's outcome and ends its per-request record: a
	// probe the target policy refused leaves no new request tracked.
	finish := func(ok, refused bool) {
		duration := time.Since(start)
		rec.update(func(x *serverStats) {
			if ok {
				x.success++
			}
			x.lastDuration = duration.Seconds()
		})
		rec.commit(refused)
	}
	forwarded := forwardedHeaders(r, c.Request)
	key := s.probeCacheKey(cfg, c, target, probeKeyQuery(c, r.URL.Query()), forwarded)
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
			finish(true, false)
			writeMetricSet(w, r, &answer)
			s.queueProbeOTLP(answer, name, logTarget)
			return
		}
		// A miss is counted when the trip goes to the target (probeTrip);
		// a probe that shares another's trip is counted as coalesced.
	}
	budget, budgetSource := s.probeDeadline(r.Header, overrides)
	upstream := func(ctx context.Context) *probeResult {
		return s.probeUpstream(ctx, upstreamProbe{
			collector: c, target: target, logTarget: logTarget, overrides: overrides,
			forwarded: forwarded, rec: rec, cacheKey: cacheKey,
			budget: budget, budgetSource: budgetSource,
			// Probes of one target differing in their parameters or
			// forwarded headers — tenants, paths — fail apart in the log.
			failureTarget: target + "\x00" + key, requestURL: requestURL,
		})
	}
	// No key, when the collector's definition could not be fingerprinted,
	// is no identity to share a trip by: every such probe would share one.
	if !coalesceProbes(c) || key == "" {
		result := upstream(r.Context())
		finish(result.ok, result.refused)
		result.writeTo(w, r)
		return
	}
	result, shared, err := s.flights.do(r.Context(), key, upstream)
	if err != nil {
		// This caller went away while it waited; there is nobody to answer,
		// and no verdict of the target policy to go by.
		finish(false, true)
		return
	}
	if shared {
		rec.update(func(x *serverStats) { x.coalesced++ })
		s.logger.Debug("probe shared an identical probe in flight", "collector", name, "target", logTarget)
	}
	finish(result.ok, result.refused)
	result.writeTo(w, r)
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
	// failureTarget tells the probe apart in the failure log
	// (failurelog.go): its target with everything else that makes it the
	// probe it is, parameters and forwarded headers; the target alone when
	// empty. requestURL is its url label, which logs show.
	failureTarget string
	requestURL    string
}

// failureKeyTarget is what the failure log tells p apart by.
func (p upstreamProbe) failureKeyTarget() string {
	if p.failureTarget != "" {
		return p.failureTarget
	}
	return p.target
}

// logAttrs are the attributes p's log lines carry.
func (p upstreamProbe) logAttrs() []any {
	attrs := []any{"collector", p.collector.Name, "target", p.logTarget}
	if p.requestURL != "" {
		attrs = append(attrs, "url", p.requestURL)
	}
	return attrs
}

// probeUpstream goes to the target, decodes, transforms and validates, and
// records the answer instead of writing it, so identical probes waiting on it
// can each be given a copy. Its self-metrics, logs, cache entry and OTLP
// export happen once, however many probes share it. When the trip fails and
// the collector has cache.stale_if_error, the last good result answers
// instead of the error (cache.go).
func (s *Server) probeUpstream(ctx context.Context, p upstreamProbe) *probeResult {
	c, name := p.collector, p.collector.Name
	result := s.probeTrip(ctx, p)
	if result.abandoned || model.StaleIfError(c) <= 0 || p.cacheKey == "" {
		return result
	}
	// A target that refused the credential is not answered for with what an
	// earlier credential, perhaps since revoked, was given.
	if result.unauthorized {
		s.logger.Debug("probe failed on its credential; no stale result served", p.logAttrs()...)
		return result
	}
	// Nor is a target the collector may not reach answered for: the refusal
	// is the answer, whatever an earlier probe, made before allowed_targets
	// or denied_targets changed or before the target's name resolved
	// elsewhere, left in the cache.
	if result.refused {
		s.logger.Debug("probe refused by the target policy; no stale result served", p.logAttrs()...)
		return result
	}
	staleKey := failureKey(name, p.failureKeyTarget(), "\x00stale")
	if result.ok {
		s.failures.recovered(s.logger, staleKey, "probe answered with a fresh result again", p.logAttrs()...)
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
	s.failures.failed(s.logger, slog.LevelWarn, staleKey, "probe failed; answered with the last successful result (cache.stale_if_error)", "stale", nil, append(p.logAttrs(), "result_age", now.Sub(fetched).Round(time.Second).String())...)
	out := newProbeRecorder()
	out.metrics = &answer
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
			rec.update(func(x *serverStats) {
				x.cacheHits++
				x.emitted += uint64(len(cached.Metrics))
			})
			answer, _ := withFreshness(cached, c, false, fetched, time.Now())
			out.metrics = &answer
			// Queued for OTLP as a hit before the trip is (probeHandler):
			// once, since the probes sharing this trip queue nothing of
			// their own, and the probe that filled the entry queued its own
			// answer, not this one's.
			s.queueProbeOTLP(answer, name, logTarget)
			return out.result(true)
		}
		rec.update(func(x *serverStats) { x.cacheMisses++ })
	}
	// Answered at once when the collector's backend already has all it may
	// get, rather than queued behind the probes in progress (triplimit.go).
	limit := maxConcurrentProbes(c)
	if full := s.trips.tryAcquire(name, limit); full != nil {
		rec.update(func(x *serverStats) { countRejection(x, full) })
		s.failures.failed(s.logger, slog.LevelWarn, failureKey(name, p.failureKeyTarget(), ""), "probe rejected: too many probes in progress", "concurrency", nil, append(p.logAttrs(), "reason", full.message)...)
		http.Error(out, full.message+"; this probe was not sent", http.StatusServiceUnavailable)
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
			key:    failureKey(name, p.failureKeyTarget(), ""),
			failed: "probe failed", continuing: "probe stage failed; continuing", recovery: "probe recovered",
			attrs: p.logAttrs(),
		},
	})
	switch {
	case result.aborted:
		// Every probe waiting for it went away: no stale answer, no OTLP
		// point, no failure logged.
		http.Error(out, "probe cancelled: the caller went away", http.StatusServiceUnavailable)
		abandoned := out.result(false)
		abandoned.abandoned = true
		return abandoned
	case result.metric != "":
		writeProbeError(out, http.StatusBadGateway, probeError{
			Stage:     result.stage,
			Collector: name,
			Metric:    result.metric,
			Target:    logTarget,
			Error:     result.err.Error(),
		})
		return out.result(false)
	case result.refused:
		// The caller asked for a target the collector may not reach.
		http.Error(out, fmt.Sprintf("collector %s refused the target: %v", name, result.err), http.StatusForbidden)
		refused := out.result(false)
		refused.refused = true
		return refused
	case result.failed():
		http.Error(out, fmt.Sprintf("collector %s %s failed: %v", name, result.stage, result.err), http.StatusBadGateway)
		failure := out.result(false)
		failure.unauthorized = result.unauthorized
		return failure
	case result.carriedOn:
		// Answered 200 with nothing: the stage's policy said to carry on.
		return out.result(true)
	}
	out.metrics = &result.answer
	s.queueProbeOTLP(result.answer, name, logTarget)
	return out.result(true)
}

// unforwardableHeaders are never forwarded, whatever request.forward_headers
// lists: Authorization has request.forward_authorization of its own,
// Accept-Encoding is the exporter's to set (Prometheus asks it for gzip on
// every scrape, and Go decompresses an answer only when it asked itself), and
// the rest describe the connection to the exporter, not the request to the
// target.
// Neither is any Proxy- header (unforwardable), whatever follows the dash.
var unforwardableHeaders = map[string]bool{
	"Accept-Encoding":     true,
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

// unforwardable reports whether name, canonical, is never forwarded.
func unforwardable(name string) bool {
	return unforwardableHeaders[name] || strings.HasPrefix(name, "Proxy-")
}

// forwardableHeaders is request.forward_headers as they are forwarded:
// canonical, without the ones never forwarded, each once, sorted.
func forwardableHeaders(request model.RequestConfig) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range request.ForwardHeaders {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name != "" && !unforwardable(name) && !seen[name] {
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
		if len(key) <= len(headerParamPrefix) || !strings.EqualFold(key[:len(headerParamPrefix)], headerParamPrefix) {
			continue
		}
		name := http.CanonicalHeaderKey(strings.TrimSpace(key[len(headerParamPrefix):]))
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

// utf8Repairs is what a transform repaired for invalid UTF-8: how many label
// values and help texts, and the first metric.
type utf8Repairs struct {
	count uint64
	first string
}

// noteUTF8Repairs counts and logs what the transform repaired for invalid
// UTF-8 (transform.Transform repairs it before labels are mapped and
// truncated), so the one scrape still reaches Prometheus and the problem is
// still seen.
func (s *Server) noteUTF8Repairs(ctx context.Context, repaired utf8Repairs, rec statsRecorder, c *model.Collector, target, keyTarget string) {
	key := failureKey(c.Name, keyTarget, "\x00utf8")
	changed, first := repaired.count, repaired.first
	if changed == 0 {
		s.tripRecovered(ctx, key, "output is valid UTF-8 again", "collector", c.Name, "target", target)
		return
	}
	rec.update(func(x *serverStats) { x.invalidUTF8 += changed })
	s.tripFailed(ctx, slog.LevelWarn, key, "label values or help text were not valid UTF-8; the invalid bytes were replaced with U+FFFD. If the target uses another encoding without declaring it, set response.charset", "utf8", nil, "collector", c.Name, "target", target, "values", changed, "first_metric", first)
}
