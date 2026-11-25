package fetch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// With otlp.traces.propagate, a trip's requests to its target carry the
// trip's W3C trace context, so the target's own spans become children of
// the exporter's. An HTTP request (including a graphite request) carries it
// in its traceparent and tracestate headers, and a gRPC call carries the
// same in its metadata. The exporter puts what to send in the trip's
// context (WithTracePropagation). A trip without it sends nothing of the
// kind.
//
// There are two modes, depending on whether the exporter's trace export is
// on or off:
//
//   - Its own trace: each request is sent with its own span id, and its
//     CLIENT span is then made with that id (TracedRequest.SpanID). The
//     target's span is a child of that span.
//   - A caller's trace passed on: with the trace export off, the
//     traceparent and tracestate sent by the probe's caller are forwarded
//     unchanged, and the target's span is a child of the caller's.
//
// A traceparent the request already sets, from request.headers or
// forward_headers, belongs to the operator. It is sent unchanged, with no
// exporter tracestate next to it. A login request (request.login) carries
// none. It goes to an identity provider, which is often not part of the
// target, and the token it fetches is kept for later probes whose traces
// it is not part of.

// TracePropagation is what a trip's requests carry under
// otlp.traces.propagate. A set TraceID is the exporter's own trace: each
// request's traceparent names it, the request's own span id, and the
// sampled flag from Sampled. State, when set, is its tracestate. A zero
// TraceID forwards Parent and ParentState, the caller's traceparent and
// tracestate, unchanged.
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

// propagateTrace sets the trace context of ctx on header, the header of an
// HTTP request about to be sent, unless it already sets a traceparent. It
// returns the span id the request was sent under, or zero if it carries no
// span id of the exporter's own. redirect says the request follows a redirect, which
// carries the headers of the previous request. In that case an exporter
// traceparent is replaced by one with the redirect's own span id, and an
// operator traceparent is left unchanged.
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
