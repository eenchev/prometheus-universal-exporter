package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// When a result's response carried an ETag or a Last-Modified, an http or
// graphite collector's response cache keeps the response itself with the
// result (exporter/cache.go). Once the result has expired, the next trip of
// the same request asks the target whether that response is still current,
// with If-None-Match and If-Modified-Since (RFC 9110 § 13.1). A 304 Not
// Modified answer has no body. The kept response, with its headers updated
// from the 304's (RFC 9111 § 4.3.4), is then the trip's answer, and is
// decoded and transformed like a freshly downloaded response. Only the
// download is saved. The series are made again on every trip.

// CachedResponse is a response kept to be revalidated: what the target
// answered, before the decode converted its body or rewrote its headers.
// Nothing changes it once it is kept; a 304 makes a new one.
type CachedResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Validators are the response's ETag and Last-Modified, as the target sent
// them; both empty when it has neither, and cannot be revalidated.
func (r *CachedResponse) Validators() (etag, lastModified string) {
	if r == nil {
		return "", ""
	}
	return r.Headers.Get("ETag"), r.Headers.Get("Last-Modified")
}

// HasValidator reports whether the response can be revalidated.
func HasValidator(h http.Header) bool {
	return h.Get("ETag") != "" || h.Get("Last-Modified") != ""
}

// KeepResponse is what a response is kept as, to be revalidated later, or
// nil when it has no validator. The body is shared, not copied. Nothing
// writes into a response's body: the decode replaces it rather than
// converting it in place (decode/textencoding.go).
func KeepResponse(r *HTTPResponse) *CachedResponse {
	if r == nil || r.Directory != nil || r.Pages != nil || !HasValidator(r.Headers) {
		return nil
	}
	return &CachedResponse{StatusCode: r.StatusCode, Headers: r.Headers.Clone(), Body: r.Body}
}

type revalidationKey struct{}

// WithRevalidation returns a context whose trip sends the validators of
// cached, when Revalidable says its request may be conditional.
func WithRevalidation(ctx context.Context, cached *CachedResponse) context.Context {
	if cached == nil {
		return ctx
	}
	return context.WithValue(ctx, revalidationKey{}, cached)
}

func revalidationFrom(ctx context.Context) *CachedResponse {
	cached, _ := ctx.Value(revalidationKey{}).(*CachedResponse)
	return cached
}

// Revalidable reports whether a request of c, with overrides, has a
// response that can be revalidated. That is a request that:
//   - is of a type that says so (RequestType.Revalidates): http and graphite;
//   - fetches one response with GET or HEAD;
//   - holds no clock placeholder.
//
// A paginated collector's response is many responses merged, and no single
// validator speaks for them. Another method is left alone: a matching
// If-None-Match on it asks for 412 Precondition Failed, not 304 (RFC 9110
// § 13.1.2). A graphite request too long for a URL is sent as a POST
// (requesttype_graphite.go).
func Revalidable(c *model.Collector, overrides RequestOverrides) bool {
	// One answer's validators cannot speak for the pages of a paginated
	// answer, nor for the answers of a chain.
	if rt := requestTypeOf(c); rt == nil || !rt.Revalidates || CollectorPolicy(c).Pagination != nil || CollectorPolicy(c).Chain != nil {
		return false
	}
	// A request with a clock placeholder asks for something else as the
	// clock moves. The validators of yesterday's answer say nothing of
	// today's. A Last-Modified older than the new day would make the target
	// answer 304, and yesterday's answer would be kept for today's.
	if c.Clock.Any || overrides.targetClock {
		return false
	}
	method := RequestMethodFor(c, overrides)
	return method == "" || method == http.MethodGet || method == http.MethodHead
}

// conditionalHeaders are the request headers that make a request
// conditional on a cached response.
var conditionalHeaders = []string{"If-None-Match", "If-Modified-Since"}

// setsConditional reports whether the collector's own request.headers, or
// the forwarded headers, already make the request conditional. The request
// is then sent as configured, and its answer (a 304 too) is not taken as a
// revalidation of the cache's response.
func setsConditional(headers map[string]string, forwarded []http.Header) bool {
	for name := range headers {
		for _, conditional := range conditionalHeaders {
			if strings.EqualFold(name, conditional) {
				return true
			}
		}
	}
	if len(forwarded) > 0 {
		for _, conditional := range conditionalHeaders {
			if len(forwarded[0].Values(conditional)) > 0 {
				return true
			}
		}
	}
	return false
}

// setConditional makes req conditional on cached.
func setConditional(req *http.Request, cached *CachedResponse) {
	etag, lastModified := cached.Validators()
	if etag != "" {
		// The whole value, weak or strong: If-None-Match compares weakly
		// (RFC 9110 § 13.1.2).
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
}

// notUpdated are the header fields a 304 does not update in the stored
// response:
//   - Content-Length, Content-Encoding and Transfer-Encoding describe the
//     body as it was sent. A 304 has no body, and the stored body is already
//     decoded from its content coding (RFC 9111 § 3.2).
//   - Content-Type and Content-Range also describe the body, which is the
//     stored one.
//   - The hop-by-hop fields describe the connection, not the response (RFC
//     9110 § 7.6.1).
var notUpdated = map[string]bool{
	"Content-Length": true, "Content-Encoding": true, "Transfer-Encoding": true,
	"Content-Type": true, "Content-Range": true,
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true,
	"Te": true, "Trailer": true, "Upgrade": true,
}

// revalidated is the response that a 304 to a request conditional on cached
// stands for. It has the cached status and body, and the cached headers,
// with every field the 304 sent replacing that field's values (RFC 9111
// § 4.3.4). The exceptions are fields that describe the body or the
// connection (notUpdated) and those the 304's Connection header names.
func revalidated(cached *CachedResponse, notModified http.Header) http.Header {
	headers := cached.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	hopByHop := map[string]bool{}
	for _, value := range notModified.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				hopByHop[textproto.CanonicalMIMEHeaderKey(name)] = true
			}
		}
	}
	for name, values := range notModified {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if notUpdated[canonical] || hopByHop[canonical] {
			continue
		}
		headers[canonical] = append([]string(nil), values...)
	}
	return headers
}

// notModifiedNote is what a debug report says of a 304 the cached response
// answered for.
func notModifiedNote(cached *CachedResponse) string {
	return fmt.Sprintf("; the cached body of %d bytes was reused, with status %d", len(cached.Body), cached.StatusCode)
}
