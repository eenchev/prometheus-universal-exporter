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
