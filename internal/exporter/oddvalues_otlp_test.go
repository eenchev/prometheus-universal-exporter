package exporter

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A family with values its type does not allow — a counter that is NaN or
// negative, a histogram whose bucket counts fall, a summary with a quantile
// of 1.5 — cannot be a monotonic sum, a histogram or a summary in OTLP any
// more than in OpenMetrics. It is exported as gauges under the names of its
// samples, with the series and values the exposition writes, so every point
// of an export is valid and none is left out (otlp.go: otlpMetrics).

// otlpError is why an export's metrics are not valid OTLP of their kinds, or
// nil: a monotonic sum is neither NaN nor negative; a histogram has bounds
// in ascending order, one more bucket count than bounds, bucket counts that
// add up to its count, and a sum, if any, that is neither NaN nor negative
// and stands beside no negative bound; a summary has quantiles within 0 to 1
// whose values are not negative, and such a sum too; and a name is one
// metric.
func otlpError(metrics []otlpMetric) error {
	names := map[string]bool{}
	for _, m := range metrics {
		if names[m.Name] {
			return fmt.Errorf("two metrics are named %s", m.Name)
		}
		names[m.Name] = true
		switch {
		case m.Sum != nil:
			for _, p := range m.Sum.DataPoints {
				if v := float64(*p.AsDouble); !(v >= 0) {
					return fmt.Errorf("the monotonic sum %s is %v", m.Name, v)
				}
			}
		case m.Histogram != nil:
			for _, p := range m.Histogram.DataPoints {
				if len(p.BucketCounts) != len(p.ExplicitBounds)+1 {
					return fmt.Errorf("the histogram %s has %d bucket counts for %d bounds", m.Name, len(p.BucketCounts), len(p.ExplicitBounds))
				}
				negative := false
				for i, bound := range p.ExplicitBounds {
					if math.IsNaN(float64(bound)) || (i > 0 && !(bound > p.ExplicitBounds[i-1])) {
						return fmt.Errorf("the histogram %s has the bounds %v", m.Name, p.ExplicitBounds)
					}
					negative = negative || bound < 0
				}
				var total uint64
				for _, text := range p.BucketCounts {
					count, err := strconv.ParseUint(text, 10, 64)
					if err != nil || total+count < total {
						return fmt.Errorf("the histogram %s has the bucket counts %v", m.Name, p.BucketCounts)
					}
					total += count
				}
				if strconv.FormatUint(total, 10) != p.Count {
					return fmt.Errorf("the bucket counts %v of the histogram %s do not add up to its count %s", p.BucketCounts, m.Name, p.Count)
				}
				if p.Sum != nil && (!(*p.Sum >= 0) || negative) {
					return fmt.Errorf("the histogram %s has the sum %v and the bounds %v", m.Name, *p.Sum, p.ExplicitBounds)
				}
			}
		case m.Summary != nil:
			for _, p := range m.Summary.DataPoints {
				for _, q := range p.QuantileValues {
					if !(q.Quantile >= 0 && q.Quantile <= 1) || q.Value < 0 {
						return fmt.Errorf("the summary %s has the quantile %v with the value %v", m.Name, q.Quantile, q.Value)
					}
				}
				if !(p.Sum >= 0) {
					return fmt.Errorf("the summary %s has the sum %v", m.Name, p.Sum)
				}
			}
		case m.Gauge == nil:
			return errors.New("a metric of no kind")
		}
	}
	return nil
}

// otlpGaugeSeries are the points of an export's gauges and sums as the text
// format writes series, sorted, without their timestamps.
func otlpGaugeSeries(metrics []otlpMetric) []string {
	var series []string
	for _, m := range metrics {
		var points []otlpNumberDataPoint
		switch {
		case m.Gauge != nil:
			points = m.Gauge.DataPoints
		case m.Sum != nil:
			points = m.Sum.DataPoints
		}
		for _, p := range points {
			s := exposedSample{name: m.Name, labels: map[string]string{}, value: float64(*p.AsDouble)}
			for _, attribute := range p.Attributes {
				s.labels[attribute.Key] = attribute.Value.StringValue
			}
			series = append(series, s.String())
		}
	}
	slices.Sort(series)
	return series
}

// Every set of odd values is exported as valid OTLP. A family whose values
// its type allows is one metric of its kind, and any other is gauges holding
// the series and values of the text format's answer. Under the race
// detector the sets of one family, which are all that is counted, are joined
// by the first 100 of the 1,000 random sets of several families.
func TestOTLPExportOfOddValuesIsValidAndLeavesNothingOut(t *testing.T) {
	kinds := map[model.MetricType]int{
		model.GaugeMetricType: otlpKindGauge, model.UntypedMetricType: otlpKindGauge, model.CounterMetricType: otlpKindSum,
		model.HistogramMetricType: otlpKindHistogram, model.SummaryMetricType: otlpKindSummary,
	}
	typed, untyped := 0, 0
	for _, c := range oddCases(alloctest.UnlessRaced(1000, 100)) {
		out := roundTripOTLP(t, c.set.Metrics...)
		if err := otlpError(out); err != nil {
			t.Fatalf("%s: %v\n%+v", c.name, err, out)
		}
		if !c.single {
			continue
		}
		fits := !slices.ContainsFunc(c.set.Metrics, func(m model.Metric) bool { return !typedValues(m) })
		if fits {
			typed++
			if len(out) != 1 || otlpKind(out[0]) != kinds[c.set.Metrics[0].Type] || out[0].Name != c.set.Metrics[0].Name {
				t.Fatalf("%s: a family whose values its type allows is exported as %+v", c.name, out)
			}
			if kind := otlpKind(out[0]); kind == otlpKindHistogram || kind == otlpKindSummary {
				continue
			}
		} else {
			untyped++
			if slices.ContainsFunc(out, func(m otlpMetric) bool { return m.Gauge == nil }) {
				t.Fatalf("%s: a family whose values its type does not allow is exported as %+v", c.name, out)
			}
		}
		var want []string
		for _, line := range strings.Split(string(appendMetricSet(nil, c.set)), "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			s, err := readSampleLine(line, false)
			if err != nil {
				t.Fatal(err)
			}
			s.millis = nil
			want = append(want, s.String())
		}
		slices.Sort(want)
		if got := otlpGaugeSeries(out); !slices.Equal(got, want) {
			t.Fatalf("%s: the export holds\n%s\nand the text format\n%s", c.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	if typed < 100 || untyped < 100 {
		t.Fatalf("%d families kept their kind and %d did not: the cases do not cover both", typed, untyped)
	}
}

// A counter family with a series that is NaN or negative is a gauge, every
// series of it, so the name is one kind of metric; it carries no start time,
// which a gauge has none of; and a counter family beside it stays a
// monotonic sum.
func TestACounterFamilyThatDoesNotCountIsAnOTLPGauge(t *testing.T) {
	started := 0
	start := func(model.Metric, string) string { started++; return "5" }
	out := otlpMetrics(model.MetricSet{Metrics: []model.Metric{
		{Name: "jobs_total", Help: "Jobs.", Type: model.CounterMetricType, Value: 3, Labels: map[string]string{"queue": "a"}},
		{Name: "served_total", Type: model.CounterMetricType, Value: 7},
		{Name: "jobs_total", Help: "Jobs.", Type: model.CounterMetricType, Value: -5, Labels: map[string]string{"queue": "b"}},
		{Name: "lost_total", Type: model.CounterMetricType, Value: math.NaN()},
	}}, "1", start)
	if len(out) != 3 || out[0].Name != "jobs_total" || out[0].Gauge == nil || out[0].Description != "Jobs." || len(out[0].Gauge.DataPoints) != 2 {
		t.Fatalf("the family with a negative series: %+v", out)
	}
	if a, b := out[0].Gauge.DataPoints[0], out[0].Gauge.DataPoints[1]; *a.AsDouble != 3 || *b.AsDouble != -5 || a.StartTimeUnixNano != "" || b.Attributes[0].Value.StringValue != "b" {
		t.Fatalf("its points: %+v %+v", a, b)
	}
	if out[1].Sum == nil || !out[1].Sum.IsMonotonic || out[1].Sum.DataPoints[0].StartTimeUnixNano != "5" || started != 1 {
		t.Fatalf("the counter that counts: %+v, %d start times asked for", out[1], started)
	}
	if out[2].Gauge == nil || !math.IsNaN(float64(*out[2].Gauge.DataPoints[0].AsDouble)) {
		t.Fatalf("the NaN counter: %+v", out[2])
	}
}

// A histogram family with a series whose bucket counts fall is exported as
// the gauges of its lines: its buckets, in order, with le, its _sum and its
// _count, each series of the family with its own labels and timestamp. One
// read without a _sum has no such gauge.
func TestAHistogramFamilyThatIsNoOTLPHistogramIsExportedAsGauges(t *testing.T) {
	at := int64(1727000000123)
	out := roundTripOTLP(t,
		model.Metric{Name: "h", Help: "Latency.", Type: model.HistogramMetricType, Labels: map[string]string{"op": "get", "zone": "a"}, Timestamp: &at, Histogram: &model.Histogram{
			Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 2}, {UpperBound: 0.5, CumulativeCount: 3}}, Sum: 1.5, Count: 4,
		}},
		model.Metric{Name: "h", Help: "Latency.", Type: model.HistogramMetricType, Labels: map[string]string{"op": "put"}, Histogram: &model.Histogram{
			Buckets: []model.Bucket{{UpperBound: math.Inf(1), CumulativeCount: 9}}, Count: 9, NoSum: true, NoCount: true,
		}},
	)
	if len(out) != 3 || out[0].Name != "h_bucket" || out[1].Name != "h_sum" || out[2].Name != "h_count" || out[0].Description != "Latency." {
		t.Fatalf("%+v", out)
	}
	want := []string{
		`h_bucket{le="+Inf",op="get",zone="a"} 4`, `h_bucket{le="+Inf",op="put"} 9`, `h_bucket{le="0.5",op="get",zone="a"} 3`, `h_bucket{le="1",op="get",zone="a"} 2`,
		`h_count{op="get",zone="a"} 4`, `h_sum{op="get",zone="a"} 1.5`,
	}
	if got := otlpGaugeSeries(out); !slices.Equal(got, want) {
		t.Fatalf("the export holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	buckets := out[0].Gauge.DataPoints
	keys := func(p otlpNumberDataPoint) (names []string) {
		for _, attribute := range p.Attributes {
			names = append(names, attribute.Key+"="+attribute.Value.StringValue)
		}
		return names
	}
	if !reflect.DeepEqual(keys(buckets[0]), []string{"le=0.5", "op=get", "zone=a"}) || !reflect.DeepEqual(keys(buckets[1]), []string{"le=1", "op=get", "zone=a"}) {
		t.Fatalf("the buckets are not in order, each with le among its attributes in name order: %v %v", keys(buckets[0]), keys(buckets[1]))
	}
	if buckets[0].TimeUnixNano != "1727000000123000000" || buckets[3].TimeUnixNano != "1" {
		t.Fatalf("the points' times: %s and %s", buckets[0].TimeUnixNano, buckets[3].TimeUnixNano)
	}
}

// A summary family with a quantile outside 0 to 1, or a negative value or
// sum, is exported as the gauges of its lines, its quantiles in order. One
// whose values its type allows stays a summary, and one read without a _sum
// and a _count is sent with 0 for them whatever its fields hold.
func TestASummaryFamilyThatIsNoOTLPSummaryIsExportedAsGauges(t *testing.T) {
	out := roundTripOTLP(t,
		model.Metric{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 99, Value: 2}, {Quantile: 50, Value: 1}}, Sum: 3, Count: 4}},
		model.Metric{Name: "t", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: -2}}, Sum: -8, NoCount: true}},
		model.Metric{Name: "u", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.99, Value: math.NaN()}, {Quantile: 0.5, Value: 2}}, Sum: 8, Count: 4}},
		model.Metric{Name: "v", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 2}}, Sum: -8, Count: 4, NoSum: true, NoCount: true}},
	)
	if len(out) != 7 || out[5].Name != "u" || out[5].Summary == nil || out[6].Name != "v" || out[6].Summary == nil {
		t.Fatalf("%+v", out)
	}
	if alone := out[6].Summary.DataPoints[0]; alone.Sum != 0 || alone.Count != "0" || len(alone.QuantileValues) != 1 {
		t.Fatalf("the summary read without a sum and a count: %+v", alone)
	}
	if kept := out[5].Summary.DataPoints[0].QuantileValues; len(kept) != 2 || kept[0].Quantile != 0.5 || kept[1].Quantile != 0.99 || !math.IsNaN(float64(kept[1].Value)) {
		t.Fatalf("the summary whose values its type allows: %+v", kept)
	}
	want := []string{`s_count{} 4`, `s_sum{} 3`, `s{quantile="50"} 1`, `s{quantile="99"} 2`, `t_sum{} -8`, `t{quantile="0.5"} -2`}
	if got := otlpGaugeSeries(out[:5]); !slices.Equal(got, want) {
		t.Fatalf("the export holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if first := out[0].Gauge.DataPoints; out[0].Name != "s" || first[0].Attributes[0].Value.StringValue != "50" || first[1].Attributes[0].Value.StringValue != "99" {
		t.Fatalf("the quantiles are not in order: %+v", first)
	}
}
