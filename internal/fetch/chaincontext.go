//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http/cookiejar"
	"net/url"
)

// What only a chain (chain.go) puts in the context of its requests, read by
// the shared request code (chainreport.go, requesttrace.go).

// withChainTrip returns ctx for the requests of a chain whose origin is
// origin.
func withChainTrip(ctx context.Context, origin *url.URL) context.Context {
	jar, _ := cookiejar.New(nil)
	return context.WithValue(ctx, chainTripKey{}, &chainTrip{origin: origin, jar: jar})
}

// withChainStep returns ctx for the requests of a step, or of one of its
// items.
func withChainStep(ctx context.Context, state *chainStepState) context.Context {
	return context.WithValue(ctx, chainStepKey{}, state)
}

// withItemTrace returns ctx for a request sent at once with others, as the
// items of a fan-out are (chain.go). Its requests are recorded in a trace of
// their own, since what is learned of a request is recorded on the last one
// recorded. merge adds them to the trip's trace, after the request ended.
// Without a trace, ctx is returned as it is.
func withItemTrace(ctx context.Context) (_ context.Context, merge func()) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return ctx, func() {}
	}
	item := &RequestTrace{spans: t.spans}
	return context.WithValue(ctx, requestTraceKey{}, item), func() {
		item.mu.Lock()
		requests := item.requests
		item.mu.Unlock()
		t.mu.Lock()
		t.requests = append(t.requests, requests...)
		t.mu.Unlock()
	}
}

// keepSecret notes the text of a value marked secret, and of each entry of
// a list one.
func (t *chainTrip) keepSecret(value any) {
	if list, ok := value.([]any); ok {
		for _, entry := range list {
			t.keepSecret(entry)
		}
		return
	}
	text := chainText(value)
	if text == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.secrets = append(t.secrets, text)
}
