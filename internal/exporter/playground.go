package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The playground, /playground with --web.enable-playground, is a page for
// writing a collector: the YAML of one collector, as it is written under
// collectors:, run against a response, with the series a probe would answer
// and a debug report of the run (probedebug.go) beside it. The page posts
// each run to /playground/run, which reads the collector as the loader reads
// one (config.ParseCollector), so a mistake reads as a load would report it,
// and makes the trip collect makes, decode, transform and validation, under
// the collector's own limits.
//
// The collector is never put in force. A run reads and fills no cache,
// shares no trip, counts in no collector's self-metrics and in no failure
// log, queues nothing for OTLP and logs one line at info: what it logged
// goes to its answer, as a debug probe's goes to its report. It counts in
// http_exporter_playground_runs_total alone.
//
// What a run reads is one of two things:
//
//   - a pasted response: a body, its content type, a status and headers,
//     which the page sends. No request is made at all;
//   - a target, fetched through a collector of the configuration in force,
//     request_from, an http collector, exactly as /probe?debug=true fetches
//     it: through that collector's request block, its target policy, its
//     credentials, its accept_status and its max_concurrent_probes, with the
//     probe parameters it takes. The playground collector's own request block
//     makes no request. Since the answer shows the target's response, as a
//     debug report does, this needs --web.enable-probe-debug too.
//
// The page is for the operators of the exporter, behind web.basic_auth when
// that is on, and what it is given is code of a kind: an expression runs
// on the server, against what it was pasted or what the target sent. So a
// run is refused what would reach further than the response:
//
//   - Python, a python transform or a pre_script, is never run: a script is
//     a program run on the server, with the server's files and network;
//   - a collector of another request type than http, whose type reads files,
//     hosts or databases with the server's own keys and passwords, and an
//     http collector that names a file — basic_auth_file, bearer_token_file
//     or a tls file — are refused before they are checked, so nothing the
//     run answers can carry what such a file holds;
//   - ${NAME} references are left as written: the answer would show the
//     server's environment.
//
// And it is bounded as a probe is: the request by playgroundMaxRequestBytes,
// a pasted body by playgroundMaxBodyBytes and by the collector's response
// limit, the run by the probe's deadline (scrapetimeout.go), and the runs in
// progress by playgroundMaxConcurrent, a fetch also by the trip limits of the
// collector it goes through.
//
// A browser sends the exporter's Basic Auth credential with any request to
// it, a page of another site's included, so a run must carry the
// X-Playground header and a JSON body: neither can a page of another site
// send without asking the exporter first (a CORS preflight), which the
// exporter never allows. A browser that says where a request comes from
// (Sec-Fetch-Site) must say it comes from the exporter's own pages.

// playgroundPath and playgroundRunPath are the page and what its runs post
// to.
const (
	playgroundPath    = "/playground"
	playgroundRunPath = "/playground/run"
)

// playgroundHeader must be on every run (playgroundSameOrigin).
const playgroundHeader = "X-Playground"

// What a run may send: the whole request, the pasted body and the
// collector's YAML. The body's bound is above what a collector is usually
// written against and well under the default response limit; JSON escapes
// make the request longer than what it carries, so its own bound is twice
// that.
const (
	playgroundMaxRequestBytes   = 2 << 20
	playgroundMaxBodyBytes      = 1 << 20
	playgroundMaxCollectorBytes = 256 << 10
	playgroundMaxHeaders        = 100
)

// playgroundMaxConcurrent is how many runs may be in progress at once. Each
// holds its body, its decoded form and its series, as a trip does; the page
// sends one at a time, cancelling the last when another starts.
const playgroundMaxConcurrent = 4

// budgetFromPlayground is where a run's budget comes from when the probe's
// default timeout is off: a run is never unbounded.
const budgetFromPlayground = "the playground's own bound, as --probe.default-timeout is 0"

// playgroundHeaderName is what a pasted header's name may be: an HTTP token.
var playgroundHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// SetPlayground enables /playground and /playground/run,
// --web.enable-playground.
func (s *Server) SetPlayground(enabled bool) {
	s.playground = enabled
	if enabled && s.playgroundSlots == nil {
		s.playgroundSlots = make(chan struct{}, playgroundMaxConcurrent)
	}
}

// playgroundRuns counts the runs by result, for
// http_exporter_playground_runs_total.
type playgroundRuns struct {
	success, failure, invalid atomic.Uint64
}

// playgroundRequest is what /playground/run takes. Body set is a pasted
// response; RequestFrom set is a fetch from Target through that collector.
type playgroundRequest struct {
	Collector   string            `json:"collector"`
	Body        *string           `json:"body"`
	ContentType string            `json:"content_type"`
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	RequestFrom string            `json:"request_from"`
	Target      string            `json:"target"`
	Params      map[string]string `json:"params"`
}

// playgroundAnswer is what /playground/run answers. Error alone is a run
// refused before it ran; otherwise Series is what a probe would have served,
// as Prometheus text, Verdict what it would have answered, Stages and Notes
// the stages and what was logged, and Report the whole debug report.
type playgroundAnswer struct {
	OK      bool              `json:"ok"`
	Verdict string            `json:"verdict,omitempty"`
	Series  string            `json:"series"`
	Stages  []playgroundStage `json:"stages,omitempty"`
	Notes   []string          `json:"notes,omitempty"`
	Report  string            `json:"report,omitempty"`
	Error   string            `json:"error,omitempty"`
}

type playgroundStage struct {
	Stage   string `json:"stage"`
	Outcome string `json:"outcome"`
	Took    string `json:"took"`
	Note    string `json:"note,omitempty"`
}

// writePlaygroundAnswer writes answer as JSON with status code.
func writePlaygroundAnswer(w http.ResponseWriter, code int, answer playgroundAnswer) {
	body, err := json.Marshal(answer)
	if err != nil {
		http.Error(w, "the answer could not be written", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}

// playgroundRefusal writes a run refused before it ran.
func playgroundRefusal(w http.ResponseWriter, code int, format string, args ...any) {
	writePlaygroundAnswer(w, code, playgroundAnswer{Error: fmt.Sprintf(format, args...)})
}

// playgroundSameOrigin refuses a run that a page of another site could have
// sent: one without the X-Playground header or a JSON body, which no such
// page sends without a CORS preflight the exporter never allows, and one a
// browser says came from another site. The Origin is not compared with the
// Host, which a reverse proxy in front of the exporter may rewrite.
func playgroundSameOrigin(r *http.Request) error {
	if r.Header.Get(playgroundHeader) == "" {
		return fmt.Errorf("a playground run must carry the %s header, which the playground page sends; another site's page cannot send it", playgroundHeader)
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return fmt.Errorf("the browser says this run comes from another site (Sec-Fetch-Site: %s); runs are taken from the exporter's own playground page only", site)
	}
	return nil
}

// refusedInPlayground refuses, before it is checked, a collector the
// playground does not run: Python, a request type other than http, and an
// http collector that names a file of the server's.
func refusedInPlayground(x *model.Collector) error {
	if kind := strings.ToLower(strings.TrimSpace(x.Request.Type)); kind != "" && kind != fetch.RequestTypeHTTP {
		return fmt.Errorf("collector %q uses request.type %s; the playground runs http collectors only, since the other types read files, hosts and databases with the server's own credentials. Write it with request.type http to try its decoder and rules on a pasted response", x.Name, kind)
	}
	if runsPython(x) {
		return fmt.Errorf("collector %q runs Python (transform.type python or transform.pre_script); the playground never runs Python, since a script is a program run on the server. Try it with a debug probe of a configured collector instead (--web.enable-probe-debug)", x.Name)
	}
	var files []string
	r := x.Request
	if r.BasicAuthFile != nil {
		files = append(files, "basic_auth_file")
	}
	if r.BearerTokenFile != "" {
		files = append(files, "bearer_token_file")
	}
	for key, path := range map[string]string{"tls.ca_file": r.TLS.CAFile, "tls.cert_file": r.TLS.CertFile, "tls.key_file": r.TLS.KeyFile} {
		if path != "" {
			files = append(files, key)
		}
	}
	if len(files) > 0 {
		sort.Strings(files)
		return fmt.Errorf("collector %q sets request.%s, which names a file on the server; the playground reads no file of the server's. Leave it out: a pasted response needs no credential, and a target is fetched through a configured collector (request_from), with its credentials", x.Name, strings.Join(files, ", request."))
	}
	return nil
}

func (s *Server) playgroundPageHandler(w http.ResponseWriter, _ *http.Request) {
	page := playgroundPage{ProbeTimeoutSeconds: pageProbeTimeoutSeconds, Fetch: s.probeDebug, Docs: landingDocs, Example: playgroundExample, ExampleBody: playgroundExampleBody}
	if s.probeDebug {
		cfg := s.manager.Get()
		for i := range cfg.Collectors {
			if c := &cfg.Collectors[i]; c.Request.Type == fetch.RequestTypeHTTP {
				page.Collectors = append(page.Collectors, c.Name)
			}
		}
	}
	s.renderPage(w, playgroundTemplate, page)
}

func (s *Server) playgroundRunHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		playgroundRefusal(w, http.StatusMethodNotAllowed, "use POST to run a collector in the playground")
		return
	}
	if err := playgroundSameOrigin(r); err != nil {
		playgroundRefusal(w, http.StatusForbidden, "%v", err)
		return
	}
	if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
		playgroundRefusal(w, http.StatusUnsupportedMediaType, "a playground run is a JSON body, sent with Content-Type: application/json")
		return
	}
	req, code, err := readPlaygroundRequest(w, r)
	if err != nil {
		playgroundRefusal(w, code, "%v", err)
		return
	}
	fetching := req.Body == nil
	if fetching && !s.probeDebug {
		playgroundRefusal(w, http.StatusForbidden, "fetching from a target shows the target's response, as a debug report does, so it needs --web.enable-probe-debug as well (the chart's server.probeDebug); paste a response instead")
		return
	}
	cfg, err := config.ParseCollector([]byte(req.Collector), refusedInPlayground)
	if err != nil {
		s.playgroundRunCounts.invalid.Add(1)
		playgroundRefusal(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	c := &cfg.Collectors[0]
	run, code, err := s.preparePlaygroundRun(r, req, c)
	if err != nil {
		playgroundRefusal(w, code, "%v", err)
		return
	}
	select {
	case s.playgroundSlots <- struct{}{}:
		defer func() { <-s.playgroundSlots }()
	default:
		playgroundRefusal(w, http.StatusServiceUnavailable, "the playground already has %d runs in progress; try again when one has finished", playgroundMaxConcurrent)
		return
	}
	answer, ok := s.runPlayground(r.Context(), run)
	answer.Notes = append(append([]string(nil), cfg.Warnings...), answer.Notes...)
	if ok {
		s.playgroundRunCounts.success.Add(1)
	} else {
		s.playgroundRunCounts.failure.Add(1)
	}
	s.logger.Info("playground run served", "collector", c.Name, "on", run.probe.playground, "target", run.probe.logTarget)
	writePlaygroundAnswer(w, http.StatusOK, answer)
}

// readPlaygroundRequest reads and checks a run's request, and says with which
// status to refuse one that is wrong.
func readPlaygroundRequest(w http.ResponseWriter, r *http.Request) (playgroundRequest, int, error) {
	var req playgroundRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, playgroundMaxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return req, http.StatusRequestEntityTooLarge, fmt.Errorf("a playground run may be at most %d bytes", playgroundMaxRequestBytes)
		}
		return req, http.StatusBadRequest, fmt.Errorf("the run is not the JSON the playground takes: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, http.StatusBadRequest, errors.New("the run is not the JSON the playground takes: more follows the object")
	}
	switch {
	case len(req.Collector) > playgroundMaxCollectorBytes:
		return req, http.StatusRequestEntityTooLarge, fmt.Errorf("the collector is %d bytes; the playground takes one of at most %d", len(req.Collector), playgroundMaxCollectorBytes)
	case req.Body != nil && len(*req.Body) > playgroundMaxBodyBytes:
		return req, http.StatusRequestEntityTooLarge, fmt.Errorf("the pasted body is %d bytes; the playground takes one of at most %d", len(*req.Body), playgroundMaxBodyBytes)
	case req.Body != nil && (req.RequestFrom != "" || req.Target != ""):
		return req, http.StatusBadRequest, errors.New("a run reads a pasted body or fetches a target, not both: send body, or request_from and target")
	case req.Body == nil && req.RequestFrom == "":
		return req, http.StatusBadRequest, errors.New("a run needs a response: send body, a pasted response, or request_from, the configured collector to fetch the target through")
	case req.Body == nil && (req.ContentType != "" || req.Status != 0 || len(req.Headers) > 0):
		return req, http.StatusBadRequest, errors.New("content_type, status and headers describe a pasted body; a fetched target's response has its own")
	case req.Status != 0 && (req.Status < 100 || req.Status > 599):
		return req, http.StatusBadRequest, fmt.Errorf("status %d is no HTTP status; give one from 100 to 599", req.Status)
	case len(req.Headers) > playgroundMaxHeaders:
		return req, http.StatusBadRequest, fmt.Errorf("%d headers; a pasted response may have at most %d", len(req.Headers), playgroundMaxHeaders)
	}
	for name, value := range req.Headers {
		if !playgroundHeaderName.MatchString(name) {
			return req, http.StatusBadRequest, fmt.Errorf("header name %q is not one HTTP allows", name)
		}
		if err := fetch.CheckHeaderValue(value); err != nil {
			return req, http.StatusBadRequest, fmt.Errorf("header %s %w", name, err)
		}
	}
	for key := range req.Params {
		if probeIdentityParam(key) || key == probeDebugParam {
			return req, http.StatusBadRequest, fmt.Errorf("params holds %s, which the playground sets itself: give the target as target, and the collector as collector or request_from", key)
		}
	}
	return req, 0, nil
}

// playgroundRun is a run as preparePlaygroundRun resolved it.
type playgroundRun struct {
	probe     debugProbe
	pasted    *fetch.HTTPResponse
	requester *model.Collector
}

// preparePlaygroundRun resolves what the run reads, checking its parameters
// as /probe checks a probe's: the playground collector's for a pasted body,
// and request_from's, whose request fetches the target, for a fetch.
func (s *Server) preparePlaygroundRun(r *http.Request, req playgroundRequest, c *model.Collector) (playgroundRun, int, error) {
	query := url.Values{}
	for key, value := range req.Params {
		query.Set(key, value)
	}
	run := playgroundRun{}
	requester := c
	if req.Body == nil {
		followed := s.reconcile()
		requester = followed.collectorOf(followed.config, req.RequestFrom)
		switch {
		case requester == nil:
			return run, http.StatusBadRequest, fmt.Errorf("unknown collector %q in request_from; name a collector of the configuration in force", req.RequestFrom)
		case requester.Request.Type != fetch.RequestTypeHTTP:
			return run, http.StatusBadRequest, fmt.Errorf("collector %q in request_from is a %s collector; the playground fetches through http collectors only", requester.Name, requester.Request.Type)
		}
		if err := fetch.CheckTarget(requester, req.Target, false); err != nil {
			if errors.Is(err, fetch.ErrMissingTarget) {
				return run, http.StatusBadRequest, fmt.Errorf("a target is required to fetch through collector %q", requester.Name)
			}
			return run, http.StatusBadRequest, err
		}
		run.requester = requester
	}
	if err := checkProbeParams(query, probeRequestParam(requester)); err != nil {
		return run, http.StatusBadRequest, err
	}
	overrides, err := fetch.ParseRequestOverrides(query)
	if err != nil {
		return run, http.StatusBadRequest, err
	}
	if err := fetch.CheckOverrideParams(requester, query); err != nil {
		return run, http.StatusBadRequest, err
	}
	if err := checkForwardedHeaderParams(query, requester.Request); err != nil {
		return run, http.StatusBadRequest, err
	}
	for _, x := range []*model.Collector{requester, c} {
		if err := fetch.CheckPathParams(x, overrides); err != nil {
			return run, http.StatusBadRequest, err
		}
	}
	budget, source := s.probeDeadline(r.Header, overrides)
	if budget <= 0 {
		budget, source = DefaultProbeTimeout, budgetFromPlayground
	}
	run.probe = debugProbe{
		upstreamProbe: upstreamProbe{collector: c, overrides: overrides, budget: budget, budgetSource: source},
		playground:    "a pasted response",
	}
	if run.requester == nil {
		headers := make(http.Header, len(req.Headers)+1)
		for name, value := range req.Headers {
			headers.Set(name, value)
		}
		if req.ContentType != "" {
			headers.Set("Content-Type", req.ContentType)
		}
		status := req.Status
		if status == 0 {
			status = http.StatusOK
		}
		run.pasted = &fetch.HTTPResponse{StatusCode: status, Headers: headers, Body: []byte(*req.Body), Collector: c.Name}
		return run, 0, nil
	}
	// Only the header_<name> parameters are forwarded, and none of the
	// browser's own headers: the page's request carries the exporter's
	// credential and the browser's cookies, which are not the target's.
	forwarded := forwardedHeaders(&http.Request{Header: http.Header{}, URL: &url.URL{RawQuery: query.Encode()}}, requester.Request)
	run.probe.target, run.probe.logTarget, run.probe.forwarded = req.Target, fetch.DisplayTarget(requester, req.Target), forwarded
	run.probe.playground = fmt.Sprintf("target %s, fetched through collector %q", run.probe.logTarget, requester.Name)
	run.probe.method = fetch.RequestMethodFor(requester, overrides)
	if label, err := fetch.RequestLabelFor(req.Target, requester, overrides); err == nil {
		run.probe.requestURL = label
	}
	return run, 0, nil
}

// pastedResponse is a copy of a pasted response for a trip to decode, held
// to the response limit of c as a fetched body is held to it: a body over it
// fails the trip as a target's would.
func pastedResponse(c *model.Collector, pasted *fetch.HTTPResponse) (*fetch.HTTPResponse, error) {
	if limit := fetch.ResponseLimit(c); int64(len(pasted.Body)) > limit {
		return nil, model.MarkError(fmt.Errorf("response size %d exceeds limit %d", len(pasted.Body), limit), model.ErrLimitExceeded)
	}
	response := *pasted
	response.Headers = pasted.Headers.Clone()
	return &response, nil
}

// runPlayground makes the run's trip and says whether it went through whole:
// every stage succeeded, none carried on.
func (s *Server) runPlayground(ctx context.Context, run playgroundRun) (playgroundAnswer, bool) {
	p := run.probe
	start := time.Now()
	ctx, trace := newProbeTrace(ctx)
	ctx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()
	var verdict string
	var answer *model.MetricSet
	ok := false
	// A fetch takes a trip slot of the collector it goes through, as a debug
	// probe of it would; a pasted response is no trip to anything.
	var full *tripLimitError
	if run.requester != nil {
		full = s.trips.tryAcquire(run.requester.Name, maxConcurrentProbes(run.requester))
	}
	if full != nil {
		verdict = "503: " + full.message + "; this probe was not sent"
	} else {
		verdict, answer, ok = s.playgroundTrip(ctx, trace, run)
	}
	out := playgroundAnswer{OK: ok, Verdict: verdict}
	if answer != nil {
		out.Series = string(appendMetricSet(nil, answer))
	}
	out.Stages, out.Notes = trace.stagesAndLogs()
	out.Report = string(trace.report(p, verdict, answer, time.Since(start)))
	return out, ok
}

// playgroundTrip is debugTrip for a run: the trip, with the slot of a fetch
// given back and a panic recovered however it ends.
func (s *Server) playgroundTrip(ctx context.Context, trace *probeTrace, run playgroundRun) (verdict string, answer *model.MetricSet, ok bool) {
	p := run.probe
	if run.requester != nil {
		defer s.trips.release(run.requester.Name)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			trace.logger.Error("run panicked; its stack is in the exporter's log", "panic", panicText(recovered))
			s.logger.Error("playground run panicked", "collector", p.collector.Name, "panic", panicText(recovered), "stack", string(debug.Stack()))
			verdict, answer, ok = "500: probe failed: internal error: "+panicText(recovered), nil, false
		}
	}()
	result := s.collect(ctx, collectJob{
		collector: p.collector, target: p.target, overrides: p.overrides, headers: p.forwarded,
		display: p.logTarget, budget: p.budget, budgetSource: p.budgetSource,
		log: collectLog{
			failed: "playground run failed", continuing: "playground stage failed; continuing", recovery: "playground run recovered",
			attrs: []any{"collector", p.collector.Name, "target", p.logTarget},
		},
		pasted: run.pasted, requester: run.requester,
	})
	verdict, answer = s.debugVerdict(result, p)
	return verdict, answer, !result.failed() && !result.carriedOn && !result.aborted
}

// stagesAndLogs are the stages the trace recorded and the lines it logged.
func (t *probeTrace) stagesAndLogs() ([]playgroundStage, []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	stages := make([]playgroundStage, 0, len(t.steps))
	for _, step := range t.steps {
		stages = append(stages, playgroundStage{Stage: step.stage, Outcome: step.outcome, Took: step.took.Round(time.Millisecond).String(), Note: step.note})
	}
	var logs []string
	for _, line := range strings.Split(strings.TrimRight(t.logs.String(), "\n"), "\n") {
		if line != "" {
			logs = append(logs, shownStart(line, debugLineLimit))
		}
	}
	return stages, logs
}

// playgroundRunMetrics is http_exporter_playground_runs_total, by result,
// from 0, and nothing without the playground.
func (s *Server) playgroundRunMetrics() []model.Metric {
	if !s.playground {
		return nil
	}
	name := "http_exporter_playground_runs_total"
	var out []model.Metric
	for _, x := range []struct {
		result string
		count  *atomic.Uint64
	}{{"success", &s.playgroundRunCounts.success}, {"failure", &s.playgroundRunCounts.failure}, {"invalid", &s.playgroundRunCounts.invalid}} {
		out = append(out, model.Metric{Name: name, Help: exporterMetricHelp[name], Type: model.CounterMetricType, Value: float64(x.count.Load()), Labels: map[string]string{"result": x.result}})
	}
	return out
}
