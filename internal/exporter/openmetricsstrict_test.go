package exporter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The Prometheus text format lets a target write values OpenMetrics does not
// allow a series of its type: a counter that is NaN or negative, a histogram
// whose bucket counts fall, a summary with a quantile of 1.5. The exporter
// passes them on, and an OpenMetrics answer has to stay one a strict parser
// reads, with the same series and values as the text format's
// (exposition.go: typedValues, planOpenMetrics).
//
// These tests make metric sets of every type with such values, and check
// every answer with strictOpenMetricsError, the rules of a strict parser
// written out here, and, where python3 has it, with the strict parser itself:
// prometheus_client's.

// exposedSample is one sample line of an answer in either format, with the
// family it stands under.
type exposedSample struct {
	family, typ, name string
	labels            map[string]string
	value             float64
	// count is the value where it is written as a whole number that is not
	// negative, as counts are: a float does not tell the largest of them
	// apart.
	count   uint64
	counted bool
	// millis is the sample's timestamp, in milliseconds in either format.
	millis *int64
}

// below reports whether the sample's value is less than another's, exactly
// where both are counts.
func (s exposedSample) below(other exposedSample) bool {
	if s.counted && other.counted {
		return s.count < other.count
	}
	return s.value < other.value
}

// String is the sample as the text format writes it, with le and quantile
// as plain numbers, so the two formats' samples compare as text.
func (s exposedSample) String() string {
	var b strings.Builder
	b.WriteString(s.name)
	b.WriteByte('{')
	for i, name := range model.SortedKeys(s.labels) {
		value := s.labels[name]
		if name == "le" || name == "quantile" {
			if number, err := strconv.ParseFloat(value, 64); err == nil {
				value = strconv.FormatFloat(number, 'g', -1, 64)
			}
		}
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", name, value)
	}
	b.WriteString("} ")
	value := s.value
	if value == 0 {
		// 0 and -0 are one value to every parser.
		value = 0
	}
	b.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	if s.millis != nil {
		fmt.Fprintf(&b, " @%d", *s.millis)
	}
	return b.String()
}

// readSampleLine reads a sample line as the exporter writes it: a name,
// labels, a value and a timestamp, in milliseconds in the text format and in
// seconds in OpenMetrics.
func readSampleLine(line string, openMetrics bool) (exposedSample, error) {
	s := exposedSample{labels: map[string]string{}}
	malformed := fmt.Errorf("malformed sample line %q", line)
	end := strings.IndexAny(line, "{ ")
	if end <= 0 {
		return s, malformed
	}
	s.name = line[:end]
	rest := line[end:]
	if rest[0] == '{' {
		rest = rest[1:]
		for !strings.HasPrefix(rest, "}") {
			name, after, found := strings.Cut(rest, `="`)
			if !found {
				return s, malformed
			}
			var value strings.Builder
			i := 0
			for ; i < len(after) && after[i] != '"'; i++ {
				c := after[i]
				if c == '\\' && i+1 < len(after) {
					i++
					if c = after[i]; c == 'n' {
						c = '\n'
					}
				}
				value.WriteByte(c)
			}
			if i == len(after) {
				return s, malformed
			}
			s.labels[name] = value.String()
			rest = strings.TrimPrefix(after[i+1:], ",")
		}
		rest = rest[1:]
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 || len(fields) > 2 || !strings.HasPrefix(rest, " ") {
		return s, malformed
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, malformed
	}
	s.value = value
	if count, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
		s.count, s.counted = count, true
	}
	if len(fields) == 2 {
		var millis int64
		if openMetrics {
			seconds, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return s, malformed
			}
			millis = int64(math.Round(seconds * 1000))
		} else if millis, err = strconv.ParseInt(fields[1], 10, 64); err != nil {
			return s, malformed
		}
		s.millis = &millis
	}
	return s, nil
}

// strictFamily is one family of an OpenMetrics answer as a strict parser
// reads it.
type strictFamily struct {
	name, typ string
	samples   []exposedSample
}

// readOpenMetrics splits an OpenMetrics answer into its families the way a
// strict parser does: a family starts at its TYPE or HELP line and holds the
// samples named as its type allows, and a sample of any other name starts an
// unknown family of that name.
func readOpenMetrics(answer string) ([]*strictFamily, error) {
	body, ended := strings.CutSuffix(answer, "# EOF\n")
	if !ended {
		return nil, errors.New("the answer does not end with # EOF")
	}
	var (
		families []*strictFamily
		current  *strictFamily
		allowed  []string
	)
	if body == "" {
		return nil, nil
	}
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		switch {
		case line == "":
			return nil, errors.New("blank line")
		case strings.HasPrefix(line, "#"):
			parts := strings.SplitN(line, " ", 4)
			if len(parts) < 4 || (parts[1] != "TYPE" && parts[1] != "HELP") {
				return nil, fmt.Errorf("invalid line %q", line)
			}
			if current == nil || current.name != parts[2] {
				current = &strictFamily{name: parts[2]}
				families = append(families, current)
				allowed = []string{current.name}
			} else if len(current.samples) > 0 {
				return nil, fmt.Errorf("metadata after samples: %q", line)
			}
			if parts[1] == "TYPE" {
				if current.typ != "" || parts[3] == "untyped" {
					return nil, fmt.Errorf("invalid TYPE line %q", line)
				}
				current.typ = parts[3]
				allowed = allowed[:0]
				for _, suffix := range strictSuffixes(current.typ) {
					allowed = append(allowed, current.name+suffix)
				}
			}
		default:
			s, err := readSampleLine(line, true)
			if err != nil {
				return nil, err
			}
			if current == nil || !slices.Contains(allowed, s.name) {
				current = &strictFamily{name: s.name, typ: "unknown"}
				families = append(families, current)
				allowed = []string{s.name}
			}
			current.samples = append(current.samples, s)
		}
	}
	for _, f := range families {
		if f.typ == "" {
			f.typ = "unknown"
		}
		for i := range f.samples {
			f.samples[i].family, f.samples[i].typ = f.name, f.typ
		}
	}
	return families, nil
}

// strictSuffixes are the suffixes the samples of a family of a type may
// have.
func strictSuffixes(typ string) []string {
	switch typ {
	case "counter":
		return []string{"_total", "_created"}
	case "histogram":
		return []string{"_count", "_sum", "_bucket", "_created"}
	case "summary":
		return []string{"", "_count", "_sum", "_created"}
	}
	return []string{""}
}

// strictOpenMetricsError is why a strict OpenMetrics parser refuses an
// answer, or nil. It holds the rules prometheus_client's parser checks
// (openmetrics/parser.py), which are OpenMetrics 1.0's for the types the
// exporter writes: no name claimed by two families; a counter's, a
// histogram's and a summary's _total, _bucket, _sum and _count neither NaN
// nor negative; whole bucket counts and counts; a histogram's buckets in
// ascending order of le, none NaN, their counts never falling, +Inf last and
// equal to _count, _sum and _count together, and no _sum beside a negative
// bound; a summary's quantiles within 0 to 1 and their values not negative;
// and a series' samples together, their timestamps all set or all not.
func strictOpenMetricsError(answer string) error {
	families, err := readOpenMetrics(answer)
	if err != nil {
		return err
	}
	claimed := map[string]bool{}
	for _, f := range families {
		names := []string{f.name}
		for _, suffix := range strictSuffixes(f.typ) {
			if suffix != "" {
				names = append(names, f.name+suffix)
			}
		}
		for _, name := range names {
			if claimed[name] {
				return fmt.Errorf("clashing name %s", name)
			}
			claimed[name] = true
		}
		if err := strictFamilyError(f); err != nil {
			return fmt.Errorf("%s %s: %w", f.typ, f.name, err)
		}
	}
	return nil
}

// strictGroup is the series a sample belongs to: its labels, without the le
// of a histogram's bucket and the quantile of a summary's.
func strictGroup(f *strictFamily, s exposedSample) string {
	labels := map[string]string{}
	for name, value := range s.labels {
		labels[name] = value
	}
	switch {
	case f.typ == "summary" && s.name == f.name:
		delete(labels, "quantile")
	case f.typ == "histogram" && s.name == f.name+"_bucket":
		delete(labels, "le")
	}
	return fmt.Sprint(labels)
}

func whole(v float64) bool { return v == math.Trunc(v) && !math.IsInf(v, 0) }

func strictFamilyError(f *strictFamily) error {
	var (
		group      string
		grouped    bool
		groupStamp *int64
		seen       = map[string]bool{}
	)
	for _, s := range f.samples {
		suffix := strings.TrimPrefix(s.name, f.name)
		if s.name == f.name+"_bucket" {
			le, labelled := s.labels["le"]
			bound, err := strconv.ParseFloat(le, 64)
			if !labelled || err != nil || math.IsNaN(bound) || (math.IsInf(bound, 1) && le != "+Inf") {
				return fmt.Errorf("invalid le label %q", le)
			}
			if !whole(s.value) {
				return fmt.Errorf("bucket value %v is not an integer", s.value)
			}
		}
		if s.name == f.name+"_count" && !whole(s.value) {
			return fmt.Errorf("count %v is not an integer", s.value)
		}
		if f.typ == "summary" && s.name == f.name {
			quantile, err := strconv.ParseFloat(s.labels["quantile"], 64)
			if err != nil || !(quantile >= 0 && quantile <= 1) {
				return fmt.Errorf("invalid quantile label %q", s.labels["quantile"])
			}
		}
		g := strictGroup(f, s)
		if grouped && g != group && seen[g] {
			return fmt.Errorf("the samples of the series %s are not together", g)
		}
		if grouped && g == group {
			if (s.millis == nil) != (groupStamp == nil) {
				return fmt.Errorf("the series %s has samples with and without a timestamp", g)
			}
			if s.millis != nil && *groupStamp > *s.millis {
				return fmt.Errorf("the timestamps of the series %s go backwards", g)
			}
		}
		group, grouped, groupStamp = g, true, s.millis
		seen[g] = true
		if f.typ == "summary" && s.name == f.name && s.value < 0 {
			return fmt.Errorf("quantile value %v is negative", s.value)
		}
		switch suffix {
		case "_total", "_sum", "_count", "_bucket":
			if math.IsNaN(s.value) || s.value < 0 {
				return fmt.Errorf("%s is %v, and cannot be NaN or negative", s.name, s.value)
			}
		}
	}
	if f.typ == "histogram" {
		return strictHistogramError(f)
	}
	return nil
}

func strictHistogramError(f *strictFamily) error {
	var (
		group                 string
		grouped               bool
		stamp                 *int64
		count, last           *exposedSample
		bucket                *float64
		negative, sum         bool
		sameStamp             = func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
		checkSeries, endGroup func() error
	)
	checkSeries = func() error {
		switch {
		case bucket == nil || !math.IsInf(*bucket, 1):
			return errors.New("no +Inf bucket")
		case count != nil && last != nil && (last.below(*count) || count.below(*last)):
			return errors.New("_count is not the +Inf bucket's count")
		case sum != (count != nil):
			return errors.New("_sum and _count are not both there or both left out")
		case negative && sum:
			return errors.New("a _sum beside a bucket with a negative bound")
		}
		return nil
	}
	endGroup = func() error {
		if !grouped {
			return nil
		}
		return checkSeries()
	}
	for _, s := range f.samples {
		suffix := strings.TrimPrefix(s.name, f.name)
		if suffix == "" {
			continue
		}
		g := strictGroup(f, s)
		if !grouped || g != group || !sameStamp(s.millis, stamp) {
			if err := endGroup(); err != nil {
				return err
			}
			count, last, bucket, negative, sum = nil, nil, nil, false, false
		}
		group, grouped, stamp = g, true, s.millis
		switch suffix {
		case "_bucket":
			bound, _ := strconv.ParseFloat(s.labels["le"], 64)
			if bound < 0 {
				negative = true
			}
			if bucket != nil && bound <= *bucket {
				return errors.New("buckets out of order")
			}
			if last != nil && s.below(*last) {
				return errors.New("bucket counts fall")
			}
			bucket, last = &bound, &s
		case "_count":
			count = &s
		case "_sum":
			sum = true
		}
	}
	return endGroup()
}

// answerSeries are the series and values of an answer, sorted, under the
// names the text format gives them: an OpenMetrics counter's sample is named
// with _total where the text format's counter is not (exposition.go), which
// is the one difference between the formats' names, and is undone here.
func answerSeries(t *testing.T, set *model.MetricSet, answer string, openMetrics bool) []string {
	t.Helper()
	var (
		samples []exposedSample
		series  []string
	)
	if openMetrics {
		families, err := readOpenMetrics(answer)
		if err != nil {
			t.Fatalf("%v:\n%s", err, answer)
		}
		for _, f := range families {
			samples = append(samples, f.samples...)
		}
	} else {
		for _, line := range strings.Split(answer, "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			s, err := readSampleLine(line, false)
			if err != nil {
				t.Fatalf("%v:\n%s", err, answer)
			}
			samples = append(samples, s)
		}
	}
	counters := counterNames(set)
	for _, s := range samples {
		if s.typ == "counter" && !counters[s.name] {
			s.name = s.family
		}
		series = append(series, s.String())
	}
	slices.Sort(series)
	return series
}

// counterNames are the names the counters of a set have in the text format.
func counterNames(set *model.MetricSet) map[string]bool {
	names := map[string]bool{}
	for _, m := range set.Metrics {
		if m.Type == model.CounterMetricType {
			names[m.Name] = true
		}
	}
	return names
}

// naturalOpenMetrics writes every family of a set as an OpenMetrics family
// of its own type, whatever its values: what the answer would be if no family
// were written as unknown. It is how these tests ask a strict parser which
// families can keep their type.
func naturalOpenMetrics(set *model.MetricSet) string {
	var (
		e    expositionWriter
		b    []byte
		last string
	)
	for _, m := range set.Metrics {
		family, sample := m.Name, m.Name
		if m.Type == model.CounterMetricType {
			family, sample, _, _ = openMetricsCounter(m.Name)
		}
		if m.Name != last {
			b = append(b, "# TYPE "+family+" "+openMetricsType(m.Type)+"\n"...)
			last = m.Name
		}
		switch {
		case m.Histogram != nil:
			b = e.appendOpenMetricsHistogram(b, family, m)
		case m.Summary != nil:
			b = e.appendOpenMetricsSummary(b, family, m)
		default:
			b = e.appendOpenMetricsSample(b, sample, m.Labels, "", "", m.Value, m)
		}
	}
	return string(append(b, "# EOF\n"...))
}

// oddCase is a metric set with values a parser may trip over. single says
// it is one family, whose type the answer can be asked about.
type oddCase struct {
	name   string
	set    *model.MetricSet
	single bool
}

// oddValues are the values a target may write for a sample: not a number,
// the infinities, negative, zero in both signs, and the largest and the
// smallest a float holds.
func oddValues() []float64 {
	return []float64{0, 1, 2.5, -1, math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, math.Copysign(0, -1)}
}

func describeFloats(values []float64) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.FormatFloat(v, 'g', -1, 64)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// oddCases are sets of every metric type with odd values: each plain type
// with each value, with and without a timestamp; histograms of every shape
// of bounds and bucket counts, with each value as the sum, and with and
// without a _sum and a _count; summaries of every shape of quantiles and
// their values likewise; families with one series that fits its type and one
// that does not; and so many sets of several families with names that meet
// through OpenMetrics' suffixes, in random order. A histogram is one the
// prometheus decoder can have read (model.Histogram.Settle): among them one
// whose +Inf bucket and _count differ, by two, and one with neither.
func oddCases(familySets int) []oddCase {
	var cases []oddCase
	stamps := []*int64{nil, new(int64), new(int64), new(int64), new(int64)}
	*stamps[1], *stamps[2], *stamps[3], *stamps[4] = 1727000000123, 0, -2000, 253402300799999
	labelSets := []map[string]string{nil, {"op": "get"}, {"le": "own", "quantile": "own"}}
	add := func(name string, metrics ...model.Metric) {
		n := len(cases)
		for i := range metrics {
			metrics[i].Timestamp = stamps[n%len(stamps)]
			if metrics[i].Labels == nil {
				metrics[i].Labels = labelSets[n%len(labelSets)]
			}
		}
		cases = append(cases, oddCase{name: name, set: &model.MetricSet{Metrics: metrics}, single: true})
	}

	for _, v := range oddValues() {
		value := strconv.FormatFloat(v, 'g', -1, 64)
		add("gauge "+value, model.Metric{Name: "g", Type: model.GaugeMetricType, Value: v})
		add("untyped "+value, model.Metric{Name: "u", Type: model.UntypedMetricType, Value: v})
		add("counter "+value, model.Metric{Name: "c_total", Help: "A counter.", Type: model.CounterMetricType, Value: v})
		add("counter without _total "+value, model.Metric{Name: "c", Type: model.CounterMetricType, Value: v})
		add("counter "+value+" beside a series that counts",
			model.Metric{Name: "c_total", Type: model.CounterMetricType, Value: 3, Labels: map[string]string{"code": "200"}},
			model.Metric{Name: "c_total", Type: model.CounterMetricType, Value: v, Labels: map[string]string{"code": "500"}})
	}

	inf, nan := math.Inf(1), math.NaN()
	bounds := [][]float64{
		{}, {1}, {0.5, 1}, {1, 0.5}, {0.25, 1, 0.5}, {-1, 0, 1}, {1, -1}, {math.Inf(-1), 1}, {nan, 1}, {1, nan},
		{1, inf}, {inf, 1}, {inf}, {math.MaxFloat64}, {0}, {math.Copysign(0, -1), 1}, {math.SmallestNonzeroFloat64, 1},
	}
	counts := map[string]func(i, n int) uint64{
		"rising":  func(i, _ int) uint64 { return uint64(i + 1) },
		"level":   func(_, _ int) uint64 { return 3 },
		"falling": func(i, n int) uint64 { return uint64(n - i) },
		"none":    func(_, _ int) uint64 { return 0 },
		"huge":    func(i, n int) uint64 { return math.MaxUint64 - uint64(n-i) },
	}
	type sumAndCount struct {
		sum            float64
		noSum, noCount bool
	}
	// Every shape is tried with a sum that counts, with each kind that does
	// not, and without a sum or a count; every odd value is a sum further
	// down.
	totals := []sumAndCount{{noSum: true}, {noSum: true, noCount: true}}
	for _, sum := range []float64{1, nan, -1, inf} {
		totals = append(totals, sumAndCount{sum: sum}, sumAndCount{sum: sum, noCount: true})
	}
	for _, bound := range bounds {
		for _, pattern := range model.SortedKeys(counts) {
			for _, more := range []uint64{0, 2} {
				for _, total := range totals {
					h := &model.Histogram{Sum: total.sum, NoSum: total.noSum, NoCount: total.noCount}
					var highest uint64
					for i, le := range bound {
						c := counts[pattern](i, len(bound))
						h.Buckets = append(h.Buckets, model.Bucket{UpperBound: le, CumulativeCount: c})
						highest = max(highest, c)
						if math.IsInf(le, 1) {
							h.Count = c
						}
					}
					switch {
					case total.noCount && more != 0:
						// Without a _count there is none to be more.
						continue
					case total.noCount && total.noSum && len(bound) == 0:
						// A histogram without any line is not one the decoder
						// can have read.
						continue
					case !slices.Contains(bound, inf):
						h.Count = highest + more
					default:
						// A _count that is more than the +Inf bucket's, as a
						// target read between two of its updates writes it.
						h.Count += more
					}
					if h.Settle() != nil {
						continue
					}
					name := fmt.Sprintf("histogram le %s %s +%d sum %v", describeFloats(bound), pattern, more, total.sum)
					if total.noSum {
						name += " (none)"
					}
					if total.noCount {
						name += " no count"
					}
					add(name, model.Metric{Name: "h", Help: "A histogram.", Type: model.HistogramMetricType, Histogram: h})
				}
			}
		}
	}
	for _, sum := range oddValues() {
		add(fmt.Sprintf("histogram sum %v", sum), model.Metric{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{
			Buckets: []model.Bucket{{UpperBound: 0.5, CumulativeCount: 1}, {UpperBound: 1, CumulativeCount: 2}}, Sum: sum, Count: 3,
		}})
		add(fmt.Sprintf("summary sum %v", sum), model.Metric{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{
			Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: sum, Count: 3,
		}})
	}
	// A label of the series' own named as the one its buckets or quantiles
	// carry, which a script may give it.
	for _, total := range totals {
		own := map[string]string{"le": "own", "quantile": "own", "op": "get"}
		add(fmt.Sprintf("histogram with a label of its own named le, sum %v %v %v", total.sum, total.noSum, total.noCount), model.Metric{Name: "h", Type: model.HistogramMetricType, Labels: own, Histogram: &model.Histogram{
			Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 2}, {UpperBound: inf, CumulativeCount: 3}}, Sum: total.sum, Count: 3, NoSum: total.noSum, NoCount: total.noCount,
		}})
		add(fmt.Sprintf("summary with a label of its own named quantile, sum %v %v %v", total.sum, total.noSum, total.noCount), model.Metric{Name: "s", Type: model.SummaryMetricType, Labels: own, Summary: &model.Summary{
			Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: total.sum, Count: 3, NoSum: total.noSum, NoCount: total.noCount,
		}})
	}
	add("histogram with one series that fits and one whose counts fall",
		model.Metric{Name: "h", Type: model.HistogramMetricType, Labels: map[string]string{"op": "get"}, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}}, Sum: 1, Count: 2}},
		model.Metric{Name: "h", Type: model.HistogramMetricType, Labels: map[string]string{"op": "put"}, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 5}, {UpperBound: 2, CumulativeCount: 4}}, Sum: 1, Count: 5}})

	quantiles := [][]float64{{}, {0.5}, {0.5, 0.99}, {0.99, 0.5}, {0, 1}, {1, 0.5, 0}, {-0.1}, {1.5}, {nan}, {inf}, {math.Inf(-1)}, {0.5, nan}, {math.Copysign(0, -1)}}
	values := oddValues()
	for _, quantile := range quantiles {
		for shift := range values {
			for _, total := range totals {
				if (total.noSum || total.sum != 1) && shift > 0 && len(quantile) == 0 {
					continue
				}
				s := &model.Summary{Sum: total.sum, Count: uint64(shift), NoSum: total.noSum, NoCount: total.noCount}
				for i, q := range quantile {
					s.Quantiles = append(s.Quantiles, model.Quantile{Quantile: q, Value: values[(shift+i)%len(values)]})
				}
				if s.Settle() != nil {
					continue
				}
				name := fmt.Sprintf("summary quantiles %s values from %v sum %v", describeFloats(quantile), values[shift], total.sum)
				if total.noSum {
					name += " (none)"
				}
				if total.noCount {
					name += " no count"
				}
				add(name, model.Metric{Name: "s", Help: "A summary.", Type: model.SummaryMetricType, Summary: s})
			}
		}
	}
	add("summary with one series that fits and one with a quantile above 1",
		model.Metric{Name: "s", Type: model.SummaryMetricType, Labels: map[string]string{"op": "get"}, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 1}}, Sum: 1, Count: 2}},
		model.Metric{Name: "s", Type: model.SummaryMetricType, Labels: map[string]string{"op": "put"}, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 50, Value: 1}}, Sum: 1, Count: 2}})

	return append(cases, oddFamilySets(familySets)...)
}

// oddFamilySets are sets of two to four families with names that meet
// through OpenMetrics' suffixes, of random types, with values that fit their
// type or do not, in random order, as a merge of several targets leaves
// them: what the planning of the families (planOpenMetrics) has to keep
// valid whichever of them are written as unknown. The random numbers are
// seeded, so the sets are the same from run to run.
func oddFamilySets(sets int) []oddCase {
	random := rand.New(rand.NewSource(20261002))
	names := []string{
		"a", "a_total", "a_count", "a_sum", "a_bucket", "a_created", "a_total_total", "a_count_total", "a_sum_total",
		"a_bucket_total", "a_created_total", "a_total_count", "a_total_sum", "a_total_created", "a_count_count",
		"a_bucket_bucket", "a_sum_count", "b", "b_total", "_total",
	}
	types := []model.MetricType{model.GaugeMetricType, model.CounterMetricType, model.HistogramMetricType, model.SummaryMetricType, model.UntypedMetricType}
	values := oddValues()
	value := func() float64 {
		if random.Intn(2) == 0 {
			return 1
		}
		return values[random.Intn(len(values))]
	}
	var cases []oddCase
	for len(cases) < sets {
		set := &model.MetricSet{}
		used := map[string]bool{}
		for families := 2 + random.Intn(3); families > 0; {
			name := names[random.Intn(len(names))]
			if used[name] {
				continue
			}
			used[name] = true
			families--
			typ := types[random.Intn(len(types))]
			for series := 1 + random.Intn(2); series > 0; series-- {
				m := model.Metric{Name: name, Type: typ, Value: value(), Labels: map[string]string{"series": strconv.Itoa(series)}}
				switch typ {
				case model.HistogramMetricType:
					m.Histogram = &model.Histogram{Sum: value(), Count: 9, NoSum: random.Intn(8) == 0}
					m.Histogram.NoCount = m.Histogram.NoSum && random.Intn(2) == 0
					for _, le := range [][]float64{{1}, {1, 2}, {2, 1}, {-1, 1}, {math.NaN()}}[random.Intn(5)] {
						m.Histogram.Buckets = append(m.Histogram.Buckets, model.Bucket{UpperBound: le, CumulativeCount: uint64(random.Intn(10))})
					}
					if m.Histogram.NoCount {
						// One read without a _count has its +Inf bucket.
						m.Histogram.Buckets = append(m.Histogram.Buckets, model.Bucket{UpperBound: math.Inf(1), CumulativeCount: m.Histogram.Count})
					}
				case model.SummaryMetricType:
					m.Summary = &model.Summary{Sum: value(), Count: 9, NoSum: random.Intn(8) == 0, NoCount: random.Intn(8) == 0}
					for _, q := range [][]float64{{}, {0.5}, {0.99, 0.5}, {1.5}}[random.Intn(4)] {
						m.Summary.Quantiles = append(m.Summary.Quantiles, model.Quantile{Quantile: q, Value: value()})
					}
				}
				set.Metrics = append(set.Metrics, m)
			}
		}
		random.Shuffle(len(set.Metrics), func(i, j int) { set.Metrics[i], set.Metrics[j] = set.Metrics[j], set.Metrics[i] })
		if set.Validate(model.Limits{}) != nil {
			// Not a set a scrape can answer with.
			continue
		}
		cases = append(cases, oddCase{name: fmt.Sprintf("families %d", len(cases)), set: set})
	}
	return cases
}

// keepsItsType reports whether the OpenMetrics answer for a set of one
// family writes it as a family of its own type.
func keepsItsType(c oddCase, answer string) bool {
	m := c.set.Metrics[0]
	family := m.Name
	if m.Type == model.CounterMetricType {
		family, _, _, _ = openMetricsCounter(m.Name)
	}
	return strings.HasPrefix(answer, "# TYPE "+family+" "+openMetricsType(m.Type)+"\n")
}

// Every answer for a set with odd values is valid OpenMetrics by a strict
// parser's rules, claims no name twice, and holds the series and values the
// text format's answer does. Under the race detector the sets of every type
// and shape are joined by the first 400 of the 4,000 random sets of several
// families, which have every type and every name many times over.
func TestOpenMetricsOfOddValuesIsValidAndHoldsTheTextFormatsSeries(t *testing.T) {
	for _, c := range oddCases(alloctest.UnlessRaced(4000, 400)) {
		answer := string(appendOpenMetrics(nil, c.set))
		if err := strictOpenMetricsError(answer); err != nil {
			t.Fatalf("%s: a strict parser refuses the answer: %v\n%s", c.name, err, answer)
		}
		checkOpenMetricsClaims(t, answer)
		text := string(appendMetricSet(nil, c.set))
		if got, want := answerSeries(t, c.set, answer, true), answerSeries(t, c.set, text, false); !slices.Equal(got, want) {
			t.Fatalf("%s: OpenMetrics holds\n%s\nand the text format\n%s\n\n%s\n%s", c.name, strings.Join(got, "\n"), strings.Join(want, "\n"), answer, text)
		}
	}
}

// A family is written as unknown only when it has to be: every family that
// a strict parser accepts as a family of its own type keeps the type, and
// every one it refuses is written as unknown. The first half is what keeps a
// counter a counter; the second is the complete list of values a type does
// not allow.
func TestOpenMetricsFamilyKeepsItsTypeExactlyWhenItsValuesAllowIt(t *testing.T) {
	kept, unknown := 0, 0
	for _, c := range oddCases(0) {
		if !c.single || c.set.Metrics[0].Type == model.UntypedMetricType {
			continue
		}
		refusal := strictOpenMetricsError(naturalOpenMetrics(c.set))
		answer := string(appendOpenMetrics(nil, c.set))
		if keeps := keepsItsType(c, answer); keeps != (refusal == nil) {
			t.Fatalf("%s: written with its type: %v, and a strict parser says of it with its type: %v\n%s", c.name, keeps, refusal, answer)
		}
		if refusal == nil {
			kept++
		} else {
			unknown++
		}
	}
	if kept < 100 || unknown < 100 {
		t.Fatalf("%d families kept their type and %d did not: the cases do not cover both", kept, unknown)
	}
}

// strictParserEnv, set to anything, says the strict reference parser has to
// be there: a test that needs it then fails where it would have been
// skipped. The workflows that run the suite set it, having installed the
// module (test/python/requirements.txt), so a run there cannot pass with
// these tests left out without anyone seeing.
const strictParserEnv = "STRICT_OPENMETRICS_PARSER"

// strictParserMissing ends a test that cannot run the strict reference
// parser: skipped, saying what is missing, or failed with the same words
// where strictParserEnv says the parser is required.
func strictParserMissing(t interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}, required bool, format string, args ...any) {
	t.Helper()
	if required {
		t.Fatalf(format+"; "+strictParserEnv+" is set, so this is a failure and not a skip", args...)
		return
	}
	t.Skipf(format, args...)
}

// endedTest records how strictParserMissing ended a test.
type endedTest struct{ failed, skipped string }

func (*endedTest) Helper()                             {}
func (e *endedTest) Fatalf(format string, args ...any) { e.failed = fmt.Sprintf(format, args...) }
func (e *endedTest) Skipf(format string, args ...any)  { e.skipped = fmt.Sprintf(format, args...) }

// Without the reference parser a test that needs it is skipped, saying what
// is missing; where the parser is required, as in the workflows, the same
// words fail it, so a run cannot pass with those tests quietly left out.
func TestAMissingStrictParserFailsTheTestWhereItIsRequired(t *testing.T) {
	var optional, required endedTest
	strictParserMissing(&optional, false, "python3 has no %s module", "prometheus_client")
	if optional.failed != "" || optional.skipped != "python3 has no prometheus_client module" {
		t.Errorf("not required: failed=%q skipped=%q, want it skipped with the message", optional.failed, optional.skipped)
	}
	strictParserMissing(&required, true, "python3 has no %s module", "prometheus_client")
	if required.skipped != "" || !strings.HasPrefix(required.failed, "python3 has no prometheus_client module; ") || !strings.Contains(required.failed, strictParserEnv) {
		t.Errorf("required: failed=%q skipped=%q, want it failed with the message and the variable's name", required.failed, required.skipped)
	}
}

// strictParser runs prometheus_client's OpenMetrics parser, the strict
// reference parser, over answers, and returns for each why it was refused
// ("" when it was not) and the samples read. The test is skipped where
// python3 or the module is missing, and fails there instead when
// strictParserEnv is set.
func strictParser(t *testing.T, answers []string) []strictVerdict {
	t.Helper()
	required := os.Getenv(strictParserEnv) != ""
	python, err := exec.LookPath("python3")
	if err != nil {
		strictParserMissing(t, required, "python3 is not available, so the strict OpenMetrics parser of prometheus_client cannot be run")
	}
	if out, err := exec.Command(python, "-c", "import prometheus_client.openmetrics.parser").CombinedOutput(); err != nil {
		strictParserMissing(t, required, "python3 has no prometheus_client module (pip install -r test/python/requirements.txt), so its strict OpenMetrics parser cannot be run: %v: %s", err, bytes.TrimSpace(out))
	}
	// The parser takes most of a millisecond for an answer, so the answers
	// are shared out among a few of it.
	parsers := min(runtime.NumCPU(), 8, len(answers))
	verdicts := make([]strictVerdict, len(answers))
	failures := make([]error, parsers)
	var wait sync.WaitGroup
	for i := range parsers {
		from, to := i*len(answers)/parsers, (i+1)*len(answers)/parsers
		wait.Add(1)
		go func() {
			defer wait.Done()
			input, err := json.Marshal(answers[from:to])
			if err != nil {
				failures[i] = err
				return
			}
			command := exec.Command(python, "-c", strictParserScript)
			command.Stdin = bytes.NewReader(input)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			output, err := command.Output()
			if err != nil {
				failures[i] = fmt.Errorf("%w\n%s", err, stderr.String())
				return
			}
			var read []strictVerdict
			if err := json.Unmarshal(output, &read); err != nil || len(read) != to-from {
				failures[i] = fmt.Errorf("%d verdicts for %d answers: %w", len(read), to-from, err)
				return
			}
			copy(verdicts[from:to], read)
		}()
	}
	wait.Wait()
	if err := errors.Join(failures...); err != nil {
		t.Fatalf("running the strict parser: %v", err)
	}
	return verdicts
}

// strictVerdict is what the strict parser made of one answer.
type strictVerdict struct {
	Error   string `json:"error"`
	Samples []struct {
		Family    string            `json:"family"`
		Type      string            `json:"type"`
		Name      string            `json:"name"`
		Labels    map[string]string `json:"labels"`
		Value     string            `json:"value"`
		Timestamp *string           `json:"timestamp"`
	} `json:"samples"`
}

// series are the samples the strict parser read, as answerSeries gives an
// answer's.
func (v strictVerdict) series(t *testing.T, set *model.MetricSet) []string {
	t.Helper()
	counters := counterNames(set)
	var series []string
	for _, read := range v.Samples {
		s := exposedSample{name: read.Name, labels: read.Labels}
		if read.Type == "counter" && !counters[read.Name] {
			s.name = read.Family
		}
		var err error
		if s.value, err = strconv.ParseFloat(read.Value, 64); err != nil {
			t.Fatalf("the strict parser gave the value %q", read.Value)
		}
		if read.Timestamp != nil {
			seconds, err := strconv.ParseFloat(*read.Timestamp, 64)
			if err != nil {
				t.Fatalf("the strict parser gave the timestamp %q", *read.Timestamp)
			}
			millis := int64(math.Round(seconds * 1000))
			s.millis = &millis
		}
		series = append(series, s.String())
	}
	slices.Sort(series)
	return series
}

// strictParserScript reads a JSON list of answers and writes, for each, the
// error prometheus_client's OpenMetrics parser raised or the samples it
// read. Values and timestamps go as text, since JSON has no NaN.
const strictParserScript = `
import json, sys
from prometheus_client.openmetrics.parser import text_string_to_metric_families

verdicts = []
for answer in json.load(sys.stdin):
    try:
        samples = []
        for family in text_string_to_metric_families(answer):
            for s in family.samples:
                samples.append({
                    "family": family.name, "type": family.type, "name": s.name, "labels": s.labels,
                    "value": repr(float(s.value)),
                    "timestamp": None if s.timestamp is None else repr(float(s.timestamp)),
                })
        verdicts.append({"error": "", "samples": samples})
    except Exception as e:
        verdicts.append({"error": type(e).__name__ + ": " + str(e), "samples": []})
json.dump(verdicts, sys.stdout)
`

// The strict reference parser, prometheus_client's, reads every answer for a
// set with odd values, and reads from it the series and values of the text
// format's answer. It also agrees with the rules written out in
// strictOpenMetricsError on which families can keep their type, so the tests
// above, which run everywhere, check what the parser would; and it does
// refuse what the exporter used to write for such values, so it is strict
// about them.
func TestTheStrictOpenMetricsParserReadsEveryAnswerOfOddValues(t *testing.T) {
	// The parser takes a millisecond or so for an answer, so it is given
	// fewer of the random sets than the test above checks.
	cases := oddCases(600)
	refused := []string{
		"# TYPE c counter\nc_total NaN\n# EOF\n",
		"# TYPE c counter\nc_total -5\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"1.0\"} 2\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"0.5\"} 3\nh_bucket{le=\"1.0\"} 2\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"0.5\"} 5\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"+Inf\"} 4\nh_sum NaN\nh_count 4\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"-1.0\"} 1\nh_bucket{le=\"+Inf\"} 4\nh_sum -3\nh_count 4\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"-1.0\"} 1\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"NaN\"} 1\nh_bucket{le=\"+Inf\"} 4\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3.5\nh_count 8\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3.5\nh_count 6\n# EOF\n",
		"# TYPE h histogram\nh_bucket{le=\"1.0\"} 5\n# EOF\n",
		"# TYPE s summary\ns{quantile=\"1.5\"} 1\n# EOF\n",
		"# TYPE s summary\ns{quantile=\"0.5\"} -1\n# EOF\n",
		"# TYPE s summary\ns_sum -1\ns_count 1\n# EOF\n",
		"# TYPE s summary\ns_sum NaN\ns_count 1\n# EOF\n",
	}
	// Each case's answer, then every family of each case in its own type, then
	// what must be refused.
	var answers []string
	for _, c := range cases {
		answers = append(answers, string(appendOpenMetrics(nil, c.set)))
	}
	for _, c := range cases {
		answers = append(answers, naturalOpenMetrics(c.set))
	}
	verdicts := strictParser(t, append(answers, refused...))
	for i, c := range cases {
		answer, text := answers[i], string(appendMetricSet(nil, c.set))
		if verdicts[i].Error != "" {
			t.Fatalf("%s: the strict parser refuses the answer: %s\n%s", c.name, verdicts[i].Error, answer)
		}
		if got, want := verdicts[i].series(t, c.set), answerSeries(t, c.set, text, false); !slices.Equal(got, want) {
			t.Fatalf("%s: the strict parser read\n%s\nand the text format holds\n%s\n\n%s\n%s", c.name, strings.Join(got, "\n"), strings.Join(want, "\n"), answer, text)
		}
		natural := answers[len(cases)+i]
		if ours, theirs := strictOpenMetricsError(natural), verdicts[len(cases)+i].Error; (ours == nil) != (theirs == "") {
			t.Fatalf("%s: with every family in its own type, the strict parser says %q and the rules written out here %v\n%s", c.name, theirs, ours, natural)
		}
	}
	for i, answer := range refused {
		if verdicts[len(answers)+i].Error == "" {
			t.Errorf("the strict parser accepts, and so is not strict about:\n%s", answer)
		}
		if strictOpenMetricsError(answer) == nil {
			t.Errorf("the rules written out here accept:\n%s", answer)
		}
	}
}

// What the two formats write for the values the text format allows and
// OpenMetrics does not: the text format passes them on, and OpenMetrics
// writes the family as unknown under the names of its samples, with the same
// series and values. Buckets and quantiles a target wrote out of order are
// written in order in both formats, and a family whose values its type
// allows keeps the type, +Inf and a NaN quantile value among them.
func TestFamiliesWithValuesTheirTypeDoesNotAllowAreUnknownInOpenMetrics(t *testing.T) {
	counter := func(name string, v float64, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Type: model.CounterMetricType, Value: v, Labels: labels}
	}
	histogram := func(h model.Histogram) model.Metric {
		return model.Metric{Name: "h", Type: model.HistogramMetricType, Histogram: &h}
	}
	summary := func(s model.Summary) model.Metric {
		return model.Metric{Name: "s", Type: model.SummaryMetricType, Summary: &s}
	}
	bucket := func(le float64, count uint64) model.Bucket {
		return model.Bucket{UpperBound: le, CumulativeCount: count}
	}
	for _, tc := range []struct {
		name              string
		metrics           []model.Metric
		text, openMetrics string
	}{
		{
			name:        "a negative counter",
			metrics:     []model.Metric{{Name: "c_total", Help: "Requests.", Type: model.CounterMetricType, Value: -5}, {Name: "g", Type: model.GaugeMetricType, Value: -5}},
			text:        "# HELP c_total Requests.\n# TYPE c_total counter\nc_total -5\n# TYPE g gauge\ng -5\n",
			openMetrics: "# TYPE c_total unknown\n# HELP c_total Requests.\nc_total -5\n# TYPE g gauge\ng -5\n# EOF\n",
		},
		{
			// The family has one type, so the series that counts goes with it.
			name:        "a NaN counter beside a series that counts",
			metrics:     []model.Metric{counter("c_total", 3, map[string]string{"code": "200"}), counter("c_total", math.NaN(), map[string]string{"code": "500"})},
			text:        "# TYPE c_total counter\nc_total{code=\"200\"} 3\nc_total{code=\"500\"} NaN\n",
			openMetrics: "# TYPE c_total unknown\nc_total{code=\"200\"} 3\nc_total{code=\"500\"} NaN\n# EOF\n",
		},
		{
			// As unknown it keeps the text format's name, without _total.
			name:        "a negative counter named without _total",
			metrics:     []model.Metric{counter("jobs", math.Inf(-1), nil)},
			text:        "# TYPE jobs counter\njobs -Inf\n",
			openMetrics: "# TYPE jobs unknown\njobs -Inf\n# EOF\n",
		},
		{
			name:        "an infinite counter and one of zero",
			metrics:     []model.Metric{counter("c_total", math.Inf(1), nil), counter("d_total", math.Copysign(0, -1), nil)},
			text:        "# TYPE c_total counter\nc_total +Inf\n# TYPE d_total counter\nd_total -0\n",
			openMetrics: "# TYPE c counter\nc_total +Inf\n# TYPE d counter\nd_total -0\n# EOF\n",
		},
		{
			name:        "a histogram with its buckets out of order",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(math.Inf(1), 4), bucket(1, 2), bucket(0.5, 1)}, Sum: 3, Count: 4})},
			text:        "# TYPE h histogram\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"1\"} 2\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n",
			openMetrics: "# TYPE h histogram\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"1.0\"} 2\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n# EOF\n",
		},
		{
			name:        "a histogram whose bucket counts fall",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(0.5, 3), bucket(1, 2)}, Sum: 3, Count: 4})},
			text:        "# TYPE h histogram\nh_bucket{le=\"0.5\"} 3\nh_bucket{le=\"1\"} 2\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"0.5\"} 3\nh_bucket{le=\"1.0\"} 2\nh_bucket{le=\"+Inf\"} 4\n# TYPE h_sum unknown\nh_sum 3\n# TYPE h_count unknown\nh_count 4\n# EOF\n",
		},
		{
			// Out of order as written, and falling once in order.
			name:        "a histogram with more in a bucket than its count",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(1, 5), bucket(math.Inf(1), 4), bucket(0.5, 1)}, Count: 4, NoSum: true, NoCount: true})},
			text:        "# TYPE h histogram\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 4\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 4\n# EOF\n",
		},
		{
			// Each number is written as it was read.
			name:        "a histogram whose +Inf bucket is behind its count",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(1, 5), bucket(math.Inf(1), 7)}, Sum: 3.5, Count: 8})},
			text:        "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3.5\nh_count 8\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\n# TYPE h_sum unknown\nh_sum 3.5\n# TYPE h_count unknown\nh_count 8\n# EOF\n",
		},
		{
			name:        "a histogram whose +Inf bucket is ahead of its count, and written first",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(math.Inf(1), 7), bucket(1, 5)}, Sum: 3.5, Count: 6})},
			text:        "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3.5\nh_count 6\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\n# TYPE h_sum unknown\nh_sum 3.5\n# TYPE h_count unknown\nh_count 6\n# EOF\n",
		},
		{
			// The family has one type, so the series whose two agree goes
			// with the one whose two do not.
			name: "a histogram with one series whose +Inf bucket is its count and one where it is not",
			metrics: []model.Metric{
				{Name: "h", Type: model.HistogramMetricType, Labels: map[string]string{"op": "get"}, Histogram: &model.Histogram{Buckets: []model.Bucket{bucket(math.Inf(1), 2)}, Sum: 1, Count: 2}},
				{Name: "h", Type: model.HistogramMetricType, Labels: map[string]string{"op": "put"}, Histogram: &model.Histogram{Buckets: []model.Bucket{bucket(math.Inf(1), 2)}, Sum: 1, Count: 3}},
			},
			text: "# TYPE h histogram\nh_bucket{le=\"+Inf\",op=\"get\"} 2\nh_sum{op=\"get\"} 1\nh_count{op=\"get\"} 2\nh_bucket{le=\"+Inf\",op=\"put\"} 2\nh_sum{op=\"put\"} 1\nh_count{op=\"put\"} 3\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"+Inf\",op=\"get\"} 2\nh_bucket{le=\"+Inf\",op=\"put\"} 2\n" +
				"# TYPE h_sum unknown\nh_sum{op=\"get\"} 1\nh_sum{op=\"put\"} 1\n# TYPE h_count unknown\nh_count{op=\"get\"} 2\nh_count{op=\"put\"} 3\n# EOF\n",
		},
		{
			// Written with the lines it has: no +Inf bucket and no _count.
			name:        "a histogram with neither a +Inf bucket nor a count",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(0.5, 1), bucket(1, 5)}, Sum: 3, NoCount: true})},
			text:        "# TYPE h histogram\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"1\"} 5\nh_sum 3\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"1.0\"} 5\n# TYPE h_sum unknown\nh_sum 3\n# EOF\n",
		},
		{
			// No bucket line at all, so no family of buckets either.
			name:        "a histogram of a sum alone",
			metrics:     []model.Metric{histogram(model.Histogram{Sum: 3, NoCount: true})},
			text:        "# TYPE h histogram\nh_sum 3\n",
			openMetrics: "# TYPE h_sum unknown\nh_sum 3\n# EOF\n",
		},
		{
			name:        "a histogram with a NaN sum",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(1, 2)}, Sum: math.NaN(), Count: 4})},
			text:        "# TYPE h histogram\nh_bucket{le=\"1\"} 2\nh_bucket{le=\"+Inf\"} 4\nh_sum NaN\nh_count 4\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"1.0\"} 2\nh_bucket{le=\"+Inf\"} 4\n# TYPE h_sum unknown\nh_sum NaN\n# TYPE h_count unknown\nh_count 4\n# EOF\n",
		},
		{
			name:        "a histogram with a negative bound and a sum",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(-1, 1), bucket(1, 2)}, Sum: 3, Count: 4})},
			text:        "# TYPE h histogram\nh_bucket{le=\"-1\"} 1\nh_bucket{le=\"1\"} 2\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"-1.0\"} 1\nh_bucket{le=\"1.0\"} 2\nh_bucket{le=\"+Inf\"} 4\n# TYPE h_sum unknown\nh_sum 3\n# TYPE h_count unknown\nh_count 4\n# EOF\n",
		},
		{
			// OpenMetrics allows negative bounds where there is no sum.
			name:        "a histogram with a negative bound and no sum",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(-1, 1), bucket(math.Inf(1), 4)}, Count: 4, NoSum: true, NoCount: true})},
			text:        "# TYPE h histogram\nh_bucket{le=\"-1\"} 1\nh_bucket{le=\"+Inf\"} 4\n",
			openMetrics: "# TYPE h histogram\nh_bucket{le=\"-1.0\"} 1\nh_bucket{le=\"+Inf\"} 4\n# EOF\n",
		},
		{
			name:        "a histogram with a bound that is not a number",
			metrics:     []model.Metric{histogram(model.Histogram{Buckets: []model.Bucket{bucket(1, 2), bucket(math.NaN(), 1)}, Sum: 3, Count: 4})},
			text:        "# TYPE h histogram\nh_bucket{le=\"NaN\"} 1\nh_bucket{le=\"1\"} 2\nh_bucket{le=\"+Inf\"} 4\nh_sum 3\nh_count 4\n",
			openMetrics: "# TYPE h_bucket unknown\nh_bucket{le=\"NaN\"} 1\nh_bucket{le=\"1.0\"} 2\nh_bucket{le=\"+Inf\"} 4\n# TYPE h_sum unknown\nh_sum 3\n# TYPE h_count unknown\nh_count 4\n# EOF\n",
		},
		{
			name:        "a summary with its quantiles out of order and nothing observed",
			metrics:     []model.Metric{summary(model.Summary{Quantiles: []model.Quantile{{Quantile: 0.99, Value: math.NaN()}, {Quantile: 0.5, Value: math.NaN()}}})},
			text:        "# TYPE s summary\ns{quantile=\"0.5\"} NaN\ns{quantile=\"0.99\"} NaN\ns_sum 0\ns_count 0\n",
			openMetrics: "# TYPE s summary\ns{quantile=\"0.5\"} NaN\ns{quantile=\"0.99\"} NaN\ns_sum 0\ns_count 0\n# EOF\n",
		},
		{
			name:        "a summary with a quantile above 1",
			metrics:     []model.Metric{summary(model.Summary{Quantiles: []model.Quantile{{Quantile: 99, Value: 2}, {Quantile: 50, Value: 1}}, Sum: 3, Count: 4})},
			text:        "# TYPE s summary\ns{quantile=\"50\"} 1\ns{quantile=\"99\"} 2\ns_sum 3\ns_count 4\n",
			openMetrics: "# TYPE s unknown\ns{quantile=\"50.0\"} 1\ns{quantile=\"99.0\"} 2\n# TYPE s_sum unknown\ns_sum 3\n# TYPE s_count unknown\ns_count 4\n# EOF\n",
		},
		{
			name:        "a summary of values below zero",
			metrics:     []model.Metric{summary(model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: -2}}, Sum: -8, Count: 4})},
			text:        "# TYPE s summary\ns{quantile=\"0.5\"} -2\ns_sum -8\ns_count 4\n",
			openMetrics: "# TYPE s unknown\ns{quantile=\"0.5\"} -2\n# TYPE s_sum unknown\ns_sum -8\n# TYPE s_count unknown\ns_count 4\n# EOF\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := &model.MetricSet{Metrics: tc.metrics}
			if got := string(appendMetricSet(nil, set)); got != tc.text {
				t.Errorf("the text format:\n%s\nwant:\n%s", got, tc.text)
			}
			got := string(appendOpenMetrics(nil, set))
			if got != tc.openMetrics {
				t.Errorf("OpenMetrics:\n%s\nwant:\n%s", got, tc.openMetrics)
			}
			if err := strictOpenMetricsError(got); err != nil {
				t.Errorf("a strict parser refuses the answer: %v\n%s", err, got)
			}
		})
	}
}

// Writing a set's buckets in order sorts the writer's own copy: the set,
// which the cache holds and the next answer is written from, keeps the order
// it was read in.
func TestWritingBucketsInOrderLeavesTheMetricSetAsItIs(t *testing.T) {
	buckets := []model.Bucket{{UpperBound: 1, CumulativeCount: 2}, {UpperBound: 0.5, CumulativeCount: 1}}
	quantiles := []model.Quantile{{Quantile: 0.99, Value: 2}, {Quantile: 0.5, Value: 1}}
	set := &model.MetricSet{Metrics: []model.Metric{
		{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Buckets: slices.Clone(buckets), Sum: 1, Count: 2}},
		{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: slices.Clone(quantiles), Sum: 1, Count: 2}},
	}}
	first := string(appendMetricSet(nil, set))
	appendOpenMetrics(nil, set)
	otlpMetrics(*set, "1", nil)
	if !slices.Equal(set.Metrics[0].Histogram.Buckets, buckets) || !slices.Equal(set.Metrics[1].Summary.Quantiles, quantiles) {
		t.Fatalf("the set was reordered: %+v %+v", set.Metrics[0].Histogram.Buckets, set.Metrics[1].Summary.Quantiles)
	}
	if again := string(appendMetricSet(nil, set)); again != first || !strings.Contains(first, "h_bucket{le=\"0.5\"} 1\nh_bucket{le=\"1\"} 2\n") {
		t.Fatalf("the second answer differs from the first:\n%s\n%s", again, first)
	}
}
