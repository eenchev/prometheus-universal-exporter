//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// valueServer answers every request with value=1 and counts them.
func valueServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("value=1"))
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// limitedCollector is an http collector named name with the rate limit l.
func limitedCollector(t *testing.T, name string, l *model.RateLimitConfig, edit func(*model.Collector)) *model.Collector {
	t.Helper()
	c := testutil.Collector(name, "text")
	c.Request.RateLimit = l
	if edit != nil {
		edit(&c)
	}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// fetchBy fetches target with c within timeout, as a debug run does, with
// a trace, whose requests count in no self-metric.
func fetchBy(target string, c *model.Collector, overrides RequestOverrides, timeout time.Duration) ([]TracedRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx, trace := WithRequestTrace(ctx)
	_, err := FetchCollector(ctx, target, c, overrides, nil)
	return trace.Requests(), err
}

// fetchCounted fetches target with c within timeout, as a probe does.
func fetchCounted(target string, c *model.Collector, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	return err
}

// A collector sends burst requests to an origin at once, and the rest one
// every 1/requests_per_second, each waiting its turn; the waits are counted,
// but a debug run's, which waits its turn as well, and the debug trace says
// how long a request waited and why.
func TestRateLimitSpacesRequestsAfterTheBurst(t *testing.T) {
	clock := freshRateLimits(t)
	server, hits := valueServer(t)
	c := limitedCollector(t, "legacy", rateLimitOf(2, 3), nil)
	for range 5 {
		if err := fetchCounted(server.URL, c, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	traced, err := fetchBy(server.URL, c, RequestOverrides{}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{500 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond}
	if got := clock.waited(); !reflect.DeepEqual(got, want) || hits.Load() != 6 {
		t.Fatalf("%d requests sent after the waits %v; want 6 after %v", hits.Load(), got, want)
	}
	if waited, refused := CollectorRateLimit("legacy"); waited != 2 || refused != 0 {
		t.Errorf("counted %d waits and %d refusals; want 2, the debug run's not counted, and 0", waited, refused)
	}
	note := "sent after waiting 500ms for its turn under the rate limit of 2 requests per second, 3 at once for " + server.URL
	if len(traced) != 1 || traced[0].Wait != note {
		t.Errorf("the trace is %+v; want the wait %q", traced, note)
	}
}

// Each collector has its own limit for an origin, and each origin its own
// within a collector: what one collector or origin used up keeps no other
// waiting.
func TestRateLimitIsPerCollectorAndOrigin(t *testing.T) {
	freshRateLimits(t)
	first, _ := valueServer(t)
	second, _ := valueServer(t)
	a := limitedCollector(t, "a", rateLimitOf(1, 1), nil)
	b := limitedCollector(t, "b", rateLimitOf(1, 1), nil)
	for _, step := range []struct {
		c       *model.Collector
		target  string
		refused bool
	}{
		{a, first.URL, false},
		{b, first.URL, false},
		{a, second.URL, false},
		{a, first.URL, true},
		{b, second.URL, false},
		{b, first.URL, true},
	} {
		// Half a second is too little for a turn a second away.
		_, err := fetchBy(step.target, step.c, RequestOverrides{}, 500*time.Millisecond)
		if refused := errors.Is(err, errRateLimitRefused); refused != step.refused {
			t.Errorf("%s to %s: %v; want refused %v", step.c.Name, step.target, err, step.refused)
		}
	}
}

// A request whose turn would come past its deadline is refused at once, with
// the limit, the origin and the times in the error, and not retried, since
// a retry would only meet the same limit closer to the deadline.
func TestRateLimitRefusesAWaitPastTheDeadline(t *testing.T) {
	clock := freshRateLimits(t)
	server, hits := valueServer(t)
	c := limitedCollector(t, "legacy", rateLimitOf(1, 1), func(c *model.Collector) {
		c.Request.Retry = model.RetryConfig{Attempts: 3}
	})
	if err := fetchCounted(server.URL, c, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := fetchCounted(server.URL, c, 300*time.Millisecond)
	want := fmt.Sprintf("rate limit of 1 request per second for %s would delay the request past its deadline: it would wait 1s for its turn, and ", server.URL)
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "raise request.rate_limit, or the timeout") {
		t.Fatalf("got %v; want %q", err, want)
	}
	if took := time.Since(start); took > 200*time.Millisecond || hits.Load() != 1 || len(clock.waited()) != 0 {
		t.Errorf("refused after %s, %d requests sent, waits %v; want refused at once, nothing more sent, no wait", took, hits.Load(), clock.waited())
	}
	if waited, refused := CollectorRateLimit("legacy"); waited != 0 || refused != 1 {
		t.Errorf("counted %d waits and %d refusals; want 0 and 1", waited, refused)
	}
}

// A redirect whose turn would come past the deadline fails the request as
// the request itself would, and is not retried either: one refusal, and
// nothing more sent.
func TestARedirectRefusedByTheRateLimitIsNotRetried(t *testing.T) {
	freshRateLimits(t)
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/new", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	c := limitedCollector(t, "moved", rateLimitOf(1, 1), func(c *model.Collector) {
		c.Request.FollowRedirects = true
		c.Request.Retry = model.RetryConfig{Attempts: 3}
	})
	err := fetchCounted(server.URL+"/old", c, 300*time.Millisecond)
	if !errors.Is(err, errRateLimitRefused) || hits.Load() != 1 {
		t.Fatalf("%d requests sent, then %v; want one, and the redirect refused", hits.Load(), err)
	}
	if _, refused := CollectorRateLimit("moved"); refused != 1 {
		t.Errorf("counted %d refusals; want 1, the redirect not retried", refused)
	}
}

// Every request sent counts: each attempt of a retry, each page, each hop
// of a followed redirect and each login request to the target's origin.
func TestRateLimitCountsEveryRequestSent(t *testing.T) {
	second := time.Second
	t.Run("retries", func(t *testing.T) {
		clock := freshRateLimits(t)
		var hits atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("value=1"))
		}))
		t.Cleanup(server.Close)
		c := limitedCollector(t, "retried", rateLimitOf(1, 1), func(c *model.Collector) { c.Request.Retry = model.RetryConfig{Attempts: 2} })
		if _, err := fetchBy(server.URL, c, RequestOverrides{}, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if got := clock.waited(); hits.Load() != 3 || !reflect.DeepEqual(got, []time.Duration{second, second}) {
			t.Errorf("%d attempts after the waits %v; want 3 after two of 1s", hits.Load(), got)
		}
	})
	t.Run("pages", func(t *testing.T) {
		clock := freshRateLimits(t)
		var hits atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if n, _ := strconv.Atoi(r.URL.Query().Get("p")); n >= 3 {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			fmt.Fprintf(w, `{"items":[%q]}`, r.URL.Query().Get("p"))
		}))
		t.Cleanup(server.Close)
		c := testutil.Collector("paged", "json")
		c.Limits.MaxResponseBytes = 1 << 20
		editHTTP(&c, func(r *HTTPConfig) { r.Pagination = &model.PaginationConfig{Type: "page", Param: "p", Items: ".items"} })
		c.Request.RateLimit = rateLimitOf(1, 1)
		if err := ValidateRequest(&c); err != nil {
			t.Fatal(err)
		}
		if _, err := fetchBy(server.URL, &c, RequestOverrides{}, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if got := clock.waited(); hits.Load() != 3 || !reflect.DeepEqual(got, []time.Duration{second, second}) {
			t.Errorf("%d pages after the waits %v; want 3 after two of 1s", hits.Load(), got)
		}
	})
	t.Run("redirects", func(t *testing.T) {
		clock := freshRateLimits(t)
		var hits atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.URL.Path == "/old" {
				http.Redirect(w, r, "/new", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte("value=1"))
		}))
		t.Cleanup(server.Close)
		c := limitedCollector(t, "moved", rateLimitOf(1, 1), func(c *model.Collector) { c.Request.FollowRedirects = true })
		traced, err := fetchBy(server.URL+"/old", c, RequestOverrides{}, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got := clock.waited(); hits.Load() != 2 || !reflect.DeepEqual(got, []time.Duration{second}) {
			t.Errorf("%d requests after the waits %v; want 2 after one of 1s", hits.Load(), got)
		}
		if len(traced) != 2 || !traced[1].Redirect || !strings.HasPrefix(traced[1].Wait, "sent after waiting 1s for its turn") {
			t.Errorf("the trace is %+v; want the redirect's wait on it", traced)
		}
	})
	t.Run("logins", func(t *testing.T) {
		clock := freshRateLimits(t)
		freshLogins(t)
		server := newLoginServer(t, tokenAnswer(3600), bearer("tok1"))
		c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "s3cret"})})
		c.Request.RateLimit = rateLimitOf(1, 1)
		if err := ValidateRequest(c); err != nil {
			t.Fatal(err)
		}
		traced, err := fetchBy(server.URL+"/data", c, RequestOverrides{}, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got := clock.waited(); server.logins.Load() != 1 || server.requests.Load() != 1 || !reflect.DeepEqual(got, []time.Duration{second}) {
			t.Errorf("%d logins and %d requests after the waits %v; want one of each after one of 1s", server.logins.Load(), server.requests.Load(), got)
		}
		if len(traced) != 2 || !traced[0].Login || traced[0].Wait != "" || !strings.HasPrefix(traced[1].Wait, "sent after waiting 1s") {
			t.Errorf("the trace is %+v; want the login sent at once and the request after its wait", traced)
		}
	})
}

// A static target's rate_limit replaces the collector's for its requests,
// which have a bucket of their own: what the collector's other targets used
// up at the origin keeps it from none of its own turns.
func TestAStaticTargetsRateLimitReplacesTheCollectors(t *testing.T) {
	freshRateLimits(t)
	server, _ := valueServer(t)
	c := limitedCollector(t, "legacy", rateLimitOf(1, 1), nil)
	target := &model.StaticTarget{Name: "fragile", Target: server.URL, Request: model.TargetRequestConfig{RateLimit: rateLimitOf(1, 2)}}
	if err := CheckTargetRequest(target, c); err != nil {
		t.Fatal(err)
	}
	overrides := TargetOverrides(target)
	steps := []struct {
		overrides RequestOverrides
		refused   bool
	}{
		{RequestOverrides{}, false},
		{RequestOverrides{}, true},
		{overrides, false},
		{overrides, false},
		{overrides, true},
	}
	for i, step := range steps {
		_, err := fetchBy(server.URL, c, step.overrides, 500*time.Millisecond)
		if refused := errors.Is(err, errRateLimitRefused); refused != step.refused {
			t.Errorf("step %d: %v; want refused %v", i, err, step.refused)
		}
	}
	bad := &model.StaticTarget{Name: "bad", Target: server.URL, Request: model.TargetRequestConfig{RateLimit: &model.RateLimitConfig{RequestsPerSecond: 0}}}
	if err := CheckTargetRequest(bad, c); err == nil || !strings.Contains(err.Error(), `target "bad" request.rate_limit.requests_per_second is 0`) {
		t.Errorf("got %v; want the target's rate_limit refused", err)
	}
}
