package exporter

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Histograms and summaries reach OTLP with their data, and every series of a
// family is a data point of one OTLP metric (otlp.go).

func roundTripOTLP(t *testing.T, metrics ...model.Metric) []otlpMetric {
	t.Helper()
	out := otlpMetrics(model.MetricSet{Metrics: metrics}, "1", nil)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("the export does not encode: %v", err)
	}
	var decoded []otlpMetric
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the export does not decode: %v\n%s", err, raw)
	}
	return decoded
}

func TestHistogramsAreExportedAsOTLPHistograms(t *testing.T) {
	for name, tc := range map[string]struct {
		buckets []model.Bucket
		count   uint64
		bounds  []otlpDouble
		counts  []string
	}{
		"with a +Inf bucket": {
			buckets: []model.Bucket{{UpperBound: 0.1, CumulativeCount: 2}, {UpperBound: 1, CumulativeCount: 5}, {UpperBound: math.Inf(1), CumulativeCount: 7}}, count: 7,
			bounds: []otlpDouble{0.1, 1}, counts: []string{"2", "3", "2"},
		},
		"without one": {
			buckets: []model.Bucket{{UpperBound: 0.1, CumulativeCount: 2}, {UpperBound: 1, CumulativeCount: 5}}, count: 9,
			bounds: []otlpDouble{0.1, 1}, counts: []string{"2", "3", "4"},
		},
		"out of order": {
			buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 5}, {UpperBound: 0.1, CumulativeCount: 2}}, count: 5,
			bounds: []otlpDouble{0.1, 1}, counts: []string{"2", "3", "0"},
		},
		"no buckets": {
			count: 3, bounds: []otlpDouble{}, counts: []string{"3"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := roundTripOTLP(t, model.Metric{Name: "latency_seconds", Type: model.HistogramMetricType, Labels: map[string]string{"path": "/"}, Histogram: &model.Histogram{Buckets: tc.buckets, Sum: 1.5, Count: tc.count}})
			if len(out) != 1 || out[0].Histogram == nil || out[0].Gauge != nil {
				t.Fatalf("not a histogram: %+v", out)
			}
			h := out[0].Histogram
			if h.AggregationTemporality != "AGGREGATION_TEMPORALITY_CUMULATIVE" || len(h.DataPoints) != 1 {
				t.Fatalf("histogram %+v", h)
			}
			point := h.DataPoints[0]
			if !reflect.DeepEqual(point.ExplicitBounds, tc.bounds) || !reflect.DeepEqual(point.BucketCounts, tc.counts) {
				t.Fatalf("bounds %v counts %v, want %v %v", point.ExplicitBounds, point.BucketCounts, tc.bounds, tc.counts)
			}
			if len(point.BucketCounts) != len(point.ExplicitBounds)+1 {
				t.Fatal("OTLP needs one more count than bounds")
			}
			if point.Count != strconvU(tc.count) || point.Sum == nil || *point.Sum != 1.5 || point.Attributes[0].Key != "path" {
				t.Fatalf("point %+v", point)
			}
		})
	}
}

func strconvU(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSummariesAreExportedAsOTLPSummaries(t *testing.T) {
	out := roundTripOTLP(t,
		model.Metric{Name: "rpc_seconds", Type: model.SummaryMetricType, Summary: &model.Summary{Count: 10, Sum: 2.5, Quantiles: []model.Quantile{{Quantile: 0.5, Value: 0.2}, {Quantile: 0.99, Value: 0.9}}}},
		model.Metric{Name: "go_gc_duration_seconds", Type: model.SummaryMetricType, Summary: &model.Summary{Count: 3, Sum: 0.01}},
	)
	if len(out) != 2 || out[0].Summary == nil || out[1].Summary == nil {
		t.Fatalf("not summaries: %+v", out)
	}
	point := out[0].Summary.DataPoints[0]
	if point.Count != "10" || point.Sum != 2.5 || !reflect.DeepEqual(point.QuantileValues, []otlpQuantileValue{{0.5, 0.2}, {0.99, 0.9}}) {
		t.Fatalf("summary point %+v", point)
	}
	if gc := out[1].Summary.DataPoints[0]; gc.Count != "3" || len(gc.QuantileValues) != 0 {
		t.Fatalf("a summary without quantiles: %+v", gc)
	}
}

// A family's series are the data points of one metric; counters are
// monotonic cumulative sums; a histogram type without data stays a gauge.
func TestSeriesOfAFamilyShareOneOTLPMetric(t *testing.T) {
	out := roundTripOTLP(t,
		model.Metric{Name: "requests_total", Type: model.CounterMetricType, Value: 1, Labels: map[string]string{"code": "200"}},
		model.Metric{Name: "temperature", Type: model.GaugeMetricType, Value: 20},
		model.Metric{Name: "requests_total", Type: model.CounterMetricType, Value: 2, Labels: map[string]string{"code": "500"}},
		model.Metric{Name: "bare", Type: model.HistogramMetricType, Value: 4},
	)
	if len(out) != 3 {
		t.Fatalf("want 3 metrics, got %d: %+v", len(out), out)
	}
	if out[0].Name != "requests_total" || out[0].Sum == nil || !out[0].Sum.IsMonotonic || len(out[0].Sum.DataPoints) != 2 {
		t.Fatalf("counter %+v", out[0])
	}
	if out[1].Gauge == nil || out[2].Gauge == nil || *out[2].Gauge.DataPoints[0].AsDouble != 4 {
		t.Fatalf("gauges %+v %+v", out[1], out[2])
	}
}

// NaN and infinities are encoded the way the protobuf JSON mapping writes
// them, instead of failing the encoding of the whole export.
func TestNonFiniteValuesEncode(t *testing.T) {
	out := roundTripOTLP(t,
		model.Metric{Name: "not_a_number", Type: model.GaugeMetricType, Value: math.NaN()},
		model.Metric{Name: "up_high", Type: model.GaugeMetricType, Value: math.Inf(1)},
		model.Metric{Name: "down_low", Type: model.GaugeMetricType, Value: math.Inf(-1)},
	)
	if !math.IsNaN(float64(*out[0].Gauge.DataPoints[0].AsDouble)) || !math.IsInf(float64(*out[1].Gauge.DataPoints[0].AsDouble), 1) || !math.IsInf(float64(*out[2].Gauge.DataPoints[0].AsDouble), -1) {
		t.Fatalf("non-finite values did not survive: %+v", out)
	}
	raw, _ := json.Marshal(otlpMetrics(model.MetricSet{Metrics: []model.Metric{{Name: "n", Type: model.GaugeMetricType, Value: math.NaN()}}}, "1", nil))
	if !strings.Contains(string(raw), `"asDouble":"NaN"`) {
		t.Fatalf("NaN encoded as %s", raw)
	}
}
