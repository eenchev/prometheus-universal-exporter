package exporter

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every line of a histogram and a summary carries the series' timestamp, as
// a plain sample's line does, and the exposition says it is UTF-8.
func TestExpositionTimestampsAndCharset(t *testing.T) {
	at := int64(1727000000000)
	set := &model.MetricSet{Metrics: []model.Metric{
		{Name: "h", Type: model.HistogramMetricType, Timestamp: &at, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 2}}, Sum: 3, Count: 4}},
		{Name: "s", Type: model.SummaryMetricType, Timestamp: &at, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: 2, Count: 3}},
		{Name: "g", Type: model.GaugeMetricType, Value: 1, Timestamp: &at},
		{Name: "u", Type: model.GaugeMetricType, Value: 1},
	}}
	recorder := httptest.NewRecorder()
	writeMetricSet(recorder, set)
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type %q", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(recorder.Body.String()), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "u ") {
			if line != "u 1" {
				t.Errorf("a series without a timestamp: %q", line)
			}
			continue
		}
		if !strings.HasSuffix(line, " 1727000000000") {
			t.Errorf("%q has no timestamp", line)
		}
	}
}
