//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// retryAfterTarget answers its first request with status and, unless
// header is "", a Retry-After of header, and every later one with 200; it
// counts the requests it was sent.
func retryAfterTarget(t *testing.T, status int, header string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// fetchWithin fetches target for c within timeout, recording the requests,
// and says how long it took.
func fetchWithin(t *testing.T, target string, c *model.Collector, overrides RequestOverrides, timeout time.Duration) (*HTTPResponse, []TracedRequest, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx, trace := WithRequestTrace(ctx)
	start := time.Now()
	resp, err := FetchCollector(ctx, target, c, overrides, nil)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	return resp, trace.Requests(), took
}
