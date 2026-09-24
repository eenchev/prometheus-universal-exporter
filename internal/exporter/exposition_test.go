package exporter

import (
	"math"
	"net/http/httptest"
	"strconv"
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

// The writer renders every kind of series exactly: help and label escaping,
// sorted labels, le and quantile over a label of that name, the +Inf bucket
// from the count, special floats and timestamps on every line.
func TestExpositionText(t *testing.T) {
	at := int64(1700000000000)
	set := &model.MetricSet{Metrics: []model.Metric{
		{Name: "g", Help: "a \\ b\nc", Type: model.GaugeMetricType, Value: 1.5, Labels: map[string]string{"z": "q\"x\\y\nz", "a": "1"}},
		{Name: "g", Type: model.GaugeMetricType, Value: math.NaN()},
		{Name: "c", Type: model.CounterMetricType, Value: math.Inf(1), Timestamp: &at},
		{Name: "h", Help: "hist", Type: model.HistogramMetricType, Labels: map[string]string{"le": "own", "b": "x"}, Timestamp: &at,
			Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 0.5, CumulativeCount: 1}, {UpperBound: math.Inf(1), CumulativeCount: 3}}, Sum: 2.25, Count: 3}},
		{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.99, Value: -1e-7}}, Sum: 1e21, Count: 4}},
		{Name: "g", Type: model.GaugeMetricType, Value: math.Inf(-1), Labels: map[string]string{"k": ""}},
	}}
	want := "# HELP g a \\\\ b\\nc\n# TYPE g gauge\n" +
		"g{a=\"1\",z=\"q\\\"x\\\\y\\nz\"} 1.5\n" +
		"g NaN\n" +
		"# TYPE c counter\nc +Inf 1700000000000\n" +
		"# HELP h hist\n# TYPE h histogram\n" +
		"h_bucket{b=\"x\",le=\"0.5\"} 1 1700000000000\n" +
		"h_bucket{b=\"x\",le=\"+Inf\"} 3 1700000000000\n" +
		"h_sum{b=\"x\",le=\"own\"} 2.25 1700000000000\n" +
		"h_count{b=\"x\",le=\"own\"} 3 1700000000000\n" +
		"# TYPE s summary\n" +
		"s{quantile=\"0.99\"} -1e-07\ns_sum 1e+21\ns_count 4\n" +
		"g{k=\"\"} -Inf\n"
	recorder := httptest.NewRecorder()
	writeMetricSet(recorder, set)
	if got := recorder.Body.String(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// A second answer from a pooled buffer holds nothing of the first.
	recorder = httptest.NewRecorder()
	writeMetricSet(recorder, &model.MetricSet{Metrics: []model.Metric{{Name: "x", Type: model.GaugeMetricType, Value: 2}}})
	if got := recorder.Body.String(); got != "# TYPE x gauge\nx 2\n" {
		t.Fatalf("got %q", got)
	}
}

func BenchmarkExposition(b *testing.B) {
	set := &model.MetricSet{}
	for i := 0; i < 1000; i++ {
		set.Metrics = append(set.Metrics, model.Metric{Name: "series", Help: "Some series.", Type: model.GaugeMetricType, Value: float64(i), Labels: map[string]string{"instance": "host:9100", "job": "node", "id": strconv.Itoa(i)}})
	}
	b.ReportAllocs()
	for b.Loop() {
		writeMetricSet(httptest.NewRecorder(), set)
	}
}
