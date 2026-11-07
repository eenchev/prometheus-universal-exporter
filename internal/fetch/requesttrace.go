package fetch

import (
	"context"
	"net/http"
	neturl "net/url"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A debug probe (/probe?debug=true) reports every request a trip sent: each
// attempt, each redirect followed and each gRPC call, with the headers as
// they went out and how each ended. The fetch functions record them in the
// RequestTrace of the context, when there is one; a trip without it records
// nothing. Credentials are recorded as sent: the caller redacts them before
// showing anything.
//
// The trace of a probe or a scrape exported over OTLP (otlp.traces) records
// the same requests, with WithSpanTrace, for its spans: each one's method,
// URL, page, status and times, and none of its headers, which no span shows
// and which would be a copy of every request's for nothing. Such a trip is
// no debug run (debugRun): it counts in every self-metric as it would
// without its trace.

// RequestTrace is the requests one trip sent, in order. spans says it is the
// trace of an exported trip's spans, which keeps no headers and is no debug
// run's.
type RequestTrace struct {
	mu       sync.Mutex
	requests []TracedRequest
	spans    bool
}

// TracedRequest is one request a trip sent.
type TracedRequest struct {
	Method string
	URL    string
	// Header is what went out, Host included when it was set.
	Header http.Header
	// Redirect is set on a request that followed a redirect.
	Redirect bool
	// Login is set on a request request.login made for its token, whose
	// body, holding the login's secrets, is never recorded (login.go).
	Login bool
	// Page is the page of request.pagination the request asked for, from
	// 1; 0 without request.pagination.
	Page int
	// Withheld names the headers of the first request that a redirect to a
	// host the collector does not trust was not sent (redirecttrust.go),
	// and WithheldWhy says why the redirect was not trusted.
	Withheld    []string
	WithheldWhy string
	// Outcome is how it ended: a status, a gRPC code, or the error, as a
	// debug report shows it. It may quote what the target sent — a
	// greeting, a command's stderr, a status line's reason — so a span,
	// which leaves the exporter as a log line does, never carries it: a
	// span has Failed and Facts instead.
	Outcome string
	// Failed says the request got no answer, or an answer its request type
	// takes for a failure: an error, or a connection lost before it was
	// answered. An HTTP answer's failure is its Status.
	Failed bool
	// Facts are what the request type tells of how the request ended, for
	// its span: numbers, yes-or-no answers and names of the exporter's own
	// vocabulary — a response code's, a TLS version's — that it worked out
	// of the answer, never text the target sent.
	Facts []TraceFact
	// Wait is, under request.retry.retry_after, how long the trip waited
	// before retrying the request and why, or why it was not retried when
	// a Retry-After asked for longer than its time left; and, under
	// request.rate_limit, how long the request waited for its turn before
	// it was sent; "" otherwise.
	Wait     string
	Duration time.Duration
	// Status is the HTTP status of the answer to an HTTP request, 0 for
	// one that got none and for a request of another type.
	Status int
	// RateLimitWait is how long the request waited for its turn under
	// request.rate_limit before it was sent.
	RateLimitWait time.Duration
	// Attempt is which attempt of the request it is under request.retry,
	// from 0; a redirect, a login and a page each begin at 0.
	Attempt int
	// SpanID is the span id the request was sent under in its traceparent
	// (otlp.traces.propagate), which its CLIENT span is made with; zero
	// when it sent none of the exporter's.
	SpanID [8]byte
	// Phases are the phases of an HTTP request that were timed, as
	// $timings has them (HTTPTrip): the name's lookup, the connection, the
	// TLS handshake, each only when the request made it, and the time to
	// the answer's first byte, from the request's start; nil when none
	// was, as for a request of another type, or one that got no
	// connection.
	Phases  []TracedPhase
	started time.Time
}

// TracedPhase is one phase of a request: dns, connect, tls or first_byte,
// with when it began and ended.
type TracedPhase struct {
	Name       string
	Start, End time.Time
}

// TraceFact is one fact of TracedRequest.Facts, under Key: an integer, a
// boolean, a floating-point number or a name, as Kind says.
type TraceFact struct {
	Key    string
	Kind   TraceFactKind
	Int    int64
	Bool   bool
	Double float64
	// Name is a word the exporter chose from a set of its own, such as the
	// name it maps a status code to: never text the target sent, which a
	// span does not carry.
	Name string
}

// TraceFactKind is which value of a TraceFact is its own.
type TraceFactKind uint8

// The kinds of TraceFact, which OTLP's attributes are of: intValue,
// boolValue, doubleValue and stringValue.
const (
	TraceFactInt TraceFactKind = iota
	TraceFactBool
	TraceFactDouble
	TraceFactName
)

// Start is when the request was sent: after any wait for its turn under
// request.rate_limit, which is no time the target took.
func (r TracedRequest) Start() time.Time { return r.started }

type requestTraceKey struct{}

// pageKey holds, in the context of a request request.pagination makes, the
// page it asks for, which the trace records with it.
type pageKey struct{}

// WithRequestTrace returns a context whose trips record their requests in
// the returned trace.
func WithRequestTrace(ctx context.Context) (context.Context, *RequestTrace) {
	t := &RequestTrace{}
	return context.WithValue(ctx, requestTraceKey{}, t), t
}

// WithSpanTrace returns a context whose trips record their requests in the
// returned trace as an exported trip's spans need them, without their
// headers, and are no debug run's.
func WithSpanTrace(ctx context.Context) (context.Context, *RequestTrace) {
	t := &RequestTrace{spans: true}
	return context.WithValue(ctx, requestTraceKey{}, t), t
}

// spanTraced reports whether ctx is an exported trip's (WithSpanTrace),
// whose requests' spans show their phases (TracedRequest.Phases).
func spanTraced(ctx context.Context) bool {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	return ok && t.spans
}

// tracePhases records phases on the last request recorded, the one being
// sent, unless it has them already.
func tracePhases(ctx context.Context, phases []TracedPhase) {
	if len(phases) == 0 {
		return
	}
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 && t.requests[n-1].Phases == nil {
		t.requests[n-1].Phases = phases
	}
}

// debugRun reports whether ctx is a debug run's — a debug probe, a debug
// scrape of a static target or a debug page's fetch — whose trips record
// their requests (WithRequestTrace) and count in no self-metric of the
// collector's (§ 42.17). An exported trip's
// trace (WithSpanTrace) is no debug run's.
func debugRun(ctx context.Context) bool {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	return ok && !t.spans
}

// Requests returns the requests recorded so far.
func (t *RequestTrace) Requests() []TracedRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TracedRequest(nil), t.requests...)
}

// traceRequest records a request about to be sent.
func traceRequest(ctx context.Context, method, url string, header http.Header, host string, redirect bool) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	if !t.spans {
		header = header.Clone()
		if header == nil {
			header = http.Header{}
		}
		// Host is shown when it is not the URL's own, as a request.headers
		// Host or a forwarded one makes it.
		if u, err := neturl.Parse(url); host != "" && (err != nil || u.Host != host) {
			header.Set("Host", host)
		}
	} else {
		header = nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	page, _ := ctx.Value(pageKey{}).(int)
	t.requests = append(t.requests, TracedRequest{Method: method, URL: url, Header: header, Redirect: redirect, Page: page, started: time.Now()})
}

// traceWithheld records the names of the headers the last request recorded,
// a redirect, was not sent, and why.
func traceWithheld(ctx context.Context, names []string, why string) {
	if len(names) == 0 {
		return
	}
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		t.requests[n-1].Withheld, t.requests[n-1].WithheldWhy = names, why
	}
}

// traceBodyError adds to the last request's outcome that its body could not
// be read, so a debug report shows why an answer that began was retried.
func traceBodyError(ctx context.Context, err error) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		r := &t.requests[n-1]
		r.Outcome += ", then the body broke off: " + model.CutTo(err.Error(), model.MaxFailureBytes, false)
		r.Duration = time.Since(r.started)
	}
}

// traceWait records, on the last request recorded, the wait before its
// retry or why it was not retried (TracedRequest.Wait).
func traceWait(ctx context.Context, wait string) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		t.requests[n-1].Wait = joinWaits(t.requests[n-1].Wait, wait)
	}
}

// traceOutcome records how the last request recorded ended, unless its end
// is recorded already.
func traceOutcome(ctx context.Context, outcome string) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 && t.requests[n-1].Outcome == "" {
		r := &t.requests[n-1]
		// An outcome is a status line or an error's text, either of which
		// the target can make as long as its headers may be: the report
		// shows no more of it than of any failure (model.MaxFailureBytes).
		r.Outcome, r.Duration = model.CutTo(outcome, model.MaxFailureBytes, false), time.Since(r.started)
	}
}

// traceFailedOutcome is traceOutcome of a request that failed: it got no
// answer, or lost its connection before one (TracedRequest.Failed).
func traceFailedOutcome(ctx context.Context, outcome string) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	if n := len(t.requests); n > 0 && t.requests[n-1].Outcome == "" {
		t.requests[n-1].Failed = true
	}
	t.mu.Unlock()
	traceOutcome(ctx, outcome)
}

// traceHTTPOutcome is traceOutcome of an HTTP answer, whose status it
// records too.
func traceHTTPOutcome(ctx context.Context, resp *http.Response) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	if n := len(t.requests); n > 0 && t.requests[n-1].Outcome == "" {
		t.requests[n-1].Status = resp.StatusCode
	}
	t.mu.Unlock()
	traceOutcome(ctx, resp.Status)
}

// traceAttempt records which attempt under request.retry the last request
// recorded is.
func traceAttempt(ctx context.Context, attempt int) {
	if attempt == 0 {
		return
	}
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		t.requests[n-1].Attempt = attempt
	}
}

// traceOutcomeNote adds note to the outcome of the last request recorded,
// as a 304 that a cached response answered for says so (revalidate.go).
func traceOutcomeNote(ctx context.Context, note string) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		t.requests[n-1].Outcome += note
	}
}
