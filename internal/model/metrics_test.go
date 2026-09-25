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

// A value that is not a number is named as a person reads it: text quoted and
// cut to a sane length, an object or an array by its kind and size, never in
// Go's syntax or with strconv's wording.
func TestNumberErrorsNameTheValue(t *testing.T) {
	long := strings.Repeat("é", 100)
	for _, test := range []struct {
		value any
		want  string
	}{
		{"n/a", `value "n/a" is not a number; map text to numbers with value_map`},
		{" n/a ", `value "n/a" is not a number`},
		{"1e999", `value "1e999" is beyond the range of a 64-bit float`},
		{long, `value "` + strings.Repeat("é", 32) + `"... (200 bytes) is not a number`},
		{map[string]any{"a": []any{1, 2, map[string]any{"b": "x"}}, "c": nil}, "value is an object with 2 keys, not a number"},
		{map[string]any{"a": 1}, "value is an object with 1 key, not a number"},
		{[]any{1, 2, 3}, "value is an array of 3 items, not a number"},
		{nil, "value is null, not a number"},
	} {
		_, err := Number(test.value)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Number(%v): err=%v, want %q", test.value, err, test.want)
			continue
		}
		for _, internal := range []string{"strconv", "invalid syntax", "map[", "<nil>"} {
			if strings.Contains(err.Error(), internal) {
				t.Errorf("Number(%v): %q shows Go internals", test.value, err)
			}
		}
	}
	for _, value := range []any{true, false, " 12.5 ", 3} {
		if _, err := Number(value); err != nil {
			t.Errorf("Number(%v): %v", value, err)
		}
	}
}
