//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A host is checked as the transport dials it: written in full-width
// characters it is the address or name they stand for, and refused as that
// is.
func TestFullWidthHostsAreCheckedAsTheyAreDialed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("reached")) }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	for _, tc := range []struct{ allowed, denied []string }{
		{nil, []string{"127.0.0.0/8", "localhost"}},
		{[]string{"203.0.113.0/24"}, nil},
		{[]string{"127.0.0.2/32"}, nil},
	} {
		c := httpCollector(t, func(c *model.Collector) { c.Request.AllowedTargets = tc.allowed; c.Request.DeniedTargets = tc.denied })
		for _, host := range []string{"127.0.0.1", "localhost", "１２７.０.０.１", "ｌｏｃａｌｈｏｓｔ"} {
			transports = newTransportCache()
			if _, err := FetchCollector(context.Background(), "http://"+host+":"+port+"/", c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
				t.Errorf("allowed=%v denied=%v: %s was not refused: %v", tc.allowed, tc.denied, host, err)
			}
		}
	}
	if host, err := canonicalHost("１６９.２５４.１６９.２５４"); err != nil || host != "169.254.169.254" {
		t.Errorf("canonicalHost = %q, %v", host, err)
	}
}

// An internationalised name is checked as its ASCII form, which is what the
// transport dials: a name that resolves into a denied network is refused.
func TestInternationalisedNamesAreCheckedAsTheyAreDialed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	backend := strings.TrimPrefix(server.URL, "http://")
	// Every name resolves to the test server, 127.0.0.1, as DNS might.
	var dialed []string
	fakeDNS := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return (&net.Dialer{}).DialContext(ctx, network, backend)
	}
	proxyOverride = func(*url.URL) bool { return false }
	defer func() { proxyOverride = nil }()
	transports = newTransportCache()
	defer func() { transports = newTransportCache() }()
	strict := httpCollector(t, func(c *model.Collector) { c.Name = "strict"; c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	tr, _ := transports.get(TransportSettings{policy: policyOf(strict)}, time.Now())
	tr.DialContext = policyDialer(fakeDNS)
	tr.Proxy = nil
	_, port, _ := net.SplitHostPort(backend)
	for _, host := range []string{"ascii.example", "bücher.example"} {
		tr.CloseIdleConnections()
		if _, err := FetchCollector(context.Background(), "http://"+host+":"+port+"/", strict, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s was not refused (dialed %v): %v", host, dialed, err)
		}
		dialed = nil
	}
}

// An idle connection one collector opened does not carry another's request
// past its own target lists.
func TestAPooledConnectionKeepsEachCollectorsPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	transports = newTransportCache()
	defer func() { transports = newTransportCache() }()
	target := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	open := httpCollector(t, func(c *model.Collector) { c.Name = "open" })
	strict := httpCollector(t, func(c *model.Collector) {
		c.Name = "strict"
		c.Request.DeniedTargets = []string{"127.0.0.0/8", "::1/128"}
	})
	if _, err := FetchCollector(context.Background(), target, open, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), target, strict, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("the strict collector reused the open one's connection: %v", err)
	}
}

// A probe cannot make a trip retry more than MaxRetryAttempts times; the
// collector and a static target cannot either.
func TestRetryAttemptsAreBounded(t *testing.T) {
	if _, err := ParseRequestOverrides(url.Values{"retry_attempts": {"11"}}); err == nil || !strings.Contains(err.Error(), "from 0 to 10") {
		t.Fatalf("11 retries were accepted: %v", err)
	}
	if o, err := ParseRequestOverrides(url.Values{"retry_attempts": {"10"}}); err != nil || *o.RetryAttempts != 10 {
		t.Fatalf("10 retries: %v", err)
	}
	c := httpCollector(t, nil)
	c.Request.Retry.Attempts = 11
	if err := validateHTTPRequest(c); err == nil || !strings.Contains(err.Error(), "from 0 to 10") {
		t.Fatalf("a collector's 11 retries were accepted: %v", err)
	}
}

// A request that fails does not put request.query or the target's query in
// its error.
func TestAFailedRequestsErrorWithholdsTheQuery(t *testing.T) {
	c := model.Collector{Name: "q", Request: model.RequestConfig{Type: RequestTypeHTTP, Method: "GET", Query: map[string]string{"api_key": "s3cretQUERY"}}, Limits: model.Limits{MaxResponseBytes: 1024}}
	_, err := FetchCollector(t.Context(), "http://127.0.0.1:1/x?token=s3cretTARGET", &c, RequestOverrides{}, nil)
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "api_key=<redacted>") {
		t.Fatalf("%v", err)
	}
	_, err = FetchCollector(t.Context(), "http://127.0.0.1:1/x%zz?token=s3cretBAD", &c, RequestOverrides{}, nil)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("an unparseable target: %v", err)
	}
}
