package config

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

func validated(t *testing.T, c model.Collector) *model.Collector {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg.Collectors[0]
}

func transformResponse(t *testing.T, c *model.Collector, body, contentType string) (*model.MetricSet, error) {
	t.Helper()
	response := &fetch.HTTPResponse{Body: []byte(body), Headers: make(http.Header)}
	if contentType != "" {
		response.Headers.Set("Content-Type", contentType)
	}
	decoded, err := decode.Decode(response, c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return transform.Transform(context.Background(), decoded, response, c, "python3")
}

func TestPreScriptScalarResultKeepsDecodedFormat(t *testing.T) {
	c := validated(t, model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "scalar",
		Transform: model.TransformConfig{Type: "regex", PreScript: `data = data.replace("cpu", "value")`},
		Limits:    scriptLimits(),
		Metrics:   []model.MetricRule{{Name: "worker_value", Type: model.GaugeMetricType, Expression: `value=(\d+)`}},
	})
	set, err := transformResponse(t, c, "cpu=42\n", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 42 {
		t.Fatalf("unexpected metrics: %#v", set.Metrics)
	}
}

func TestPreScriptStillReparsesHTML(t *testing.T) {
	c := validated(t, model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "html_prescript",
		Decoder:   model.DecoderConfig{Type: "html"},
		Transform: model.TransformConfig{Type: "css", PreScript: `data = data.replace("42", "7")`},
		Limits:    scriptLimits(),
		Metrics:   []model.MetricRule{{Name: "page_value", Type: model.GaugeMetricType, Expression: "#value"}},
	})
	set, err := transformResponse(t, c, `<html><body><span id="value">42</span></body></html>`, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 7 {
		t.Fatalf("HTML pre-script output was not reparsed: %#v", set.Metrics)
	}
}

func TestPreScriptDoesNotPromoteForTheCSVTransform(t *testing.T) {
	c := validated(t, model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "csv_prescript",
		Transform: model.TransformConfig{Type: "csv", PreScript: `data = data + [{"server": "extra", "cpu": "9"}]`},
		Limits:    scriptLimits(),
		Metrics: []model.MetricRule{{
			Name:       "server_cpu",
			Type:       model.GaugeMetricType,
			Expression: "cpu",
			Labels:     []model.LabelRule{{Name: "server", Expression: "server"}},
		}},
	})
	set, err := transformResponse(t, c, "server,cpu\nalpha,42\n", "text/csv")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 {
		t.Fatalf("expected the CSV transform to keep consuming rows, got %#v", set.Metrics)
	}
	for _, metric := range set.Metrics {
		if metric.Labels["server"] == "" {
			t.Fatalf("row labels were lost: %#v", set.Metrics)
		}
	}
}

func TestUnstructuredPreScriptResultStillFailsStructuredTransforms(t *testing.T) {
	c := validated(t, model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "unstructured",
		Transform: model.TransformConfig{Type: "jq", PreScript: `data = "still plain text"`},
		Limits:    scriptLimits(),
		Metrics:   []model.MetricRule{{Name: "value", Type: model.GaugeMetricType, Expression: ".value"}},
	})
	_, err := transformResponse(t, c, "cpu=42\n", "text/plain")
	if err == nil || !strings.Contains(err.Error(), "cannot map response format") {
		t.Fatalf("error=%v, want an incompatible response format error", err)
	}
}

func TestPreScriptPromotionSupportsArrayResults(t *testing.T) {
	c := validated(t, model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Name:      "array_prescript",
		Transform: model.TransformConfig{Type: "jq", PreScript: `data = [{"zone": "a", "cpu": 1}, {"zone": "b", "cpu": 2}]`},
		Limits:    scriptLimits(),
		Metrics: []model.MetricRule{{
			Name:       "zone_cpu",
			Type:       model.GaugeMetricType,
			Expression: ".[].cpu",
			Labels:     []model.LabelRule{{Name: "zone", Expression: ".[].zone"}},
		}},
	})
	set, err := transformResponse(t, c, "ignored\n", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["zone"] != "a" || set.Metrics[1].Value != 2 {
		t.Fatalf("unexpected metrics: %#v", set.Metrics)
	}
}
