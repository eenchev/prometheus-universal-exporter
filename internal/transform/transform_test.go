package transform

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestHTMLBareTagSelector(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "html", Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "css"}, Metrics: []model.MetricRule{{Name: "application_status", Type: model.GaugeMetricType, Expression: "h1"}}, ErrorHandling: model.ErrorHandling{AllowMissingKeys: false}, Limits: model.Limits{MaxMetrics: 10}}
	r := &fetch.HTTPResponse{Body: []byte("<html><body><h1>42</h1></body></html>"), Headers: http.Header{"Content-Type": []string{"text/html"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Value != 42 {
		t.Fatalf("unexpected metrics: %#v", m.Metrics)
	}
}

func TestMissingOptionalJSONValue(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "json", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "optional_value", Type: model.GaugeMetricType, Expression: ".missing"}}, ErrorHandling: model.ErrorHandling{AllowMissingKeys: true}, Limits: model.Limits{MaxMetrics: 10}}
	r := &fetch.HTTPResponse{Body: []byte(`{"present":1}`), Headers: make(http.Header)}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 0 {
		t.Fatalf("expected omitted metric, got %#v", m.Metrics)
	}
}

func TestHTMLCSSTableValues(t *testing.T) {
	body, err := os.ReadFile("../../testdata/html/status.html")
	if err != nil {
		t.Fatal(err)
	}
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "html_css",
		Decoder:   model.DecoderConfig{Type: "html"},
		Transform: model.TransformConfig{Type: "css"},
		Metrics:   []model.MetricRule{{Name: "server_cpu", Type: model.GaugeMetricType, Expression: "#servers tr:nth-child(2) td:nth-child(2)", Labels: []model.LabelRule{{Name: "environment", Value: "production"}}}},
		Limits:    model.Limits{MaxMetrics: 10},
	}
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{"Content-Type": []string{"text/html"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Value != 72 || m.Metrics[0].Labels["environment"] != "production" {
		t.Fatalf("unexpected HTML CSS metrics: %#v", m.Metrics)
	}
}

func TestHTMLXPathTableValuesAndLabels(t *testing.T) {
	body, err := os.ReadFile("../../testdata/html/status.html")
	if err != nil {
		t.Fatal(err)
	}
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "html_xpath",
		Decoder:   model.DecoderConfig{Type: "html"},
		Transform: model.TransformConfig{Type: "xpath"},
		Metrics: []model.MetricRule{{
			Name:       "server_cpu",
			Type:       model.GaugeMetricType,
			Expression: `//table[@id='servers']//tr/td[2]`,
			Labels:     []model.LabelRule{{Name: "server", Expression: "preceding-sibling::td[1]"}},
		}},
		Limits: model.Limits{MaxMetrics: 10},
	}
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{"Content-Type": []string{"text/html"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 2 || m.Metrics[0].Labels["server"] != "web01" || m.Metrics[1].Labels["server"] != "web02" {
		t.Fatalf("unexpected HTML XPath metrics: %#v", m.Metrics)
	}
}

func TestPrometheusInputFilteringAndRelabeling(t *testing.T) {
	body, err := os.ReadFile("../../testdata/prometheus/status.prom")
	if err != nil {
		t.Fatal(err)
	}
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "prometheus",
		Decoder:   model.DecoderConfig{Type: "prometheus"},
		Transform: model.TransformConfig{Type: "prometheus"},
		Metrics: []model.MetricRule{{
			Name:        "application_requests_total",
			Description: "Application requests",
			Type:        model.CounterMetricType,
			Expression:  `^vendor_requests_total$`,
			Labels:      []model.LabelRule{{Name: "component", Expression: "service"}},
		}},
		Limits: model.Limits{MaxMetrics: 10},
	}
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{"Content-Type": []string{"text/plain; version=0.0.4"}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Metrics) != 1 || m.Metrics[0].Name != "application_requests_total" || m.Metrics[0].Type != model.CounterMetricType || m.Metrics[0].Value != 42 || m.Metrics[0].Labels["component"] != "api" {
		t.Fatalf("unexpected Prometheus metrics: %#v", m.Metrics)
	}
}
