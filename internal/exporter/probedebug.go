package exporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
// report always answers 200, whatever the probe would have answered, which it
// says.
//
// The trace rides in the trip's context (probeTraceFrom), so the stages of
// collect, several frames down, record into it; a trip without one records
// nothing and logs as it always does (tripFailed, tripRecovered, tripDebug).
//
// Credentials are kept out of the report: request and response header values
// under a name that reads as a credential, every query value, a URL's
// userinfo, and request bodies are never shown (redactHeader, redactURL).

// probeDebugParam turns a probe into a debug probe.
const probeDebugParam = "debug"

// debugBodyLimit is how much of the response body a report shows.
const debugBodyLimit = 64 << 10

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

// probeTrace is what a debug probe's trip recorded.
type probeTrace struct {
	mu        sync.Mutex
	logs      bytes.Buffer
	logger    *slog.Logger
	requests  *fetch.RequestTrace
	steps     []traceStep
	response  *fetch.HTTPResponse
	decoded   string
	transform *model.MetricSet
	failures  []transform.RuleFailure
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

// tripFailed logs a failure of a trip: to the failure log, or for a debug
// probe to its report alone.
func (s *Server) tripFailed(ctx context.Context, level slog.Level, key, msg, stage string, err error, attrs ...any) {
	if t := probeTraceFrom(ctx); t != nil {
		if err != nil {
			attrs = append(attrs, "error", err)
		}
		t.logger.Log(ctx, level, msg, attrs...)
		return
	}
	s.failures.failed(s.logger, level, key, msg, stage, err, attrs...)
}

// tripRecovered is tripFailed's recovery: a debug probe has nothing to
// recover from, and says nothing.
func (s *Server) tripRecovered(ctx context.Context, key, msg string, attrs ...any) {
	if probeTraceFrom(ctx) != nil {
		return
	}
	s.failures.recovered(s.logger, key, msg, attrs...)
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
}

// serveDebugProbe makes the trip and answers with its report.
func (s *Server) serveDebugProbe(w http.ResponseWriter, r *http.Request, p debugProbe) {
	c, name := p.collector, p.collector.Name
	start := time.Now()
	ctx, trace := newProbeTrace(r.Context())
	var verdict string
	var answer *model.MetricSet
	if full := s.trips.tryAcquire(name, maxConcurrentProbes(c)); full != nil {
		verdict = "503: " + full.message + "; this probe was not sent"
	} else {
		if p.budget > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.budget)
			defer cancel()
		}
		result := s.collect(ctx, collectJob{
			collector: c, target: p.target, overrides: p.overrides, headers: p.forwarded,
			display: p.logTarget, budget: p.budget, budgetSource: p.budgetSource,
			log: collectLog{
				failed: "probe failed", continuing: "probe stage failed; continuing", recovery: "probe recovered",
				attrs: []any{"collector", name, "target", p.logTarget},
			},
		})
		s.trips.release(name)
		verdict, answer = s.debugVerdict(result, p)
	}
	s.logger.Info("probe debug report served", "collector", name, "target", p.logTarget)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(trace.report(p, verdict, answer, time.Since(start)))
}

// debugVerdict is what a probe would have answered, as probeTrip and
// probeUpstream would answer it, and the series it would have served.
func (s *Server) debugVerdict(result collected, p debugProbe) (string, *model.MetricSet) {
	name := p.collector.Name
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
		return "200 with no series: a stage failed and error_handling carried on", nil
	default:
		answer := result.answer
		return fmt.Sprintf("200 with %d series", len(answer.Metrics)), &answer
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

// report renders the trace.
func (t *probeTrace) report(p debugProbe, verdict string, answer *model.MetricSet, took time.Duration) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b bytes.Buffer
	c := p.collector
	fmt.Fprintf(&b, "Debug probe of collector %q, target %s\n", c.Name, orNone(p.logTarget))
	fmt.Fprintf(&b, "Took %s. A probe would have answered %s.\n", took.Round(time.Millisecond), verdict)
	b.WriteString("A debug probe skips the response cache, shares no trip, records no self-metric and exports nothing over OTLP.\n")

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
		fmt.Fprintf(&b, "  %d. %s %s%s -> %s\n", i+1, req.Method, redactURL(req.URL), via, outcome)
		writeHeaders(&b, req.Header, "     ")
	}

	b.WriteString("\nResponse\n")
	if t.response == nil {
		b.WriteString("  none\n")
	} else {
		writeResponse(&b, t.response)
	}

	b.WriteString("\nStages\n")
	for _, s := range t.steps {
		line := fmt.Sprintf("  %-12s %-10s %8s", s.stage, s.outcome, s.took.Round(time.Millisecond))
		if s.note != "" {
			line += "  " + s.note
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
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
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\nMetrics a probe would have served\n")
	if answer == nil || len(answer.Metrics) == 0 {
		b.WriteString("  none\n")
	} else {
		b.Write(appendMetricSet(nil, answer))
	}
	return b.Bytes()
}

// writeResponse renders the response: its status, headers and body, or a
// directory's files.
func writeResponse(b *bytes.Buffer, r *fetch.HTTPResponse) {
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
		return
	}
	writeBody(b, r.Body)
}

// writeBody renders a body, up to debugBodyLimit, text only.
func writeBody(b *bytes.Buffer, body []byte) {
	shown := body
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
	fmt.Fprintf(b, "  Body: %d bytes\n", len(body))
	for _, line := range strings.Split(strings.TrimRight(string(shown), "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	if cut {
		fmt.Fprintf(b, "    ... cut at %d bytes\n", debugBodyLimit)
	}
}

// writeTransform renders the series each metric got, the rules that got
// none, and the rules that carried on without some.
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
			fmt.Fprintf(b, "    %s: %d\n", name, counts[name])
		}
	}
	var empty []string
	seen := map[string]bool{}
	for _, rule := range c.Metrics {
		name := c.MetricsPrefix + rule.Name
		if rule.Name == "" || seen[name] || counts[name] > 0 {
			continue
		}
		seen[name] = true
		empty = append(empty, name)
	}
	if len(empty) > 0 && c.Transform.Type != "prometheus" {
		b.WriteString("  Rules that gave no series: " + strings.Join(empty, ", ") + "\n")
	}
	if len(failures) > 0 {
		b.WriteString("  Rules that carried on without some series\n")
		for _, f := range failures {
			line := fmt.Sprintf("    %s: %d failed", f.Metric, f.Failures)
			if f.Missing > 0 {
				line += fmt.Sprintf(", %d of them missing values", f.Missing)
			}
			if f.First != nil {
				line += "; first: " + f.First.Error()
			}
			b.WriteString(line + "\n")
		}
	}
}

// writeHeaders renders headers, sorted, their credentials redacted.
func writeHeaders(b *bytes.Buffer, h http.Header, indent string) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range h[name] {
			fmt.Fprintf(b, "%s%s: %s\n", indent, name, redactHeader(name, value))
		}
	}
}

// redacted stands in for what a report does not show.
const redacted = "<redacted>"

// credentialWords are what a header name holding a credential reads like.
var credentialWords = []string{"auth", "cookie", "token", "secret", "password", "passwd", "key", "session", "signature", "credential"}

// redactHeader is a header value as a report shows it: redacted when the
// name reads as a credential's.
func redactHeader(name, value string) string {
	lower := strings.ToLower(name)
	for _, word := range credentialWords {
		if strings.Contains(lower, word) {
			return redacted
		}
	}
	return value
}

// redactURL is a URL as a report shows it: without its userinfo's
// password, and with every query value redacted, since request.query may
// carry a token under any name.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redacted
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	if u.RawQuery != "" {
		query := u.Query()
		names := make([]string, 0, len(query))
		for name := range query {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			for range query[name] {
				parts = append(parts, url.QueryEscape(name)+"="+redacted)
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	out := u.String()
	// url.URL escapes the placeholder's angle brackets; they read better as
	// they are.
	return strings.NewReplacer("%3Credacted%3E", redacted).Replace(out)
}

// orNone is s, or "(none)" when it is empty.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// errProbeDebugDisabled answers a debug probe without --web.enable-probe-debug.
var errProbeDebugDisabled = errors.New("debug probes are not enabled; start the exporter with --web.enable-probe-debug to allow /probe?debug=true (the chart's server.probeDebug)")
