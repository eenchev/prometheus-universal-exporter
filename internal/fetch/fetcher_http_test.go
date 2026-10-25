//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestFetchBuildsConfiguredRequestAndBearerAuth(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/base/status" || r.URL.Query().Get("region") != "eu" || string(body) != "raw body" {
			t.Errorf("request=%s %s?%s body=%q", r.Method, r.URL.Path, r.URL.RawQuery, string(body))
		}
		if r.Header.Get("X-Request") != "one" || r.Header.Get("Authorization") != "Bearer target-token" {
			t.Errorf("headers=%v", r.Header)
		}
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := model.Collector{Name: "configured", Request: model.RequestConfig{Type: RequestTypeHTTP, Path: "/status", Query: map[string]string{"region": "eu"}, Headers: map[string]string{"X-Request": "one"}, BearerToken: "target-token", AllowedSchemes: []string{"http"}, Own: HTTPConfig{Method: http.MethodPost, Body: "raw body"}}, Limits: model.Limits{MaxResponseBytes: 1024}}
	response, err := fetch(context.Background(), target.URL+"/base?existing=true", &c, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(response.Body) != "value=42\n" {
		t.Fatalf("response=%#v", response)
	}
}

// A request whose method is not idempotent is sent once however many retries
// are configured, unless retry.non_idempotent allows them: sending a POST again
// may repeat what it did. GET is retried as configured.
// A connection that breaks while the body is being read is retried like one
// that breaks before the answer: the first answer promises 64 bytes and stops
// after 5, and the retry reads the whole body. Without attempts left, or for
// a method that is not retried, the read error is the fetch's error.
func TestABrokenBodyIsRetried(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("value"))
			// Taking the connection over and closing it cuts the body short.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte("value=42"))
	}))
	t.Cleanup(target.Close)
	for _, test := range []struct {
		name     string
		method   string
		attempts int
		want     int64
		ok       bool
	}{
		{"retried", http.MethodGet, 1, 2, true},
		{"no attempts left", http.MethodGet, 0, 1, false},
		{"a method not retried", http.MethodPost, 1, 1, false},
	} {
		requests.Store(0)
		c := model.Collector{Name: "broken", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Attempts: test.attempts}, Own: HTTPConfig{Method: test.method}}, Limits: model.Limits{MaxResponseBytes: 1024}}
		ctx, trace := WithRequestTrace(context.Background())
		resp, err := fetch(ctx, target.URL, &c, RequestOverrides{})
		// A debug report shows why the first answer was not the one kept.
		if traced := trace.Requests(); len(traced) == 0 || !strings.Contains(traced[0].Outcome, "200 OK, then the body broke off: ") {
			t.Errorf("%s: the trace says %+v", test.name, traced)
		}
		if got := requests.Load(); got != test.want {
			t.Errorf("%s: %d requests, want %d", test.name, got, test.want)
		}
		switch {
		case test.ok && (err != nil || string(resp.Body) != "value=42"):
			t.Errorf("%s: resp=%v err=%v", test.name, resp, err)
		case !test.ok && (err == nil || !strings.Contains(err.Error(), "reading response")):
			t.Errorf("%s: err=%v, want the read error", test.name, err)
		}
	}
}

func TestOnlyIdempotentRequestsAreRetried(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(target.Close)
	for _, test := range []struct {
		method        string
		nonIdempotent bool
		want          int64
	}{
		{http.MethodGet, false, 3},
		{http.MethodPut, false, 3},
		{http.MethodPost, false, 1},
		{http.MethodPatch, false, 1},
		{http.MethodPost, true, 3},
	} {
		requests.Store(0)
		c := model.Collector{Name: "retries", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Attempts: 2, NonIdempotent: test.nonIdempotent}, Own: HTTPConfig{Method: test.method}}}
		resp, err := fetch(context.Background(), target.URL, &c, RequestOverrides{})
		if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: resp=%v err=%v", test.method, resp, err)
		}
		if got := requests.Load(); got != test.want {
			t.Errorf("%s non_idempotent=%v: %d requests, want %d", test.method, test.nonIdempotent, got, test.want)
		}
	}
	// A probe asking for POST cannot bring back the retries its method rules out.
	requests.Store(0)
	c := model.Collector{Name: "retries", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Attempts: 2}, Own: HTTPConfig{Method: http.MethodGet}}}
	if _, err := fetch(context.Background(), target.URL, &c, RequestOverrides{Method: http.MethodPost}); err != nil || requests.Load() != 1 {
		t.Fatalf("a POST override: %d requests, err=%v", requests.Load(), err)
	}
}
