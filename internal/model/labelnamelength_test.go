package model

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// limits.max_label_name_length holds every label name of every series to a
// length, as limits.max_metric_name_length holds a metric's name: a target or
// a script that wrote a label name of megabytes had it served, cached and
// exported whole.

// A label name one byte over the limit fails the set naming the metric, the
// label, the limit and its key, and the two ways out; one at the limit
// passes; the length is counted in bytes; a limit of 0 holds nothing back.
func TestALabelNameOverItsLimitFailsTheSetNamingTheLimit(t *testing.T) {
	limits := Limits{MaxLabelNameLength: 4}
	gauge := func(labels map[string]string) MetricSet {
		return MetricSet{Metrics: []Metric{{Name: "host_note", Type: GaugeMetricType, Value: 1, Labels: labels}}}
	}
	for _, test := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{name: "one byte over", labels: map[string]string{"zones": "a"},
			want: `metric "host_note" label name "zones" is longer than limits.max_label_name_length 4; rename the label with transform.rename_labels, or raise limits.max_label_name_length`},
		{name: "at the limit", labels: map[string]string{"zone": "a"}},
		{name: "under the limit, with an empty value", labels: map[string]string{"z": ""}},
		{name: "the first of two by name", labels: map[string]string{"zones": "a", "notes": "b", "a": "c"},
			want: `metric "host_note" label name "notes" is longer than limits.max_label_name_length 4; rename the label with transform.rename_labels, or raise limits.max_label_name_length`},
	} {
		set := gauge(test.labels)
		err := set.Validate(limits)
		switch {
		case test.want == "" && err != nil:
			t.Errorf("%s: refused with %v, want accepted", test.name, err)
		case test.want != "" && (err == nil || err.Error() != test.want):
			t.Errorf("%s: the error is\n%v\nwant\n%s", test.name, err, test.want)
		}
	}
	long := gauge(map[string]string{strings.Repeat("l", 1<<16): "v"})
	if err := long.Validate(Limits{}); err != nil {
		t.Errorf("a limit of 0 refuses a long label name: %v", err)
	}
	if err := long.Validate(Limits{MaxLabelNameLength: 1 << 16}); err != nil {
		t.Errorf("a label name at a raised limit is refused: %v", err)
	}
}

// A label name of megabytes is shown in the error by its first 200 bytes and
// its length, as a metric name is, and the error is recognised without the
// length, so a name that grows from one scrape to the next is one failure to
// the log.
func TestAnEnormousLabelNameIsShownByItsStart(t *testing.T) {
	name := strings.Repeat("l", 10<<20)
	set := MetricSet{Metrics: []Metric{{Name: "m", Type: GaugeMetricType, Value: 1, Labels: map[string]string{name: "v"}}}}
	err := set.Validate(Limits{MaxLabelNameLength: 200})
	want := fmt.Sprintf(`metric "m" label name "%s"... (%d bytes) is longer than limits.max_label_name_length 200; rename the label with transform.rename_labels, or raise limits.max_label_name_length`, name[:200], len(name))
	if err == nil || err.Error() != want {
		t.Fatalf("the error is %.400v, want %.400s", err, want)
	}
	set.Metrics[0].Labels = map[string]string{name + "ll": "v"}
	if again := set.Validate(Limits{MaxLabelNameLength: 200}); SameFailureText(again) != SameFailureText(err) {
		t.Errorf("a longer name is recognised as %q, the first as %q", SameFailureText(again), SameFailureText(err))
	}
}

// The checks keep their order: a metric name that is no name or too long is
// named before any label; a label name that is no name is named as such,
// however long; a label name too long is named before its value, and before
// it starts with the __ Prometheus reserves.
func TestALabelNameTooLongIsCheckedInItsPlace(t *testing.T) {
	limits := Limits{MaxMetricNameLength: 5, MaxLabelNameLength: 5, MaxLabelValueLength: 2}
	for _, test := range []struct {
		name   string
		metric string
		labels map[string]string
		want   string
	}{
		{name: "a metric name too long beside", metric: "metric", labels: map[string]string{"labels": "v"},
			want: `invalid metric name "metric": longer than limits.max_metric_name_length 5`},
		{name: "a label name that is no name", metric: "m", labels: map[string]string{"lab-els": "v"},
			want: `metric "m" has label "lab-els", which is not a classic Prometheus label name`},
		{name: "a value too long beside", metric: "m", labels: map[string]string{"labels": "value"},
			want: `metric "m" label name "labels" is longer than limits.max_label_name_length 5`},
		{name: "a reserved name", metric: "m", labels: map[string]string{"__labels": "v"},
			want: `metric "m" label name "__labels" is longer than limits.max_label_name_length 5`},
		{name: "a value too long and a name too long after it", metric: "m", labels: map[string]string{"a": "value", "labels": "v"},
			want: `metric "m" label "a" value is 5 bytes, longer than limits.max_label_value_length 2`},
	} {
		set := MetricSet{Metrics: []Metric{{Name: test.metric, Type: GaugeMetricType, Value: 1, Labels: test.labels}}}
		if err := set.Validate(limits); err == nil || !strings.HasPrefix(err.Error(), test.want) {
			t.Errorf("%s: the error is %v, want it to start with %s", test.name, err, test.want)
		}
	}
}

// le and quantile, which a histogram's buckets and a summary's quantiles
// carry, are label names too: a histogram passes at any limit from 2 bytes
// and a summary from 8, and under that is refused naming the label.
func TestLeAndQuantileAreHeldToTheLabelNameLimit(t *testing.T) {
	histogram := Metric{Name: "h", Type: HistogramMetricType, Labels: map[string]string{"a": "1"}, Histogram: &Histogram{Buckets: []Bucket{{UpperBound: 1, CumulativeCount: 1}}, Count: 1}}
	summary := Metric{Name: "s", Type: SummaryMetricType, Labels: map[string]string{"a": "1"}, Summary: &Summary{Quantiles: []Quantile{{Quantile: 0.5, Value: 1}}, Count: 1}}
	for _, test := range []struct {
		metric Metric
		limit  int
		want   string
	}{
		{histogram, 0, ""},
		{histogram, 200, ""},
		{histogram, 2, ""},
		{histogram, 1, `metric "h" is a histogram, whose buckets carry the label le, which is longer than limits.max_label_name_length 1; raise limits.max_label_name_length`},
		{summary, 8, ""},
		{summary, 200, ""},
		{summary, 7, `metric "s" is a summary, whose quantiles carry the label quantile, which is longer than limits.max_label_name_length 7; raise limits.max_label_name_length`},
	} {
		set := MetricSet{Metrics: []Metric{test.metric}}
		err := set.Validate(Limits{MaxLabelNameLength: test.limit})
		switch {
		case test.want == "" && err != nil:
			t.Errorf("%s at %d: refused with %v", test.metric.Type, test.limit, err)
		case test.want != "" && (err == nil || err.Error() != test.want):
			t.Errorf("%s at %d: the error is %v, want %s", test.metric.Type, test.limit, err, test.want)
		}
	}
}

// A set whose label names are all within limits.max_label_name_length is
// refused or accepted, word for word, as before the limit was: for generated
// sets of names, label names, values, help, histograms, summaries and
// duplicates that fail and pass every other check, with the limit at the
// longest label name of the set, above it, and 0, the verdict is that of
// validate as it was (validateBeforeLabelNameLimit).
func TestASetWithinTheLabelNameLimitIsValidatedAsBefore(t *testing.T) {
	random := rand.New(rand.NewPCG(11, 7))
	// Each pick fails a check now and then, the rest pass, so that both
	// verdicts are common.
	pick := func(good, bad []string) string {
		if random.IntN(30) == 0 {
			return bad[random.IntN(len(bad))]
		}
		return good[random.IntN(len(good))]
	}
	metricNames, badMetricNames := []string{"m", "metric_name", "x:y", "h", "s", "h_count", strings.Repeat("n", 30)}, []string{"", "bad-name", "é", strings.Repeat("n", 60)}
	labelNames, badLabelNames := []string{"a", "b", "zone", strings.Repeat("l", 40), strings.Repeat("k", 199)}, []string{"le", "quantile", "__r", "bad-label", "", "é"}
	values, badValues := []string{"", "1", "value"}, []string{strings.Repeat("v", 60)}
	types := []MetricType{GaugeMetricType, CounterMetricType, UntypedMetricType, HistogramMetricType, SummaryMetricType}
	// A limit is left at 0, which holds nothing back, half the time, and
	// is otherwise one most picks are within.
	limit := func(least int) int {
		if random.IntN(2) == 0 {
			return 0
		}
		return least + random.IntN(least)
	}
	checked, failed := 0, 0
	for range alloctest.UnlessRaced(3000, 600) {
		set := MetricSet{}
		for range 1 + random.IntN(4) {
			m := Metric{Name: pick(metricNames, badMetricNames), Type: types[random.IntN(len(types))], Value: 1, Help: strings.Repeat("h", random.IntN(6))}
			if random.IntN(30) == 0 {
				m.Type, m.Help = "nope", strings.Repeat("h", 40)
			}
			switch {
			case m.Type == HistogramMetricType && random.IntN(30) > 0:
				m.Histogram = &Histogram{Buckets: []Bucket{{UpperBound: 1, CumulativeCount: 1}}, Count: 1}
			case m.Type == SummaryMetricType && random.IntN(30) > 0:
				m.Summary = &Summary{Quantiles: []Quantile{{Quantile: 0.5, Value: 1}}, Count: 1}
			}
			if n := random.IntN(4); n > 0 {
				m.Labels = map[string]string{}
				for range n {
					m.Labels[pick(labelNames, badLabelNames)] = pick(values, badValues)
				}
			}
			set.Metrics = append(set.Metrics, m)
			if random.IntN(30) == 0 {
				set.Metrics = append(set.Metrics, m)
			}
		}
		longest := 0
		for i := range set.Metrics {
			for k := range set.Metrics[i].Labels {
				longest = max(longest, len(k))
			}
			if own := seriesOwnLabel(&set.Metrics[i]); len(own) > longest {
				longest = len(own)
			}
		}
		limits := Limits{MaxMetrics: limit(5), MaxLabelsPerMetric: limit(3), MaxLabelValueLength: limit(40), MaxMetricNameLength: limit(40), MaxHelpLength: limit(20)}
		var want error
		if limits.MaxMetrics > 0 && len(set.Metrics) > limits.MaxMetrics {
			want = MetricCountError(len(set.Metrics), limits.MaxMetrics)
		} else {
			want = validateBeforeLabelNameLimit(&set, limits, newSeriesSet(set.Metrics))
		}
		for _, limit := range []int{0, longest, longest + 1, 200} {
			if limit != 0 && limit < longest {
				continue
			}
			with := limits
			with.MaxLabelNameLength = limit
			got := set.Validate(with)
			if fmt.Sprint(got) != fmt.Sprint(want) || got != nil && SameFailureText(got) != SameFailureText(want) {
				t.Fatalf("%+v with limits.max_label_name_length %d: the verdict is %v, was %v", set.Metrics, limit, got, want)
			}
			checked++
			if got != nil {
				failed++
			}
		}
	}
	// Both verdicts were reached often.
	if failed < checked/10 || failed > checked*9/10 {
		t.Errorf("of %d sets checked, %d failed", checked, failed)
	}
}

// validateBeforeLabelNameLimit is MetricSet.validate, with labelFailure and
// firstLabelFailure, as they were before limits.max_label_name_length.
func validateBeforeLabelNameLimit(s *MetricSet, l Limits, seen *seriesSet) error {
	types := map[string]MetricType{}
	for i := range s.Metrics {
		m := &s.Metrics[i]
		if !ValidMetricName(m.Name) {
			if m.Name != "" && utf8.ValidString(m.Name) {
				return nameErrorf("metric name %q is not a classic Prometheus name; set the collector's name_escaping to underscores or values to export it escaped", shownName(m.Name))
			}
			return nameErrorf("invalid metric name %q", shownName(m.Name))
		}
		if len(m.Name) > l.MaxMetricNameLength && l.MaxMetricNameLength > 0 {
			return nameErrorf("invalid metric name %q: longer than limits.max_metric_name_length %d", shownName(m.Name), l.MaxMetricNameLength)
		}
		switch m.Type {
		case GaugeMetricType, CounterMetricType, HistogramMetricType, SummaryMetricType, UntypedMetricType:
		default:
			return nameErrorf("metric %q has invalid type %q", shownName(m.Name), m.Type)
		}
		// A histogram is its buckets and a summary its quantiles: a series
		// typed as one without them, or with them and another type, is
		// exposition no parser reads as intended.
		switch {
		case (m.Type == HistogramMetricType) != (m.Histogram != nil):
			return nameErrorf("metric %q has type %s but %s; only a histogram read from Prometheus exposition has buckets, and metric() and the rules make gauges, counters and untyped series", shownName(m.Name), m.Type, map[bool]string{true: "buckets", false: "no buckets"}[m.Histogram != nil])
		case (m.Type == SummaryMetricType) != (m.Summary != nil):
			return nameErrorf("metric %q has type %s but %s; only a summary read from Prometheus exposition has quantiles, and metric() and the rules make gauges, counters and untyped series", shownName(m.Name), m.Type, map[bool]string{true: "quantiles", false: "no quantiles"}[m.Summary != nil])
		}
		// Prometheus accepts infinities and NaN, so no value check applies here.
		if len(m.Labels) > l.MaxLabelsPerMetric && l.MaxLabelsPerMetric > 0 {
			return Errorf("metric %q has %d labels, more than limits.max_labels_per_metric %d; drop labels it does not need or raise limits.max_labels_per_metric", shownName(m.Name), Size(len(m.Labels)), l.MaxLabelsPerMetric)
		}
		var labels uint64
		for k, v := range m.Labels {
			// What labelFailure refuses, asked here without a call: a
			// series that passes pays for the questions alone.
			if !ValidLabelName(k) || ReservedLabelName(k) || k == seriesOwnLabel(m) || l.MaxLabelValueLength > 0 && len(v) > l.MaxLabelValueLength {
				return firstLabelFailureBeforeLabelNameLimit(m, &l)
			}
			// The labels are gone through once, for these checks and for
			// the duplicate check below alike (seriesSet).
			if v != "" {
				labels += seen.labelHash(k, v)
			}
		}
		if l.MaxHelpLength > 0 && len(m.Help) > l.MaxHelpLength {
			return Errorf("metric %q help is %d bytes, longer than limits.max_help_length %d; shorten it or raise limits.max_help_length", shownName(m.Name), Size(len(m.Help)), l.MaxHelpLength)
		}
		// The series of a family nearly always follow one another, so a
		// series of the family and type of the one before it has nothing to
		// look up, and a family's type is written down once.
		if i == 0 || m.Type != s.Metrics[i-1].Type || m.Name != s.Metrics[i-1].Name {
			if prior, ok := types[m.Name]; !ok {
				types[m.Name] = m.Type
			} else if prior != m.Type {
				return nameErrorf("metric %q has inconsistent types", shownName(m.Name))
			}
		}
		if duplicate, empty := seen.addHashed(i, labels); duplicate {
			if empty {
				return nameErrorf("duplicate metric series %q: Prometheus reads a label with an empty value as no label, so series that differ only in one are the same series", shownName(m.Name))
			}
			return nameErrorf("duplicate metric series %q", shownName(m.Name))
		}
	}
	return checkDerivedNames(types)
}
func labelFailureBeforeLabelNameLimit(m *Metric, k, v string, l *Limits) error {
	if !ValidLabelName(k) {
		if k != "" && utf8.ValidString(k) {
			return nameErrorf("metric %q has label %q, which is not a classic Prometheus label name; set the collector's name_escaping to underscores or values to export it escaped", shownName(m.Name), shownName(k))
		}
		return nameErrorf("metric %q has invalid label name %q", shownName(m.Name), shownName(k))
	}
	if ReservedLabelName(k) {
		return nameErrorf("metric %q has %s", shownName(m.Name), reservedLabelError(k))
	}
	// A histogram's buckets are told apart by le and a summary's
	// quantiles by quantile. A series with that label of its own
	// would be written with it on _sum and _count, which a parser
	// reads as a bucket or a quantile without a bound, and twice on
	// every bucket.
	if own := seriesOwnLabel(m); k == own {
		return nameErrorf("metric %q is a %s and has a label %s of its own, which its %s carry; name the label something else", shownName(m.Name), m.Type, own, map[string]string{"le": "buckets", "quantile": "quantiles"}[own])
	}
	if l.MaxLabelValueLength > 0 && len(v) > l.MaxLabelValueLength {
		return Errorf("metric %q label %q value is %d bytes, longer than limits.max_label_value_length %d; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length", shownName(m.Name), shownName(k), Size(len(v)), l.MaxLabelValueLength)
	}
	return nil
}
func firstLabelFailureBeforeLabelNameLimit(m *Metric, l *Limits) error {
	names := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := labelFailureBeforeLabelNameLimit(m, k, m.Labels[k], l); err != nil {
			return err
		}
	}
	return nil
}
