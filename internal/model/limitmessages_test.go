package model

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// A scrape that fails for a limit says which: the metric, what was over the
// limit and by how much, the limit with its key, and what to change. A label
// value names both ways out, truncate: true on the label where one of the
// collector's rules gives it, and a larger limit: the check knows no
// collector, and a label may be one no rule gives.
func TestALimitFailureNamesTheLimitItsKeyAndTheRemedy(t *testing.T) {
	limits := Limits{MaxLabelsPerMetric: 2, MaxLabelValueLength: 10, MaxMetricNameLength: 12, MaxHelpLength: 8}
	gauge := func(name, help string, labels map[string]string) MetricSet {
		return MetricSet{Metrics: []Metric{{Name: name, Help: help, Type: GaugeMetricType, Value: 1, Labels: labels}}}
	}
	for _, test := range []struct {
		name string
		set  MetricSet
		want string
	}{
		{
			name: "a label value one byte over the limit",
			set:  gauge("host_note", "", map[string]string{"note": "abcdefghijk"}),
			want: `metric "host_note" label "note" value is 11 bytes, longer than limits.max_label_value_length 10; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length`,
		},
		{
			name: "a label value counted in bytes, not characters",
			set:  gauge("host_note", "", map[string]string{"note": "ééééééé"}),
			want: `metric "host_note" label "note" value is 14 bytes, longer than limits.max_label_value_length 10; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length`,
		},
		{
			name: "a label value at the limit",
			set:  gauge("host_note", "", map[string]string{"note": "abcdefghij"}),
		},
		{
			name: "one label more than the limit",
			set:  gauge("host_note", "", map[string]string{"a": "1", "b": "2", "c": "3"}),
			want: `metric "host_note" has 3 labels, more than limits.max_labels_per_metric 2; drop labels it does not need or raise limits.max_labels_per_metric`,
		},
		{
			name: "as many labels as the limit",
			set:  gauge("host_note", "", map[string]string{"a": "1", "b": "2"}),
		},
		{
			name: "a help text one byte over the limit",
			set:  gauge("host_note", "123456789", nil),
			want: `metric "host_note" help is 9 bytes, longer than limits.max_help_length 8; shorten it or raise limits.max_help_length`,
		},
		{
			name: "a help text at the limit",
			set:  gauge("host_note", "12345678", nil),
		},
		{
			// It named its limit and the limit's key already, and reads as it did.
			name: "a metric name one byte over the limit",
			set:  gauge("host_note_xyz", "", nil),
			want: `invalid metric name "host_note_xyz": longer than limits.max_metric_name_length 12`,
		},
		{
			name: "a metric name at the limit",
			set:  gauge("host_note_xy", "", nil),
		},
	} {
		err := test.set.Validate(limits)
		switch {
		case test.want == "" && err != nil:
			t.Errorf("%s: refused with %v, want accepted", test.name, err)
		case test.want != "" && (err == nil || err.Error() != test.want):
			t.Errorf("%s: the error is\n%v\nwant\n%s", test.name, err, test.want)
		}
	}
	// A limit left at 0 holds nothing back.
	long := strings.Repeat("x", 4096)
	unlimited := gauge("m"+long, long, map[string]string{"a": long, "b": "2", "c": "3"})
	if err := unlimited.Validate(Limits{}); err != nil {
		t.Errorf("without limits the set is refused: %v", err)
	}
}

// formerValidate is MetricSet.validate as it was when a label value, a count
// of labels and a help text over their limits were reported without the
// limit.
func formerValidate(s *MetricSet, l Limits, seen *seriesSet) error {
	types := map[string]MetricType{}
	for i := range s.Metrics {
		m := &s.Metrics[i]
		if !ValidMetricName(m.Name) {
			if m.Name != "" && utf8.ValidString(m.Name) {
				return fmt.Errorf("metric name %q is not a classic Prometheus name; set the collector's name_escaping to underscores or values to export it escaped", m.Name)
			}
			return fmt.Errorf("invalid metric name %q", m.Name)
		}
		if len(m.Name) > l.MaxMetricNameLength && l.MaxMetricNameLength > 0 {
			return fmt.Errorf("invalid metric name %q: longer than limits.max_metric_name_length %d", m.Name, l.MaxMetricNameLength)
		}
		switch m.Type {
		case GaugeMetricType, CounterMetricType, HistogramMetricType, SummaryMetricType, UntypedMetricType:
		default:
			return fmt.Errorf("metric %q has invalid type %q", m.Name, m.Type)
		}
		// A histogram is its buckets and a summary its quantiles: a series
		// typed as one without them, or with them and another type, is
		// exposition no parser reads as intended.
		switch {
		case (m.Type == HistogramMetricType) != (m.Histogram != nil):
			return fmt.Errorf("metric %q has type %s but %s; only a histogram read from Prometheus exposition has buckets, and metric() and the rules make gauges, counters and untyped series", m.Name, m.Type, map[bool]string{true: "buckets", false: "no buckets"}[m.Histogram != nil])
		case (m.Type == SummaryMetricType) != (m.Summary != nil):
			return fmt.Errorf("metric %q has type %s but %s; only a summary read from Prometheus exposition has quantiles, and metric() and the rules make gauges, counters and untyped series", m.Name, m.Type, map[bool]string{true: "quantiles", false: "no quantiles"}[m.Summary != nil])
		}
		// Prometheus accepts infinities and NaN, so no value check applies here.
		if len(m.Labels) > l.MaxLabelsPerMetric && l.MaxLabelsPerMetric > 0 {
			return fmt.Errorf("metric %q has too many labels", m.Name)
		}
		var labels uint64
		for k, v := range m.Labels {
			if !ValidLabelName(k) {
				if k != "" && utf8.ValidString(k) {
					return fmt.Errorf("metric %q has label %q, which is not a classic Prometheus label name; set the collector's name_escaping to underscores or values to export it escaped", m.Name, k)
				}
				return fmt.Errorf("metric %q has invalid label name %q", m.Name, k)
			}
			if ReservedLabelName(k) {
				return fmt.Errorf("metric %q has %s", m.Name, reservedLabelError(k))
			}
			// A histogram's buckets are told apart by le and a summary's
			// quantiles by quantile. A series with that label of its own
			// would be written with it on _sum and _count, which a parser
			// reads as a bucket or a quantile without a bound, and twice on
			// every bucket.
			if own := seriesOwnLabel(m); k == own {
				return fmt.Errorf("metric %q is a %s and has a label %s of its own, which its %s carry; name the label something else", m.Name, m.Type, own, map[string]string{"le": "buckets", "quantile": "quantiles"}[own])
			}
			if l.MaxLabelValueLength > 0 && len(v) > l.MaxLabelValueLength {
				return fmt.Errorf("metric %q label %q is too long", m.Name, k)
			}
			// The labels are gone through once, for these checks and for
			// the duplicate check below alike (seriesSet).
			if v != "" {
				labels += seen.labelHash(k, v)
			}
		}
		if l.MaxHelpLength > 0 && len(m.Help) > l.MaxHelpLength {
			return fmt.Errorf("metric %q help is too long", m.Name)
		}
		// The series of a family nearly always follow one another, so a
		// series of the family and type of the one before it has nothing to
		// look up, and a family's type is written down once.
		if i == 0 || m.Type != s.Metrics[i-1].Type || m.Name != s.Metrics[i-1].Name {
			if prior, ok := types[m.Name]; !ok {
				types[m.Name] = m.Type
			} else if prior != m.Type {
				return fmt.Errorf("metric %q has inconsistent types", m.Name)
			}
		}
		if duplicate, empty := seen.addHashed(i, labels); duplicate {
			if empty {
				return fmt.Errorf("duplicate metric series %q: Prometheus reads a label with an empty value as no label, so series that differ only in one are the same series", m.Name)
			}
			return fmt.Errorf("duplicate metric series %q", m.Name)
		}
	}
	return checkDerivedNames(types)
}

// formerLimitMessage is the message a set refused for its label count, a
// label value or its help text has now, given the one it had, and the message
// itself for any other. The series it names is the first over that limit:
// validate stops at the first problem, so no earlier one was.
func formerLimitMessage(set *MetricSet, l Limits, former string) string {
	for i := range set.Metrics {
		m := &set.Metrics[i]
		switch {
		case len(m.Labels) > l.MaxLabelsPerMetric && former == fmt.Sprintf("metric %q has too many labels", m.Name):
			return fmt.Sprintf("metric %q has %d labels, more than limits.max_labels_per_metric %d; drop labels it does not need or raise limits.max_labels_per_metric", m.Name, len(m.Labels), l.MaxLabelsPerMetric)
		case len(m.Help) > l.MaxHelpLength && former == fmt.Sprintf("metric %q help is too long", m.Name):
			return fmt.Sprintf("metric %q help is %d bytes, longer than limits.max_help_length %d; shorten it or raise limits.max_help_length", m.Name, len(m.Help), l.MaxHelpLength)
		}
		for k, v := range m.Labels {
			if len(v) > l.MaxLabelValueLength && former == fmt.Sprintf("metric %q label %q is too long", m.Name, k) {
				return fmt.Sprintf("metric %q label %q value is %d bytes, longer than limits.max_label_value_length %d; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length", m.Name, k, len(v), l.MaxLabelValueLength)
			}
		}
	}
	return former
}

// Only the wording of three failures changed: over 20,000 generated sets of
// series under generated limits — names valid and not, long and short, of
// every type, with labels few and many, short and long, reserved and
// repeated, and help texts short and long — a set is accepted exactly when
// it was, and refused with the error it was refused with, but for a count of
// labels, a label value and a help text over their limits, which now name
// the limit.
func TestValidateRefusesWhatItDidAndSaysItAsItDidButForThreeLimits(t *testing.T) {
	random := rand.New(rand.NewSource(11))
	names := []string{"up", "host_note", "a_rather_long_metric_name", "http.requests", "", "9lives", "caf\u00e9", "bad\xffname"}
	labelNames := []string{"a", "b", "c", "note", "le", "quantile", "__name__", "__meta", "bad-name", ""}
	types := []MetricType{GaugeMetricType, CounterMetricType, UntypedMetricType, HistogramMetricType, SummaryMetricType, "info"}
	text := func(limit int) string {
		// Around the limit, in ASCII or in two-byte characters.
		length := max(limit+random.Intn(5)-2, 0)
		if random.Intn(3) == 0 {
			return strings.Repeat("é", length/2)
		}
		return strings.Repeat("x", length)
	}
	changed, refused := map[string]int{}, 0
	const sets = 20000
	for n := range sets {
		limits := Limits{
			MaxLabelsPerMetric:  random.Intn(4),
			MaxLabelValueLength: random.Intn(3) * 6,
			MaxMetricNameLength: random.Intn(3) * 12,
			MaxHelpLength:       random.Intn(3) * 6,
		}
		set := MetricSet{}
		for range 1 + random.Intn(3) {
			m := Metric{Name: names[random.Intn(len(names))], Type: types[random.Intn(len(types))], Value: 1}
			if random.Intn(4) == 0 {
				m.Name = "up"
			}
			if random.Intn(2) == 0 {
				m.Help = text(limits.MaxHelpLength)
			}
			switch {
			case m.Type == HistogramMetricType && random.Intn(4) > 0:
				m.Histogram = &Histogram{Count: 1}
			case m.Type == SummaryMetricType && random.Intn(4) > 0:
				m.Summary = &Summary{Count: 1}
			case random.Intn(20) == 0:
				m.Histogram = &Histogram{Count: 1}
			}
			for range random.Intn(5) {
				if m.Labels == nil {
					m.Labels = map[string]string{}
				}
				m.Labels[labelNames[random.Intn(4)]] = []string{"v", "w", ""}[random.Intn(3)]
			}
			// At most one label of a series is refused for itself, its
			// name or its value: of two, the map's order decides which is
			// named.
			switch random.Intn(4) {
			case 0:
				if m.Labels == nil {
					m.Labels = map[string]string{}
				}
				m.Labels[labelNames[random.Intn(len(labelNames))]] = "v"
			case 1:
				if m.Labels == nil {
					m.Labels = map[string]string{}
				}
				m.Labels[labelNames[random.Intn(4)]] = text(limits.MaxLabelValueLength)
			}
			set.Metrics = append(set.Metrics, m)
		}
		former := formerValidate(&set, limits, newSeriesSet(set.Metrics))
		err := set.Validate(limits)
		if (former == nil) != (err == nil) {
			t.Fatalf("set %d, %+v under %+v: refused with %v, and it was with %v", n, set, limits, err, former)
		}
		if former == nil {
			continue
		}
		refused++
		want := formerLimitMessage(&set, limits, former.Error())
		if err.Error() != want {
			t.Fatalf("set %d, %+v under %+v: the error is\n%v\nwant\n%s\nfor what was\n%v", n, set, limits, err, want, former)
		}
		if want != former.Error() {
			changed[strings.SplitN(strings.TrimPrefix(want, "metric "), " ", 3)[1]]++
		}
	}
	if refused < sets/4 || refused > sets-sets/20 || changed["has"] < 100 || changed["label"] < 100 || changed["help"] < 100 {
		t.Fatalf("of %d sets %d were refused, and the changed messages were %v; the generator no longer covers them", sets, refused, changed)
	}
}

// A limit's failure says by how much the series was over it, which a
// response that changes makes another number on every scrape: the failure
// is recognised without it, by the metric, the label and the limit — a
// label's value, a count of labels, a help text and a count of series of
// another size are each one failure, and of another metric, label or limit
// another.
func TestLimitFailuresOfAnotherSizeAreRecognisedAsOne(t *testing.T) {
	limits := Limits{MaxLabelsPerMetric: 2, MaxLabelValueLength: 10, MaxHelpLength: 8}
	failure := func(l Limits, name, help string, labels map[string]string) error {
		t.Helper()
		set := MetricSet{Metrics: []Metric{{Name: name, Help: help, Type: GaugeMetricType, Value: 1, Labels: labels}}}
		err := set.Validate(l)
		if err == nil {
			t.Fatalf("%v under %+v is accepted", set, l)
		}
		return err
	}
	wider := limits
	wider.MaxLabelValueLength, wider.MaxLabelsPerMetric, wider.MaxHelpLength = 11, 1, 7
	long, longer := strings.Repeat("x", 11), strings.Repeat("x", 40)
	for name, errs := range map[string][3]error{
		"a label value":                     {failure(limits, "m", "", map[string]string{"note": long}), failure(limits, "m", "", map[string]string{"note": longer}), failure(limits, "m", "", map[string]string{"text": long})},
		"a label value under another limit": {failure(limits, "m", "", map[string]string{"note": longer}), failure(limits, "m", "", map[string]string{"note": long}), failure(wider, "m", "", map[string]string{"note": longer})},
		"a count of labels":                 {failure(limits, "m", "", map[string]string{"a": "1", "b": "2", "c": "3"}), failure(limits, "m", "", map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}), failure(wider, "m", "", map[string]string{"a": "1", "b": "2", "c": "3"})},
		"a help text":                       {failure(limits, "m", long, nil), failure(limits, "m", longer, nil), failure(limits, "n", long, nil)},
		"a count of series":                 {MetricCountError(11, 10), MetricCountError(250, 10), MetricCountError(11, 9)},
	} {
		a, b, other := errs[0], errs[1], errs[2]
		if a.Error() == b.Error() || SameFailureText(a) != SameFailureText(b) || SameFailureText(a) == SameFailureText(other) {
			t.Errorf("%s: %v and %v are recognised by\n%s\n%s\nand %v by\n%s\nwant the first two as one and the third as another", name, a, b, SameFailureText(a), SameFailureText(b), other, SameFailureText(other))
		}
	}
	if err := MetricCountError(11, 10); !errors.Is(err, ErrLimitExceeded) || err.Error() != "metric count 11 exceeds limit 10" {
		t.Errorf("the count's failure is %v, marked as a limit's: %v", err, errors.Is(err, ErrLimitExceeded))
	}
}
