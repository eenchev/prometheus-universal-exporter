package exporter

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestProbeTextCollector(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer monitor-token" {
			t.Errorf("authorization=%q", got)
		}
		if got := r.Header.Get("X-Tenant"); got != "team-a" {
			t.Errorf("X-Tenant=%q", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("text", "text")
	c.Request.ForwardAuthorization = true
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	m := config.NewManager(cfg, "", slog.Default())
	s := NewServer(m, "python3", slog.Default())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=text&header_X-Tenant=team-a", nil)
	req.Header.Set("Authorization", "Bearer monitor-token")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "demo_value 42") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestProbeRequestOverrides(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/status" || string(body) != "raw payload" {
			t.Errorf("request=%s %s %q", r.Method, r.URL.Path, string(body))
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("override", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=override&method=POST&path=%2Fapi%2Fstatus&timeout=2s&body=raw+payload", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
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

func TestExporterBasicAuthProtection(t *testing.T) {
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		Web:        model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/self-metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	if got := unauthorized.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("missing WWW-Authenticate challenge")
	}
	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/self-metrics", nil)
	request.SetBasicAuth("exporter", "secret")
	server.Handler().ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", authorized.Code, authorized.Body.String())
	}
	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d", health.Code)
	}
}

func TestBearerTokenFileIsUsedIndependentlyOfExporterAuth(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte(" target-token \n"), 0600); err != nil {
		t.Fatal(err)
	}
	var received string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("file_token", "text")
	c.Request.BearerTokenFile = tokenFile
	cfg := &model.Config{Collectors: []model.Collector{c}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=file_token", nil)
	request.SetBasicAuth("exporter", "secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if received != "Bearer target-token" {
		t.Fatalf("target authorization=%q", received)
	}
}

func TestBasicAuthFileIsUsedForTarget(t *testing.T) {
	dir := t.TempDir()
	usernameFile := dir + "/username"
	passwordFile := dir + "/password"
	if err := os.WriteFile(usernameFile, []byte(" target-user \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordFile, []byte(" target-password \n"), 0600); err != nil {
		t.Fatal(err)
	}
	var receivedUser, receivedPassword string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUser, receivedPassword, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	c := testutil.Collector("file_basic", "text")
	c.Request.BasicAuthFile = &model.BasicAuthFile{Username: usernameFile, Password: passwordFile}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=file_basic", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if receivedUser != "target-user" || receivedPassword != "target-password" {
		t.Fatalf("target basic auth=%q/%q", receivedUser, receivedPassword)
	}
}

// The exporter's own metrics are served at one path: /self-metrics unless
// --web.self-metrics-path names another, and nowhere else.
func TestSelfMetricsAreServedAtOnePath(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("health", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		set       string
		served    string
		notServed []string
	}{
		{set: "", served: "/self-metrics", notServed: []string{"/metrics"}},
		{set: "/metrics", served: "/metrics", notServed: []string{"/self-metrics"}},
		{set: "/internal/stats", served: "/internal/stats", notServed: []string{"/metrics", "/self-metrics"}},
	} {
		t.Run(tc.served, func(t *testing.T) {
			server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
			if tc.set != "" {
				server.SetSelfMetricsPath(tc.set)
			}
			handler := server.Handler()
			for path, want := range map[string]string{"/health": "ok\n", "/ready": "ready\n", tc.served: "http_exporter_collector_config_valid"} {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
					t.Fatalf("%s: status=%d body=%q", path, rr.Code, rr.Body.String())
				}
			}
			for _, path := range tc.notServed {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				if rr.Code != http.StatusNotFound {
					t.Fatalf("%s answered %d, want 404: self-metrics are served at %s only", path, rr.Code, tc.served)
				}
			}
		})
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
