//go:build !select_request_types || request_type_http

package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An http collector with request.then makes a chain of requests per trip,
// all to its one target: its own request, the chain's first, named first,
// then each step of request.then in order. A request reads values from its
// answer (request.extract, a step's extract), which the later steps send as
// {{chain.<name>}} (chaintemplate.go). A step with for_each makes one
// request per entry of a list value, a fan-out, concurrency at a time and
// at most max_items of them.
//
// Every request is one the collector's own code makes (fetch), with the
// collector's credentials, signing, TLS settings, proxy, retries, rate
// limits and target policy. What the chain adds goes through the context
// (chainreport.go): every request and redirect keeps to the target's
// origin, the requests share a cookie jar, and a login logs in again at
// most once per trip. The whole chain shares one budget, the trip's
// deadline: a probe's timeout bounds the chain, not each request.
//
// The trip's response is the last step's answer (answer: last), or one
// JSON document of every answer (answer: all), keyed by step name, with
// the first request's status and headers. The response carries a
// ChainReport of how each step went, and a failed chain's error is a
// ChainError, which carries it too.

// chainRun is one trip of a chain.
type chainRun struct {
	ctx       context.Context
	target    string
	c         *model.Collector
	overrides RequestOverrides
	forwarded http.Header
	own       HTTPConfig
	// origin is the first request's URL.
	origin *url.URL
	// values are the values read so far, by name.
	values map[string]chainValueRead
	report *ChainReport
	// answers are each step's answer, for answer: all.
	answers []json.RawMessage
	// pasted are the answers pasted on the debug page, by step name, for a
	// chain run on them (pastedChain); nil for one that asks the target.
	pasted map[string]*HTTPResponse
}

// ask sends a request of the chain, c's with overrides, and returns its
// answer: the target's, or the one pasted for the step.
func (r *chainRun) ask(ctx context.Context, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	if r.pasted == nil {
		return fetch(ctx, r.target, c, overrides, forwarded)
	}
	countChainRequest(ctx)
	step := chainStepOf(ctx).name
	pasted, ok := r.pasted[step]
	if !ok {
		return nil, errors.New("no answer was pasted for it")
	}
	if limit := responseLimit(c); int64(len(pasted.Body)) > limit {
		return nil, model.MarkError(fmt.Errorf("response size %d exceeds limit %d", len(pasted.Body), limit), model.ErrLimitExceeded)
	}
	answer := *pasted
	answer.Headers = pasted.Headers.Clone()
	if u, err := requestURL(r.target, c, overrides); err == nil {
		answer.Trip = &HTTPTrip{URL: u}
	}
	return &answer, nil
}

// chainValueRead is a value read from an answer: its value, text or a
// number or, for a json value, a list; the URL of the answer it came from,
// against which a relative link is resolved; and whether it is secret.
type chainValueRead struct {
	value  any
	from   *url.URL
	secret bool
}

// fetchChain makes the requests of a chain and returns the trip's
// response.
func fetchChain(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	// The probe's timeout covers every request, so it is applied once,
	// here, and not again by each request's fetch.
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
		overrides.Timeout = 0
	}
	ctx = expr.WithMemoryLimit(ctx, c.Limits.MaxExpressionMemory)
	return runChain(ctx, target, c, overrides, forwarded, nil)
}

// runChain makes a chain's requests, or reads the answers pasted for them.
func runChain(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header, pasted map[string]*HTTPResponse) (*HTTPResponse, error) {
	own := httpRequest(c)
	origin, err := resolveRequestURL(target, c, overrides)
	if err != nil {
		return nil, err
	}
	r := &chainRun{ctx: withChainTrip(ctx, origin), target: target, c: c, overrides: overrides, forwarded: forwarded, own: own, origin: origin, values: map[string]chainValueRead{}, pasted: pasted}
	r.report = &ChainReport{Answer: c.Answer}
	first := ChainStepReport{Name: model.ChainFirst, Method: ConfiguredMethod(c), State: ChainStepNotReached}
	if overrides.Method != "" {
		first.Method = overrides.Method
	}
	r.report.Steps = append(r.report.Steps, first)
	for _, step := range own.Then {
		r.report.Steps = append(r.report.Steps, ChainStepReport{Name: step.Name, Method: step.Method, State: ChainStepNotReached, ForEach: step.ForEach != "", OnItemError: step.OnItemError})
	}
	start := time.Now()
	response, err := r.run()
	if response != nil {
		response.Chain = r.report
		response.Duration = time.Since(start)
	}
	return response, err
}

// run makes the chain's requests, each step after the one before.
func (r *chainRun) run() (*HTTPResponse, error) {
	first, done, err := r.first()
	if err != nil || done {
		return first, err
	}
	last := first
	for i, step := range r.own.Then {
		report := &r.report.Steps[i+1]
		var response *HTTPResponse
		if step.ForEach != "" {
			response, done, err = r.fanOut(step, report)
		} else {
			response, done, err = r.single(step, report)
		}
		if err != nil || done {
			return response, err
		}
		if response != nil {
			last = response
		}
	}
	if r.c.Answer == model.ChainAnswerAll {
		return r.document(first)
	}
	return last, nil
}

// first makes the chain's first request, the collector's own. done says
// the trip ends with the response, which is the first answer, with a
// status the collector does not accept.
func (r *chainRun) first() (response *HTTPResponse, done bool, err error) {
	report := &r.report.Steps[0]
	state := &chainStepState{name: model.ChainFirst, requests: new(atomic.Int64)}
	report.Started = time.Now()
	response, err = r.ask(withChainStep(r.ctx, state), r.c, r.overrides, r.forwarded)
	report.Duration, report.Requests = time.Since(report.Started), int(state.requests.Load())
	report.URL = RedactURL(r.origin, MaskCredentialQueryValues)
	if err != nil {
		return nil, true, r.failed(report, -1, 0, "", err)
	}
	r.answered(report, response)
	if !AcceptedStatus(r.c, r.overrides, response.StatusCode) {
		return r.refusedStatus(report, response, -1, 0, ""), true, nil
	}
	if err := r.read(report, model.ChainFirst, r.own.Extract, response); err != nil {
		return nil, true, r.failed(report, -1, 0, "", err)
	}
	r.masked(report)
	if err := r.keep(report, response); err != nil {
		return nil, true, r.failed(report, -1, 0, "", err)
	}
	report.State = ChainStepOK
	return response, false, nil
}

// single makes a step that is no fan-out.
func (r *chainRun) single(step model.ChainStep, report *ChainStepReport) (response *HTTPResponse, done bool, err error) {
	params := r.params()
	state := &chainStepState{name: step.Name, requests: new(atomic.Int64)}
	report.Started = time.Now()
	response, shown, err := r.send(withChainStep(r.ctx, state), step, params)
	report.Duration, report.Requests, report.URL = time.Since(report.Started), int(state.requests.Load()), shown
	if err != nil {
		return nil, true, r.failed(report, -1, 0, "", err)
	}
	r.answered(report, response)
	if !stepAccepts(r.c, r.overrides, step, response.StatusCode) {
		return r.refusedStatus(report, response, -1, 0, ""), true, nil
	}
	if err := r.read(report, step.Name, step.Extract, response); err != nil {
		return nil, true, r.failed(report, -1, 0, "", err)
	}
	r.masked(report)
	if err := r.keep(report, response); err != nil {
		return nil, true, r.failed(report, -1, 0, "", err)
	}
	report.State = ChainStepOK
	return response, false, nil
}

// masked writes the secret values a step read as *** in what its report
// shows of its answer: the start of its body and its headers.
func (r *chainRun) masked(report *ChainStepReport) {
	report.Excerpt = maskSecrets(r.ctx, report.Excerpt)
	report.Headers = maskHeaderSecrets(r.ctx, report.Headers)
}

// answered notes a step's answer in its report.
func (r *chainRun) answered(report *ChainStepReport, response *HTTPResponse) {
	report.Status, report.Headers, report.Bytes, report.Trip = response.StatusCode, response.Headers, len(response.Body), response.Trip
	report.Excerpt = chainExcerpt(response.Body)
}

// chainExcerpt is the start of an answer, as a debug report shows it.
func chainExcerpt(body []byte) string {
	return model.CutTo(string(body), 512, false)
}

// stepAccepts reports whether a step takes status as an answer: by its own
// accept_status, or the collector's.
func stepAccepts(c *model.Collector, overrides RequestOverrides, step model.ChainStep, status int) bool {
	if step.AcceptStatus != nil {
		sc := *c
		sc.Request.AcceptStatus = step.AcceptStatus
		overrides.AcceptStatus = nil
		return AcceptedStatus(&sc, overrides, status)
	}
	return AcceptedStatus(c, overrides, status)
}

// refusedStatus is the trip's response when a step, or an item of a
// fan-out, is answered with a status it does not accept: that answer,
// which the exporter fails as such, its error starting with the step.
func (r *chainRun) refusedStatus(report *ChainStepReport, response *HTTPResponse, item, items int, each string) *HTTPResponse {
	report.State = ChainStepFailed
	if report.Error == "" {
		report.Error = fmt.Sprintf("received HTTP status %d", response.StatusCode)
	}
	r.where(report.Name, item, items, each)
	return response
}

// where notes where the trip failed.
func (r *chainRun) where(step string, item, items int, each string) {
	r.report.FailedStep = step
	r.report.Failed = fmt.Sprintf("step %q", step)
	if item >= 0 {
		r.report.FailedItem, r.report.Items = item+1, items
		r.report.Failed = fmt.Sprintf("step %q item %d of %d (%q)", step, item+1, items, model.CutTo(each, 64, false))
	}
}

// failed notes in the report that a step failed with err, and returns the
// trip's error, which starts with the step.
func (r *chainRun) failed(report *ChainStepReport, item, items int, each string, err error) error {
	err = r.shownError(err)
	report.State = ChainStepFailed
	if ranOut(r.ctx, err) {
		report.State, report.Deadline = ChainStepTimedOut, "the trip's deadline"
	}
	if report.Error == "" {
		report.Error = model.CutTo(err.Error(), model.MaxFailureBytes, false)
	}
	r.where(report.Name, item, items, each)
	return &ChainError{Report: r.report, err: err}
}

// shownError is err as the chain shows it: a placeholder of a value named
// as the configuration writes it, {{chain.id}} and not the parameter it is
// filled in through, and each value marked secret written as ***.
func (r *chainRun) shownError(err error) error {
	if err == nil {
		return nil
	}
	text := maskSecrets(r.ctx, chainNames.Replace(err.Error()))
	if text == err.Error() {
		return err
	}
	return &chainShownError{text: text, err: err}
}

// chainShownError is an error with the text the chain shows of it, which
// is still the error it wraps (errors.Is).
type chainShownError struct {
	text string
	err  error
}

func (e *chainShownError) Error() string { return e.text }

func (e *chainShownError) Unwrap() error { return e.err }

// chainNames writes the parameters a chain fills its placeholders through
// (chaintemplate.go) as the placeholders the configuration writes.
var chainNames = strings.NewReplacer(chainParamPrefix+"chain_", "chain.", chainParamPrefix+"each__", "each.", chainParamPrefix+"each_index", "each_index", chainParamPrefix+"each", "each")

// ranOut reports whether err is the trip's deadline running out.
func ranOut(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
}

// params are the parameters a step's placeholders are filled from: the
// probe's, but those of the names the chain keeps for itself, and the
// values read so far.
func (r *chainRun) params() map[string]string {
	params := make(map[string]string, len(r.overrides.Params)+len(r.values))
	for name, value := range r.overrides.Params {
		if !strings.HasPrefix(name, chainParamPrefix) {
			params[name] = value
		}
	}
	for name, value := range r.values {
		params[chainValueParam(name)] = chainText(value.value)
	}
	return params
}

// reportedText is a value as a report keeps it: *** for a secret one, which
// is never kept.
func reportedText(v any, secret bool) string {
	if secret {
		return "***"
	}
	return chainText(v)
}

// chainText is a value as a placeholder writes it: text as it is, a number
// as JSON writes it, and a list or an object as its JSON text.
func chainText(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// send sends a step's request with params, and returns its answer and
// the URL it was sent to, masked.
func (r *chainRun) send(ctx context.Context, step model.ChainStep, params map[string]string) (*HTTPResponse, string, error) {
	sc, overrides, err := r.stepRequest(step, params)
	if err != nil {
		return nil, "", err
	}
	shown := ""
	if u, err := requestURL(r.target, sc, overrides); err == nil {
		shown = RedactURL(u, MaskCredentialQueryValues)
	}
	response, err := r.ask(ctx, sc, overrides, stepForwarded(step, r.forwarded))
	return response, maskSecrets(r.ctx, shown), err
}

// stepForwarded are the forwarded headers a step sends: every one, or with
// forwarded_headers: false only those that are no header of the probe's
// choosing: a forwarded Authorization (forward_authorization), the
// collector's credential, and a forwarded Host, which keeps the step on the
// first request's virtual host.
func stepForwarded(step model.ChainStep, forwarded http.Header) http.Header {
	if step.ForwardedHeaders == nil || *step.ForwardedHeaders || forwarded == nil {
		return forwarded
	}
	kept := http.Header{}
	for name, values := range forwarded {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Host") {
			kept[name] = values
		}
	}
	return kept
}

// stepRequest is the collector and the overrides a step's request is made
// with: the collector's own, with the step's path or link, method, query,
// headers, body and accepted statuses, and the probe's overrides that the
// first request alone takes left out.
func (r *chainRun) stepRequest(step model.ChainStep, params map[string]string) (*model.Collector, RequestOverrides, error) {
	at := "step " + strconv.Quote(step.Name)
	sc := *r.c
	sc.Request = r.c.Request
	overrides := r.overrides
	overrides.Method, overrides.Body, overrides.Params = "", nil, params
	if step.Path != "" || step.URL != "" {
		overrides.Path, overrides.PathSet = "", false
	}
	if step.Path != "" {
		path, _, err := rewriteChainRefs(at+" path", step.Path)
		if err != nil {
			return nil, overrides, err
		}
		sc.Request.Path = path
	}
	if step.URL != "" {
		link, err := r.link(step)
		if err != nil {
			return nil, overrides, err
		}
		overrides.pageURL = link
	}
	query, err := rewrittenValues(at+" query", step.Query)
	if err != nil {
		return nil, overrides, err
	}
	sc.Request.Query = query
	headers, err := rewrittenValues(at+" headers", step.Headers)
	if err != nil {
		return nil, overrides, err
	}
	sc.Request.Headers = mergedHeaders(r.c.Request.Headers, headers)
	body, contentType, err := stepBody(at, step, params)
	if err != nil {
		return nil, overrides, err
	}
	if contentType != "" && !hasHeader(sc.Request.Headers, "Content-Type") {
		sc.Request.Headers["Content-Type"] = contentType
	}
	if body != "" {
		overrides.Body = &body
	}
	own := r.own
	own.Method, own.Body, own.Pagination, own.Extract, own.Then = step.Method, "", nil, nil, nil
	sc.Request.Own = own
	if step.AcceptStatus != nil {
		sc.Request.AcceptStatus = step.AcceptStatus
		overrides.AcceptStatus = nil
	}
	if overrides.pageURL != nil && len(query) > 0 {
		// A link's query is kept, and the step's added to it.
		u := *overrides.pageURL
		rendered, err := renderedValues(query, "query", at+" query.", params)
		if err != nil {
			return nil, overrides, err
		}
		values := u.Query()
		for name, value := range rendered {
			values.Set(name, value)
		}
		u.RawQuery = values.Encode()
		overrides.pageURL = &u
	}
	return &sc, overrides, nil
}

// rewrittenValues is a step's header or query values, their chain
// placeholders rewritten.
func rewrittenValues(where string, values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for name, value := range values {
		rewritten, _, err := rewriteChainRefs(where+"."+name, value)
		if err != nil {
			return nil, err
		}
		out[name] = rewritten
	}
	return out, nil
}

// mergedHeaders is the collector's headers with a step's added, each
// replacing one of the same name.
func mergedHeaders(collector, step map[string]string) map[string]string {
	out := make(map[string]string, len(collector)+len(step)+1)
	for name, value := range collector {
		out[name] = value
	}
	for name, value := range step {
		for other := range out {
			if strings.EqualFold(other, name) {
				delete(out, other)
			}
		}
		out[name] = value
	}
	return out
}

// hasHeader reports whether headers sets name, in any case.
func hasHeader(headers map[string]string, name string) bool {
	for other := range headers {
		if strings.EqualFold(other, name) {
			return true
		}
	}
	return false
}

// stepBody is a step's body with its placeholders filled in, and the
// Content-Type of a form or a JSON body.
func stepBody(at string, step model.ChainStep, params map[string]string) (body, contentType string, err error) {
	switch {
	case step.Body != "":
		rewritten, _, err := rewriteChainRefs(at+" body", step.Body)
		if err != nil {
			return "", "", err
		}
		body, err = templateField{at + " body", rewritten, "body"}.render(params)
		return body, "", err
	case step.Form != nil:
		rendered, err := renderedStepValues(at+" form", step.Form, params)
		if err != nil {
			return "", "", err
		}
		form := url.Values{}
		for name, value := range rendered {
			form.Set(name, value)
		}
		return form.Encode(), "application/x-www-form-urlencoded", nil
	case step.JSON != nil:
		rendered, err := renderedStepValues(at+" json", step.JSON, params)
		if err != nil {
			return "", "", err
		}
		encoded, err := json.Marshal(rendered)
		if err != nil {
			return "", "", fmt.Errorf("%s json: %w", at, err)
		}
		return string(encoded), "application/json", nil
	}
	return "", "", nil
}

// renderedStepValues is a step's form or json values with their
// placeholders filled in, each value as text.
func renderedStepValues(where string, values map[string]string, params map[string]string) (map[string]string, error) {
	rewritten, err := rewrittenValues(where, values)
	if err != nil {
		return nil, err
	}
	return renderedValues(rewritten, "arg", where+".", params)
}

// link is the URL a step's url names: the link a step read, resolved
// against the answer it came from, which must have the target's origin.
func (r *chainRun) link(step model.ChainStep) (*url.URL, error) {
	name := chainURLPattern.FindStringSubmatch(step.URL)[1]
	value := r.values[name]
	text := chainText(value.value)
	from := value.from
	if from == nil {
		from = r.origin
	}
	parsed, err := url.Parse(strings.TrimSpace(text))
	if err != nil || text == "" {
		return nil, fmt.Errorf("the value %q is not a link: %q", name, shownValue(value, text))
	}
	link := from.ResolveReference(parsed)
	if !sameOrigin(r.origin, link) {
		onHost, err := onVirtualHost(r.c, r.overrides, r.forwarded, r.origin, link)
		if err != nil {
			return nil, err
		}
		if onHost == nil {
			return nil, fmt.Errorf("the link %q leads to another origin than the target's, %s; a chain asks one target only: %w", shownLink(value, link), originOf(r.origin), ErrTargetRefused)
		}
		link = onHost
	}
	return link, nil
}

// shownValue is a value as an error may quote it: *** when it is secret.
func shownValue(value chainValueRead, text string) string {
	if value.secret {
		return "***"
	}
	return model.CutTo(text, 128, false)
}

// shownLink is a link as an error may quote it: its query masked, and ***
// when the value is secret.
func shownLink(value chainValueRead, link *url.URL) string {
	if value.secret {
		return "***"
	}
	return RedactURL(link, MaskQueryValues)
}

// read reads the values of a step's extract from its answer.
func (r *chainRun) read(report *ChainStepReport, step string, extract map[string]model.ChainExtract, response *HTTPResponse) error {
	for _, name := range model.SortedKeys(extract) {
		e := extract[name]
		value, found, err := extractChainValue(r.ctx, e, response)
		if err != nil {
			return err
		}
		if !found && !e.Optional {
			return fmt.Errorf("value %q was not found in the answer", name)
		}
		var from *url.URL
		if response.Trip != nil {
			from = response.Trip.URL
		}
		r.values[name] = chainValueRead{value: value, from: from, secret: e.Secret}
		if e.Secret && found {
			chainTripOf(r.ctx).keepSecret(value)
		}
		report.Values = append(report.Values, ChainValueReport{Name: name, Text: reportedText(value, e.Secret), Secret: e.Secret, Found: found})
	}
	return nil
}

// extractChainValue reads one value of an answer, as a steps login reads
// one (extractLoginValue), but a json value may be a list too. found is
// false for a value that is missing or empty. An error says what was not
// found, never what was.
func extractChainValue(ctx context.Context, e model.ChainExtract, response *HTTPResponse) (value any, found bool, err error) {
	if e.JSON == "" {
		resp := &http.Response{StatusCode: response.StatusCode, Header: response.Headers}
		// The only error is a value not found, which the caller words
		// with the value's name.
		read, notFound := extractLoginValue(ctx, e.Place(), resp, response.Body, time.Now())
		return read.Text, notFound == nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, false, errors.New("the answer is not JSON, which a json value is read from")
	}
	resp := &http.Response{StatusCode: response.StatusCode, Header: response.Headers}
	found2, kind, err := jqOne(ctx, e.JSON, document, resp)
	if err != nil {
		return nil, false, fmt.Errorf("the json value %q %w", e.JSON, chainJQError(err))
	}
	switch v := found2.(type) {
	case nil:
		return nil, false, nil
	case string:
		return v, v != "", nil
	case json.Number:
		return v, true, nil
	case []any:
		return v, true, nil
	}
	return nil, false, fmt.Errorf("the json value %q gave %s; it must give text, a number or a list", e.JSON, kind)
}

// chainJQError is a jq error of a value, in the chain's words.
func chainJQError(err error) error {
	if err != nil && err.Error() == "failed on the login's answer" {
		return errors.New("failed on the answer")
	}
	return err
}

// keep keeps a step's answer for the document of answer: all.
func (r *chainRun) keep(report *ChainStepReport, response *HTTPResponse) error {
	if r.c.Answer != model.ChainAnswerAll {
		return nil
	}
	body, err := documentBody(report.Method, response.Body)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}{response.StatusCode, body})
	if err != nil {
		return err
	}
	r.answers = append(r.answers, encoded)
	return nil
}

// documentBody is an answer's body as the document of answer: all holds
// it: null for a HEAD request's, the JSON as it came otherwise.
func documentBody(method string, body []byte) (json.RawMessage, error) {
	if method == http.MethodHead {
		return json.RawMessage("null"), nil
	}
	if !json.Valid(body) {
		return nil, errors.New("the answer is not JSON, which answer: all makes one JSON document of")
	}
	return json.RawMessage(bytes.TrimSpace(body)), nil
}

// document is the trip's response under answer: all: the first answer,
// with a JSON document of every answer, keyed by step name, as its body.
func (r *chainRun) document(first *HTTPResponse) (*HTTPResponse, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	names := append([]string{model.ChainFirst}, stepNames(r.own.Then)...)
	for i, answer := range r.answers {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(names[i])
		b.Write(key)
		b.WriteByte(':')
		b.Write(answer)
	}
	b.WriteByte('}')
	if limit := responseLimit(r.c); int64(b.Len()) > limit {
		return nil, model.MarkError(model.Errorf("the document of answer: all comes to %d bytes, which exceeds the response limit %d", model.Size(b.Len()), limit), model.ErrLimitExceeded)
	}
	response := *first
	response.Body = b.Bytes()
	response.Headers = first.Headers.Clone()
	response.Headers.Del("Content-Length")
	response.Headers.Del("Content-Encoding")
	response.Headers.Set("Content-Type", "application/json")
	return &response, nil
}

// stepNames are the names of steps, in order.
func stepNames(steps []model.ChainStep) []string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = step.Name
	}
	return names
}

// fanOut makes a fan-out step: one request per entry of its list, up to
// max_items, concurrency at a time.
func (r *chainRun) fanOut(step model.ChainStep, report *ChainStepReport) (*HTTPResponse, bool, error) {
	report.Started = time.Now()
	list, ok := r.values[step.ForEach].value.([]any)
	if !ok && r.values[step.ForEach].value != nil {
		return nil, true, r.failed(report, -1, 0, "", fmt.Errorf("the value %q is not a list; for_each goes over a list", step.ForEach))
	}
	report.ItemsTotal = len(list)
	if len(list) > step.MaxItems {
		report.ItemsCut = len(list) - step.MaxItems
		list = list[:step.MaxItems]
	}
	items := make([]chainItem, len(list))
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	turns := make(chan struct{}, step.Concurrency)
	var wg sync.WaitGroup
	var failedOnce sync.Once
	failedAt := -1
	requests := new(atomic.Int64)
	for i, entry := range list {
		if ctx.Err() != nil {
			break
		}
		select {
		case turns <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		items[i].started = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-turns }()
			done := r.item(ctx, step, i, len(list), entry, requests)
			done.started = true
			items[i] = done
			if done.fails && step.OnItemError == model.ChainItemFail {
				failedOnce.Do(func() {
					failedAt = i
					cancel()
				})
			}
		}()
	}
	wg.Wait()
	for i := range items {
		if items[i].merge != nil {
			items[i].merge()
		}
	}
	report.Duration, report.Requests = time.Since(report.Started), int(requests.Load())
	return r.fannedOut(step, report, items, list, failedAt)
}

// chainItem is one item of a fan-out: how it went, and its answer.
type chainItem struct {
	started  bool
	report   ChainItemReport
	response *HTTPResponse
	values   map[string]any
	// fails says the item failed; err is its error, nil for a status it
	// does not accept. cancelled says it was cancelled when another item
	// failed first.
	fails, cancelled bool
	err              error
	merge            func()
}

// item makes one item of a fan-out.
func (r *chainRun) item(ctx context.Context, step model.ChainStep, i, items int, entry any, requests *atomic.Int64) chainItem {
	item := chainItem{report: ChainItemReport{Index: i, Each: chainText(entry), Entry: entry}, merge: func() {}}
	if r.values[step.ForEach].secret {
		// The entries of a secret list are shown as *** too.
		item.report.Each = "***"
	}
	params := r.params()
	chainEachParams(params, entry, i)
	state := &chainStepState{name: step.Name, item: i + 1, requests: requests}
	ctx, item.merge = withItemTrace(ctx)
	start := time.Now()
	response, shown, err := r.send(withChainStep(ctx, state), step, params)
	item.report.Duration, item.report.URL = time.Since(start), shown
	if err != nil && ctx.Err() != nil && r.ctx.Err() == nil {
		// Cancelled, not failed: another item failed the fan-out first,
		// under on_item_error: fail.
		item.cancelled, item.report.Error = true, "cancelled: another item failed first"
		return item
	}
	if err != nil {
		err = r.shownError(err)
		item.fails, item.err, item.report.Error = true, err, model.CutTo(err.Error(), model.MaxFailureBytes, false)
		return item
	}
	item.response = response
	item.report.Status, item.report.Headers, item.report.Bytes, item.report.Trip = response.StatusCode, response.Headers, len(response.Body), response.Trip
	if !stepAccepts(r.c, r.overrides, step, response.StatusCode) {
		item.fails, item.report.Error = true, fmt.Sprintf("received HTTP status %d", response.StatusCode)
		return item
	}
	item.values = map[string]any{}
	for _, name := range model.SortedKeys(step.Extract) {
		e := step.Extract[name]
		value, found, err := extractChainValue(ctx, e, response)
		if err == nil && !found && !e.Optional {
			err = fmt.Errorf("value %q was not found in the answer", name)
		}
		if err != nil {
			item.fails, item.err, item.report.Error = true, err, err.Error()
			return item
		}
		item.values[name] = value
	}
	if r.c.Answer == model.ChainAnswerAll {
		body, err := documentBody(step.Method, response.Body)
		if err != nil {
			item.fails, item.err, item.report.Error = true, err, err.Error()
			return item
		}
		item.report.Body = body
	}
	return item
}

// fannedOut ends a fan-out step: it reads the values its items read, as
// lists, keeps the answers for answer: all, and fails the trip when an
// item failed under on_item_error: fail.
func (r *chainRun) fannedOut(step model.ChainStep, report *ChainStepReport, items []chainItem, list []any, failedAt int) (*HTTPResponse, bool, error) {
	var last *HTTPResponse
	var entries []json.RawMessage
	values := map[string][]any{}
	for i := range items {
		item := &items[i]
		if !item.started {
			continue
		}
		report.Items = append(report.Items, item.report)
		if item.response != nil {
			last = item.response
			report.Status, report.Headers, report.Bytes = item.report.Status, item.report.Headers, item.report.Bytes
		}
		if item.fails {
			report.ItemsFailed++
			if report.Error == "" {
				report.Error = fmt.Sprintf("item %d of %d (%q): %s", i+1, len(list), model.CutTo(item.report.Each, 64, false), item.report.Error)
			}
		}
		for _, name := range model.SortedKeys(step.Extract) {
			values[name] = append(values[name], item.values[name])
		}
		entries = append(entries, itemEntry(item))
	}
	if len(report.Items) > 0 {
		report.URL = report.Items[0].URL
	}
	done, cut := 0, false
	for i := range items {
		if items[i].started && !items[i].fails && !items[i].cancelled {
			done++
		} else if !items[i].started || ranOut(r.ctx, items[i].err) {
			cut = true
		}
	}
	if r.ctx.Err() != nil && cut {
		// The deadline came before every item was answered; one that
		// comes after does not fail the fan-out.
		report.State, report.Deadline = ChainStepTimedOut, "the trip's deadline"
		err := fmt.Errorf("the trip's deadline came after %d of %d items: %w", done, len(list), r.ctx.Err())
		report.Error = err.Error()
		r.where(step.Name, -1, 0, "")
		return nil, true, &ChainError{Report: r.report, err: err}
	}
	if failedAt >= 0 {
		item := &items[failedAt]
		r.where(step.Name, failedAt, len(list), item.report.Each)
		report.State = ChainStepFailed
		if item.err == nil {
			return item.response, true, nil
		}
		if ranOut(r.ctx, item.err) {
			report.State, report.Deadline = ChainStepTimedOut, "the trip's deadline"
		}
		return nil, true, &ChainError{Report: r.report, err: item.err}
	}
	for name, list := range values {
		r.values[name] = chainValueRead{value: list, secret: step.Extract[name].Secret}
		if step.Extract[name].Secret {
			chainTripOf(r.ctx).keepSecret(list)
		}
		report.Values = append(report.Values, ChainValueReport{Name: name, Text: reportedText(list, step.Extract[name].Secret), Secret: step.Extract[name].Secret, Found: true})
	}
	sortValueReports(report.Values)
	if r.c.Answer == model.ChainAnswerAll {
		encoded, err := json.Marshal(entries)
		if err != nil {
			return nil, true, r.failed(report, -1, 0, "", err)
		}
		if entries == nil {
			encoded = []byte("[]")
		}
		r.answers = append(r.answers, encoded)
	}
	report.State = ChainStepOK
	if report.ItemsFailed > 0 {
		report.State = ChainStepFailed
	}
	return last, false, nil
}

// sortValueReports sorts values by name.
func sortValueReports(values []ChainValueReport) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].Name < values[j-1].Name; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// itemEntry is an item's entry in the document of answer: all.
func itemEntry(item *chainItem) json.RawMessage {
	var encoded []byte
	if item.fails {
		encoded, _ = json.Marshal(struct {
			Each  any    `json:"each"`
			Error string `json:"error"`
		}{item.report.Entry, item.report.Error})
	} else {
		encoded, _ = json.Marshal(struct {
			Each   any             `json:"each"`
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		}{item.report.Entry, item.report.Status, item.report.Body})
	}
	return encoded
}

func init() { pastedChain = runPastedChain }

// runPastedChain runs a chain on answers pasted on the debug page: first is
// the first request's, and steps each step's by name, a fan-out step's
// used for every item. No request is sent. The report shows the URLs the
// chain would ask target for.
func runPastedChain(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, first *HTTPResponse, steps map[string]*HTTPResponse) (*HTTPResponse, error) {
	pasted := map[string]*HTTPResponse{model.ChainFirst: first}
	for name, answer := range steps {
		pasted[name] = answer
	}
	return runChain(ctx, target, c, overrides, nil, pasted)
}
