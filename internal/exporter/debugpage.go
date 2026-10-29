package exporter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The debug page, /debug with --web.debug at edit or full (debuglevel.go),
// runs one collector and shows what it gave: the series a probe would
// answer, the requests, the response, the stages, what was logged and the
// whole debug report. The collector is either written on the page, the YAML
// of one collector as it is written under collectors:, or one of the
// configuration in force, named. The page posts each run to /debug/run,
// which makes it with the engine a debug probe is made with (runDebug,
// probedebug.go) and answers its report as JSON.
//
// A collector written on the page is read as the loader reads one
// (config.ParseCollector), so a mistake reads as a load would report it, and
// is never put in force. A run reads and fills no cache, shares no trip,
// counts in no collector's self-metrics and in no failure log, queues
// nothing for OTLP and logs one line at info: what it logged goes to its
// answer, as a debug probe's goes to its report. It counts in
// http_exporter_debug_runs_total{kind="page"} alone, but for the Python
// worker series of a configured collector that runs a script, which count
// what the workers did, as for a debug probe.
//
// What a run reads is one of three things:
//
//   - a pasted response: a body, its content type, a status and headers,
//     which the page sends. No request is made at all. This is all edit
//     allows, so that at edit nothing a target answers is ever shown;
//   - at full, a target, fetched exactly as /probe?debug=true fetches it:
//     through a configured collector's request block, its target policy,
//     its credentials, its accept_status and its max_concurrent_probes, with
//     the probe parameters it takes. That collector is the one run, when it
//     is configured, or the one request_from names. A collector written on
//     the page never makes a request with its own request block;
//   - at full, one scrape of a static target, as /static-targets?debug=<name>
//     makes it, read by the target's collector or by the one run.
//
// The page is for the operators of the exporter, behind web.basic_auth when
// that is on, and what it is given is code of a kind: an expression runs on
// the server, against what it was pasted or what the target sent. So a
// collector written on the page is refused what would reach further than
// the response:
//
//   - Python, a python transform or a pre_script, is never run: a script is
//     a program run on the server, with the server's files and network;
//   - a collector of another request type than http, whose type reads files,
//     hosts or databases with the server's own keys and passwords, and an
//     http collector that names a file — basic_auth_file, bearer_token_file,
//     a tls file or a login's — are refused before they are checked, so nothing the
//     run answers can carry what such a file holds;
//   - ${NAME} references are left as written: the answer would show the
//     server's environment.
//
// A configured collector is the operator's own, put in force by the
// configuration, and is run as it is, its Python included: a pasted body is
// input chosen by whoever uses the page, as a target's answer is chosen by
// the target, which the collector's scripts read on every probe.
//
// And a run is bounded as a probe is: the request by debugMaxRequestBytes, a
// pasted body by debugMaxBodyBytes and by the collector's response limit,
// the run by the probe's deadline (scrapetimeout.go), and the runs in
// progress by debugMaxConcurrent, a fetch also by the trip limits of the
// collector it goes through.
//
// A browser sends the exporter's Basic Auth credential with any request to
// it, a page of another site's included, so a run must carry the
// X-Debug-Run header and a JSON body: neither can a page of another site
// send without asking the exporter first (a CORS preflight), which the
// exporter never allows. A browser that says where a request comes from
// (Sec-Fetch-Site) must say it comes from the exporter's own pages.

// debugPagePath and debugRunPath are the page and what its runs post to.
const (
	debugPagePath = "/debug"
	debugRunPath  = "/debug/run"
)

// debugRunHeader must be on every run (debugSameOrigin).
const debugRunHeader = "X-Debug-Run"

// What a run may send: the whole request, the pasted body and the
// collector's YAML. The body's bound is above what a collector is usually
// written against and well under the default response limit; JSON escapes
// make the request longer than what it carries, so its own bound is twice
// that.
const (
	debugMaxRequestBytes   = 2 << 20
	debugMaxBodyBytes      = 1 << 20
	debugMaxCollectorBytes = 256 << 10
	debugMaxHeaders        = 100
)

// debugMaxConcurrent is how many of the page's runs may be in progress at
// once. Each holds its body, its decoded form and its series, as a trip
// does; the page sends one at a time, cancelling the last when another
// starts. A debug probe or scrape is bounded by its collector's trip slots
// instead, as a probe is.
const debugMaxConcurrent = 4

// budgetFromDebugPage is where a run's budget comes from when the probe's
// default timeout is off: a run is never unbounded.
const budgetFromDebugPage = "the debug page's own bound, as --probe.default-timeout is 0"

// debugHeaderName is what a pasted header's name may be: an HTTP token.
var debugHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// debugRunRequest is what /debug/run takes: the collector, written
// (Collector) or configured (Configured), and what it reads — Body, a
// pasted response; Target with RequestFrom, a fetch; or StaticTarget, a
// static target's scrape.
type debugRunRequest struct {
	Collector    *string           `json:"collector"`
	Configured   string            `json:"configured"`
	Body         *string           `json:"body"`
	ContentType  string            `json:"content_type"`
	Status       int               `json:"status"`
	Headers      map[string]string `json:"headers"`
	RequestFrom  string            `json:"request_from"`
	Target       *string           `json:"target"`
	StaticTarget string            `json:"static_target"`
	Params       map[string]string `json:"params"`
}

// debugSource is what a run reads.
type debugSource uint8

const (
	debugSourcePasted debugSource = iota
	debugSourceTarget
	debugSourceStatic
)

// sources counts the sources req names; a run must name one.
func (req debugRunRequest) sources() (debugSource, int) {
	var source debugSource
	n := 0
	if req.Body != nil {
		source, n = debugSourcePasted, n+1
	}
	if req.Target != nil || req.RequestFrom != "" {
		source, n = debugSourceTarget, n+1
	}
	if req.StaticTarget != "" {
		source, n = debugSourceStatic, n+1
	}
	return source, n
}

// debugAnswer is what /debug/run answers: the run's report as JSON. Error
// alone is a run refused before it ran; otherwise Series is what a probe
// would have served, as Prometheus text, Verdict what it would have
// answered, Requests and Response what was sent and what came back, Stages
// and Notes the stages and what was logged, Lint the metric lint's advice,
// and Report the whole report as /probe?debug=true writes it.
type debugAnswer struct {
	OK       bool                    `json:"ok"`
	Verdict  string                  `json:"verdict,omitempty"`
	Series   string                  `json:"series"`
	Requests []debugShownRequest     `json:"requests,omitempty"`
	Response *debugShownResponse     `json:"response,omitempty"`
	Stages   []debugStage            `json:"stages,omitempty"`
	Notes    []string                `json:"notes,omitempty"`
	Lint     []transform.LintFinding `json:"lint,omitempty"`
	Report   string                  `json:"report,omitempty"`
	Error    string                  `json:"error,omitempty"`
}

type debugStage struct {
	Stage   string `json:"stage"`
	Outcome string `json:"outcome"`
	Took    string `json:"took"`
	Note    string `json:"note,omitempty"`
}

// debugShownRequest and debugShownResponse are a request and the response
// as the text report shows them: redacted and bounded alike (headerLines,
// shownBody).
type debugShownRequest struct {
	Method      string   `json:"method"`
	URL         string   `json:"url"`
	Page        int      `json:"page,omitempty"`
	Redirect    bool     `json:"redirect,omitempty"`
	Login       bool     `json:"login,omitempty"`
	Outcome     string   `json:"outcome,omitempty"`
	Took        string   `json:"took,omitempty"`
	Headers     []string `json:"headers,omitempty"`
	Withheld    []string `json:"withheld,omitempty"`
	WithheldWhy string   `json:"withheld_why,omitempty"`
	Wait        string   `json:"wait,omitempty"`
}

type debugShownResponse struct {
	Status        string   `json:"status,omitempty"`
	Headers       []string `json:"headers,omitempty"`
	BodyBytes     int      `json:"body_bytes"`
	Body          string   `json:"body"`
	BodyNotText   bool     `json:"body_not_text,omitempty"`
	BodyCut       bool     `json:"body_cut,omitempty"`
	ConvertedFrom string   `json:"converted_from,omitempty"`
	Files         []string `json:"files,omitempty"`
}

// debugAnswerOf writes report as the page's JSON.
func debugAnswerOf(report *debugReport) debugAnswer {
	out := debugAnswer{OK: report.ok, Verdict: report.verdict, Lint: report.lint, Report: string(report.text())}
	if report.answer != nil {
		out.Series = string(appendMetricSet(nil, report.answer))
	}
	t, p := report.trace, report.probe
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, req := range t.requests.Requests() {
		shown := debugShownRequest{
			Method: req.Method, URL: shownStart(fetch.RedactURLString(req.URL, fetch.MaskQueryValues), debugURLLimit),
			Page: req.Page, Redirect: req.Redirect, Login: req.Login, Outcome: req.Outcome, Headers: headerLines(req.Header),
			Withheld: req.Withheld, WithheldWhy: req.WithheldWhy, Wait: req.Wait,
		}
		if req.Outcome != "" {
			shown.Took = req.Duration.Round(time.Millisecond).String()
		}
		out.Requests = append(out.Requests, shown)
	}
	if len(out.Requests) == 0 && p.requestURL != "" {
		out.Requests = []debugShownRequest{{Method: p.method, URL: p.requestURL}}
	}
	if r := t.response; r != nil {
		shown := &debugShownResponse{Headers: headerLines(r.Headers)}
		switch {
		case r.GRPCCode != nil:
			shown.Status = fmt.Sprintf("gRPC status %d", *r.GRPCCode)
		case r.NoStatus:
		case r.StatusCode != 0:
			shown.Status = fmt.Sprintf("%d %s", r.StatusCode, http.StatusText(r.StatusCode))
		}
		if d := r.Directory; d != nil {
			var b strings.Builder
			writeDirectory(&b, d)
			shown.Files = strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
		} else {
			body := shownBody(r.Body, t.convertedFrom)
			shown.BodyBytes, shown.BodyNotText, shown.BodyCut, shown.ConvertedFrom = len(r.Body), !body.text, body.cut, body.convertedFrom
			if body.text {
				shown.Body = string(body.shown)
			}
		}
		out.Response = shown
	}
	for _, step := range t.steps {
		out.Stages = append(out.Stages, debugStage{Stage: step.stage, Outcome: step.outcome, Took: step.took.Round(time.Millisecond).String(), Note: step.note})
	}
	for _, line := range strings.Split(strings.TrimRight(t.logs.String(), "\n"), "\n") {
		if line != "" {
			out.Notes = append(out.Notes, shownStart(line, debugLineLimit))
		}
	}
	return out
}

// writeDebugAnswer writes answer as JSON with status code.
func writeDebugAnswer(w http.ResponseWriter, code int, answer debugAnswer) {
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

// debugSameOrigin refuses a run that a page of another site could have
// sent: one without the X-Debug-Run header or a JSON body, which no such
// page sends without a CORS preflight the exporter never allows, and one a
// browser says came from another site. The Origin is not compared with the
// Host, which a reverse proxy in front of the exporter may rewrite.
func debugSameOrigin(r *http.Request) error {
	if r.Header.Get(debugRunHeader) == "" {
		return fmt.Errorf("a debug run must carry the %s header, which the debug page sends; another site's page cannot send it", debugRunHeader)
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return fmt.Errorf("the browser says this run comes from another site (Sec-Fetch-Site: %s); runs are taken from the exporter's own debug page only", site)
	}
	return nil
}

// refusedOnDebugPage refuses, before it is checked, a collector written on
// the debug page that the page does not run: Python, a request type other
// than http, the one the page writes (fetch.RequestType.DebugPage), and an
// http collector that names a file of the server's.
func refusedOnDebugPage(x *model.Collector) error {
	if kind := x.Request.Type; kind != "" && !onDebugPage(kind) {
		return fmt.Errorf("collector %q uses request.type %s; a collector written on the debug page is an http collector, since the other types read files, hosts and databases with the server's own credentials. Write it with request.type http to try its decoder and rules on a pasted response, or run a configured collector by name", x.Name, kind)
	}
	if runsPython(x) {
		return fmt.Errorf("collector %q runs Python (transform.type python or transform.pre_script); a collector written on the debug page never runs Python, since a script is a program run on the server. Put it in the configuration and run it here by name instead", x.Name)
	}
	var files []string
	r := x.Request
	if r.BasicAuthFile != nil {
		files = append(files, "basic_auth_file")
	}
	if r.BearerTokenFile != "" {
		files = append(files, "bearer_token_file")
	}
	if r.Login != nil {
		// A login reads its secrets from files, and sends them.
		files = append(files, "login")
	}
	for key, path := range map[string]string{"tls.ca_file": r.TLS.CAFile, "tls.cert_file": r.TLS.CertFile, "tls.key_file": r.TLS.KeyFile} {
		if path != "" {
			files = append(files, key)
		}
	}
	if len(files) > 0 {
		// In the order the file has them; tls's keys, which the map above
		// gives in no order, in theirs where it says none.
		sort.Strings(files)
		files = model.InFileOrder(files, x.Request.Order)
		return fmt.Errorf("collector %q sets request.%s, which names a file on the server; a collector written on the debug page reads no file of the server's. Leave it out: a pasted response needs no credential, and a target is fetched through a configured collector (request_from), with its credentials", x.Name, strings.Join(files, ", request."))
	}
	return nil
}

// onDebugPage reports whether a collector written on the debug page may be
// of the request type, and another collector read what one of it fetches:
// http's, which reads nothing of the server's but what its configuration
// names (fetch.RequestType.DebugPage).
func onDebugPage(requestType string) bool {
	rt := fetch.RequestTypes[requestType]
	return rt != nil && rt.DebugPage
}

// debugPageCollector is a configured collector as the page offers it.
type debugPageCollector struct {
	Name, RequestType string
}

func (s *Server) debugPageHandler(w http.ResponseWriter, _ *http.Request) {
	page := debugPage{Full: s.debug == DebugFull, ProbeTimeoutSeconds: pageProbeTimeoutSeconds, Docs: landingDocs, Example: debugExample, ExampleBody: debugExampleBody}
	cfg := s.manager.Get()
	for i := range cfg.Collectors {
		c := &cfg.Collectors[i]
		page.Collectors = append(page.Collectors, debugPageCollector{Name: c.Name, RequestType: c.Request.Type})
		if onDebugPage(c.Request.Type) {
			page.HTTPCollectors = append(page.HTTPCollectors, c.Name)
		}
	}
	s.renderPage(w, debugPageTemplate, page)
}

func (s *Server) debugRunHandler(w http.ResponseWriter, r *http.Request) {
	// Every refusal is counted invalid: the run was asked for, and did not
	// run for what was asked.
	refuse := func(code int, format string, args ...any) {
		s.countDebugRun(debugKindPage, debugInvalid)
		writeDebugAnswer(w, code, debugAnswer{Error: fmt.Sprintf(format, args...)})
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		refuse(http.StatusMethodNotAllowed, "use POST to run a collector on the debug page")
		return
	}
	if err := debugSameOrigin(r); err != nil {
		refuse(http.StatusForbidden, "%v", err)
		return
	}
	if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
		refuse(http.StatusUnsupportedMediaType, "a debug run is a JSON body, sent with Content-Type: application/json")
		return
	}
	req, code, err := readDebugRunRequest(w, r)
	if err != nil {
		refuse(code, "%v", err)
		return
	}
	if source, _ := req.sources(); source != debugSourcePasted && s.debug != DebugFull {
		refuse(http.StatusForbidden, "fetching a target or scraping a static target shows what the target answers, so it needs --web.debug=full (the chart's server.debug: full); at --web.debug=edit, paste a response instead")
		return
	}
	p, notes, code, err := s.prepareDebugRun(r, req)
	if err != nil {
		refuse(code, "%v", err)
		return
	}
	select {
	case s.debugSlots <- struct{}{}:
		defer func() { <-s.debugSlots }()
	default:
		// Taken, and not run: as a probe its collector's trip limit refuses.
		s.countDebugRun(debugKindPage, debugFailure)
		writeDebugAnswer(w, http.StatusServiceUnavailable, debugAnswer{Error: fmt.Sprintf("the debug page already has %d runs in progress; try again when one has finished", debugMaxConcurrent)})
		return
	}
	report := s.runDebug(r.Context(), p)
	answer := debugAnswerOf(report)
	answer.Notes = append(notes, answer.Notes...)
	s.countDebugRun(debugKindPage, report.result())
	s.logger.Info("debug run served", "collector", p.collector.Name, "edited", p.edited, "on", p.source)
	writeDebugAnswer(w, http.StatusOK, answer)
}

// readDebugRunRequest reads and checks a run's request, and says with which
// status to refuse one that is wrong.
func readDebugRunRequest(w http.ResponseWriter, r *http.Request) (debugRunRequest, int, error) {
	var req debugRunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, debugMaxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return req, http.StatusRequestEntityTooLarge, fmt.Errorf("a debug run may be at most %d bytes", debugMaxRequestBytes)
		}
		return req, http.StatusBadRequest, fmt.Errorf("the run is not the JSON the debug page takes: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, http.StatusBadRequest, errors.New("the run is not the JSON the debug page takes: more follows the object")
	}
	source, sources := req.sources()
	switch {
	case req.Collector != nil && len(*req.Collector) > debugMaxCollectorBytes:
		return req, http.StatusRequestEntityTooLarge, fmt.Errorf("the collector is %d bytes; the debug page takes one of at most %d", len(*req.Collector), debugMaxCollectorBytes)
	case req.Body != nil && len(*req.Body) > debugMaxBodyBytes:
		return req, http.StatusRequestEntityTooLarge, fmt.Errorf("the pasted body is %d bytes; the debug page takes one of at most %d", len(*req.Body), debugMaxBodyBytes)
	case req.Collector != nil && req.Configured != "":
		return req, http.StatusBadRequest, errors.New("a run takes one collector: send collector, the YAML of one written here, or configured, the name of one of the configuration in force, not both")
	case req.Collector == nil && req.Configured == "":
		return req, http.StatusBadRequest, errors.New("a run needs a collector: send collector, the YAML of one written here, or configured, the name of one of the configuration in force")
	case sources > 1:
		return req, http.StatusBadRequest, errors.New("a run reads one response, not several: send body, a pasted response; target, with request_from for a written collector; or static_target")
	case sources == 0:
		return req, http.StatusBadRequest, errors.New("a run needs a response: send body, a pasted response; target, with request_from for a written collector; or static_target")
	case source != debugSourcePasted && (req.ContentType != "" || req.Status != 0 || len(req.Headers) > 0):
		return req, http.StatusBadRequest, errors.New("content_type, status and headers describe a pasted body; a fetched target's response has its own")
	case source == debugSourceStatic && len(req.Params) > 0:
		return req, http.StatusBadRequest, errors.New("params are not taken with static_target: a static target's scrape is made with the target's own settings")
	case req.Status != 0 && (req.Status < 100 || req.Status > 599):
		return req, http.StatusBadRequest, fmt.Errorf("status %d is no HTTP status; give one from 100 to 599", req.Status)
	case len(req.Headers) > debugMaxHeaders:
		return req, http.StatusBadRequest, fmt.Errorf("%d headers; a pasted response may have at most %d", len(req.Headers), debugMaxHeaders)
	}
	for name, value := range req.Headers {
		if !debugHeaderName.MatchString(name) {
			return req, http.StatusBadRequest, fmt.Errorf("header name %q is not one HTTP allows", name)
		}
		if err := fetch.CheckHeaderValue(value); err != nil {
			return req, http.StatusBadRequest, fmt.Errorf("header %s %w", name, err)
		}
	}
	for key := range req.Params {
		if probeIdentityParam(key) || key == probeDebugParam {
			return req, http.StatusBadRequest, fmt.Errorf("params holds %s, which the debug page sets itself: give the target as target, and the collector as collector, configured or request_from", key)
		}
	}
	return req, 0, nil
}

// prepareDebugRun resolves the run's collector and what it reads, checking
// its parameters as /probe checks a probe's: the run collector's for a
// pasted body, and those of the collector whose request fetches the target
// for a fetch. It returns, beside the run, the warnings a load would give
// for a collector written on the page.
func (s *Server) prepareDebugRun(r *http.Request, req debugRunRequest) (debugProbe, []string, int, error) {
	followed := s.reconcile()
	p := debugProbe{kind: debugKindPage, generation: followed.generation}
	var notes []string
	if req.Configured != "" {
		p.collector = followed.collectorOf(followed.config, req.Configured)
		if p.collector == nil {
			return p, nil, http.StatusBadRequest, fmt.Errorf("unknown collector %q in configured; name a collector of the configuration in force", req.Configured)
		}
	} else {
		cfg, err := config.ParseCollector([]byte(*req.Collector), refusedOnDebugPage)
		if err != nil {
			return p, nil, http.StatusUnprocessableEntity, err
		}
		p.collector, p.edited, notes = &cfg.Collectors[0], true, cfg.Warnings
	}
	c := p.collector
	source, _ := req.sources()
	if source == debugSourceStatic {
		static, code, err := s.staticDebugProbe(req.StaticTarget)
		if err != nil {
			return p, nil, code, err
		}
		through := static.collector
		static.kind, static.edited = debugKindPage, p.edited
		if p.edited || through.Name != c.Name {
			// Another collector reads what the target's fetches: the
			// target's request, read by the collector run.
			if !onDebugPage(through.Request.Type) {
				return p, nil, http.StatusBadRequest, fmt.Errorf("static target %q is scraped through collector %q, a %s collector; another collector reads what an http collector fetches only. Run collector %q itself on it", static.static.Name, through.Name, through.Request.Type, through.Name)
			}
			static.collector, static.requester, static.generation = c, through, p.generation
			// The stale result would be the target's collector's, not this
			// one's, and so would its backoff.
			static.staleKey, static.backoffKey = "", ""
		}
		static.source = fmt.Sprintf("static target %q, scraped through collector %q", static.static.Name, through.Name)
		return static, notes, 0, nil
	}
	query := url.Values{}
	for key, value := range req.Params {
		query.Set(key, value)
	}
	requester := c
	if source == debugSourceTarget {
		switch {
		case req.RequestFrom != "":
			requester = followed.collectorOf(followed.config, req.RequestFrom)
			if requester == nil {
				return p, nil, http.StatusBadRequest, fmt.Errorf("unknown collector %q in request_from; name a collector of the configuration in force", req.RequestFrom)
			}
		case p.edited:
			return p, nil, http.StatusBadRequest, errors.New("a collector written on the debug page makes no request of its own: name in request_from the configured collector to fetch the target through")
		}
		if (p.edited || requester.Name != c.Name) && !onDebugPage(requester.Request.Type) {
			return p, nil, http.StatusBadRequest, fmt.Errorf("collector %q in request_from is a %s collector; another collector reads what an http collector fetches only", requester.Name, requester.Request.Type)
		}
		target := ""
		if req.Target != nil {
			target = *req.Target
		}
		if err := fetch.CheckTarget(requester, target, false); err != nil {
			if errors.Is(err, fetch.ErrMissingTarget) {
				return p, nil, http.StatusBadRequest, fmt.Errorf("a target is required to fetch through collector %q", requester.Name)
			}
			return p, nil, http.StatusBadRequest, err
		}
		p.target = target
	}
	if err := checkProbeParams(query, probeRequestParam(requester)); err != nil {
		return p, nil, http.StatusBadRequest, err
	}
	overrides, err := fetch.ParseRequestOverrides(query)
	if err != nil {
		return p, nil, http.StatusBadRequest, err
	}
	if err := fetch.CheckOverrideParams(requester, query); err != nil {
		return p, nil, http.StatusBadRequest, err
	}
	if err := checkForwardedHeaderParams(query, requester.Request); err != nil {
		return p, nil, http.StatusBadRequest, err
	}
	for _, x := range []*model.Collector{requester, c} {
		if err := fetch.CheckPathParams(x, overrides); err != nil {
			return p, nil, http.StatusBadRequest, err
		}
	}
	budget, budgetSource := s.probeDeadline(r.Header, overrides)
	if budget <= 0 {
		budget, budgetSource = DefaultProbeTimeout, budgetFromDebugPage
	}
	p.overrides, p.budget, p.budgetSource = overrides, budget, budgetSource
	if source == debugSourcePasted {
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
		p.pasted = &fetch.HTTPResponse{StatusCode: status, Headers: headers, Body: []byte(*req.Body), Collector: c.Name}
		p.source = "a pasted response"
		return p, notes, 0, nil
	}
	// Only the header_<name> parameters are forwarded, and none of the
	// browser's own headers: the page's request carries the exporter's
	// credential and the browser's cookies, which are not the target's.
	p.forwarded = forwardedHeaders(&http.Request{Header: http.Header{}, URL: &url.URL{RawQuery: query.Encode()}}, requester.Request)
	p.logTarget = fetch.DisplayTarget(requester, p.target)
	p.source = fmt.Sprintf("target %s, fetched through collector %q", orNone(p.logTarget), requester.Name)
	p.method = fetch.RequestMethodFor(requester, overrides)
	if label, err := fetch.RequestLabelFor(p.target, requester, overrides); err == nil {
		p.requestURL = label
	}
	if requester != c {
		p.requester = requester
	} else if model.UsesCache(c) || c.FailureBackoff != nil {
		// A configured collector fetching for itself is a debug probe of it,
		// whose stale answer and backoff are the same probe's
		// (probeHandler).
		key := s.probeCacheKey(followed.config, c, p.target, probeKeyQuery(c.Name, p.target, overrides), p.forwarded)
		if model.UsesCache(c) {
			p.staleKey = key
		}
		if c.FailureBackoff != nil {
			p.backoffKey = key
		}
	}
	return p, notes, 0, nil
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
