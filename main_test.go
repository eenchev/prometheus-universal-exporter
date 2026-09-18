package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testCollector(name, format string) Collector {
	return Collector{Name: name, Request: RequestConfig{Method: "GET"}, Response: ResponseConfig{Format: format}, Transform: TransformConfig{Type: "regex"}, Metrics: []MetricRule{{Name: "demo_value", Type: GaugeMetricType, Expression: `value=(\d+)`}}, ErrorHandling: ErrorHandling{OnHTTPError: "fail", OnDecodeError: "fail", OnTransformError: "fail"}, Limits: Limits{MaxResponseBytes: 1024}}
}

func TestMetricValidationAndExpositionEscaping(t *testing.T) {
	m := MetricSet{Metrics: []Metric{{Name: "demo", Type: GaugeMetricType, Value: 2, Labels: map[string]string{"text": "a\n\\b\"c"}}}}
	if err := m.Validate(Limits{MaxMetrics: 3, MaxLabelsPerMetric: 3, MaxLabelValueLength: 20, MaxMetricNameLength: 20}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	writeMetricSet(w, &m)
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
	c := testCollector("text", "text")
	c.Request.ForwardAuthorization = true
	c.Request.ForwardHeaders = []string{"X-Tenant"}
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	m := NewConfigManager(cfg, "", slog.Default())
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
	cfg := &Config{Collectors: []Collector{testCollector("override", "text")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
	request := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL+"&collector=override&method=POST&path=%2Fapi%2Fstatus&timeout=2s&body=raw+payload", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHTMLBareTagSelector(t *testing.T) {
	c := Collector{Name: "html", Response: ResponseConfig{Format: "html"}, Transform: TransformConfig{Type: "css"}, Metrics: []MetricRule{{Name: "application_status", Type: GaugeMetricType, Expression: "h1"}}, ErrorHandling: ErrorHandling{AllowMissingKeys: false}, Limits: Limits{MaxMetrics: 10}}
	r := &HTTPResponse{Body: []byte("<html><body><h1>42</h1></body></html>"), Headers: http.Header{"Content-Type": []string{"text/html"}}}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Value != 42 {
		t.Fatalf("unexpected metrics: %#v", m.Metrics)
	}
}

func TestMissingOptionalJSONValue(t *testing.T) {
	c := Collector{Name: "json", Response: ResponseConfig{Format: "json"}, Transform: TransformConfig{Type: "jq"}, Metrics: []MetricRule{{Name: "optional_value", Type: GaugeMetricType, Expression: ".missing"}}, ErrorHandling: ErrorHandling{AllowMissingKeys: true}, Limits: Limits{MaxMetrics: 10}}
	r := &HTTPResponse{Body: []byte(`{"present":1}`), Headers: make(http.Header)}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 0 {
		t.Fatalf("expected omitted metric, got %#v", m.Metrics)
	}
}

func TestStandardJSONMetricAndPreScript(t *testing.T) {
	c := Collector{Name: "json", Response: ResponseConfig{Format: "json"}, Transform: TransformConfig{Type: "jq", PreScript: `data["requests"] = 42`}, Metrics: []MetricRule{{Name: "application_requests_total", Description: "Total application requests", Type: CounterMetricType, Expression: ".requests", Labels: []LabelRule{{Name: "environment", Type: "expression", Expression: ".environment"}}}}, Limits: Limits{MaxMetrics: 10}}
	r := &HTTPResponse{Body: []byte(`{"environment":"test"}`), Headers: make(http.Header)}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Value != 42 || m.Metrics[0].Type != CounterMetricType || m.Metrics[0].Labels["environment"] != "test" {
		t.Fatalf("unexpected metrics: %#v", m.Metrics)
	}
}

func TestStandardCSVMetricLabelsUseRowExpressions(t *testing.T) {
	c := Collector{Name: "csv", Response: ResponseConfig{Format: "csv", CSV: CSVConfig{Header: boolPtr(true)}}, Transform: TransformConfig{Type: "csv"}, Metrics: []MetricRule{{Name: "server_cpu", Description: "Server CPU utilization", Type: GaugeMetricType, Expression: "cpu", Labels: []LabelRule{{Name: "server", Type: "expression", Expression: "server"}, {Name: "environment", Type: "string", Value: "production"}}}}, Limits: Limits{MaxMetrics: 10}}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: []byte("server,cpu\nweb01,72\nweb02,31\n"), Headers: http.Header{"Content-Type": []string{"text/csv"}}}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 2 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Labels["server"] != "web02" || m.Metrics[0].Labels["environment"] != "production" {
		t.Fatalf("unexpected CSV metrics: %#v", m.Metrics)
	}
}

func TestPythonIsConfiguredAsTransform(t *testing.T) {
	c := Collector{Name: "python", Response: ResponseConfig{Format: "text"}, Transform: TransformConfig{Type: "python", Script: `metric(name="python_value", type="gauge", value=7)`, Libraries: []string{"beautifulsoup4"}}, Metrics: []MetricRule{}, Limits: Limits{MaxMetrics: 10}}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: []byte("ignored\n"), Headers: make(http.Header)}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Name != "python_value" || m.Metrics[0].Value != 7 {
		t.Fatalf("unexpected Python metrics: %#v", m.Metrics)
	}
}

func boolPtr(value bool) *bool { return &value }

func TestForwardedHeadersAreExplicitAndAllowlisted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/probe?header_X-Tenant=team-a&header_X-Unsafe=secret", nil)
	r.Header.Set("Authorization", "Bearer monitor-token")
	forwarded := forwardedHeaders(r, RequestConfig{ForwardAuthorization: true, ForwardHeaders: []string{"x-tenant"}})
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
}

func TestExporterBasicAuthProtection(t *testing.T) {
	cfg := &Config{
		Collectors: []Collector{testCollector("text", "text")},
		Web:        WebConfig{BasicAuth: &ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	if got := unauthorized.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("missing WWW-Authenticate challenge")
	}
	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
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

func TestExporterBasicAuthRejectsAuthorizationBridge(t *testing.T) {
	c := testCollector("text", "text")
	c.Request.ForwardAuthorization = true
	cfg := &Config{Collectors: []Collector{c}, Web: WebConfig{BasicAuth: &ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "forward_authorization") {
		t.Fatalf("expected auth bridge conflict, got %v", err)
	}
}

func TestOTLPIntervalDefaultsAndCanBeConfigured(t *testing.T) {
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: OTLPConfig{Enabled: true, Endpoint: "http://otel-collector:4318/v1/metrics"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.OTLP.Interval); got != 30*time.Second {
		t.Fatalf("default OTLP interval=%s", got)
	}
	cfg.OTLP.Interval = Duration(2 * time.Minute)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.OTLP.Interval); got != 2*time.Minute {
		t.Fatalf("configured OTLP interval=%s", got)
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
	c := testCollector("file_token", "text")
	c.Request.BearerTokenFile = tokenFile
	cfg := &Config{Collectors: []Collector{c}, Web: WebConfig{BasicAuth: &ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
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
	c := testCollector("file_basic", "text")
	c.Request.BasicAuthFile = &BasicAuthFile{Username: usernameFile, Password: passwordFile}
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
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
