package exporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A debug probe, /probe?...&debug=true, makes one trip to the target and
// answers with a plain-text report of it instead of the metrics: the
// requests sent, the response, each stage and how long it took, the series
// each metric got and the rules that carried on without theirs, what was
// logged, and what a probe would have answered. It is for finding out why a
// collector does not give what it should, from a browser or curl.
//
// It shows a target's response, so it answers only with
// --web.enable-probe-debug, and is otherwise refused 403. The target policy,
// the trip limits and web.basic_auth apply as to any probe. A debug probe
// always goes to the target, alone, and leaves nothing behind: it neither
// reads nor fills the response cache, shares no trip with an identical probe,
// records no self-metric, queues nothing for OTLP, and logs its failures to
// its report rather than the exporter's log, where they would take part in
// the failure log's reckoning of what is failing (failurelog.go). Its
// scripts, though, run in the collector's Python workers, whose series count
// what the workers did, a debug probe's runs among them (pythonstats.go). Its
// report always answers 200, whatever the probe would have answered, which it
// says.
//
// The trace rides in the trip's context (probeTraceFrom), so the stages of
// collect, several frames down, record into it; a trip without one records
// nothing and logs as it always does (tripFailed, tripRecovered, tripDebug).
//
// Credentials are kept out of the report: request and response header values
// under a name that reads as a credential, every query value, a URL's
// userinfo, and request bodies are never shown (fetch/redact.go).

// probeDebugParam turns a probe into a debug probe.
const probeDebugParam = "debug"

// debugBodyLimit is how much of the response body a report shows.
const debugBodyLimit = 64 << 10

// What a report shows is the target's to make long, and a script's: the
// request a redirect led to is for the URL its Location named, most of a
// megabyte of it; a response header's value, and the message of a gRPC
// status the collector accepts, are as long as the target sends them, and
// the headers as many; a metric's name is as long as the response. The
// report wrote each whole, in one line. It is read by a person, in a browser
// or a terminal, and is made in memory whole, so beside the body, which has
// its bound (debugBodyLimit), no line of it is longer than debugLineLimit:
// a longer one is shown by its start and its length, as a failure's text is
// (model.CutMark). That is done to the lines once they are written,
// whatever section wrote them and whatever they are made of (cutLongLines),
// and costs a report without such a line the reading of it. Four things are
// bounded before it, where the line says more after them, where the length
// that tells the reader something is theirs and not the line's, or where
// there is no end of them:
//
//   - the URL of a request is shown as a failure shows one, by its first
//     debugURLLimit bytes and its length, and how the request ended is said
//     after it;
//   - a metric's name in the Transform section is shown as a failure shows
//     one, by its first debugNameLimit bytes and its length, and its count
//     is said after it, so the section is no longer than limits.max_metrics
//     such lines;
//   - a header's value is shown by its first debugHeaderValueLimit bytes
//     and its length, and is not copied whole to be cut;
//   - of the headers of a request or a response the first debugHeaderLimit
//     are listed, and then how many there are.
//
// What is withheld is withheld before anything is cut, so a cut shows
// nothing redaction hides, and none ends inside the <redacted> that stands
// for a credential, where the part shown would read as the start of one
// (shownHead).

// debugLineLimit is how long a line of a report may be, the body's aside.
// It is past every line whose parts the exporter bounds already, which are
// not to be cut twice: a failure's text, 2,000 bytes with its own length at
// its end (model.MaxFailureBytes), in the sentence or the log line that
// quotes it, and the log line of what a script printed, the first 4,096
// characters of it.
const debugLineLimit = 8 << 10

// debugURLLimit and debugNameLimit are how much of a request's URL and of a
// metric's name a report shows: what a failure's text shows of one
// (fetch/urlcut.go, model.Validate).
const (
	debugURLLimit  = 512
	debugNameLimit = 200
)

// debugHeaderValueLimit is how much of a header's value a report shows, and
// debugHeaderLimit how many headers of a request or a response it lists:
// several times what an API sends, and a small part of what fits in the
// megabyte a response's headers may take.
const (
	debugHeaderValueLimit = 1 << 10
	debugHeaderLimit      = 100
)

// SetProbeDebug enables debug probes, --web.enable-probe-debug.
func (s *Server) SetProbeDebug(enabled bool) { s.probeDebug = enabled }

// probeDebugRequested reads the debug parameter: absent or false is a
// normal probe.
func probeDebugRequested(query url.Values) (bool, error) {
	values, given := query[probeDebugParam]
	if !given {
		return false, nil
	}
	value := strings.TrimSpace(values[len(values)-1])
	if value == "" {
		return true, nil
	}
	debug, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("the %s parameter must be true or false, not %q", probeDebugParam, value)
	}
	return debug, nil
}

// probeTrace is what a debug probe's trip recorded. response is the
// response as the target sent it (sentResponse), and convertedFrom the
// encoding its body was converted from before it was decoded, empty when it
// was not converted.
type probeTrace struct {
	mu            sync.Mutex
	logs          bytes.Buffer
	logger        *slog.Logger
	requests      *fetch.RequestTrace
	steps         []traceStep
	response      *fetch.HTTPResponse
	convertedFrom string
	decoded       string
	transform     *model.MetricSet
	failures      []transform.RuleFailure
}

// sentResponse is a copy of a response as it was fetched, for a report to
// show after the decode has converted the response's body to UTF-8 and
// rewritten its Content-Type to say so (decode/textencoding.go). The decode
// replaces the body rather than changing its bytes, so the copy shares them:
// only a trip that is reported keeps the body the target sent, and only for
// a body that was converted is that a second one. A directory's files are
// copied the same way.
func sentResponse(r *fetch.HTTPResponse) *fetch.HTTPResponse {
	if r == nil {
		return nil
	}
	sent := *r
	sent.Headers = r.Headers.Clone()
	if r.Directory != nil {
		directory := *r.Directory
		directory.Files = slices.Clone(r.Directory.Files)
		for i, f := range directory.Files {
			directory.Files[i].Response = sentResponse(f.Response)
		}
		sent.Directory = &directory
	}
	return &sent
}

// traceStep is one stage of the trip.
type traceStep struct {
	stage, outcome, note string
	took                 time.Duration
}

type probeTraceKey struct{}

// newProbeTrace returns a trace, and a context carrying it and the request
// trace it reads the requests from.
func newProbeTrace(ctx context.Context) (context.Context, *probeTrace) {
	t := &probeTrace{}
	t.logger = slog.New(slog.NewTextHandler(lockedWriter{t}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	ctx, t.requests = fetch.WithRequestTrace(ctx)
	ctx = transform.WithRuleLogger(ctx, t.logger)
	return context.WithValue(ctx, probeTraceKey{}, t), t
}

// probeTraceFrom is the trace of the trip ctx is for, nil for a trip that is
// not a debug probe's.
func probeTraceFrom(ctx context.Context) *probeTrace {
	t, _ := ctx.Value(probeTraceKey{}).(*probeTrace)
	return t
}

// lockedWriter writes to a trace's log buffer under its lock.
type lockedWriter struct{ t *probeTrace }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	return w.t.logs.Write(p)
}

// step records a stage; t may be nil.
func (t *probeTrace) step(stage, outcome string, took time.Duration, note string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, traceStep{stage: stage, outcome: outcome, note: note, took: took})
}

// record applies change to the trace under its lock; t may be nil.
func (t *probeTrace) record(change func(*probeTrace)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	change(t)
}

// converted records the encoding the decode converted the response's body
// from, which the report says beside the body the target sent; t may be nil.
// decoded is what the decode gave, nil when it failed, where the encoding is
// the one the collector's own decoder type would have had converted.
func (t *probeTrace) converted(c *model.Collector, decoded *decode.Decoded) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.response == nil {
		return
	}
	kind := c.Decoder.Type
	if decoded != nil {
		kind = decoded.Kind
	}
	t.convertedFrom = decode.ConvertedFrom(t.response, c, kind)
}

// tripFailed logs a failure of a trip: to the failure log, or for a debug
// probe to its report alone. read is the configuration the trip read its
// collector in: the failure log remembers nothing of a collector that no
// longer stands (failedFor).
func (s *Server) tripFailed(ctx context.Context, read configRead, level slog.Level, key subjectKey, msg, stage string, err error, attrs ...any) {
	if t := probeTraceFrom(ctx); t != nil {
		t.failed(ctx, level, msg, err, attrs...)
		return
	}
	s.failures.failedFor(read, s.logger, level, key, msg, stage, err, attrs...)
}

// failed logs a failure of a debug probe's trip to its report, which is
// under no key: the failure log is told nothing of it.
func (t *probeTrace) failed(ctx context.Context, level slog.Level, msg string, err error, attrs ...any) {
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	t.logger.Log(ctx, level, msg, attrs...)
}

// ruleFailed is tripFailed for the failure of a rule of the trip or file
// whose key is trip, the rule told apart by its metric name, its expression
// and its items: the failure log is told which trip the rule is of, and
// does not read it out of a key, and makes the rule's key itself, when it
// first remembers the failure (ruleFailedFor). What is returned is the
// bytes of the key a failure of the rule is remembered under, and nothing
// when none is. A debug probe's failure is remembered nowhere, so for one
// it is the key an earlier scrape's is remembered under, which is asked
// for without a key (rememberedRule).
func (s *Server) ruleFailed(ctx context.Context, read configRead, level slog.Level, trip subjectKey, metric, expression, items, msg, stage string, err error, attrs ...any) string {
	if t := probeTraceFrom(ctx); t != nil {
		key, _ := s.failures.rememberedRule(trip, metric, expression, items)
		t.failed(ctx, level, msg, err, attrs...)
		return key
	}
	return s.failures.ruleFailedFor(read, s.logger, level, trip, metric, expression, items, msg, stage, err, attrs...)
}

// tripRecovered is tripFailed's recovery: a debug probe has nothing to
// recover from, and says nothing.
func (s *Server) tripRecovered(ctx context.Context, read configRead, key subjectKey, msg string, attrs ...any) {
	if probeTraceFrom(ctx) != nil {
		return
	}
	s.failures.recoveredFor(read, s.logger, key, msg, attrs...)
}

// tripDebug logs at debug level, to a debug probe's report when it is one.
func (s *Server) tripDebug(ctx context.Context, msg string, attrs ...any) {
	if t := probeTraceFrom(ctx); t != nil {
		t.logger.Debug(msg, attrs...)
		return
	}
	s.logger.Debug(msg, attrs...)
}

// debugProbe is what a debug probe needs, as probeHandler resolved it.
type debugProbe struct {
	upstreamProbe
	requestURL, method string
	// staleKey is the key a probe's stale answer would come from, empty when
	// the collector has none.
	staleKey string
	// static is the static target a debug scrape is of, nil for a probe
	// (serveStaticTargetDebug).
	static *model.StaticTarget
	// generation is that of the configuration the collector was read in,
	// which says whose the worker statistics are that the probe's scripts
	// count in (pythonStatsSince).
	generation uint64
}

// whose is what a report is of, in its first line.
func (p debugProbe) whose() string {
	if p.static != nil {
		return fmt.Sprintf("Debug scrape of static target %q, collector %q, target %s", p.static.Name, p.collector.Name, orNone(p.logTarget))
	}
	return fmt.Sprintf("Debug probe of collector %q, target %s", p.collector.Name, orNone(p.logTarget))
}

// serveDebugProbe makes the trip and answers with its report.
func (s *Server) serveDebugProbe(w http.ResponseWriter, r *http.Request, p debugProbe) {
	c, name := p.collector, p.collector.Name
	start := time.Now()
	ctx, trace := newProbeTrace(r.Context())
	var verdict string
	var answer *model.MetricSet
	if p.budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.budget)
		defer cancel()
	}
	// A probe is answered at once when its collector is at its limit; a
	// static target's scrape waits for a slot within its interval, as the
	// scrape itself would.
	var full error
	if p.static != nil {
		full = s.trips.acquire(ctx, name, maxConcurrentProbes(c))
	} else if limited := s.trips.tryAcquire(name, maxConcurrentProbes(c)); limited != nil {
		full = errors.New(limited.message)
	}
	if full != nil {
		verdict = "503: " + full.Error() + "; this probe was not sent"
		if p.static != nil {
			verdict = "target up 0: " + full.Error() + "; the scrape was not sent"
		}
	} else {
		log := collectLog{
			failed: "probe failed", continuing: "probe stage failed; continuing", recovery: "probe recovered",
			attrs: []any{"collector", name, "target", p.logTarget},
		}
		if p.static != nil {
			log = collectLog{
				failed: "static target scrape failed", continuing: "static target stage failed; continuing", recovery: "static target recovered",
				attrs: []any{"target", p.static.Name, "collector", name, "address", p.logTarget},
			}
		}
		verdict, answer = s.debugTrip(ctx, trace, p, log)
	}
	if p.static != nil {
		s.logger.Info("static target debug report served", "static_target", p.static.Name, "collector", name)
	} else {
		s.logger.Info("probe debug report served", "collector", name, "target", p.logTarget)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(trace.report(p, verdict, answer, time.Since(start)))
}

// debugTrip makes a debug probe's trip, holding the slot serveDebugProbe
// took for it, and says what it would have answered. The slot is given back
// however the trip ends: a panic in a request type, a decoder or a transform
// would otherwise keep it for good, and after max_concurrent_probes of them
// every probe of the collector would be refused. The panic is recovered, as a
// probe's is (probeFlights.run), and the report shows it.
func (s *Server) debugTrip(ctx context.Context, trace *probeTrace, p debugProbe, log collectLog) (verdict string, answer *model.MetricSet) {
	defer s.trips.release(p.collector.Name)
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := string(debug.Stack())
			// The stack goes to the exporter's log, where a bug is reported
			// from; the report says what happened.
			trace.logger.Error("probe panicked; its stack is in the exporter's log", "panic", panicText(recovered))
			s.logger.Error("debug probe panicked", "collector", p.collector.Name, "target", p.logTarget, "panic", panicText(recovered), "stack", stack)
			verdict, answer = "500: probe failed: internal error: "+panicText(recovered), nil
			if p.static != nil {
				verdict = "target up 0: the scrape failed: internal error: " + panicText(recovered)
			}
		}
	}()
	result := s.collect(ctx, collectJob{
		collector: p.collector, target: p.target, overrides: p.overrides, headers: p.forwarded,
		display: p.logTarget, budget: p.budget, budgetSource: p.budgetSource,
		scrape: p.static != nil, log: log,
		python: s.pythonStatsSince(p.generation, p.collector),
	})
	return s.debugVerdict(result, p)
}

// debugVerdict is what a probe would have answered, as probeTrip and
// probeUpstream would answer it, and the series it would have served.
func (s *Server) debugVerdict(result collected, p debugProbe) (string, *model.MetricSet) {
	name := p.collector.Name
	if p.static != nil {
		return s.staticDebugVerdict(result, p)
	}
	var verdict string
	switch {
	case result.aborted:
		return "503: probe cancelled: the caller went away", nil
	case result.metric != "":
		verdict = fmt.Sprintf("502: metric %s failed: %v", result.metric, result.err)
	case result.refused:
		verdict = fmt.Sprintf("403: collector %s refused the target: %v", name, result.err)
	case result.failed():
		verdict = fmt.Sprintf("502: collector %s %s failed: %v", name, result.stage, result.err)
	case result.carriedOn:
		// What probeTrip answers: nothing of the collector's.
		answer := carriedOnAnswer(p.collector)
		return "200 with no series of the collector's: a stage failed and error_handling carried on", &answer
	default:
		answer := result.answer
		return fmt.Sprintf("200 with %d series", len(answer.Metrics)), &answer
	}
	if result.unauthorized {
		return verdict + " (no stale result: the target refused the credential)", nil
	}
	if result.refused {
		return verdict + " (no stale result: the target policy refused the target)", nil
	}
	if model.StaleIfError(p.collector) > 0 && p.staleKey != "" {
		now := time.Now()
		if cached, fetched, found := s.cache.GetStale(p.staleKey, now); found {
			if answer, err := withFreshness(cached, p.collector, true, fetched, now); err == nil {
				return fmt.Sprintf("200 with the last good result, %s old, marked stale (cache.stale_if_error), instead of %s",
					now.Sub(fetched).Round(time.Second), verdict), &answer
			}
		}
	}
	return verdict, nil
}

// staticDebugVerdict is what a static target's scrape would have published,
// as scrapeStaticTarget would publish it, and its series with the target's
// labels, as the endpoint serves them.
func (s *Server) staticDebugVerdict(result collected, p debugProbe) (string, *model.MetricSet) {
	labelled := func(set model.MetricSet) *model.MetricSet {
		out := withStaticTargetLabel(withTargetLabels(set, p.static.Labels), p.static.Name)
		return &out
	}
	var reason string
	switch {
	case result.aborted:
		return "nothing: the scrape was cancelled", nil
	case result.metric != "":
		reason = fmt.Sprintf("metric %s failed: %v", result.metric, result.err)
	case result.refused:
		reason = fmt.Sprintf("collector %s refused the target: %v", p.collector.Name, result.err)
	case result.failed():
		reason = fmt.Sprintf("%s failed: %v", result.stage, result.err)
	case result.carriedOn:
		return "target up 1 with no series: a stage failed and error_handling carried on", nil
	default:
		return fmt.Sprintf("target up 1 with %d series", len(result.answer.Metrics)), labelled(result.answer)
	}
	if result.unauthorized {
		return "target up 0: " + reason + " (no stale result: the target refused the credential)", nil
	}
	if result.refused {
		return "target up 0: " + reason + " (no stale result: the target policy refused the target)", nil
	}
	if model.StaleIfError(p.collector) > 0 && p.staleKey != "" {
		now := time.Now()
		if cached, fetched, found := s.cache.GetStale(p.staleKey, now); found {
			if answer, err := withFreshness(cached, p.collector, true, fetched, now); err == nil {
				return fmt.Sprintf("target up 0 with the last good result, %s old, marked stale (cache.stale_if_error): %s",
					now.Sub(fetched).Round(time.Second), reason), labelled(answer)
			}
		}
	}
	return "target up 0: " + reason, nil
}

// serveStaticTargetDebug answers /static-targets?debug=<name>: one scrape of
// the static target of that name, as its schedule would make it, reported as
// a debug probe is. It publishes nothing: the endpoint keeps serving the
// target's last scheduled result.
func (s *Server) serveStaticTargetDebug(w http.ResponseWriter, r *http.Request, name string) {
	// The target and its collector are read together, as its scrape reads
	// them, and with them the generation of the configuration they are in
	// (followedInForce), which says below whose worker statistics the
	// scrape's scripts count in. Asked for once the target's credential
	// files are read, it would be that of another configuration when a
	// reload came meanwhile, or of none: the scripts of a collector the
	// reload kept would be counted under no collector, in its own worker.
	followed := s.followedInForce()
	cfg := followed.config
	target, c := staticDebugTarget(followed, name)
	if target == nil {
		http.Error(w, fmt.Sprintf("no static target is named %q", name), http.StatusNotFound)
		return
	}
	if c == nil {
		http.Error(w, fmt.Sprintf("static target %q references unknown collector %q", target.Name, target.Collector), http.StatusNotFound)
		return
	}
	headers, err := fetch.TargetHeaders(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("static target %q: %v", target.Name, err), http.StatusInternalServerError)
		return
	}
	overrides := fetch.TargetOverrides(target)
	p := debugProbe{
		upstreamProbe: upstreamProbe{
			collector: c, target: target.Target, logTarget: fetch.DisplayTarget(c, target.Target),
			overrides: overrides, forwarded: headers,
			budget: time.Duration(target.Interval), budgetSource: budgetFromInterval,
		},
		method: fetch.RequestMethodFor(c, overrides),
		static: target, generation: followed.generation,
	}
	if label, err := fetch.RequestLabelFor(target.Target, c, overrides); err == nil {
		p.requestURL = label
	}
	if model.UsesCache(c) {
		p.staleKey = s.probeCacheKey(cfg, c, target.Target, targetCacheQuery(target), headers, targetOwnRequest(target)...)
	}
	s.serveDebugProbe(w, r, p)
}

// staticDebugTarget is the static target of followed named name, the first
// so named, nil when its file has none, and the collector of followed's
// configuration it references, nil when the configuration has none of that
// name. The target is the one in the file in force, not a copy: going
// through the targets by value copied each of them on the heap, its pointer
// being kept, which for 10,000 targets took milliseconds before the one
// asked for was found. Nothing the debug scrape does with it writes to it.
// The collector is found where followed keeps its place
// (followedConfig.collectorOf), as a scrape finds it, and the collectors
// are not gone through for it.
func staticDebugTarget(followed *followedConfig, name string) (*model.StaticTarget, *model.Collector) {
	targets := staticTargetsOf(followed.targets)
	for i := range targets {
		if targets[i].Name == name {
			return &targets[i], followed.collectorOf(followed.config, targets[i].Collector)
		}
	}
	return nil, nil
}

// report renders the trace. Its lines are cut where they are longer than a
// line may be (cutLongLines), but for the body's: those before the body
// when the body is to be written, and those after it at the end.
func (t *probeTrace) report(p debugProbe, verdict string, answer *model.MetricSet, took time.Duration) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b bytes.Buffer
	// uncut is where the lines not yet looked at for their length start.
	uncut := 0
	c := p.collector
	b.WriteString(p.whose())
	b.WriteString("\n")
	if p.static != nil {
		fmt.Fprintf(&b, "Took %s. The scrape would have published %s.\n", took.Round(time.Millisecond), verdict)
		b.WriteString("A debug scrape skips the response cache, publishes nothing on the endpoint, records no self-metric and exports nothing over OTLP.\n")
	} else {
		fmt.Fprintf(&b, "Took %s. A probe would have answered %s.\n", took.Round(time.Millisecond), verdict)
		b.WriteString("A debug probe skips the response cache, shares no trip, records no self-metric and exports nothing over OTLP.\n")
	}

	b.WriteString("\nRequests\n")
	requests := t.requests.Requests()
	if len(requests) == 0 {
		if p.requestURL != "" {
			fmt.Fprintf(&b, "  %s %s\n", p.method, p.requestURL)
		} else {
			b.WriteString("  none sent\n")
		}
	}
	for i, req := range requests {
		via := ""
		if req.Redirect {
			via = " (redirect)"
		}
		outcome := req.Outcome
		if outcome == "" {
			outcome = "no answer"
		} else {
			outcome += " in " + req.Duration.Round(time.Millisecond).String()
		}
		fmt.Fprintf(&b, "  %d. %s %s%s -> %s\n", i+1, req.Method, shownStart(fetch.RedactURLString(req.URL, fetch.MaskQueryValues), debugURLLimit), via, outcome)
		writeHeaders(&b, req.Header, "     ")
		if len(req.Withheld) > 0 {
			// The names only: the values were not sent, and are not shown.
			fmt.Fprintf(&b, "     not sent: %s — %s\n", strings.Join(req.Withheld, ", "), req.WithheldWhy)
		}
	}

	b.WriteString("\nResponse\n")
	if t.response == nil {
		b.WriteString("  none\n")
	} else if writeResponse(&b, t.response) {
		// The body is shown as it is, up to its own bound.
		cutLongLines(&b, uncut)
		writeBody(&b, t.response.Body, t.convertedFrom)
		uncut = b.Len()
	}

	b.WriteString("\nStages\n")
	for _, s := range t.steps {
		line := fmt.Sprintf("  %-12s %-10s %8s", s.stage, s.outcome, s.took.Round(time.Millisecond))
		if s.note != "" {
			line += "  " + s.note
		}
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteString("\n")
	}

	if t.transform != nil || len(t.failures) > 0 {
		b.WriteString("\nTransform\n")
		writeTransform(&b, c, t.transform, t.failures, t.decoded)
	}

	b.WriteString("\nLogs\n")
	if t.logs.Len() == 0 {
		b.WriteString("  none\n")
	}
	for _, line := range strings.Split(strings.TrimRight(t.logs.String(), "\n"), "\n") {
		if line != "" {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	if p.static != nil {
		b.WriteString("\nMetrics the scrape would have published, without its health series\n")
	} else {
		b.WriteString("\nMetrics a probe would have served\n")
	}
	if answer == nil || len(answer.Metrics) == 0 {
		b.WriteString("  none\n")
		cutLongLines(&b, uncut)
		return b.Bytes()
	}
	// The exposition is what a probe serves, to the byte, unless a line of
	// it is cut, which makes it no exposition: the section then says so
	// before it. It is not written to the report to be cut there, which
	// would hold every long line of it twice.
	exposition := appendMetricSet(nil, answer)
	long := longLines(exposition)
	if long > 0 {
		served := "a probe serves them whole"
		if p.static != nil {
			served = "the endpoint serves them whole"
		}
		fmt.Fprintf(&b, "  Lines longer than %d bytes are cut below (%d of them), so this is not valid exposition as it stands; %s.\n", debugLineLimit, long, served)
	}
	cutLongLines(&b, uncut)
	if long == 0 {
		b.Write(exposition)
	} else {
		appendCutLines(&b, exposition)
	}
	return b.Bytes()
}

// cutLongLines cuts the lines of the report from the byte uncut on that are
// longer than debugLineLimit (appendCutLines). A report without one, as
// nearly every report is, is read for its new-lines and left as it was
// written.
func cutLongLines(b *bytes.Buffer, uncut int) {
	written := b.Bytes()[uncut:]
	if longLines(written) == 0 {
		return
	}
	written = bytes.Clone(written)
	b.Truncate(uncut)
	appendCutLines(b, written)
}

// appendCutLines appends text to the report line by line: a line of
// debugLineLimit bytes or fewer as it is, and a longer one as its first
// debugLineLimit bytes (shownHead) and its length, as a failure's text says
// where it was cut (model.CutMark).
func appendCutLines(b *bytes.Buffer, text []byte) {
	for len(text) > 0 {
		line, rest, ended := bytes.Cut(text, []byte("\n"))
		if len(line) > debugLineLimit {
			b.Write(line[:shownHead(line, debugLineLimit)])
			b.WriteString(model.CutMark(len(line), false))
		} else {
			b.Write(line)
		}
		if ended {
			b.WriteByte('\n')
		}
		text = rest
	}
}

// longLines counts the lines of text that are longer than debugLineLimit.
func longLines(text []byte) (long int) {
	for len(text) > 0 {
		line, rest, _ := bytes.Cut(text, []byte("\n"))
		if len(line) > debugLineLimit {
			long++
		}
		text = rest
	}
	return long
}

// shownStart is text when it is no longer than limit bytes, and otherwise
// its first limit bytes (shownHead) and its length. The cut text is made
// anew, and holds nothing of the text it was cut from.
func shownStart(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:shownHead(text, limit)] + model.CutMark(len(text), false)
}

// shownHead is how many bytes of a text longer than limit a report shows:
// limit, or up to three fewer where the text would be cut inside a
// character, as a failure's text is cut (model.HeadOf); and where that ends
// inside a <redacted>, the few bytes more that show it whole, since
// `token=<red` reads as the start of a token.
func shownHead[T ~string | ~[]byte](text T, limit int) int {
	head := limit
	for start := limit; start > 0 && start > limit-utf8.UTFMax; start-- {
		if utf8.RuneStart(text[start]) {
			head = start
			break
		}
	}
	for had := 1; had < len(fetch.Redacted) && had <= head; had++ {
		if rest := len(fetch.Redacted) - had; len(text)-head >= rest && string(text[head-had:head+rest]) == fetch.Redacted {
			return head + rest
		}
	}
	return head
}

// writeResponse renders the response as the target sent it: its status and
// headers, and a directory's files. It reports whether the response has a
// body for writeBody to render after them, which a directory has not.
func writeResponse(b *bytes.Buffer, r *fetch.HTTPResponse) (body bool) {
	switch {
	case r.GRPCCode != nil:
		fmt.Fprintf(b, "  gRPC status %d\n", *r.GRPCCode)
	case r.NoStatus:
	case r.StatusCode != 0:
		fmt.Fprintf(b, "  Status %d %s\n", r.StatusCode, http.StatusText(r.StatusCode))
	}
	if len(r.Headers) > 0 {
		b.WriteString("  Headers\n")
		writeHeaders(b, r.Headers, "    ")
	}
	if d := r.Directory; d != nil {
		fmt.Fprintf(b, "  Directory %s, %d files read\n", d.Path, len(d.Files))
		for _, f := range d.Files {
			if f.Err != nil {
				fmt.Fprintf(b, "    %s: %v\n", f.Name, f.Err)
				continue
			}
			size := 0
			if f.Response != nil {
				size = len(f.Response.Body)
			}
			fmt.Fprintf(b, "    %s: %d bytes\n", f.Name, size)
		}
		for _, name := range d.Skipped {
			fmt.Fprintf(b, "    %s: skipped, over request.max_files\n", name)
		}
		return false
	}
	return true
}

// writeBody renders a body, up to debugBodyLimit, text only. The report is
// UTF-8, so a body in another encoding, whose bytes it cannot show as they
// are, is shown as the same text in UTF-8, under a line that gives its size
// as sent and says what it was converted from: convertedFrom, when not
// empty, is the encoding the body was converted from before it was decoded,
// which the report says, since the rules read the converted text and not the
// bytes shown.
func writeBody(b *bytes.Buffer, body []byte, convertedFrom string) {
	shown, note := body, ""
	if convertedFrom != "" {
		if text, err := decode.TextFrom(body, convertedFrom); err == nil {
			shown, note = text, " in "+convertedFrom+", converted to UTF-8 before decoding and shown here as UTF-8"
		}
	}
	cut := len(shown) > debugBodyLimit
	if cut {
		shown = shown[:debugBodyLimit]
		// A character cut in two at the limit is dropped, not shown broken.
		for i := 0; i < utf8.UTFMax && len(shown) > 0 && !utf8.Valid(shown); i++ {
			shown = shown[:len(shown)-1]
		}
	}
	if !utf8.Valid(shown) {
		fmt.Fprintf(b, "  Body: %d bytes, not text\n", len(body))
		return
	}
	fmt.Fprintf(b, "  Body: %d bytes%s\n", len(body), note)
	for _, line := range strings.Split(strings.TrimRight(string(shown), "\n"), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if cut {
		fmt.Fprintf(b, "    ... cut at %d bytes\n", debugBodyLimit)
	}
}

// writeTransform renders the series each metric got, the rules that got
// none, and the rules that carried on without some: each rule by itself,
// under its metric name, and with its expression, and its items when it has
// any, where several of the collector's rules export the name
// (sharedRuleNames). A prometheus rule without a name is shown as that.
func writeTransform(b *bytes.Buffer, c *model.Collector, set *model.MetricSet, failures []transform.RuleFailure, decoded string) {
	if decoded != "" {
		fmt.Fprintf(b, "  %s transform of a %s response\n", c.Transform.Type, decoded)
	}
	counts := map[string]int{}
	var order []string
	if set != nil {
		for _, m := range set.Metrics {
			if _, seen := counts[m.Name]; !seen {
				order = append(order, m.Name)
			}
			counts[m.Name]++
		}
	}
	if len(order) == 0 {
		b.WriteString("  no series\n")
	} else {
		b.WriteString("  Series by metric\n")
		for _, name := range order {
			fmt.Fprintf(b, "    %s: %d\n", shownStart(name, debugNameLimit), counts[name])
		}
	}
	var empty []string
	seen := map[string]bool{}
	for _, rule := range c.Metrics {
		// The series are named as they are exported, so the rule is looked
		// for, and listed, by the name its series are exported under.
		name := transform.ExportedMetricName(c, rule.Name)
		if rule.Name == "" || seen[name] || counts[name] > 0 {
			continue
		}
		seen[name] = true
		empty = append(empty, name)
	}
	if len(empty) > 0 && c.Transform.Type != "prometheus" {
		b.WriteString("  Rules that gave no series: ")
		b.WriteString(strings.Join(empty, ", "))
		b.WriteString("\n")
	}
	if len(failures) > 0 {
		b.WriteString("  Rules that carried on without some series\n")
		var shared sharedRuleNames
		for _, f := range failures {
			rule := f.Metric
			if rule == "" {
				// A prometheus rule may have no name, and a line that
				// began with nothing would not read as a rule's.
				rule = "rule without a name"
			}
			if shared.has(c, f.Metric) {
				rule += fmt.Sprintf(" (expression %q", f.Expression)
				if f.Items != "" {
					rule += fmt.Sprintf(", items %q", f.Items)
				}
				rule += ")"
			}
			line := fmt.Sprintf("    %s: %d failed", rule, f.Failures)
			if f.Missing > 0 {
				line += fmt.Sprintf(", %d of them missing values", f.Missing)
			}
			if f.First != nil {
				line += "; first: " + f.First.Error()
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
}

// writeHeaders renders headers, sorted, their credentials redacted: the
// first debugHeaderLimit of them, each value by no more than its first
// debugHeaderValueLimit bytes, and how many there are when there are more.
func writeHeaders(b *bytes.Buffer, h http.Header, indent string) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	headers := 0
	for _, name := range names {
		for _, value := range h[name] {
			if headers++; headers <= debugHeaderLimit {
				fmt.Fprintf(b, "%s%s: %s\n", indent, name, shownStart(fetch.RedactHeaderValue(name, value), debugHeaderValueLimit))
			}
		}
	}
	if headers > debugHeaderLimit {
		fmt.Fprintf(b, "%s... (%d headers)\n", indent, headers)
	}
}

// orNone is s, or "(none)" when it is empty.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// errProbeDebugDisabled answers a debug probe without --web.enable-probe-debug.
var errProbeDebugDisabled = errors.New("debug reports are not enabled; start the exporter with --web.enable-probe-debug to allow /probe?debug=true and /static-targets?debug=<name> (the chart's server.probeDebug)")
