//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A transform or the prometheus decoder stops at the first series past
// limits.max_metrics. The scrape then fails as the validation failed it when
// the count came afterwards: at the validation stage, counted in
// http_exporter_series_limit_exceeded_total, and whatever on_transform_error
// and on_decode_error say, since they are about a response the exporter could
// not read, not one it would not keep.
func TestSeriesLimitFailsTheScrapeAsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		collector               model.Collector
	}{
		{"regex", "text/plain", strings.Repeat("1\n", 1000), model.Collector{
			Transform: model.TransformConfig{Type: "regex"},
			Metrics:   []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Expression: `(\d+)`}},
		}},
		{"prometheus", "text/plain; version=0.0.4", promExposition(1000), model.Collector{
			Transform: model.TransformConfig{Type: "prometheus"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(target.Close)
			c := tc.collector
			c.Name = "limited"
			c.Request = model.RequestConfig{Type: fetch.RequestTypeHTTP, Method: "GET"}
			c.ErrorHandling = model.ErrorHandling{OnDecodeError: "ignore", OnTransformError: "ignore"}
			c.Limits = model.Limits{MaxMetrics: 10}
			server := modeServer(t, c)
			response := probe(t, server, url.QueryEscape(target.URL), "limited")
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "metric count 11 exceeds limit 10") {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			metrics := selfMetrics(t, server)
			for series, want := range map[string]float64{
				`http_exporter_series_limit_exceeded_total{collector="limited"}`: 1,
				`http_exporter_transform_errors_total{collector="limited"}`:      0,
				`http_exporter_parse_errors_total{collector="limited"}`:          0,
			} {
				if got := seriesValue(t, metrics, series); got != want {
					t.Errorf("%s = %v, want %v", series, got, want)
				}
			}
		})
	}
}

func promExposition(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(`n{i="`)
		b.WriteString(strings.Repeat("x", i))
		b.WriteString("\"} 1\n")
	}
	return b.String()
}
