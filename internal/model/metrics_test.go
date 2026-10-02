package model

import (
	"math"
	"slices"
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

// Settle gives a histogram read without a _count its +Inf bucket's count,
// and refuses what cannot be one series: a bound or a quantile twice, in
// whatever order the buckets come. Buckets out of order are not an error.
// Neither is a count that is not the +Inf bucket's, which a target read
// between its updating the one and the other writes: Settle refused it, and
// with it every family of the scrape, and now leaves both numbers as they
// are. A histogram with neither is left as it is too.
func TestHistogramsAndSummariesAreSettled(t *testing.T) {
	bucket := func(le float64, count uint64) Bucket { return Bucket{UpperBound: le, CumulativeCount: count} }
	inf, nan := math.Inf(1), math.NaN()
	for name, tc := range map[string]struct {
		h     Histogram
		count uint64
		want  string
	}{
		"count from +Inf":        {h: Histogram{Buckets: []Bucket{bucket(1, 5), bucket(inf, 7)}, NoCount: true}, count: 7},
		"count and +Inf agree":   {h: Histogram{Buckets: []Bucket{bucket(1, 5), bucket(inf, 7)}, Count: 7}, count: 7},
		"count without +Inf":     {h: Histogram{Buckets: []Bucket{bucket(1, 5)}, Count: 7}, count: 7},
		"no buckets, a count":    {h: Histogram{Count: 3}, count: 3},
		"out of order":           {h: Histogram{Buckets: []Bucket{bucket(5, 3), bucket(nan, 1), bucket(1, 2), bucket(inf, 3)}, Count: 3}, count: 3},
		"count and +Inf differ":  {h: Histogram{Buckets: []Bucket{bucket(inf, 7)}, Count: 9}, count: 9},
		"neither":                {h: Histogram{Buckets: []Bucket{bucket(1, 5)}, NoCount: true}, count: 0},
		"a bound twice":          {h: Histogram{Buckets: []Bucket{bucket(1, 5), bucket(1, 6), bucket(inf, 7)}, Count: 7}, want: "has two buckets with the upper bound 1"},
		"a bound twice, apart":   {h: Histogram{Buckets: []Bucket{bucket(0.5, 5), bucket(2, 6), bucket(inf, 7), bucket(0.5, 5)}, Count: 7}, want: "has two buckets with the upper bound 0.5"},
		"NaN twice":              {h: Histogram{Buckets: []Bucket{bucket(nan, 5), bucket(1, 6), bucket(nan, 7)}, Count: 7}, want: "has two buckets with the upper bound NaN"},
		"+Inf twice, both count": {h: Histogram{Buckets: []Bucket{bucket(inf, 7), bucket(inf, 7)}, NoCount: true}, want: "has two buckets with the upper bound +Inf"},
	} {
		err := tc.h.Settle()
		switch {
		case tc.want == "" && (err != nil || tc.h.Count != tc.count):
			t.Errorf("%s: count %d, %v; want %d", name, tc.h.Count, err, tc.count)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	torn := Histogram{Buckets: []Bucket{bucket(1, 5), bucket(inf, 7)}, Sum: 3, Count: 9}
	if err := torn.Settle(); err != nil || torn.Count != 9 || !slices.Equal(torn.Buckets, []Bucket{bucket(1, 5), bucket(inf, 7)}) {
		t.Errorf("a count that is not the +Inf bucket's: %v, %+v; want both numbers kept", err, torn)
	}
	quantile := func(q float64) Quantile { return Quantile{Quantile: q, Value: 1} }
	if err := (&Summary{Quantiles: []Quantile{quantile(0.99), quantile(0.5)}}).Settle(); err != nil {
		t.Errorf("quantiles out of order: %v", err)
	}
	if err := (&Summary{Quantiles: []Quantile{quantile(0.5), quantile(0.99), quantile(0.5)}}).Settle(); err == nil || !strings.Contains(err.Error(), "has two values for the quantile 0.5") {
		t.Errorf("a quantile twice: %v", err)
	}
}

// A histogram's +Inf bucket is the one it was read with, wherever among its
// buckets, with the count that bucket has, also when its _count is another;
// one read without the bucket has its _count as it; and one read with
// neither has none.
func TestAHistogramsInfBucketIsItsOwnOrItsCount(t *testing.T) {
	bucket := func(le float64, count uint64) Bucket { return Bucket{UpperBound: le, CumulativeCount: count} }
	inf := math.Inf(1)
	for name, tc := range map[string]struct {
		h     Histogram
		count uint64
		has   bool
	}{
		"its own, last":                 {Histogram{Buckets: []Bucket{bucket(1, 5), bucket(inf, 7)}, Count: 7}, 7, true},
		"its own, first":                {Histogram{Buckets: []Bucket{bucket(inf, 7), bucket(1, 5)}, Count: 7}, 7, true},
		"its own, beside another count": {Histogram{Buckets: []Bucket{bucket(1, 5), bucket(inf, 7)}, Count: 9}, 7, true},
		"its own, without a count":      {Histogram{Buckets: []Bucket{bucket(inf, 7)}, NoCount: true}, 7, true},
		"the count":                     {Histogram{Buckets: []Bucket{bucket(1, 5)}, Count: 9}, 9, true},
		"the count, without buckets":    {Histogram{Count: 9}, 9, true},
		"-Inf is not it":                {Histogram{Buckets: []Bucket{bucket(math.Inf(-1), 5)}, NoCount: true}, 0, false},
		"neither":                       {Histogram{Buckets: []Bucket{bucket(1, 5)}, NoCount: true}, 0, false},
		"nothing":                       {Histogram{NoCount: true, NoSum: true}, 0, false},
	} {
		if count, has := tc.h.InfBucket(); has != tc.has || has && count != tc.count {
			t.Errorf("%s: %d, %v; want %d, %v", name, count, has, tc.count, tc.has)
		}
	}
}

// le tells a histogram's buckets apart and quantile a summary's quantiles,
// so neither series may have that label for itself; on any other series, and
// the other way round, they are labels like any other.
func TestAHistogramHasNoLeOfItsOwnNorASummaryAQuantile(t *testing.T) {
	histogram := func(labels map[string]string) Metric {
		return Metric{Name: "h", Type: HistogramMetricType, Labels: labels, Histogram: &Histogram{Buckets: []Bucket{{UpperBound: 1, CumulativeCount: 1}}, Count: 1}}
	}
	summary := func(labels map[string]string) Metric {
		return Metric{Name: "s", Type: SummaryMetricType, Labels: labels, Summary: &Summary{Quantiles: []Quantile{{Quantile: 0.5, Value: 1}}, Count: 1}}
	}
	for name, test := range map[string]struct {
		metric Metric
		want   string
	}{
		"a histogram with le":       {histogram(map[string]string{"le": "own"}), `metric "h" is a histogram and has a label le of its own, which its buckets carry; name the label something else`},
		"a summary with quantile":   {summary(map[string]string{"quantile": "own"}), `metric "s" is a summary and has a label quantile of its own, which its quantiles carry; name the label something else`},
		"a histogram with quantile": {histogram(map[string]string{"quantile": "0.5"}), ""},
		"a summary with le":         {summary(map[string]string{"le": "1"}), ""},
		"a gauge with both":         {Metric{Name: "g", Type: GaugeMetricType, Labels: map[string]string{"le": "1", "quantile": "0.5"}}, ""},
	} {
		set := MetricSet{Metrics: []Metric{test.metric}}
		err := set.Validate(Limits{})
		if test.want == "" && err != nil || test.want != "" && (err == nil || err.Error() != test.want) {
			t.Errorf("%s: %v, want %q", name, err, test.want)
		}
	}
}
