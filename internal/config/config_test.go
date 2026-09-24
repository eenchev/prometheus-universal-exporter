package config

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

func TestStandardJSONMetricAndPreScript(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "json", Response: model.ResponseConfig{Format: "json"}, Transform: model.TransformConfig{Type: "jq", PreScript: `data["requests"] = 42`}, Metrics: []model.MetricRule{{Name: "application_requests_total", Description: "Total application requests", Type: model.CounterMetricType, Expression: ".requests", Labels: []model.LabelRule{{Name: "environment", Type: "expression", Expression: ".environment"}}}}, Limits: model.Limits{MaxMetrics: 10}}
	r := &fetch.HTTPResponse{Body: []byte(`{"environment":"test"}`), Headers: make(http.Header)}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform.Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Value != 42 || m.Metrics[0].Type != model.CounterMetricType || m.Metrics[0].Labels["environment"] != "test" {
		t.Fatalf("unexpected metrics: %#v", m.Metrics)
	}
}

func TestJSONArrayMetricsPairLabelsByIndex(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "json_array",
		Response:  model.ResponseConfig{Format: "json"},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{
			Name:        "server_cpu",
			Description: "Server CPU utilization",
			Type:        model.GaugeMetricType,
			Expression:  ".servers[] | .cpu",
			Labels: []model.LabelRule{{
				Name:       "server",
				Type:       "expression",
				Expression: ".servers[] | .name",
			}},
		}},
		Limits: model.Limits{MaxMetrics: 10},
	}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte(`{"servers":[{"name":"web01","cpu":72},{"name":"web02","cpu":31}]}`), Headers: make(http.Header)}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform.Transform(context.Background(), d, r, &c, "python3")
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
			c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
				Name:      "json_array_missing",
				Response:  model.ResponseConfig{Format: "json"},
				Transform: model.TransformConfig{Type: "jq"},
				Metrics: []model.MetricRule{{
					Name:       "server_cpu",
					Type:       model.GaugeMetricType,
					ErrorMode:  errorMode,
					Expression: ".servers[] | .cpu",
					Labels:     []model.LabelRule{{Name: "server", Type: "expression", Expression: ".servers[] | .name"}},
				}},
				Limits: model.Limits{MaxMetrics: 10},
			}
			if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
				t.Fatal(err)
			}
			r := &fetch.HTTPResponse{Body: []byte(`{"servers":[{"name":"web01","cpu":72},{"name":"web02"},{"name":"web03","cpu":31}]}`), Headers: make(http.Header)}
			d, err := decode.Decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			m, err := transform.Transform(context.Background(), d, r, &c, "python3")
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
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "csv", Response: model.ResponseConfig{Format: "csv", CSV: model.CSVConfig{Header: boolPtr(true)}}, Transform: model.TransformConfig{Type: "csv"}, Metrics: []model.MetricRule{{Name: "server_cpu", Description: "Server CPU utilization", Type: model.GaugeMetricType, Expression: "cpu", Labels: []model.LabelRule{{Name: "server", Type: "expression", Expression: "server"}, {Name: "environment", Type: "string", Value: "production"}}}}, Limits: model.Limits{MaxMetrics: 10}}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte("server,cpu\nweb01,72\nweb02,31\n"), Headers: http.Header{"Content-Type": []string{"text/csv"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform.Transform(context.Background(), d, r, &c, "python3")
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
			c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
				Name:      "csv_missing",
				Response:  model.ResponseConfig{CSV: model.CSVConfig{Header: boolPtr(true)}},
				Transform: model.TransformConfig{Type: "csv"},
				Metrics:   []model.MetricRule{{Name: "server_cpu", Type: model.GaugeMetricType, ErrorMode: errorMode, Expression: "cpu", Labels: []model.LabelRule{{Name: "server", Type: "expression", Expression: "server"}}}},
				Limits:    model.Limits{MaxMetrics: 10},
			}
			cfg := &model.Config{Collectors: []model.Collector{c}}
			if err := Validate(cfg); err != nil {
				t.Fatal(err)
			}
			c = cfg.Collectors[0]
			if c.Decoder.Type != "csv" {
				t.Fatalf("decoder was not inferred as CSV: %q", c.Decoder.Type)
			}
			r := &fetch.HTTPResponse{Body: []byte("server,cpu\nweb01,72\nweb02,\nweb03,31\n"), Headers: make(http.Header)}
			d, err := decode.Decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			m, err := transform.Transform(context.Background(), d, r, &c, "python3")
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Metrics) != 2 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Labels["server"] != "web03" {
				t.Fatalf("unexpected metrics after missing CSV field: %#v", m.Metrics)
			}
		})
	}
}

func TestCSVTransformDefaultsWithoutResponseConfiguration(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "csv_defaults",
		Transform: model.TransformConfig{Type: "csv"},
		Metrics: []model.MetricRule{{
			Name:       "server_cpu",
			Type:       model.GaugeMetricType,
			Expression: "cpu",
			Labels:     []model.LabelRule{{Name: "server", Type: "expression", Expression: "server"}},
		}},
	}}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	if c.Response.Format != "auto" || c.Decoder.Type != "csv" || c.Response.CSV.Header != nil {
		t.Fatalf("unexpected CSV defaults: format=%q decoder=%q header=%v", c.Response.Format, c.Decoder.Type, c.Response.CSV.Header)
	}
	r := &fetch.HTTPResponse{Body: []byte("server,cpu\nweb01,72\nweb02,31\n"), Headers: make(http.Header)}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform.Transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 2 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Labels["server"] != "web02" {
		t.Fatalf("unexpected metrics with omitted response config: %#v", m.Metrics)
	}
}

func TestPythonIsConfiguredAsTransform(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "python", Response: model.ResponseConfig{Format: "text"}, Transform: model.TransformConfig{Type: "python", Script: `metric(name="python_value", type="gauge", value=7)`, Libraries: []string{"lxml"}}, Metrics: []model.MetricRule{}, Limits: model.Limits{MaxMetrics: 10}}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte("ignored\n"), Headers: make(http.Header)}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform.Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Name != "python_value" || m.Metrics[0].Value != 7 {
		t.Fatalf("unexpected Python metrics: %#v", m.Metrics)
	}
}

func TestTransformInfersResponseFormatAndMetricErrorMode(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "text", Transform: model.TransformConfig{Type: "regex"}, Metrics: []model.MetricRule{{Name: "value", Type: model.GaugeMetricType, ErrorMode: "ignore", Expression: `missing=(\d+)`}}}}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	if c.Decoder.Type != "text" {
		t.Fatalf("inferred decoder=%q", c.Decoder.Type)
	}
	r := &fetch.HTTPResponse{Body: []byte("value=42\n"), Headers: make(http.Header)}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := transform.Transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 0 {
		t.Fatalf("expected ignored metric, got %#v", m.Metrics)
	}
}

func TestTransformRejectsIncompatibleResponseFormat(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "invalid", Response: model.ResponseConfig{Format: "json"}, Transform: model.TransformConfig{Type: "regex"}, Metrics: []model.MetricRule{{Name: "value", Type: model.GaugeMetricType, Expression: `value=(\d+)`}}}}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]
	r := &fetch.HTTPResponse{Body: []byte(`{"value":42}`), Headers: make(http.Header)}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transform.Transform(context.Background(), d, r, c, "python3"); err == nil || !strings.Contains(err.Error(), "requires a text response") {
		t.Fatalf("expected incompatible response error, got %v", err)
	}
}

func boolPtr(value bool) *bool { return &value }

func TestExporterBasicAuthRejectsAuthorizationBridge(t *testing.T) {
	c := testutil.Collector("text", "text")
	c.Request.ForwardAuthorization = true
	cfg := &model.Config{Collectors: []model.Collector{c}, Web: model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "secret"}}}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "forward_authorization") {
		t.Fatalf("expected auth bridge conflict, got %v", err)
	}
}

func TestOTLPIntervalDefaultsAndCanBeConfigured(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: model.OTLPConfig{Enabled: true, Endpoint: "http://otel-collector:4318/v1/metrics"}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.OTLP.Interval); got != 30*time.Second {
		t.Fatalf("default OTLP interval=%s", got)
	}
	cfg.OTLP.Interval = model.Duration(2 * time.Minute)
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.OTLP.Interval); got != 2*time.Minute {
		t.Fatalf("configured OTLP interval=%s", got)
	}
}

func TestConfigValidationAppliesDefaults(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "defaults",
		Transform: model.TransformConfig{Type: "regex"},
		Metrics:   []model.MetricRule{{Name: "value", Expression: `value=(\d+)`}},
	}}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg.Collectors[0]
	if c.Request.Method != http.MethodGet || c.Response.Format != "auto" || c.Decoder.Type != "text" {
		t.Fatalf("unexpected inferred defaults: method=%q format=%q decoder=%q", c.Request.Method, c.Response.Format, c.Decoder.Type)
	}
	if c.ErrorHandling.OnFetchError != "fail" || c.ErrorHandling.OnDecodeError != "fail" || c.ErrorHandling.OnTransformError != "fail" {
		t.Fatalf("unexpected error policy defaults: %#v", c.ErrorHandling)
	}
	if c.Metrics[0].Type != model.GaugeMetricType || c.Metrics[0].ErrorMode != "log" {
		t.Fatalf("unexpected metric defaults: %#v", c.Metrics[0])
	}
	if c.Limits.MaxResponseBytes <= 0 || c.Limits.MaxMetrics <= 0 || c.Limits.ScriptTimeout <= 0 {
		t.Fatalf("limits were not defaulted: %#v", c.Limits)
	}
}

func TestConfigValidationRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  *model.Config
		want string
	}{
		{
			name: "empty collectors",
			cfg:  &model.Config{},
			want: "collectors must not be empty",
		},
		{
			name: "invalid collector name",
			cfg:  &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "bad-name"}}},
			want: "invalid name",
		},
		{
			name: "unsupported method",
			cfg:  &model.Config{Collectors: []model.Collector{{Name: "invalid_method", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP, Method: "TRACE"}}}},
			want: "unsupported method",
		},
		{
			name: "negative retry attempts",
			cfg:  &model.Config{Collectors: []model.Collector{{Name: "invalid_retries", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP, Retry: model.RetryConfig{Attempts: -1}}}}},
			want: "retry.attempts",
		},
		{
			name: "negative retry backoff",
			cfg:  &model.Config{Collectors: []model.Collector{{Name: "invalid_backoff", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP, Retry: model.RetryConfig{Backoff: model.Duration(-time.Second)}}}}},
			want: "retry.backoff",
		},
		{
			name: "unknown transform",
			cfg:  &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "invalid_transform", Transform: model.TransformConfig{Type: "lua"}}}},
			want: "unknown transform",
		},
		{
			name: "invalid metric type",
			cfg:  &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "invalid_metric_type", Metrics: []model.MetricRule{{Name: "value", Type: "rate", Expression: ".value"}}}}},
			want: "invalid type",
		},
		{
			name: "invalid metric error mode",
			cfg:  &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "invalid_error_mode", Metrics: []model.MetricRule{{Name: "value", ErrorMode: "panic", Expression: ".value"}}}}},
			want: "want fail, log or ignore",
		},
		{
			name: "invalid label type",
			cfg:  &model.Config{Collectors: []model.Collector{{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "invalid_label", Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "value", Expression: ".value", Labels: []model.LabelRule{{Name: "source", Type: "xpath", Expression: ".source"}}}}}}},
			want: "invalid type",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.cfg)
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
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Load() error=%v, want unknown-field error", err)
	}
}
