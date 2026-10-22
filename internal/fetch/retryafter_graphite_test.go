//go:build !select_request_types || request_type_graphite

package fetch

import (
	"net/http"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A graphite collector's retry.retry_after waits a retried answer's
// Retry-After in place of the backoff, as an http collector's does: with
// an hour's backoff, a 503 asking for no wait is retried at once, and
// without it the deadline cuts the backoff short.
func TestAGraphiteCollectorsRetryAfterReplacesTheBackoff(t *testing.T) {
	for _, on := range []bool{true, false} {
		c := graphiteCollector("a.b")
		c.Request.Retry = model.RetryConfig{Attempts: 1, Backoff: model.Duration(time.Hour), RetryAfter: on}
		if err := ValidateRequest(&c); err != nil {
			t.Fatal(err)
		}
		target, hits := retryAfterTarget(t, http.StatusServiceUnavailable, "0")
		timeout, status, sent := 300*time.Millisecond, http.StatusServiceUnavailable, int32(1)
		if on {
			timeout, status, sent = 5*time.Second, http.StatusOK, 2
		}
		resp, _, _ := fetchWithin(t, target.URL, &c, RequestOverrides{}, timeout)
		if resp.StatusCode != status || hits.Load() != sent {
			t.Errorf("retry_after %v: answer %d after %d requests; want %d after %d", on, resp.StatusCode, hits.Load(), status, sent)
		}
	}
}
