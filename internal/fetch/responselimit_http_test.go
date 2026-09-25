//go:build !select_request_types || request_type_http

package fetch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A body is read only up to the size limit: a target streaming without end
// fails with a limit error at once rather than filling memory until the
// scrape's deadline.
func TestAnEndlessBodyIsNotReadPastTheLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte("y"), 64<<10)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) { c.Request.MaxResponseBytes = 1024 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := FetchCollector(ctx, server.URL, c, RequestOverrides{}, nil)
	if elapsed := time.Since(start); !errors.Is(err, model.ErrLimitExceeded) || elapsed > 2*time.Second {
		t.Fatalf("err=%v after %v, want a limit error well before the deadline", err, elapsed)
	}
}
