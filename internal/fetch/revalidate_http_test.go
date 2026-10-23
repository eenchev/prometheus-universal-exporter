//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A 304 updates the stored headers as RFC 9111 § 4.3.4 says: each field it
// sends replaces that field's values, except those that describe the body
// or the connection, and those its Connection names.
func TestA304UpdatesTheStoredHeaders(t *testing.T) {
	cached := &CachedResponse{StatusCode: 200, Body: []byte("{}"), Headers: http.Header{
		"Etag": {`"v1"`}, "Last-Modified": {"Mon, 19 Oct 2026 10:00:00 GMT"}, "Cache-Control": {"max-age=10"},
		"Expires": {"Mon, 19 Oct 2026 10:00:10 GMT"}, "Date": {"Mon, 19 Oct 2026 10:00:00 GMT"},
		"Content-Length": {"2"}, "Content-Type": {"application/json"}, "X-Kept": {"yes"},
	}}
	got := revalidated(cached, http.Header{
		"Etag": {`"v2"`}, "Last-Modified": {"Fri, 23 Oct 2026 10:00:00 GMT"}, "Cache-Control": {"max-age=60"},
		"Expires": {"Fri, 23 Oct 2026 10:01:00 GMT"}, "Date": {"Fri, 23 Oct 2026 10:00:00 GMT"},
		"Content-Length": {"0"}, "Content-Encoding": {"gzip"}, "Transfer-Encoding": {"chunked"}, "Content-Type": {"text/html"},
		"Connection": {"X-Private"}, "X-Private": {"hop"}, "Keep-Alive": {"timeout=5"},
	})
	want := http.Header{
		"Etag": {`"v2"`}, "Last-Modified": {"Fri, 23 Oct 2026 10:00:00 GMT"}, "Cache-Control": {"max-age=60"},
		"Expires": {"Fri, 23 Oct 2026 10:01:00 GMT"}, "Date": {"Fri, 23 Oct 2026 10:00:00 GMT"},
		"Content-Length": {"2"}, "Content-Type": {"application/json"}, "X-Kept": {"yes"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("updated headers\n got %v\nwant %v", got, want)
	}
	if cached.Headers.Get("ETag") != `"v1"` {
		t.Fatal("the cached response was changed")
	}
}

// Every attempt of a revalidating request is conditional, a retry's too, and
// the 304 that ends it is answered with the cached response; a request the
// collector makes conditional itself is sent as configured, and its 304 is
// its answer.
func TestEveryAttemptOfARevalidationIsConditional(t *testing.T) {
	var attempts, conditional atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional.Add(1)
		}
		if attempts.Load() == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(server.Close)
	c := httpCollector(t, func(c *model.Collector) { c.Request.Retry = model.RetryConfig{Attempts: 1} })
	cached := &CachedResponse{StatusCode: http.StatusOK, Headers: http.Header{"Etag": {`"v1"`}}, Body: []byte("value=1")}
	response, err := FetchCollector(WithRevalidation(context.Background(), cached), server.URL, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !response.NotModified || !response.Conditional || response.StatusCode != http.StatusOK || string(response.Body) != "value=1" {
		t.Fatalf("answered %+v", response)
	}
	if attempts.Load() != 2 || conditional.Load() != 2 {
		t.Fatalf("%d attempts, %d conditional, want 2 of each", attempts.Load(), conditional.Load())
	}

	attempts.Store(1)
	own := httpCollector(t, func(c *model.Collector) { c.Request.Headers = map[string]string{"If-None-Match": `"v1"`} })
	response, err = FetchCollector(WithRevalidation(context.Background(), cached), server.URL, own, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Conditional || response.NotModified || response.StatusCode != http.StatusNotModified {
		t.Fatalf("a request the collector made conditional was taken for a revalidation: %+v", response)
	}
}
