//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// The key a probe uses is the one probeCacheKey gives, so remembering the
// fingerprint changes nothing a cached result is filed under.
func TestServerCacheKeyMatchesProbeCacheKey(t *testing.T) {
	server, manager := newCacheTestServer(t, cachingCollector("first", time.Minute))
	cfg := manager.Get()
	c := &cfg.Collectors[0]
	query := url.Values{"collector": {"first"}, "target": {"http://target.invalid"}}
	header := http.Header{"X-Tenant": {"a"}}
	want := probeCacheKey(c, "http://target.invalid", query, header)
	for range 2 {
		if got := server.probeCacheKey(cfg, c, "http://target.invalid", query, header); got != want {
			t.Fatalf("server key=%s, want %s", got, want)
		}
	}
}
