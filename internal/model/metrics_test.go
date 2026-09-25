package model

import (
	"strings"
	"testing"
)

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
			// Prometheus reads a label with an empty value as no label.
			name: "duplicate through an empty label",
			set:  MetricSet{Metrics: []Metric{{Name: "value", Type: GaugeMetricType, Value: 1, Labels: map[string]string{"a": ""}}, {Name: "value", Type: GaugeMetricType, Value: 2}}},
			want: "duplicate metric series \"value\": Prometheus reads a label with an empty value as no label",
		},
		{
			name: "a reserved label name",
			set:  MetricSet{Metrics: []Metric{{Name: "value", Type: GaugeMetricType, Value: 1, Labels: map[string]string{"__name__": "other"}}}},
			want: "starts with __, which Prometheus reserves",
		},
		{
			name: "a gauge named like a histogram's series",
			set: MetricSet{Metrics: []Metric{
				{Name: "foo", Type: HistogramMetricType, Histogram: &Histogram{Count: 1}},
				{Name: "foo_count", Type: GaugeMetricType, Value: 1},
			}},
			want: `metric "foo_count" (gauge) clashes with the histogram "foo"`,
		},
		{
			name: "a counter named like a summary's series",
			set: MetricSet{Metrics: []Metric{
				{Name: "bar_sum", Type: CounterMetricType, Value: 1},
				{Name: "bar", Type: SummaryMetricType, Summary: &Summary{Count: 1}},
			}},
			want: `clashes with the summary "bar"`,
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
