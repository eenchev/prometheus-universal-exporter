package config

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// Every transform reports a failing rule the same way, so fail behaves the
// same whatever the response format.
func TestFailIsReportedByEveryTransform(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		transform   string
		contentType string
		body        string
		expression  string
	}{
		{"jq", "json", "jq", "application/json", `{"present":1}`, ".absent"},
		{"regex", "text", "regex", "text/plain", "present=1\n", `absent=(\d+)`},
		{"csv", "csv", "csv", "text/csv", "server,present\nweb01,1\n", "absent"},
		{"xpath", "xml", "xpath", "application/xml", "<root><present>1</present></root>", "//absent"},
		{"html xpath", "html", "xpath", "text/html", "<html><body><p>1</p></body></html>", "//span"},
		{"css", "html", "css", "text/html", "<html><body><p>1</p></body></html>", "span.absent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testutil.CaptureLogs(t)
			c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
				Name:      "every",
				Response:  model.ResponseConfig{Format: test.format, CSV: model.CSVConfig{Header: boolPtr(true)}},
				Transform: model.TransformConfig{Type: test.transform},
				Metrics:   []model.MetricRule{{Name: "demo_absent", Type: model.GaugeMetricType, Expression: test.expression, ErrorMode: model.ErrorModeFail}},
				Limits:    model.Limits{MaxMetrics: 10},
			}
			cfg := &model.Config{Collectors: []model.Collector{c}}
			if err := Validate(cfg); err != nil {
				t.Fatal(err)
			}
			c = cfg.Collectors[0]
			response := &fetch.HTTPResponse{Body: []byte(test.body), Headers: http.Header{"Content-Type": []string{test.contentType}}}
			decoded, err := decode.Decode(response, &c)
			if err != nil {
				t.Fatal(err)
			}
			_, err = transform.Transform(context.Background(), decoded, response, &c, "python3")
			var failure *transform.MetricFailure
			if !errors.As(err, &failure) {
				t.Fatalf("err=%v (%T), want a *MetricFailure", err, err)
			}
			if failure.Metric != "demo_absent" || failure.Collector != "every" {
				t.Fatalf("failure names %q/%q, want every/demo_absent", failure.Collector, failure.Metric)
			}
		})
	}
}

// The fetch policy is on_fetch_error, named for the stage, for every request
// type; the old http-only name is an unknown key.
func TestFetchPolicyIsNamedForTheStage(t *testing.T) {
	conf := testutil.WriteFile(t, "config.yaml", strings.Replace(testutil.MinimalConfig, "    transform:", "    error_handling:\n      on_http_error: log\n    transform:", 1))
	if _, err := Load(conf); err == nil || !strings.Contains(err.Error(), "on_http_error") {
		t.Fatalf("on_http_error was accepted: %v", err)
	}
	conf = testutil.WriteFile(t, "config.yaml", strings.Replace(testutil.MinimalConfig, "    transform:", "    error_handling:\n      on_fetch_error: log\n    transform:", 1))
	c, err := Load(conf)
	if err != nil || c.Collectors[0].ErrorHandling.OnFetchError != model.ErrorPolicyLog {
		t.Fatalf("on_fetch_error: %v %+v", err, c)
	}
}
