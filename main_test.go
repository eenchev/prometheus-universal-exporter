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

func TestParseInsecureSkipVerifyOverride(t *testing.T) {
	withoutOverride := httptest.NewRequest(http.MethodGet, "/probe", nil).URL.Query()
	overrides, err := parseRequestOverrides(withoutOverride)
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
			overrides, err := parseRequestOverrides(request.URL.Query())
			if err != nil {
				t.Fatal(err)
			}
			if overrides.InsecureSkipVerify == nil || *overrides.InsecureSkipVerify != test.want {
				t.Fatalf("override=%v, want %t", overrides.InsecureSkipVerify, test.want)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/probe?insecure_skip_verify=1", nil)
	if _, err := parseRequestOverrides(request.URL.Query()); err == nil || !strings.Contains(err.Error(), "insecure_skip_verify") {
		t.Fatalf("invalid override error=%v", err)
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

func TestJSONArrayMetricsPairLabelsByIndex(t *testing.T) {
	c := Collector{
		Name:      "json_array",
		Response:  ResponseConfig{Format: "json"},
		Transform: TransformConfig{Type: "jq"},
		Metrics: []MetricRule{{
			Name:        "server_cpu",
			Description: "Server CPU utilization",
			Type:        GaugeMetricType,
			Expression:  ".servers[] | .cpu",
			Labels: []LabelRule{{
				Name:       "server",
				Type:       "expression",
				Expression: ".servers[] | .name",
			}},
		}},
		Limits: Limits{MaxMetrics: 10},
	}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: []byte(`{"servers":[{"name":"web01","cpu":72},{"name":"web02","cpu":31}]}`), Headers: make(http.Header)}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 2 {
		t.Fatalf("expected two array metrics, got %#v", m.Metrics)
	}
	if m.Metrics[0].Value != 72 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Value != 31 || m.Metrics[1].Labels["server"] != "web02" {
		t.Fatalf("array metric labels were not paired by index: %#v", m.Metrics)
	}
}

func TestJSONArrayMissingValuesRespectMetricErrorMode(t *testing.T) {
	for _, errorMode := range []string{"ignore", "log"} {
		t.Run(errorMode, func(t *testing.T) {
			c := Collector{
				Name:      "json_array_missing",
				Response:  ResponseConfig{Format: "json"},
				Transform: TransformConfig{Type: "jq"},
				Metrics: []MetricRule{{
					Name:       "server_cpu",
					Type:       GaugeMetricType,
					ErrorMode:  errorMode,
					Expression: ".servers[] | .cpu",
					Labels:     []LabelRule{{Name: "server", Type: "expression", Expression: ".servers[] | .name"}},
				}},
				Limits: Limits{MaxMetrics: 10},
			}
			if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
				t.Fatal(err)
			}
			r := &HTTPResponse{Body: []byte(`{"servers":[{"name":"web01","cpu":72},{"name":"web02"},{"name":"web03","cpu":31}]}`), Headers: make(http.Header)}
			d, err := decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			m, err := transform(context.Background(), d, r, &c, "python3")
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Metrics) != 2 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Labels["server"] != "web03" {
				t.Fatalf("unexpected metrics after missing array value: %#v", m.Metrics)
			}
		})
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

func TestCSVFormatIsInferredAndMissingRowsRespectMetricErrorMode(t *testing.T) {
	for _, errorMode := range []string{"ignore", "log"} {
		t.Run(errorMode, func(t *testing.T) {
			c := Collector{
				Name:      "csv_missing",
				Response:  ResponseConfig{CSV: CSVConfig{Header: boolPtr(true)}},
				Transform: TransformConfig{Type: "csv"},
				Metrics:   []MetricRule{{Name: "server_cpu", Type: GaugeMetricType, ErrorMode: errorMode, Expression: "cpu", Labels: []LabelRule{{Name: "server", Type: "expression", Expression: "server"}}}},
				Limits:    Limits{MaxMetrics: 10},
			}
			cfg := &Config{Collectors: []Collector{c}}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			c = cfg.Collectors[0]
			if c.Decoder.Type != "csv" {
				t.Fatalf("decoder was not inferred as CSV: %q", c.Decoder.Type)
			}
			r := &HTTPResponse{Body: []byte("server,cpu\nweb01,72\nweb02,\nweb03,31\n"), Headers: make(http.Header)}
			d, err := decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			m, err := transform(context.Background(), d, r, &c, "python3")
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Metrics) != 2 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Labels["server"] != "web03" {
				t.Fatalf("unexpected metrics after missing CSV field: %#v", m.Metrics)
			}
		})
	}
}

func TestHTMLCSSTableValues(t *testing.T) {
	body, err := os.ReadFile("testdata/html/status.html")
	if err != nil {
		t.Fatal(err)
	}
	c := Collector{
		Name:      "html_css",
		Response:  ResponseConfig{Format: "html"},
		Transform: TransformConfig{Type: "css"},
		Metrics:   []MetricRule{{Name: "server_cpu", Type: GaugeMetricType, Expression: "#servers td:nth-child(2)", Labels: []LabelRule{{Name: "environment", Type: "string", Value: "production"}}}},
		Limits:    Limits{MaxMetrics: 10},
	}
	r := &HTTPResponse{Body: body, Headers: http.Header{"Content-Type": []string{"text/html"}}}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 2 || m.Metrics[0].Value != 72 || m.Metrics[1].Value != 31 || m.Metrics[0].Labels["environment"] != "production" {
		t.Fatalf("unexpected HTML CSS metrics: %#v", m.Metrics)
	}
}

func TestHTMLXPathTableValuesAndLabels(t *testing.T) {
	body, err := os.ReadFile("testdata/html/status.html")
	if err != nil {
		t.Fatal(err)
	}
	c := Collector{
		Name:      "html_xpath",
		Response:  ResponseConfig{Format: "html"},
		Transform: TransformConfig{Type: "xpath"},
		Metrics: []MetricRule{{
			Name:       "server_cpu",
			Type:       GaugeMetricType,
			Expression: `//table[@id='servers']//tr/td[2]`,
			Labels:     []LabelRule{{Name: "server", Type: "expression", Expression: "preceding-sibling::td[1]"}},
		}},
		Limits: Limits{MaxMetrics: 10},
	}
	r := &HTTPResponse{Body: body, Headers: http.Header{"Content-Type": []string{"text/html"}}}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 2 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Labels["server"] != "web02" {
		t.Fatalf("unexpected HTML XPath metrics: %#v", m.Metrics)
	}
}

func TestPrometheusInputFilteringAndRelabeling(t *testing.T) {
	body, err := os.ReadFile("testdata/prometheus/status.prom")
	if err != nil {
		t.Fatal(err)
	}
	c := Collector{
		Name:      "prometheus",
		Response:  ResponseConfig{Format: "prometheus"},
		Transform: TransformConfig{Type: "prometheus"},
		Metrics: []MetricRule{{
			Name:        "application_requests_total",
			Description: "Application requests",
			Type:        CounterMetricType,
			Expression:  `^vendor_requests_total$`,
			Labels:      []LabelRule{{Name: "component", Type: "expression", Expression: "service"}},
		}},
		Limits: Limits{MaxMetrics: 10},
	}
	r := &HTTPResponse{Body: body, Headers: http.Header{"Content-Type": []string{"text/plain; version=0.0.4"}}}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Name != "application_requests_total" || m.Metrics[0].Type != CounterMetricType || m.Metrics[0].Value != 42 || m.Metrics[0].Labels["component"] != "api" {
		t.Fatalf("unexpected Prometheus metrics: %#v", m.Metrics)
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

func TestTransformInfersResponseFormatAndMetricErrorMode(t *testing.T) {
	cfg := &Config{Collectors: []Collector{{Name: "text", Transform: TransformConfig{Type: "regex"}, Metrics: []MetricRule{{Name: "value", Type: GaugeMetricType, ErrorMode: "ignore", Expression: `missing=(\d+)`}}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	if c.Decoder.Type != "text" {
		t.Fatalf("inferred decoder=%q", c.Decoder.Type)
	}
	r := &HTTPResponse{Body: []byte("value=42\n"), Headers: make(http.Header)}
	d, err := decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 0 {
		t.Fatalf("expected ignored metric, got %#v", m.Metrics)
	}
}

func TestTransformRejectsIncompatibleResponseFormat(t *testing.T) {
	cfg := &Config{Collectors: []Collector{{Name: "invalid", Response: ResponseConfig{Format: "json"}, Transform: TransformConfig{Type: "regex"}, Metrics: []MetricRule{{Name: "value", Type: GaugeMetricType, Expression: `value=(\d+)`}}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	r := &HTTPResponse{Body: []byte(`{"value":42}`), Headers: make(http.Header)}
	d, err := decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transform(context.Background(), d, r, c, "python3"); err == nil || !strings.Contains(err.Error(), "requires a text response") {
		t.Fatalf("expected incompatible response error, got %v", err)
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

func TestConfigValidationAppliesDefaults(t *testing.T) {
	cfg := &Config{Collectors: []Collector{{
		Name:      "defaults",
		Transform: TransformConfig{Type: "regex"},
		Metrics:   []MetricRule{{Name: "value", Expression: `value=(\d+)`}},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c := cfg.Collectors[0]
	if c.Request.Method != http.MethodGet || c.Response.Format != "auto" || c.Decoder.Type != "text" {
		t.Fatalf("unexpected inferred defaults: method=%q format=%q decoder=%q", c.Request.Method, c.Response.Format, c.Decoder.Type)
	}
	if c.ErrorHandling.OnHTTPError != "fail" || c.ErrorHandling.OnDecodeError != "fail" || c.ErrorHandling.OnTransformError != "fail" {
		t.Fatalf("unexpected error policy defaults: %#v", c.ErrorHandling)
	}
	if c.Metrics[0].Type != GaugeMetricType || c.Metrics[0].ErrorMode != "log" {
		t.Fatalf("unexpected metric defaults: %#v", c.Metrics[0])
	}
	if c.Limits.MaxResponseBytes <= 0 || c.Limits.MaxMetrics <= 0 || c.Limits.ScriptTimeout <= 0 {
		t.Fatalf("limits were not defaulted: %#v", c.Limits)
	}
}

func TestConfigValidationRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{
			name: "empty collectors",
			cfg:  &Config{},
			want: "collectors must not be empty",
		},
		{
			name: "invalid collector name",
			cfg:  &Config{Collectors: []Collector{{Name: "bad-name"}}},
			want: "invalid name",
		},
		{
			name: "unsupported method",
			cfg:  &Config{Collectors: []Collector{{Name: "invalid_method", Request: RequestConfig{Method: "TRACE"}}}},
			want: "unsupported method",
		},
		{
			name: "unknown transform",
			cfg:  &Config{Collectors: []Collector{{Name: "invalid_transform", Transform: TransformConfig{Type: "lua"}}}},
			want: "unknown transform",
		},
		{
			name: "invalid metric type",
			cfg:  &Config{Collectors: []Collector{{Name: "invalid_metric_type", Metrics: []MetricRule{{Name: "value", Type: "rate", Expression: ".value"}}}}},
			want: "invalid type",
		},
		{
			name: "invalid metric error mode",
			cfg:  &Config{Collectors: []Collector{{Name: "invalid_error_mode", Metrics: []MetricRule{{Name: "value", ErrorMode: "fail", Expression: ".value"}}}}},
			want: "invalid error_mode",
		},
		{
			name: "invalid label type",
			cfg:  &Config{Collectors: []Collector{{Name: "invalid_label", Transform: TransformConfig{Type: "jq"}, Metrics: []MetricRule{{Name: "value", Expression: ".value", Labels: []LabelRule{{Name: "source", Type: "xpath", Expression: ".source"}}}}}}},
			want: "invalid type",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("collectors:\n  - name: app\n    unknown: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("LoadConfig() error=%v, want unknown-field error", err)
	}
}

func TestDecodeJSONAutoDetectionAndMalformedInput(t *testing.T) {
	c := Collector{}
	r := &HTTPResponse{Body: []byte(`[{"value":7}]`), Headers: make(http.Header)}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "json" {
		t.Fatalf("detected kind=%q, want json", d.Kind)
	}
	values, ok := d.Data.([]any)
	if !ok || len(values) != 1 {
		t.Fatalf("decoded JSON array=%#v", d.Data)
	}
	row, ok := values[0].(map[string]any)
	if !ok || row["value"] != float64(7) {
		t.Fatalf("decoded JSON row=%#v", values[0])
	}

	c.Response.Format = "json"
	r.Body = []byte(`{"value":`)
	if _, err := decode(r, &c); err == nil || !strings.Contains(err.Error(), "JSON decode") {
		t.Fatalf("malformed JSON error=%v", err)
	}
}

func TestDecodeCSVQuotedFieldsAndRowsWithoutHeader(t *testing.T) {
	c := Collector{Response: ResponseConfig{Format: "csv", CSV: CSVConfig{Header: boolPtr(true), Delimiter: ";", TrimSpace: true}}}
	r := &HTTPResponse{Body: []byte("server;note;cpu\n\"web;01\";\"up;ok\"; 72 \n"), Headers: make(http.Header)}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	rows := d.Data.([]any)
	row := rows[0].(map[string]any)
	if row["server"] != "web;01" || row["note"] != "up;ok" || row["cpu"] != "72" {
		t.Fatalf("decoded CSV row=%#v", row)
	}

	c.Response.CSV.Header = boolPtr(false)
	r.Body = []byte("web01;72\nweb02;31\n")
	d, err = decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	rows = d.Data.([]any)
	first := rows[0].([]any)
	if len(rows) != 2 || first[0] != "web01" || first[1] != "72" {
		t.Fatalf("decoded headerless CSV rows=%#v", rows)
	}
}

func TestDecodePrometheusPreservesTimestamp(t *testing.T) {
	c := Collector{Response: ResponseConfig{Format: "prometheus"}}
	r := &HTTPResponse{Body: []byte("# TYPE vendor_value gauge\nvendor_value 42 1700000000000\n"), Headers: make(http.Header)}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set := d.Data.(MetricSet)
	if len(set.Metrics) != 1 || set.Metrics[0].Timestamp == nil || *set.Metrics[0].Timestamp != 1700000000000 {
		t.Fatalf("decoded Prometheus timestamp=%#v", set.Metrics)
	}
}

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
	c := Collector{Name: "configured", Request: RequestConfig{Method: http.MethodPost, Path: "/status", Query: map[string]string{"region": "eu"}, Headers: map[string]string{"X-Request": "one"}, Body: "raw body", BearerToken: "target-token", AllowedSchemes: []string{"http"}}, Limits: Limits{MaxResponseBytes: 1024}}
	response, err := fetch(context.Background(), target.URL+"/base?existing=true", &c, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(response.Body) != "value=42\n" {
		t.Fatalf("response=%#v", response)
	}
}

func TestFetchRejectsDisallowedSchemeAndOversizedResponse(t *testing.T) {
	c := Collector{Name: "scheme", Request: RequestConfig{AllowedSchemes: []string{"https"}}}
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

func TestFetchTLSVerificationCanBeConfiguredAndOverridden(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secure response"))
	}))
	defer target.Close()
	c := Collector{Name: "tls", Request: RequestConfig{AllowedSchemes: []string{"https"}}, Limits: Limits{MaxResponseBytes: 1024}}

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

func TestServerHealthAndCustomSelfMetricsEndpoint(t *testing.T) {
	cfg := &Config{Collectors: []Collector{testCollector("health", "text")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())
	server.SetSelfMetricsPath("/probe")
	handler := server.Handler()
	for _, test := range []struct {
		path   string
		status int
		body   string
	}{
		{path: "/health", status: http.StatusOK, body: "ok\n"},
		{path: "/ready", status: http.StatusOK, body: "ready\n"},
		{path: "/self-metrics", status: http.StatusOK, body: "http_exporter_collector_config_valid"},
		{path: "/metrics", status: http.StatusOK, body: "http_exporter_collector_config_valid"},
	} {
		t.Run(test.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, test.path, nil))
			if rr.Code != test.status || !strings.Contains(rr.Body.String(), test.body) {
				t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSafeTargetRedactsCredentials(t *testing.T) {
	if got := safeTarget("https://user:password@example.com/status"); strings.Contains(got, "password") || !strings.Contains(got, "redacted") {
		t.Fatalf("safe target=%q", got)
	}
}

func TestMetricSetValidationRejectsDuplicateAndInconsistentSeries(t *testing.T) {
	tests := []struct {
		name string
		set  MetricSet
		want string
	}{
		{
			name: "duplicate series",
			set:  MetricSet{Metrics: []Metric{{Name: "value", Type: GaugeMetricType, Value: 1}, {Name: "value", Type: GaugeMetricType, Value: 2}}},
			want: "duplicate metric series",
		},
		{
			name: "inconsistent types",
			set:  MetricSet{Metrics: []Metric{{Name: "value", Type: GaugeMetricType, Value: 1}, {Name: "value", Type: CounterMetricType, Value: 2, Labels: map[string]string{"source": "api"}}}},
			want: "inconsistent types",
		},
		{
			name: "label limit",
			set:  MetricSet{Metrics: []Metric{{Name: "value", Type: GaugeMetricType, Value: 1, Labels: map[string]string{"one": "1", "two": "2"}}}},
			want: "too many labels",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.set.Validate(Limits{MaxLabelsPerMetric: 1})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error=%v, want substring %q", err, test.want)
			}
		})
	}
}
