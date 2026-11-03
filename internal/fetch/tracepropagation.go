package fetch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// With otlp.traces.propagate, a trip's requests to its target carry the
// trip's W3C trace context, so that the target's own spans are children of
// the exporter's: an HTTP request (a graphite request is one) its
// traceparent and tracestate headers, and a gRPC call the same as its
// metadata. The exporter puts what to send in the trip's context
// (WithTracePropagation); a trip without it sends nothing of the kind.
//
// Two ways, as the exporter's trace export is on or off:
//
//   - a trace of its own: each request is sent with a span id of its own,
//     which its CLIENT span is then made with (TracedRequest.SpanID), so
//     the target's span is the child of that one;
//   - a caller's trace passed on: with the trace export off, the
//     traceparent and tracestate the probe's caller sent are sent as they
//     came, and the target's span is a child of the caller's.
//
// A traceparent the request sets already — one of request.headers, or a
// forwarded one (forward_headers) — is the operator's and is sent as it is,
// with no tracestate of the exporter's beside it. A login's request
// (request.login) carries none: it goes to an identity provider, often no
// part of the target, and the token it fetches is kept for later probes,
// whose traces it is no part of.

// TracePropagation is what a trip's requests carry under
// otlp.traces.propagate. TraceID set is a trace of the exporter's own:
// each request's traceparent names it, the request's own span id and, as
// Sampled says, the sampled flag, with State, when set, as its tracestate.
// TraceID zero passes Parent and ParentState, the caller's traceparent and
// tracestate, on as they came.
type TracePropagation struct {
	TraceID     [16]byte
	Sampled     bool
	State       string
	Parent      string
	ParentState string
}

type tracePropagationKey struct{}

// WithTracePropagation returns a context whose trips' requests carry p.
func WithTracePropagation(ctx context.Context, p TracePropagation) context.Context {
	return context.WithValue(ctx, tracePropagationKey{}, &p)
}

func tracePropagationFrom(ctx context.Context) *TracePropagation {
	p, _ := ctx.Value(tracePropagationKey{}).(*TracePropagation)
	return p
}

// traceparent is the traceparent header of a request whose span is span.
func (p *TracePropagation) traceparent(span [8]byte) string {
	flags := "00"
	if p.Sampled {
		flags = "01"
	}
	return "00-" + hex.EncodeToString(p.TraceID[:]) + "-" + hex.EncodeToString(span[:]) + "-" + flags
}

// values are the traceparent and tracestate a request sends, and the span id
// it is sent under, zero when the caller's are passed on; ok says there is
// anything to send.
func (p *TracePropagation) values() (parent, state string, span [8]byte, ok bool) {
	if p.TraceID == [16]byte{} {
		return p.Parent, p.ParentState, span, p.Parent != ""
	}
	_, _ = rand.Read(span[:])
	return p.traceparent(span), p.State, span, true
}

// propagateTrace sets the trace context of ctx on header, an HTTP request's
// about to be sent, unless it sets a traceparent already, and returns the
// span id it was sent under, zero for none of the exporter's own. redirect
// says the request follows a redirect, which carries the headers of the
// request before it: a traceparent of the exporter's is replaced by one
// with the redirect's own span id, and one of the operator's is left as it
// is.
func propagateTrace(ctx context.Context, header http.Header, redirect bool) [8]byte {
	p := tracePropagationFrom(ctx)
	if p == nil {
		return [8]byte{}
	}
	if set := headerValue(header, "traceparent"); set != "" {
		if !redirect || p.TraceID == [16]byte{} || !strings.HasPrefix(set, "00-"+hex.EncodeToString(p.TraceID[:])+"-") {
			return [8]byte{}
		}
	}
	parent, state, span, ok := p.values()
	if !ok {
		return span
	}
	for name := range header {
		if strings.EqualFold(name, "traceparent") || strings.EqualFold(name, "tracestate") {
			delete(header, name)
		}
	}
	header.Set("Traceparent", parent)
	if state != "" {
		header.Set("Tracestate", state)
	}
	return span
}

// headerValue is header's value of name, in whatever case the name was set:
// a forwarded header keeps the case its caller sent.
func headerValue(header http.Header, name string) string {
	if v := header.Get(name); v != "" {
		return v
	}
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// traceSpanID records the span id the last request recorded was sent under.
func traceSpanID(ctx context.Context, span [8]byte) {
	if span == [8]byte{} {
		return
	}
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		t.requests[n-1].SpanID = span
	}
}
