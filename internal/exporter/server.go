package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
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
	// debug is the level of --web.debug (debuglevel.go); debugSlots bounds
	// the debug page's runs in progress (debugpage.go), and debugRunCounts
	// counts every run of the debug engine.
	debug      DebugLevel
	debugSlots chan struct{}
	// debugOTLPTesting is set while the debug page's OTLP endpoint test is
	// in progress, which one at a time may be (debugotlptest.go).
	debugOTLPTesting atomic.Bool
	// debugDangerous is --web.debug.dangerous: the playground may run
	// Python, name the server's files and make its own request
	// (debuglevel.go).
	debugDangerous bool
	// fetchCollector makes a trip's request: fetch.FetchCollector, which the
	// debug page's tests wrap to see whose request block every run makes,
	// since the playground's must make none (debugpage.go).
	fetchCollector func(ctx context.Context, target string, c *model.Collector, overrides fetch.RequestOverrides, forwarded http.Header) (*fetch.HTTPResponse, error)
	// statusRefresh is the status page's auto-refresh when its URL does
	// not say, --web.status-refresh, 0 for off (statusrefresh.go).
	statusRefresh     time.Duration
	debugRunCounts    debugRuns
	staticMu          sync.Mutex
	staticResults     map[string]model.MetricSet
	staticFetched     map[string]time.Time
	staticLastSuccess map[string]time.Time
	// staticStatus is how each target's last scrape went, by name, for the
	// status page (statictargetstatus.go), under staticMu.
	staticStatus map[string]*staticTargetStatus
	// recent is the recent probes the status page shows (recentprobes.go).
	recent *recentProbes
	// staticGeneration counts the results published, under staticMu, and
	// staticView is the endpoint's merge of them, rebuilt when a result is
	// published or the targets change (statictargetsendpoint.go).
	staticGeneration uint64
	staticViewMu     sync.Mutex
	staticView       *staticTargetsView
	// staticClashes are the targets' metrics the last read of the endpoint
	// left out for their type (statictargetsendpoint.go).
	staticClashMu sync.Mutex
	staticClashes map[subjectKey]staticClash
	// timeoutOffset is how much of Prometheus's scrape timeout a probe leaves
	// unused (scrapetimeout.go).
	timeoutOffset time.Duration
	// answerWriteTimeout is how long a client has to read an answer
	// (middleware.go): AnswerWriteTimeout, but for tests.
	answerWriteTimeout time.Duration
	// compression gzips the answers of /probe, the self-metrics and the
	// static targets at --web.gzip-level (middleware.go).
	compression *answerCompression
	// answers counts the bytes of those answers, before and after
	// compression (answermetrics.go).
	answers answerCounters
	// defaultProbeTimeout bounds a probe without a scrape timeout
	// (scrapetimeout.go).
	defaultProbeTimeout time.Duration
	statsMu             sync.Mutex
	stats               map[string]*serverStats
	otlpMu              sync.Mutex
	otlpPending         map[string]*otlpBatch
	// otlpPoints counts the pending data points. otlpSeq orders them by age
	// for otlp.max_pending_points: each queued point takes the next number
	// and keeps it when a failed export queues it again. So the oldest
	// points are dropped first, however many exports have failed.
	otlpPoints int
	otlpSeq    int64
	// otlpStarts remembers when each cumulative series exported over OTLP
	// started (otlpstart.go).
	otlpStarts *otlpStartTimes
	cache      *responseCache
	requests   *requestTracker
	flights    *probeFlights
	// trips bounds each collector's trips to its targets (triplimit.go).
	trips *tripLimiter
	// backoffs keeps the targets failure_backoff keeps trips from
	// (backoff.go).
	backoffs *failureBackoffs
	// chains are the last trips of the chained collectors and static
	// targets, which the status page draws (chainreport.go).
	chains chainTrips
	// lastSuccesses keeps when each trip whose clock placeholders read
	// ~last_success last succeeded (lastsuccess.go).
	lastSuccesses lastSuccesses
	// load adds up the time spent on static target scrapes and probes
	// (loadmetrics.go).
	load loadCounters
	// lifecycle enables POST /-/reload (lifecycle.go).
	lifecycle bool
	// otlp is how exports are going (otlp.go).
	otlp *otlpStatus
	// otlpLogs is the tee the exporter's log goes through, nil unless the
	// exporter set one (SetOTLPLogs), and otlpLogsLogger the logger beneath
	// it, to stderr alone, that the log export logs to (otlplogs.go).
	otlpLogs       *OTLPLogs
	otlpLogsLogger *slog.Logger
	// otlpTraces is the trace export's queue (otlptraces.go).
	otlpTraces *otlpTraces
	// installs wakes the OTLP export loops when a configuration is put in
	// force, so they wait by its intervals (otlpEvery).
	installs installSignal
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
	// answering and probing count the requests being answered, and the
	// probes among them: what a shutdown waits for (InProgress).
	answering, probing atomic.Int64
	// followed is the configuration that the per-collector state was last
	// reconciled with, together with the static target file in force with
	// it and its generation (reconcile.go). It is replaced under statsMu,
	// together with the state it describes, and under staticMu.
	followed atomic.Pointer[followedConfig]
	// appliedGeneration is the generation of the last configuration whose
	// connections and rate limits the request types follow (applyFollowed),
	// under appliedMu.
	appliedMu         sync.Mutex
	appliedGeneration uint64
	// since says, for each collector of the followed configuration, the
	// generation it has been there from, under statsMu: the statistics kept
	// for it are those of that stay, and no others' (statsSince).
	since map[string]uint64
	// awaitingReason is what /probe answers while the manager awaits its
	// first configuration, and allocatorStatus the source of the allocator
	// self-metrics, nil outside allocator mode (allocatormode.go).
	awaitingReason  string
	allocatorStatus func() AllocatorStatus
}

// NewServer returns a server using the configuration m holds and running
// Python scripts with the interpreter at p.
func NewServer(m *config.Manager, p string, l *slog.Logger) *Server {
	s := &Server{manager: m, pythonPath: p, logger: l, stats: map[string]*serverStats{}, otlpPending: map[string]*otlpBatch{}, cache: newResponseCache(), requests: newRequestTracker(), flights: newProbeFlights(), recent: newRecentProbes(), trips: newTripLimiter(), backoffs: newFailureBackoffs(), otlp: &otlpStatus{}, otlpTraces: &otlpTraces{}, otlpStarts: newOTLPStartTimes(), failures: newFailureLog(), fingerprints: &fingerprintMemo{}, timeoutOffset: DefaultTimeoutOffset, defaultProbeTimeout: DefaultProbeTimeout, answerWriteTimeout: AnswerWriteTimeout, compression: newAnswerCompression(DefaultGzipLevel), fetchCollector: fetch.FetchCollector}
	// The collectors the exporter starts with count from its start; one a
	// reload adds, from when its statistics are made (selfcreated.go).
	cfg, targets := m.InForce()
	s.statsMu.Lock()
	first := s.followLocked(planFollowing(nil, cfg, targets, s.fingerprints), nil)
	if cfg != nil {
		for i := range cfg.Collectors {
			stats := newServerStats(exporterStart())
			stats.since = first.generation
			s.stats[cfg.Collectors[i].Name] = stats
		}
	}
	s.statsMu.Unlock()
	// The entries of rate_limits are in force from the start, counting
	// since it; a reload's are followed with its collectors (reconcile).
	var hostRateLimits []model.HostRateLimit
	var secretStores []model.SecretStore
	if cfg != nil {
		hostRateLimits = cfg.RateLimits
		secretStores = cfg.SecretStores
	}
	fetch.FollowHostRateLimits(hostRateLimits)
	// So are the secret stores, which the first scrape may read from.
	fetch.FollowSecretStores(secretStores)
	// And the states of the scripts are those of its collectors.
	var collectors []model.Collector
	if cfg != nil {
		collectors = cfg.Collectors
	}
	transform.FollowPythonStates(collectors)
	// The per-collector state follows a reload when the reload is made
	// (reconcile.go). The slow parts of following it are worked out before
	// the reload puts its configuration in force (prepareReload).
	// This is asked for once statsMu is released: the manager calls it under
	// the lock that serializes reloads, and the following takes statsMu
	// under that lock.
	m.OnPrepare(s.prepareReload)
	m.OnInstall(s.followReload)
	// A reload that switches the trace export off drops what waits for it.
	m.OnInstall(func() { s.otlpTraces.configure(s.manager.Get().OTLP) })
	// The OTLP export loops wait by the intervals of the configuration in
	// force, the one a reload put in force included.
	m.OnInstall(s.installs.installed)
	// What a reload changed, which it logs, is told by the same fingerprints
	// (keptFingerprint).
	m.SetFingerprint(s.keptFingerprint)
	return s
}

// DefaultSelfMetricsPath is where the exporter serves its own metrics unless
// --web.self-metrics-path says otherwise.
const DefaultSelfMetricsPath = "/self-metrics"

// endpointPathRE matches a path of plain segments, for the self-metrics and
// static targets paths. It allows no query, fragment, trailing slash or
// ServeMux wildcard, since any of these would make the endpoint something
// other than one fixed path. It also allows no segment of dots alone:
// ServeMux cleans a request's path before routing it, so /metrics/.. is
// requested as / and an endpoint registered there is never reached.
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
	case "/probe", "/health", "/ready", "/collectors", "/-/reload", debugPagePath, debugRunPath, debugCollectorPath, debugOTLPTestPath:
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

// probeConfigReadHook, set by tests, is called with a probe's collector when
// the probe has read the configuration and is about to take the collector's
// statistics, so a test can put a reload between the two.
var probeConfigReadHook atomic.Pointer[func(string)]

// statsFor returns the statistics of the collector name in the configuration
// in force. It is for readers of the statistics, and for callers that name a
// collector of the current configuration. A probe or scrape may have read its
// collector some time ago, so it says when (statsSince).
func (s *Server) statsFor(name string) *serverStats {
	return s.statsSince(s.reconcile().generation, name)
}

// statsSince returns the statistics that a probe or scrape of the collector
// name counts in, when it read the collector in the configuration followed at
// generation (reconcile). They are the collector's own while it has been in
// every configuration followed since that one, and are made at first use. A
// collector that a reload removed since then has none, even if it is back
// under the name. Its statistics were dropped. Those now kept under the name,
// or made at its next use, belong to the collector brought back, which starts
// from zero and counts nothing begun before its return. The caller then gets
// statistics of its own, retired from the start, that no answer shows and
// nothing inherits.
//
// A caller that asked later, by name alone, could not tell its collector from
// one brought back since. If its collector was dropped between reading the
// configuration and asking, it would make statistics for a collector that is
// gone: its request would be shown until it expired, and a collector brought
// back would inherit what it counted.
func (s *Server) statsSince(generation uint64, name string) *serverStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.statsSinceLocked(generation, name)
}

// statsSinceLocked is statsSince under statsMu.
func (s *Server) statsSinceLocked(generation uint64, name string) *serverStats {
	return s.statsSinceLockedWith(generation, name, nil)
}

// statsSinceLockedWith is statsSinceLocked, given fresh, statistics made
// before the lock was taken, to use for those it would make. fresh nil, it
// makes them itself.
func (s *Server) statsSinceLockedWith(generation uint64, name string, fresh *serverStats) *serverStats {
	x := s.stats[name]
	if x != nil && x.since <= generation {
		return x
	}
	if fresh == nil {
		fresh = newServerStats(time.Now())
	}
	if x == nil {
		if since, named := s.since[name]; named && since <= generation {
			fresh.since = since
			s.stats[name] = fresh
			return fresh
		}
	}
	fresh.retired.Store(true)
	return fresh
}

// statsFoundLocked is the statistics statsSinceLocked would return for
// name, nil where it would make some: then the caller makes them without
// the lock, and hands them to statsSinceLockedWith.
func (s *Server) statsFoundLocked(generation uint64, name string) *serverStats {
	if x := s.stats[name]; x != nil && x.since <= generation {
		return x
	}
	return nil
}

// Handler routes the exporter's endpoints: the landing page at /, the
// collectors page at /collectors, /probe, the self-metrics path, the static
// targets path and its status page, /health, /ready and /-/reload. With
// --web.debug on, it also routes /debug, /debug/run and /debug/collector.
// Whatever answers, its client has AnswerWriteTimeout to read the answer
// (middleware.go).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ready", s.readyHandler)
	// Every endpoint but /health and /ready refuses what a page of another
	// site makes a browser send (crosssite.go), with Basic Auth on or off.
	protected := func(handler http.HandlerFunc) http.HandlerFunc { return sameOriginOnly(s.basicAuthMiddleware(handler)) }
	// The answers Prometheus scrapes are gzipped when it asks (middleware.go).
	// Their bytes are counted there too, before and after compression.
	mux.HandleFunc(s.selfMetricsEndpoint(), s.compression.wrap(&s.answers[answerSelfMetrics], protected(s.metricsHandler)))
	mux.HandleFunc(s.staticTargetsEndpoint(), s.compression.wrap(&s.answers[answerStaticTargets], protected(s.staticTargetsHandler)))
	mux.HandleFunc(s.staticTargetsStatusEndpoint(), protected(s.staticTargetsStatusHandler))
	mux.HandleFunc("/probe", s.compression.wrap(&s.answers[answerProbe], protected(s.probeHandler)))
	mux.HandleFunc("/-/reload", protected(s.reloadHandler))
	// Only / itself: any other unknown path is still a 404.
	mux.HandleFunc("GET /{$}", protected(s.landingHandler))
	mux.HandleFunc("GET /collectors", protected(s.collectorsHandler))
	// With --web.debug off the debug page's paths are not found, as any
	// other path the exporter does not serve.
	if s.debug != DebugOff {
		mux.HandleFunc("GET "+debugPagePath, protected(s.debugPageHandler))
		mux.HandleFunc(debugRunPath, protected(s.debugRunHandler))
		mux.HandleFunc(debugCollectorPath, protected(s.debugCollectorHandler))
		mux.HandleFunc(debugOTLPTestPath, protected(s.debugOTLPTestHandler))
	}
	return s.countInProgress(s.writeBounded(mux))
}

// readOnly reports whether r is a GET or a HEAD. Endpoints that only read
// (/probe, the static targets, the self-metrics) serve only those. Any other
// method is answered 405, with the allowed methods and refusal, which says
// what to use.
func readOnly(w http.ResponseWriter, r *http.Request, refusal string) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, refusal, http.StatusMethodNotAllowed)
	return false
}

func (s *Server) probeHandler(w http.ResponseWriter, r *http.Request) {
	// A probe only reads; HEAD runs it in full, as net/http answers HEAD with
	// GET's status and headers. Anything else is refused before the target
	// is contacted or the probe counted.
	if !readOnly(w, r, "use GET or HEAD to probe a target") {
		return
	}
	start := time.Now()
	// A debug probe is refused before anything else when it is not enabled,
	// so the refusal does not depend on the rest of the probe being right.
	query := r.URL.Query()
	debug, err := probeDebugRequested(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if debug && !s.debugServes(debugKindProbe) {
		http.Error(w, s.debugReportsRefused(), http.StatusForbidden)
		return
	}
	p, err := s.readProbe(query)
	if err != nil {
		// A debug probe refused for what it asked is counted invalid; one
		// that is served counts as its trip ended (serveDebugProbe).
		if debug {
			s.countDebugRun(debugKindProbe, debugInvalid)
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if debug {
		s.serveProbeDebug(w, r, p)
		return
	}
	if hook := probeConfigReadHook.Load(); hook != nil {
		(*hook)(p.name)
	}
	run := s.startProbe(r, p, start)
	if run.answeredFromCache(w) {
		return
	}
	run.budget, run.budgetSource = s.probeDeadline(r.Header, p.overrides)
	if !coalesceProbes(p.c) || run.key == "" {
		// No key, when the collector's definition could not be
		// fingerprinted, is no identity to share a trip by: every such
		// probe would share one.
		run.alone(w)
		return
	}
	run.shared(w)
}

// probeQuery is what a probe's query asks for, read and checked.
type probeQuery struct {
	followed     *followedConfig
	cfg          *model.Config
	c            *model.Collector
	name, target string
	logTarget    string
	overrides    fetch.RequestOverrides
	requestURL   string
	method       string
}

// readProbe reads and checks a probe's query. Its error is the caller's
// mistake, answered 400 before the target is contacted or anything is
// counted.
func (s *Server) readProbe(query url.Values) (*probeQuery, error) {
	// One target and one collector, neither longer than a probe parameter may
	// be (probeparams.go): nothing below reads a second value, and nothing
	// below is then given a name too long to remember.
	if err := checkProbeParams(query, probeIdentityParam); err != nil {
		return nil, err
	}
	p := &probeQuery{target: query.Get("target"), name: query.Get("collector")}
	if p.name == "" {
		return nil, errors.New("the collector parameter is required: /probe?collector=<name>&target=<target>")
	}
	// The configuration is read with its generation, which tells below which
	// statistics the probe counts in (statsSince). The collector is found by
	// its name, where the followed configuration keeps its position
	// (collectorOf), so its collectors are not scanned for every probe.
	p.followed = s.reconcile()
	p.cfg = p.followed.config
	c := p.followed.collectorOf(p.cfg, p.name)
	if c == nil {
		return nil, fmt.Errorf("unknown collector %q", p.name)
	}
	p.c = c
	// Whether a target is needed, and what it may be, is the request type's
	// to say: http needs a URL, localfile a file under its root, or nothing.
	if err := fetch.CheckTarget(c, p.target, false); err != nil {
		if errors.Is(err, fetch.ErrMissingTarget) {
			return nil, fmt.Errorf("the target parameter is required for collector %q, whose request.type is %s", p.name, c.Request.Type)
		}
		return nil, err
	}
	p.logTarget = fetch.DisplayTarget(c, p.target)
	// The same holds for the parameters that change the request: each once,
	// but for header_<name>, and none over-long.
	if err := checkProbeParams(query, probeRequestParam(c)); err != nil {
		return nil, err
	}
	// The request parameters are resolved before anything is counted, so every
	// counter this probe raises lands on the individual request as well as on
	// the collector rather than only on the collector.
	var err error
	if p.overrides, err = fetch.ParseRequestOverrides(query); err != nil {
		return nil, err
	}
	// Each request type accepts its own probe parameters; one that belongs to
	// another type is a mistake, reported before anything else happens.
	if err := fetch.CheckOverrideParams(c, query); err != nil {
		return nil, err
	}
	// A forwarded header_<name> value Go would refuse to send is the
	// caller's mistake too, not the target's failure (probeparams.go).
	if err := checkForwardedHeaderParams(query, c.Request); err != nil {
		return nil, err
	}
	// A path parameter the scrape did not supply and that has no default is the
	// caller's mistake, reported before the target is contacted or anything is
	// counted against it.
	if err := fetch.CheckPathParams(c, p.overrides); err != nil {
		return nil, err
	}
	p.method = fetch.RequestMethodFor(c, p.overrides)
	if label, labelErr := fetch.RequestLabelFor(p.target, c, p.overrides); labelErr == nil {
		p.requestURL = label
	}
	return p, nil
}

// serveProbeDebug serves a debug probe: counted nowhere, cached nowhere,
// shared with nobody (probedebug.go).
func (s *Server) serveProbeDebug(w http.ResponseWriter, r *http.Request, p *probeQuery) {
	c := p.c
	forwarded := forwardedHeaders(r, c.Request)
	budget, budgetSource := s.probeDeadline(r.Header, p.overrides)
	d := debugProbe{
		upstreamProbe: upstreamProbe{
			collector: c, target: p.target, logTarget: p.logTarget, overrides: p.overrides,
			forwarded: forwarded, budget: budget, budgetSource: budgetSource,
		},
		requestURL: p.requestURL, method: p.method, generation: p.followed.generation, kind: debugKindProbe,
	}
	if model.UsesCache(c) || c.FailureBackoff != nil {
		// The key the same probe without debug has: debug is no
		// parameter of the request (probeKeyQuery).
		key := s.probeCacheKey(p.cfg, c, p.target, probeKeyQuery(p.name, p.target, p.overrides), forwarded)
		if model.UsesCache(c) {
			d.staleKey = key
		}
		if c.FailureBackoff != nil {
			d.backoffKey = key
		}
	}
	s.serveDebugProbe(w, r, d)
}

// probeRun is a probe being answered, once its query has been read.
type probeRun struct {
	s *Server
	*probeQuery
	r     *http.Request
	start time.Time
	rec   statsRecorder
	ctx   context.Context
	// parent is the caller's trace, and spans the probe's own, nil but
	// while otlp.traces is on.
	parent traceParent
	spans  *tripSpans
	// traced is how the probe's trace ends.
	traced    probeTraceEnd
	forwarded http.Header
	keyQuery  url.Values
	// key is the probe's identity: the cache's and the shared trip's.
	key string
	// cacheKey is key when the collector caches its results.
	cacheKey string
	// budget is how long the trip may take, and budgetSource what said so.
	budget       time.Duration
	budgetSource string
}

// startProbe counts the probe and begins its trace.
func (s *Server) startProbe(r *http.Request, p *probeQuery, start time.Time) *probeRun {
	run := &probeRun{s: s, probeQuery: p, r: r, start: start}
	st := s.statsSince(p.followed.generation, p.name)
	run.rec = s.probeRecorderFor(st, p.name, p.requestURL, p.method)
	// What the probe writes under the collector's name at its end goes
	// nowhere once a reload has removed or changed the collector read here
	// (configRead).
	run.rec.read = s.readAt(p.followed.generation)
	run.rec.update(func(x *serverStats) { x.probes++ })
	// With otlp.traces on, the probe records its trace. The trace is exported
	// at the end when a sampling rule keeps it (otlptracesampling.go). A
	// caller's traceparent makes the probe a span of the caller's trace; its
	// sampled flag and tracestate are read for the rules and the spans.
	if otlpTracesOn(p.cfg.OTLP) || p.cfg.OTLP.Traces.Propagate {
		run.parent = callerTrace(r.Header)
	}
	run.ctx, run.spans = beginTrace(r.Context(), p.cfg.OTLP, run.parent)
	// With otlp.traces.propagate, the trip's requests carry the probe's
	// trace, or with the trace export off the caller's as it came.
	run.ctx = withPropagation(run.ctx, p.cfg.OTLP, run.spans.traceLink(), run.parent, r.Header)
	run.forwarded = forwardedHeaders(r, p.c.Request)
	run.keyQuery = probeKeyQuery(p.name, p.target, p.overrides)
	run.key = s.probeCacheKey(p.cfg, p.c, p.target, run.keyQuery, run.forwarded)
	if model.UsesCache(p.c) {
		run.cacheKey = run.key
	}
	return run
}

// finish counts the probe's outcome and ends its per-request record. A
// probe that the target policy refused, or whose caller went away, leaves
// no new tracked request. finish is called once the answer is ready, before
// it is written. It also ends the probe's trace then, as traced says. The
// time a caller takes to read the answer is not part of the probe's
// duration or its span's. It returns the probe's duration, which
// report_failures answers (probereport.go).
func (run *probeRun) finish(ok, refused bool) time.Duration {
	ready := time.Now()
	duration := ready.Sub(run.start)
	if run.spans != nil {
		run.s.endProbeTrace(run.cfg.OTLP, run.spans, run.c, run.logTarget, run.r.Method, run.start, ready, run.parent, run.traced)
	}
	run.s.load.addProbe(duration)
	// Into the collector's statistics alone, as the trip histogram's: a
	// request's has no histogram.
	observeProbe(run.rec.collector, duration, run.spans.traceLink())
	run.rec.update(func(x *serverStats) {
		if ok {
			x.success++
		}
		x.lastDuration = duration.Seconds()
	})
	run.rec.commit(refused)
	return duration
}

// recentKey is what the status page records the probe under, made from the
// probe's key once it is known to be answered.
func (run *probeRun) recentKey() recentProbeKey {
	return run.s.recent.keyOf(run.key, run.c, run.logTarget, run.keyQuery)
}

// answeredFromCache answers the probe from the response cache, and says
// whether it did.
func (run *probeRun) answeredFromCache(w http.ResponseWriter) bool {
	s, c := run.s, run.c
	if model.CacheTTL(c) <= 0 {
		return false
	}
	mark := time.Now()
	cached, fetched, ok := s.cache.Get(run.cacheKey, mark)
	if !ok {
		// A miss is counted when the trip goes to the target (probeTrip);
		// a probe that shares another's trip is counted as coalesced.
		run.spans.step("cache", "miss", mark, time.Now(), "")
		return false
	}
	run.rec.update(func(x *serverStats) {
		x.cacheHits++
		x.emitted += uint64(len(cached.Metrics))
	})
	// The names were checked before the result was stored.
	answer, _ := withFreshness(cached, c, false, fetched, time.Now())
	hit := &probeResult{status: http.StatusOK, metrics: &answer, ok: true, outcome: probeCached}
	series := len(answer.Metrics)
	if reportsProbe(c, hit) {
		series = reportedSeries(hit)
	}
	if run.spans != nil {
		run.spans.step("cache", "hit", mark, time.Now(), "answered from the response cache, fetched "+fetched.UTC().Format(time.RFC3339))
		run.traced = probeTraceEnd{status: http.StatusOK, series: series, cached: true}
	}
	elapsed := run.finish(true, false)
	s.noteProbe(run.rec.read, run.recentKey(), recentProbe{
		at: run.start, duration: time.Since(run.start), result: probeCached,
		status: http.StatusOK, series: series,
	})
	writeProbeAnswer(w, run.r, c, hit, elapsed, transform.UTF8Names(c))
	s.queueProbeOTLP(answer, c, run.logTarget, fetched)
	return true
}

// upstream makes the probe's trip to the target, counted on trip.
func (run *probeRun) upstream(ctx context.Context, trip statsRecorder) *probeResult {
	return run.s.probeUpstream(ctx, upstreamProbe{
		collector: run.c, target: run.target, logTarget: run.logTarget, overrides: run.overrides,
		forwarded: run.forwarded, rec: trip, cacheKey: run.cacheKey,
		budget: run.budget, budgetSource: run.budgetSource,
		// Probes of one target that differ in their parameters or
		// forwarded headers (tenants, paths) fail separately in the
		// log. The collector name is the configuration's, which the
		// probe's matches. The name read from the query is part of the
		// request line, and the failure log entry, kept for an hour,
		// would keep it whole, target and all.
		failure: probeFailureKey(run.c.Name, run.target, run.key), requestURL: run.requestURL,
		// A probe backs off by the identity it shares a trip by.
		backoffKey: run.key,
	})
}

// alone answers the probe by a trip of its own, counted where the probe is.
func (run *probeRun) alone(w http.ResponseWriter) {
	c := run.c
	result := run.upstream(run.ctx, run.rec)
	if run.spans != nil {
		run.traced = probeTraceEndOf(result)
		reportedTrace(c, result, &run.traced)
	}
	// A trip its caller abandoned ends without the target policy's verdict,
	// as a shared one every caller left does: counted on the collector, and
	// no new request tracked.
	elapsed := run.finish(result.ok, result.refused || result.abandoned)
	if !result.abandoned {
		run.s.noteProbe(run.rec.read, run.recentKey(), reportedRecent(c, result, result.recentProbe(run.start, false)))
	}
	writeProbeAnswer(w, run.r, c, result, elapsed, transform.UTF8Names(c))
}

// shared answers the probe by a trip that identical probes in flight share.
func (run *probeRun) shared(w http.ResponseWriter) {
	s, c, spans := run.s, run.c, run.spans
	// A shared trip is counted apart from the probe that happens to start it
	// (forTrip), since that probe may leave while the trip goes on for the
	// others. What the trip counted belongs to the request when it ends,
	// before any probe is answered by it. The exceptions: the target policy
	// refused it, or it was cancelled because every probe left, without the
	// policy's verdict. Only probes of this stay of the collector share it
	// (flightKey): the trip writes under the collector's name, as the probe
	// that started it may.
	trip := run.rec.forTrip()
	mark := time.Now()
	// The trip records its own trace, which the trace of every probe that
	// uses it gets a copy of: the probe that started it may leave before it
	// ends (probeFlights.doTraced).
	result, shared, tripTrace, err := s.flights.doTraced(run.ctx, flightKey{probe: run.key, defined: run.followed.defined[run.name]}, spans != nil, func(ctx context.Context) *probeResult {
		result := run.upstream(ctx, trip)
		trip.commit(result.refused || result.abandoned)
		return result
	})
	if err != nil {
		// This caller went away while it waited. There is nobody to answer
		// and no target policy verdict to go by. The probe starts no tracked
		// request, whatever happens to the trip. Its trace has what the trip
		// had done by then, with the requests still unanswered marked
		// unfinished. The trip goes on for the probes still waiting for it,
		// and their traces have the rest.
		cut := time.Now()
		failure := "the caller went away while the probe waited for an identical probe's trip"
		if shared {
			spans.step("shared_trip", "cancelled", mark, cut, "the caller went away before the trip of an identical probe ended")
			spans.adopt(tripTrace, "shared_trip", cut)
		} else {
			failure = "the caller went away before the probe's trip ended"
			spans.adopt(tripTrace, "", cut)
		}
		run.traced = probeTraceEnd{status: http.StatusServiceUnavailable, failed: true, stage: "abandoned", failure: failure}
		run.finish(false, true)
		return
	}
	if spans != nil {
		run.traced = probeTraceEndOf(result)
		reportedTrace(c, result, &run.traced)
	}
	if shared {
		// The trip was another probe's: its stages and requests are under
		// this probe's shared_trip span.
		spans.step("shared_trip", "ok", mark, time.Now(), "answered by the trip of an identical probe in flight")
		spans.adopt(tripTrace, "shared_trip", time.Time{})
		run.rec.update(func(x *serverStats) { x.coalesced++ })
		s.logger.Debug("probe shared an identical probe in flight", "collector", run.name, "target", run.logTarget)
	} else {
		spans.adopt(tripTrace, "", time.Time{})
	}
	elapsed := run.finish(result.ok, result.refused)
	s.noteProbe(run.rec.read, run.recentKey(), reportedRecent(c, result, result.recentProbe(run.start, shared)))
	writeProbeAnswer(w, run.r, c, result, elapsed, transform.UTF8Names(c))
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
	// failure identifies the probe in the failure log (probeFailureKey): its
	// target plus everything else that makes it the probe it is, parameters
	// and forwarded headers. When not set, it is the target alone.
	// requestURL is its url label, which logs show.
	failure    subjectKey
	requestURL string
	// backoffKey is what failure_backoff tells the target apart by
	// (backoff.go): the probe's cache key, which a static target's scrape
	// of the same request has too. Empty, the trip neither asks nor tells.
	backoffKey string
}

// failureKey is what the failure log tells p apart by.
func (p upstreamProbe) failureKey() subjectKey {
	if p.failure.bytes != "" {
		return p.failure
	}
	return probeFailureKey(p.collector.Name, p.target, "")
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
	c := p.collector
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
	// A target the collector may not reach is not answered from the cache
	// either. The refusal is the answer, whatever an earlier probe left in
	// the cache, even one made before allowed_targets or denied_targets
	// changed or before the target's name resolved elsewhere.
	if result.refused {
		s.logger.DebugContext(ctx, "probe refused by the target policy; no stale result served", p.logAttrs()...)
		return result
	}
	staleKey := p.failureKey().aspect(staleAspect)
	if result.ok {
		s.failures.recoveredFor(ctx, p.rec.read, s.logger, staleKey, "probe answered with a fresh result again", p.logAttrs()...)
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
	s.failures.failedFor(ctx, p.rec.read, s.logger, slog.LevelWarn, staleKey, "probe failed; answered with the last successful result (cache.stale_if_error)", "stale", nil, append(p.logAttrs(), "result_age", now.Sub(fetched).Round(time.Second).String())...)
	out := newProbeRecorder()
	out.metrics = &answer
	s.queueProbeOTLP(answer, c, p.logTarget, fetched)
	if spans := tripSpansFrom(ctx); spans != nil {
		spans.step("stale", "ok", now, time.Now(), "answered with the last good result, "+now.Sub(fetched).Round(time.Second).String()+" old (cache.stale_if_error)")
	}
	// A stale answer is not a success: the trip failed.
	stale := out.result(false)
	stale.outcome, stale.failure, stale.stage, stale.step = probeStale, result.failure, result.stage, result.step
	return stale
}

// probeTrip is one trip to the target, or the fresh cache entry a probe that
// finished while this one waited to start left behind.
func (s *Server) probeTrip(ctx context.Context, p upstreamProbe) *probeResult {
	c, name, rec, logTarget := p.collector, p.collector.Name, p.rec, p.logTarget
	out := newProbeRecorder()
	// A probe that finished while this one waited to start may have just
	// filled the cache. A probe that finds nothing there is a miss once it
	// goes to the target. One that the concurrency limit turns away is a
	// rejection, not a miss, since nothing was fetched in place of a cache
	// entry.
	missed := false
	if p.cacheKey != "" && model.CacheTTL(c) > 0 {
		if cached, fetched, ok := s.cache.Get(p.cacheKey, time.Now()); ok {
			rec.update(func(x *serverStats) {
				x.cacheHits++
				x.emitted += uint64(len(cached.Metrics))
			})
			answer, _ := withFreshness(cached, c, false, fetched, time.Now())
			out.metrics = &answer
			// Queued for OTLP as a hit before the trip is (probeHandler).
			// It is queued once, since the probes sharing this trip queue
			// nothing of their own. The probe that filled the entry queued
			// its own answer, not this one.
			s.queueProbeOTLP(answer, c, logTarget, fetched)
			hit := out.result(true)
			hit.outcome = probeCached
			return hit
		}
		missed = true
	}
	job := collectJob{
		collector: c, target: p.target, overrides: p.overrides, headers: p.forwarded,
		rec: rec, display: logTarget, cacheKey: p.cacheKey, revalidateKey: p.cacheKey, budget: p.budget, budgetSource: p.budgetSource,
		lastSuccess: lastSuccessKey(c, nil, p.target, p.overrides),
		log: collectLog{
			key:    p.failureKey(),
			failed: "probe failed", continuing: "probe stage failed; continuing", recovery: "probe recovered",
			attrs: p.logAttrs(),
		},
	}
	// A target that failure_backoff is backing off from is not contacted.
	// The trip fails at once, as its fetch would have, and takes no slot
	// (backoff.go). A trip it lets through records its outcome. A trip that
	// ends without an outcome (turned away below, or panicking) gives the
	// target's next trip back.
	turn := s.backoffs.admit(c, p.backoffKey, logTarget)
	defer turn.settle(rec.read, backoffUnknown)
	var result collected
	if turn != nil && turn.refusal != nil {
		job.backingOff = turn.refusal
		result = s.collect(ctx, job)
		result.unauthorized = turn.unauthorized
	} else {
		// Answered at once when the collector's backend already has all it
		// may get, rather than queued behind the probes in progress
		// (triplimit.go).
		limit := maxConcurrentProbes(c)
		if full := s.trips.tryAcquire(name, limit); full != nil {
			now := time.Now()
			tripSpansFrom(ctx).step("slot", "refused", now, now, full.message)
			rec.update(func(x *serverStats) { countRejection(x, full) })
			s.failures.failedFor(ctx, rec.read, s.logger, slog.LevelWarn, p.failureKey(), "probe rejected: too many probes in progress", "concurrency", nil, append(p.logAttrs(), "reason", full.message)...)
			http.Error(out, full.message+"; this probe was not sent", http.StatusServiceUnavailable)
			rejected := out.result(false)
			rejected.outcome, rejected.failure = probeRejected, shownFailureText(full.message)
			return rejected
		}
		defer s.trips.release(name)
		if missed {
			rec.update(func(x *serverStats) { x.cacheMisses++ })
		}
		if p.budget > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.budget)
			defer cancel()
		}
		result = s.collect(ctx, job)
		s.settleBackoff(turn, rec.read, result, p.logAttrs())
	}
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
			// The caller's text, as a failed stage's: a metric's error
			// quotes the value of the answer it could not read.
			Error: model.CallerText(result.err),
		})
		failure := out.result(false)
		failure.failure, failure.stage, failure.step = s.shownStatusError(result.err), result.stage, result.step
		return failure
	case result.refused:
		// The caller asked for a target the collector may not reach.
		http.Error(out, fmt.Sprintf("collector %s refused the target: %v", name, result.err), http.StatusForbidden)
		refused := out.result(false)
		refused.refused = true
		refused.outcome, refused.failure = probeRefused, s.shownStatusError(result.err)
		return refused
	case result.failed():
		// What is for the operator alone, such as a remote command's
		// stderr, stays in the log, and with --web.debug=full the debug
		// report and the status page.
		http.Error(out, fmt.Sprintf("collector %s %s failed: %s", name, result.stage, model.CallerText(result.err)), http.StatusBadGateway)
		failure := out.result(false)
		failure.unauthorized = result.unauthorized
		failure.failure, failure.stage, failure.step = s.shownStatusError(result.err), result.stage, result.step
		if job.backingOff != nil {
			failure.outcome = probeBackoff
		}
		return failure
	case result.carriedOn:
		// The stage's policy said to carry on. The probe is answered like a
		// probe whose rules produced no series, by the same writer
		// (writeMetricSet). An exposition is identified by its Content-Type,
		// and an OpenMetrics one ends in # EOF. An answer with neither is in
		// no format a scraper could read, and carrying on exists to prevent
		// that.
		//
		// Under report_failures the probe is a failure, as its answer says
		// (probereport.go). It is counted as one, shown failed with its
		// error on the status page, answered with the last good result
		// under cache.stale_if_error (probeUpstream) and exported as a
		// failed trace. So nothing the exporter says about the probe
		// disagrees with its http_exporter_probe_success 0. It is still
		// logged as its policy says.
		answer := carriedOnAnswer(c)
		out.metrics = &answer
		carried := out.result(!c.ReportFailures)
		carried.stage, carried.step = result.stage, result.step
		if c.ReportFailures {
			carried.carriedOn, carried.unauthorized = true, result.unauthorized
			carried.failure = s.shownStatusError(result.carriedErr)
			if job.backingOff != nil {
				carried.outcome = probeBackoff
			}
		}
		return carried
	}
	out.metrics = &result.answer
	s.queueProbeOTLP(result.answer, c, logTarget, result.fetched)
	return out.result(true)
}

// settleBackoff records what the trip that ended as result says of its
// target (backoff.go), and logs a target that starts to back off with it.
func (s *Server) settleBackoff(turn *backoffTurn, read configRead, result collected, attrs []any) {
	if turn == nil {
		return
	}
	floor := model.DefaultFailureBackoffMinBudget
	if turn.config.MinBudget != nil {
		floor = time.Duration(*turn.config.MinBudget)
	}
	turn.unauthorized = result.unauthorized
	started, failures, until := turn.settle(read, outcomeOf(result, floor))
	if started {
		s.logger.Warn("target is backing off after consecutive failed scrapes; its probes and scrapes fail without contacting it until the wait has passed (failure_backoff)",
			append(append([]any{}, attrs...), "failures", failures, "next_try", until.UTC().Format(time.RFC3339))...)
	}
}

// carriedOnAnswer is the answer for a probe of c that carried on past a failed
// stage, under error_handling log or ignore. It is the answer of a trip that
// went through whole and produced no series: nothing of the collector's, plus
// the freshness series of a collector with cache.stale_if_error, which every
// answer of such a collector has. The answer is not a stale result, and the
// trip was just made. Unlike that trip's result, it is neither cached nor
// exported over OTLP: no result was read from the target, and a failed stage
// is never kept as a good one.
func carriedOnAnswer(c *model.Collector) model.MetricSet {
	now := time.Now()
	// No series, so none whose name the freshness series could clash with.
	answer, _ := withFreshness(model.MetricSet{}, c, false, now, now)
	return answer
}

// unforwardable reports whether name, canonical, is never forwarded, whatever
// request.forward_headers lists (fetch.UnforwardableReason says which and
// why); the configuration refuses such an entry when it loads.
func unforwardable(name string) bool {
	return fetch.UnforwardableReason(name) != ""
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
		if !headerParam(key) {
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

// noteUTF8Repairs counts and logs the invalid UTF-8 that the transform
// repaired (transform.Transform repairs it before labels are mapped and
// truncated). So the scrape still reaches Prometheus and the problem is still
// seen. file is the directory's file the transform was of, or empty. target
// is what the lines name. keyTarget is the address as the failure log tells
// addresses apart.
//
// The first metric is named before the set is validated, so its name is as
// long as the target or a script made it. The line shows it the way an error
// shows a name (model.ShownName), and a name within that bound as it is. The
// name is not part of what the warning is remembered by: that is the
// collector, the address and the file.
func (s *Server) noteUTF8Repairs(ctx context.Context, repaired utf8Repairs, rec statsRecorder, c *model.Collector, target, keyTarget, file string) {
	key := aspectKey(c.Name, keyTarget, file, utf8Aspect)
	changed, first := repaired.count, repaired.first
	if changed == 0 {
		s.tripRecovered(ctx, rec.read, key, "output is valid UTF-8 again", "collector", c.Name, "target", target)
		return
	}
	rec.update(func(x *serverStats) { x.invalidUTF8 += changed })
	s.tripFailed(ctx, rec.read, slog.LevelWarn, key, "label values or help text were not valid UTF-8; the invalid bytes were replaced with U+FFFD. If the target uses another encoding without declaring it, set response.charset", "utf8", nil, "collector", c.Name, "target", target, "values", changed, "first_metric", model.ShownName(first))
}
