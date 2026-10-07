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

// RequestTrace is the requests one trip sent, in order.
type RequestTrace struct {
	mu       sync.Mutex
	requests []TracedRequest
}

// TracedRequest is one request a trip sent.
type TracedRequest struct {
	Method string
	URL    string
	// Header is what went out, Host included when it was set.
	Header http.Header
	// Redirect is set on a request that followed a redirect.
	Redirect bool
	// Withheld names the headers of the first request that a redirect to a
	// host the collector does not trust was not sent (redirecttrust.go),
	// and WithheldWhy says why the redirect was not trusted.
	Withheld    []string
	WithheldWhy string
	// Outcome is how it ended: a status, a gRPC code, or the error.
	Outcome  string
	Duration time.Duration
	started  time.Time
}

type requestTraceKey struct{}

// WithRequestTrace returns a context whose trips record their requests in
// the returned trace.
func WithRequestTrace(ctx context.Context) (context.Context, *RequestTrace) {
	t := &RequestTrace{}
	return context.WithValue(ctx, requestTraceKey{}, t), t
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
	header = header.Clone()
	if header == nil {
		header = http.Header{}
	}
	// Host is shown when it is not the URL's own, as a request.headers Host
	// or a forwarded one makes it.
	if u, err := neturl.Parse(url); host != "" && (err != nil || u.Host != host) {
		header.Set("Host", host)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests = append(t.requests, TracedRequest{Method: method, URL: url, Header: header, Redirect: redirect, started: time.Now()})
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
