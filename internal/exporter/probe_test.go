package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestMetricValidationAndExpositionEscaping(t *testing.T) {
	m := model.MetricSet{Metrics: []model.Metric{{Name: "demo", Type: model.GaugeMetricType, Value: 2, Labels: map[string]string{"text": "a\n\\b\"c"}}}}
	if err := m.Validate(model.Limits{MaxMetrics: 3, MaxLabelsPerMetric: 3, MaxLabelValueLength: 20, MaxMetricNameLength: 20}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	writeMetricSet(w, nil, &m)
	if !strings.Contains(w.Body.String(), `text="a\n\\b\"c"`) {
		t.Fatalf("unexpected exposition: %s", w.Body.String())
	}
}

func TestForwardedHeadersAreExplicitAndAllowlisted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/probe?header_X-Tenant=team-a&header_X-Unsafe=secret", nil)
	r.Header.Set("Authorization", "Bearer monitor-token")
	forwarded := forwardedHeaders(r, model.RequestConfig{Type: fetch.RequestTypeHTTP, ForwardAuthorization: true, ForwardHeaders: []string{"x-tenant"}})
	if got := forwarded.Get("Authorization"); got != "Bearer monitor-token" {
		t.Fatalf("authorization was not forwarded: %q", got)
	}
	if got := forwarded.Get("X-Tenant"); got != "team-a" {
		t.Fatalf("allowlisted header was not forwarded: %q", got)
	}
	if got := forwarded.Get("X-Unsafe"); got != "" {
		t.Fatalf("unallowlisted header was forwarded: %q", got)
	}
	if got := forwarded.Get("Host"); got != "" {
		t.Fatalf("hop-by-hop/transport header was forwarded: %q", got)
	}
	// Accept-Encoding is the exporter's own, even listed: Prometheus sends
	// it on every scrape.
	r.Header.Set("Accept-Encoding", "gzip")
	if got := forwardedHeaders(r, model.RequestConfig{ForwardHeaders: []string{"Accept-Encoding", "X-Tenant"}}); got.Get("Accept-Encoding") != "" {
		t.Fatalf("Accept-Encoding was forwarded: %v", got)
	}
	// No Proxy- header is forwarded, even listed.
	if got := forwardableHeaders(model.RequestConfig{ForwardHeaders: []string{"Proxy-Connection", "proxy-x", "X-Tenant", "Proxy-Authorization"}}); len(got) != 1 || got[0] != "X-Tenant" {
		t.Fatalf("forwardable: %v", got)
	}
}

// A header_ parameter left empty, as a blank field of the collectors page's
// form sends it, is a header not given: it is not forwarded empty, and a
// non-empty value beside it still is.
func TestAnEmptyForwardedHeaderIsNotSent(t *testing.T) {
	request := model.RequestConfig{Type: fetch.RequestTypeHTTP, ForwardHeaders: []string{"X-Tenant", "X-Region"}}
	r := httptest.NewRequest(http.MethodGet, "/probe?header_X-Tenant=&header_X-Region=eu&header_X-Region=", nil)
	forwarded := forwardedHeaders(r, request)
	if _, sent := forwarded["X-Tenant"]; sent {
		t.Fatalf("an empty header was forwarded: %v", forwarded)
	}
	if got := forwarded.Values("X-Region"); len(got) != 1 || got[0] != "eu" {
		t.Fatalf("X-Region=%q, want only eu", got)
	}
}

// --web.self-metrics-path must be one fixed path no other endpoint uses.
func TestSelfMetricsPathIsChecked(t *testing.T) {
	for path, want := range map[string]string{
		"/self-metrics":   "/self-metrics",
		"metrics":         "/metrics",
		"/internal/stats": "/internal/stats",
		"/v1.2/.well":     "/v1.2/.well",
		"/a..b/..c":       "/a..b/..c",
	} {
		if got, err := SelfMetricsPath(path); err != nil || got != want {
			t.Errorf("SelfMetricsPath(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"", "/", "/probe", "/health", "/ready", "/collectors", "/-/reload", "/stats/", "/stats?x=1", "/{name}", "/a b", "//x", "/m/..", "/m/.", "/./m", "/../m", "/...", "/.."} {
		if _, err := SelfMetricsPath(path); err == nil {
			t.Errorf("SelfMetricsPath(%q) was accepted", path)
		}
	}
}

// Every path the checks accept is one a request reaches: ServeMux cleans a
// request's path before routing it, so a path it would clean differently
// could never be served.
func TestAcceptedEndpointPathsAreReachable(t *testing.T) {
	for _, path := range []string{"/self", "/v1.2/.well", "/a..b/..c", "/x/y-z_~"} {
		checked, err := SelfMetricsPath(path)
		if err != nil {
			t.Fatal(err)
		}
		server := NewServer(config.NewManager(&model.Config{}, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
		server.SetSelfMetricsPath(checked)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, checked, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "http_exporter_build_info") {
			t.Errorf("%s answered %d", checked, recorder.Code)
		}
	}
}
