package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestParseInsecureSkipVerifyOverride(t *testing.T) {
	withoutOverride := httptest.NewRequest(http.MethodGet, "/probe", nil).URL.Query()
	overrides, err := ParseRequestOverrides(withoutOverride)
	if err != nil {
		t.Fatal(err)
	}
	if overrides.InsecureSkipVerify != nil {
		t.Fatal("unexpected TLS verification override when parameter is absent")
	}

	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "true", want: true},
		{value: "false", want: false},
	} {
		t.Run(test.value, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/probe?insecure_skip_verify="+test.value, nil)
			overrides, err := ParseRequestOverrides(request.URL.Query())
			if err != nil {
				t.Fatal(err)
			}
			if overrides.InsecureSkipVerify == nil || *overrides.InsecureSkipVerify != test.want {
				t.Fatalf("override=%v, want %t", overrides.InsecureSkipVerify, test.want)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/probe?insecure_skip_verify=1", nil)
	if _, err := ParseRequestOverrides(request.URL.Query()); err == nil || !strings.Contains(err.Error(), "insecure_skip_verify") {
		t.Fatalf("invalid override error=%v", err)
	}
}

func TestParseRetryOverrides(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/probe?retry_attempts=2&retry_backoff=3s", nil)
	overrides, err := ParseRequestOverrides(request.URL.Query())
	if err != nil {
		t.Fatal(err)
	}
	if overrides.RetryAttempts == nil || *overrides.RetryAttempts != 2 || overrides.RetryBackoff == nil || *overrides.RetryBackoff != 3*time.Second {
		t.Fatalf("retry overrides=%v/%v", overrides.RetryAttempts, overrides.RetryBackoff)
	}

	for _, query := range []string{"retry_attempts=-1", "retry_attempts=bad", "retry_backoff=-1s", "retry_backoff=bad"} {
		t.Run(query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/probe?"+query, nil)
			if _, err := ParseRequestOverrides(request.URL.Query()); err == nil || !strings.Contains(err.Error(), "retry_") {
				t.Fatalf("invalid retry override error=%v", err)
			}
		})
	}
}

func TestFetchRejectsDisallowedSchemeAndOversizedResponse(t *testing.T) {
	c := model.Collector{Name: "scheme", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: []string{"https"}}}
	if _, err := fetch(context.Background(), "http://example.com", &c, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("scheme error=%v", err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("123456789")) }))
	defer target.Close()
	c.Request.AllowedSchemes = []string{"http"}
	c.Limits.MaxResponseBytes = 4
	if _, err := fetch(context.Background(), target.URL, &c, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("response-size error=%v", err)
	}
}

func TestFetchRetriesTransientResponsesAndQueryOverrides(t *testing.T) {
	var requests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("recovered"))
	}))
	defer target.Close()
	c := model.Collector{Name: "retry", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: []string{"http"}, Retry: model.RetryConfig{Attempts: 1, Backoff: model.Duration(10 * time.Millisecond)}}, Limits: model.Limits{MaxResponseBytes: 1024}}
	started := time.Now()
	response, err := fetch(context.Background(), target.URL, &c, RequestOverrides{})
	if err != nil || response.StatusCode != http.StatusOK || string(response.Body) != "recovered" {
		t.Fatalf("configured retry response=%#v error=%v", response, err)
	}
	if requests != 2 || time.Since(started) < 10*time.Millisecond {
		t.Fatalf("configured retry requests=%d elapsed=%s", requests, time.Since(started))
	}

	requests = 0
	c.Request.Retry.Attempts = 0
	overrideRequest := httptest.NewRequest(http.MethodGet, "/probe?retry_attempts=1&retry_backoff=0s", nil)
	overrides, err := ParseRequestOverrides(overrideRequest.URL.Query())
	if err != nil {
		t.Fatal(err)
	}
	response, err = fetch(context.Background(), target.URL, &c, overrides)
	if err != nil || response.StatusCode != http.StatusOK || requests != 2 {
		t.Fatalf("query retry response=%#v error=%v requests=%d", response, err, requests)
	}
}

func TestFetchDoesNotRetryNonTransientHTTPStatus(t *testing.T) {
	requests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer target.Close()
	c := model.Collector{Name: "no_retry", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: []string{"http"}, Retry: model.RetryConfig{Attempts: 3}}, Limits: model.Limits{MaxResponseBytes: 1024}}
	response, err := fetch(context.Background(), target.URL, &c, RequestOverrides{})
	if err != nil || response.StatusCode != http.StatusBadRequest || requests != 1 {
		t.Fatalf("non-transient response=%#v error=%v requests=%d", response, err, requests)
	}
}

func TestFetchTLSVerificationCanBeConfiguredAndOverridden(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secure response"))
	}))
	defer target.Close()
	c := model.Collector{Name: "tls", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: []string{"https"}}, Limits: model.Limits{MaxResponseBytes: 1024}}

	if _, err := fetch(context.Background(), target.URL, &c, RequestOverrides{}); err == nil {
		t.Fatal("expected certificate verification to fail by default")
	}
	c.Request.TLS.InsecureSkipVerify = true
	response, err := fetch(context.Background(), target.URL, &c, RequestOverrides{})
	if err != nil || string(response.Body) != "secure response" {
		t.Fatalf("configured insecure request failed: response=%#v error=%v", response, err)
	}

	verified := false
	if _, err := fetch(context.Background(), target.URL, &c, RequestOverrides{InsecureSkipVerify: &verified}); err == nil {
		t.Fatal("expected query override false to restore certificate verification")
	}
	unsafe := true
	c.Request.TLS.InsecureSkipVerify = false
	response, err = fetch(context.Background(), target.URL, &c, RequestOverrides{InsecureSkipVerify: &unsafe})
	if err != nil || string(response.Body) != "secure response" {
		t.Fatalf("query override true failed: response=%#v error=%v", response, err)
	}
}

func TestSafeTargetRedactsCredentials(t *testing.T) {
	if got := safeTarget("https://user:password@example.com/status"); strings.Contains(got, "password") || !strings.Contains(got, "redacted") {
		t.Fatalf("safe target=%q", got)
	}
}

// The smaller of the two response limits applies, and 10 MiB when neither is
// set.
func TestResponseLimit(t *testing.T) {
	for _, test := range []struct {
		request, limits model.ByteSize
		want            int64
	}{
		{0, 0, 10 << 20},
		{1 << 20, 0, 1 << 20},
		{0, 2_000_000, 2_000_000},
		{1 << 20, 2_000_000, 1 << 20},
		{3_000_000, 2_000_000, 2_000_000},
	} {
		c := model.Collector{Request: model.RequestConfig{MaxResponseBytes: test.request}, Limits: model.Limits{MaxResponseBytes: test.limits}}
		if got := responseLimit(&c); got != test.want {
			t.Errorf("request=%d limits=%d: limit %d, want %d", test.request, test.limits, got, test.want)
		}
	}
}

// A metric label is persisted by Prometheus and handed to anything federating
// from it, so credentials and query strings must never reach one.
func TestRequestLabelDropsCredentialsAndQuery(t *testing.T) {
	tests := []struct {
		target string
		path   string
		query  map[string]string
		want   string
	}{
		{target: "http://api.example:8080", want: "http://api.example:8080"},
		{target: "http://user:secret@api.example:8080", want: "http://api.example:8080"},
		{target: "https://api.example", path: "/v1/status", want: "https://api.example/v1/status"},
		{target: "http://api.example?token=abc", want: "http://api.example"},
		{target: "http://api.example", query: map[string]string{"token": "abc"}, want: "http://api.example"},
		{target: "http://api.example/base", path: "/v1", query: map[string]string{"t": "1"}, want: "http://api.example/base/v1"},
		{target: "api.example", want: "http://api.example"},
	}
	for _, test := range tests {
		t.Run(test.target+test.path, func(t *testing.T) {
			c := pathCollector("")
			c.Request.Path = test.path
			c.Request.Query = test.query
			resolved, err := resolveRequestURL(test.target, &c, RequestOverrides{})
			if err != nil {
				t.Fatal(err)
			}
			if got := requestLabelURL(resolved); got != test.want {
				t.Fatalf("label=%q, want %q", got, test.want)
			}
			for _, leaked := range []string{"secret", "token", "abc"} {
				if strings.Contains(requestLabelURL(resolved), leaked) {
					t.Fatalf("label %q leaked %q", requestLabelURL(resolved), leaked)
				}
			}
		})
	}
}

// A retry wait the deadline cuts short keeps the target's answer: the 503 it
// gave, with its body, rather than the bare context error, which would only
// say that time ran out.
//
// The target has to have answered when the deadline passes, and the wait
// must not be over by then. Two seconds on the machine's clock did not
// always leave a busy machine the time for the answer, so the fetch runs on
// a clock of the test's own (onItsOwnClock): the deadline is two seconds
// away on it and the wait an hour, and the clock stands still until the
// answer has been read and the fetch waits. The fetch then returns at the
// deadline to the nanosecond, which is the deadline cutting the wait short.
func TestACutShortRetryWaitKeepsTheTargetsAnswer(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		// A connection kept for the next request is one the client still
		// reads from, and the clock would stand still for as long as it did.
		w.Header().Set("Connection", "close")
		http.Error(w, "maintenance until noon", http.StatusServiceUnavailable)
	}))
	defer target.Close()
	c := model.Collector{Name: "retry", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: []string{"http"}, Retry: model.RetryConfig{Attempts: 2, Backoff: model.Duration(time.Hour)}}, Limits: model.Limits{MaxResponseBytes: 1024}}
	onItsOwnClock(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		began := time.Now()
		response, err := fetch(ctx, target.URL, &c, RequestOverrides{})
		if err != nil {
			t.Fatalf("got error %v, want the target's answer", err)
		}
		if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(response.Body), "maintenance until noon") {
			t.Fatalf("status=%d body=%q, want the 503 and its body", response.StatusCode, response.Body)
		}
		if took := time.Since(began); requests.Load() != 1 || ctx.Err() == nil || took != 2*time.Second {
			t.Fatalf("requests=%d after %s, and the deadline had passed: %v; want one request and an answer at the deadline", requests.Load(), took, ctx.Err() != nil)
		}
	})
}

// A network error whose retry wait is cut short keeps its own error, and says
// the wait was cut short.
func TestACutShortRetryWaitAfterANetworkErrorKeepsTheError(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	address := closed.URL
	closed.Close()
	// As above: the clock stands still while the connection is being
	// refused, and the deadline passes in the hour's wait.
	c := model.Collector{Name: "retry", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: []string{"http"}, Retry: model.RetryConfig{Attempts: 2, Backoff: model.Duration(time.Hour)}}, Limits: model.Limits{MaxResponseBytes: 1024}}
	onItsOwnClock(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		began := time.Now()
		_, err := fetch(ctx, address, &c, RequestOverrides{})
		if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "the wait before retrying was cut short") {
			t.Fatalf("got %v, want the connection error and the cut-short wait", err)
		}
		if took := time.Since(began); took != 2*time.Second {
			t.Fatalf("the fetch returned after %s, want at the deadline", took)
		}
	})
}

// onItsOwnClock runs a test of a fetch's deadline on a clock of its own
// (testing/synctest). That clock moves only when every goroutine of the
// test waits for it or for another of them, and stands still while one of
// them is at work or waits for the network: a deadline on it passes when
// the fetch has got as far as it gets without the clock, in however long
// the machine takes over that, and not while a request is still on its way.
// The test's target is started outside, so that its goroutines, which wait
// for connections for as long as it runs, do not hold the clock; and the
// fetch has connection pools of its own, since a pool made on this clock
// would be kept with the clock's time on it.
func onItsOwnClock(t *testing.T, test func(t *testing.T)) {
	t.Helper()
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	synctest.Test(t, test)
}
