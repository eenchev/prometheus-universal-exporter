//go:build !select_request_types || request_type_graphite

package fetch

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// A graphite collector's requests wait their turn under its
// request.rate_limit, as an http collector's do.
func TestAGraphiteCollectorsRateLimitSpacesItsRequests(t *testing.T) {
	clock := freshRateLimits(t)
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	c := graphiteCollector("a.b")
	c.Request.RateLimit = rateLimitOf(4, 1)
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		fetchWithin(t, server.URL, &c, RequestOverrides{}, 10*time.Second)
	}
	if got, want := clock.waited(), []time.Duration{250 * time.Millisecond, 250 * time.Millisecond}; hits.Load() != 3 || !reflect.DeepEqual(got, want) {
		t.Errorf("%d requests after the waits %v; want 3 after %v", hits.Load(), got, want)
	}
}
