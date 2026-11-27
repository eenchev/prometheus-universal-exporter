package model

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"math/big"
	"math/bits"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MetricType is the type of a metric family, as the exposition format names
// it.
type MetricType string

// The metric types.
const (
	GaugeMetricType     MetricType = "gauge"
	CounterMetricType   MetricType = "counter"
	HistogramMetricType MetricType = "histogram"
	SummaryMetricType   MetricType = "summary"
	UntypedMetricType   MetricType = "untyped"
)

// Metric is one series: a name, labels and a value. A histogram or a summary
// carries its buckets or quantiles in Histogram or Summary instead of Value.
// Timestamp, when set, is in milliseconds since the Unix epoch.
//
// Created is when a counter, a histogram or a summary began to count, in
// milliseconds since the Unix epoch. It is 0 when that is not known. Only the
// exporter's own series have it (exporter/selfcreated.go). A target does not
// say when its series started, so every series of a probe has 0, and nothing
// is written for 0.
type Metric struct {
	Name      string            `json:"name"`
	Help      string            `json:"help,omitempty"`
	Type      MetricType        `json:"type"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	Timestamp *int64            `json:"timestamp,omitempty"`
	Histogram *Histogram        `json:"-"`
	Summary   *Summary          `json:"-"`
	Created   int64             `json:"-"`
	// Exemplar is a counter's exemplar, as an OpenMetrics target wrote it
	// after the counter's sample; nil for none.
	Exemplar *Exemplar `json:"-"`
}

// Histogram is the value of a histogram series. NoSum and NoCount say that
// the source gave no _sum or no _count sample, so none is written for it. A
// histogram made here always has both, because its zero value has both.
//
// A histogram's +Inf bucket counts every observation, as its _count does, so
// the two are the same number written twice. A histogram keeps each as its
// source wrote it: the +Inf bucket in Buckets and the _count in Count. A
// source may write only one of them, and the other is then read from it
// (InfBucket, Settle). It may write two that differ. This happens when a
// target counts an observation in its buckets and in its count without a
// lock and is read between the two. Both are then kept and written as read,
// since nothing says which is right. It may also write neither, and the
// histogram then has no +Inf bucket.
type Histogram struct {
	Buckets []Bucket
	Sum     float64
	Count   uint64
	NoSum   bool
	NoCount bool
	// Exponential, when set, holds the same observations in exponential
	// buckets (exponential.go). Only the exporter's own durations have it.
	// It is written beside the classic buckets where a format can carry it,
	// and left out where not.
	Exponential *ExponentialHistogram
	// InfExemplar is the exemplar of the +Inf bucket, which Buckets has no
	// place for when the bucket is written from the count; nil for none.
	InfExemplar *Exemplar
}

// Exemplar is an observation that stands for a histogram's bucket or a
// counter's increase. It holds the value, when it was made, and what it was
// made in. The exporter's own durations have exemplars of the probes and the
// scrapes whose traces were exported, with TraceID and SpanID. A series
// read from an OpenMetrics target has the exemplars the target wrote, with
// their labels in Labels, and Time zero for one written without a time. Only
// a format that can carry exemplars writes them: OpenMetrics, protobuf and
// OTLP.
type Exemplar struct {
	// TraceID and SpanID are as OTLP/JSON writes them, 32 and 16 hex digits.
	TraceID, SpanID string
	// Labels are a target's exemplar's labels, as it wrote them.
	Labels map[string]string
	Value  float64
	Time   time.Time
}

// MaxExemplarRunes is how many characters an exemplar's label names and
// values may have together, as OpenMetrics bounds them.
const MaxExemplarRunes = 128

// Pairs are the exemplar's labels in the order they are written: trace_id and
// span_id of the exporter's own, then a target's by name.
func (x *Exemplar) Pairs() [][2]string {
	out := make([][2]string, 0, 2+len(x.Labels))
	if x.TraceID != "" {
		out = append(out, [2]string{"trace_id", x.TraceID})
	}
	if x.SpanID != "" {
		out = append(out, [2]string{"span_id", x.SpanID})
	}
	for _, name := range SortedKeys(x.Labels) {
		out = append(out, [2]string{name, x.Labels[name]})
	}
	return out
}

// InfBucket returns the count of the histogram's +Inf bucket and whether it
// has one. This is the bucket with that bound in Buckets. A histogram without
// such a bucket uses its _count, and the +Inf bucket is then written from it.
// A histogram with neither has none. The +Inf bucket is nearly always the
// last, so it is checked first.
func (h *Histogram) InfBucket() (count uint64, ok bool) {
	for i := len(h.Buckets) - 1; i >= 0; i-- {
		if math.IsInf(h.Buckets[i].UpperBound, 1) {
			return h.Buckets[i].CumulativeCount, true
		}
	}
	return h.Count, !h.NoCount
}

// Settle checks a histogram read from a source and settles its count. Two
// buckets with the same upper bound, such as le="1" and le="1.0", are an
// error, as two samples of one series are. Without a _count (NoCount), the
// count is taken from the +Inf bucket, which counts every observation. A +Inf
// bucket and a _count that differ are not an error, and neither is a
// histogram with neither (Histogram), because one such series must not fail
// a whole scrape. The error does not name the series; the caller knows it.
func (h *Histogram) Settle() error {
	if bound, duplicate := duplicateFloat(len(h.Buckets), func(i int) float64 { return h.Buckets[i].UpperBound }); duplicate {
		return fmt.Errorf("has two buckets with the upper bound %s", formatBound(bound))
	}
	if h.NoCount {
		h.Count, _ = h.InfBucket()
	}
	return nil
}

// Bucket is one bucket of a histogram: how many observations were at most
// UpperBound.
type Bucket struct {
	UpperBound      float64
	CumulativeCount uint64
	// Exemplar is the bucket's exemplar, nil for none.
	Exemplar *Exemplar
}

// Summary is the value of a summary series. NoSum and NoCount say that the
// source it was read from gave no _sum or no _count sample, so that none is
// written for it, as for a Histogram.
type Summary struct {
	Quantiles []Quantile
	Sum       float64
	Count     uint64
	NoSum     bool
	NoCount   bool
}

// Settle checks a summary read from a source: two values for one quantile,
// such as quantile="0.5" and quantile="0.50", are an error, as two samples of
// one series are. The error does not name the series, which its caller knows.
func (s *Summary) Settle() error {
	if quantile, duplicate := duplicateFloat(len(s.Quantiles), func(i int) float64 { return s.Quantiles[i].Quantile }); duplicate {
		return fmt.Errorf("has two values for the quantile %s", formatBound(quantile))
	}
	return nil
}

// duplicateFloat reports a value that appears twice among the n values at
// gives. Two NaN count as the same value, since both are written as NaN.
// Ascending values, the usual order of buckets and quantiles, hold none, and
// one pass shows it. Only values in another order are copied and sorted.
func duplicateFloat(n int, at func(int) float64) (float64, bool) {
	ascending := true
	for i := 1; i < n && ascending; i++ {
		ascending = at(i) > at(i-1)
	}
	if ascending {
		return 0, false
	}
	values := make([]float64, n)
	for i := range values {
		values[i] = at(i)
	}
	slices.Sort(values)
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] || (math.IsNaN(values[i]) && math.IsNaN(values[i-1])) {
			return values[i], true
		}
	}
	return 0, false
}

// formatBound writes a bucket's bound or a quantile as the exposition does.
func formatBound(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// Quantile is one quantile of a summary and its value.
type Quantile struct {
	Quantile float64
	Value    float64
}

// MetricSet is the metrics one scrape of a collector produced.
type MetricSet struct{ Metrics []Metric }

// ValidMetricName reports whether name is a classic Prometheus metric name,
// matching [a-zA-Z_:][a-zA-Z0-9_:]*. It runs for every series of every
// scrape, so it loops over the bytes instead of using a regular expression,
// which took a quarter of a large scrape's time.
func ValidMetricName(name string) bool { return classicName(name, true) }

// ValidLabelName reports whether name is a classic Prometheus label name,
// matching [a-zA-Z_][a-zA-Z0-9_]*.
func ValidLabelName(name string) bool { return classicName(name, false) }

// classicName is ValidMetricName, with colon, or ValidLabelName.
func classicName(name string, colon bool) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', c == '_':
		case c == ':' && colon:
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ReservedLabelName reports whether name is reserved by Prometheus. This is
// __name__, which holds the metric's name and which its parser refuses in
// exposition, and every other name starting with __, which Prometheus drops
// from a scraped series after relabelling.
func ReservedLabelName(name string) bool { return strings.HasPrefix(name, "__") }

// seriesOwnLabel is the label a series of m's type keeps for its own
// samples: le for a histogram's buckets, quantile for a summary's quantiles,
// and none for any other type, where both are labels like any other.
func seriesOwnLabel(m *Metric) string {
	switch {
	case m.Histogram != nil:
		return "le"
	case m.Summary != nil:
		return "quantile"
	}
	return ""
}

// SeriesOwnLabel is seriesOwnLabel for a metric held by value.
func SeriesOwnLabel(m Metric) string { return seriesOwnLabel(&m) }

// reservedLabelError says why a label name is refused.
func reservedLabelError(name string) string {
	return fmt.Sprintf("label name %q starts with __, which Prometheus reserves for its own labels", name)
}

// CheckLabelName refuses a label name that is not a classic Prometheus name
// or is reserved (ReservedLabelName).
func CheckLabelName(name string) error {
	if !ValidLabelName(name) {
		return fmt.Errorf("invalid label name %q", name)
	}
	if ReservedLabelName(name) {
		return errors.New(reservedLabelError(name))
	}
	return nil
}

// seriesSet holds the series of a set that Validate has seen so far, to
// detect a series seen twice. To Prometheus a series is its name and its
// labels, in any order. A label with an empty value counts as no label, so
// m{a=""} and m are one series.
//
// Validate looks at every series of every scrape, so this is built to cost
// little and allocate nothing. A series is stored by a hash, not by a key
// written out. Its name and each label's name and value are hashed in place,
// and the label hashes are summed. The sum is the same in any order, so
// nothing is sorted or copied. The first series with a hash holds it in
// byHash. A later series with that hash is compared, label by label, with
// the one holding it, and only this comparison decides that they are the
// same series. Two different series with one hash cost a comparison, and the
// second then goes into others. That list holds the few series that share a
// hash with a different, earlier one, and they are compared in turn. So a
// hash never makes two different series a duplicate, and never hides a real
// one.
//
// The hashes are seeded anew for every set, so a target cannot choose names
// that share a hash and make the check slow.
type seriesSet struct {
	metrics []Metric
	seed    maphash.Seed
	// hash makes a series' hash of its digest: the hash of its name and the
	// sum of its labels' hashes, eight bytes each. A test replaces it with a
	// weak one, to make series collide.
	hash   func([]byte) uint64
	digest [16]byte
	byHash map[uint64]int
	// others holds, by hash, the series whose hash a different series took
	// first. It is made when the first such series is seen, which with a
	// 64-bit hash is as good as never.
	others map[uint64][]int
}

func newSeriesSet(metrics []Metric) *seriesSet {
	seed := maphash.MakeSeed()
	return &seriesSet{
		metrics: metrics,
		seed:    seed,
		hash:    func(digest []byte) uint64 { return maphash.Bytes(seed, digest) },
		byHash:  make(map[uint64]int, len(metrics)),
	}
}

// The label hash mixes the hashes of a label's name and of its value with
// these, so that a="b" and b="a" do not hash alike.
const (
	labelNameSalt  = 0x9e3779b97f4a7c15
	labelValueSalt = 0xc2b2ae3d27d4eb4f
)

// labelHash is the hash of one label, to be added to those of the series'
// other labels in any order. A label with an empty value is not hashed: it
// is no label.
func (s *seriesSet) labelHash(name, value string) uint64 {
	hi, lo := bits.Mul64(maphash.String(s.seed, name)^labelNameSalt, maphash.String(s.seed, value)^labelValueSalt)
	return hi ^ lo
}

// add adds metrics[i]. When the series was seen already, it reports so, and
// whether either of the two has a label with an empty value.
func (s *seriesSet) add(i int) (duplicate, empty bool) {
	var labels uint64
	for k, v := range s.metrics[i].Labels {
		if v != "" {
			labels += s.labelHash(k, v)
		}
	}
	return s.addHashed(i, labels)
}

// addHashed is add for a caller that has already gone through the labels of
// metrics[i], as Validate does to check them, and summed the labelHash of
// each label with a value. Going through a map a second time costs more than
// the hashing does.
func (s *seriesSet) addHashed(i int, labels uint64) (duplicate, empty bool) {
	binary.LittleEndian.PutUint64(s.digest[:8], maphash.String(s.seed, s.metrics[i].Name))
	binary.LittleEndian.PutUint64(s.digest[8:], labels)
	h := s.hash(s.digest[:])
	first, taken := s.byHash[h]
	if !taken {
		s.byHash[h] = i
		return false, false
	}
	if same, empty := sameSeries(&s.metrics[first], &s.metrics[i]); same {
		return true, empty
	}
	for _, other := range s.others[h] {
		if same, empty := sameSeries(&s.metrics[other], &s.metrics[i]); same {
			return true, empty
		}
	}
	if s.others == nil {
		s.others = map[uint64][]int{}
	}
	s.others[h] = append(s.others[h], i)
	return false, false
}

// sameSeries reports whether a and b are one series to Prometheus: the same
// name and the same labels, leaving out those with an empty value. When they
// are, it also reports whether either has such a label.
func sameSeries(a, b *Metric) (same, empty bool) {
	if a.Name != b.Name {
		return false, false
	}
	// Every label of a with a value is one of b, and b has as many: a label
	// b lacks reads as "" there, which is not a's value.
	valued := 0
	for k, v := range a.Labels {
		if v == "" {
			empty = true
			continue
		}
		if b.Labels[k] != v {
			return false, false
		}
		valued++
	}
	for _, v := range b.Labels {
		if v == "" {
			empty = true
			continue
		}
		valued--
	}
	if valued != 0 {
		return false, false
	}
	return true, empty
}

// MetricCountError is the error of a scrape with more series than
// limits.max_metrics allows. The transforms and the prometheus decoder stop
// making series at the first one past the limit, so count is then limit+1.
// The scrape has at least that many. Finding out how many more is not worth
// the memory.
func MetricCountError(count, limit int) error {
	return MarkError(Errorf("metric count %d exceeds limit %d", Size(count), limit), ErrLimitExceeded)
}

// Validate checks the set against the collector's limits and the rules of
// the exposition format: valid names and types, no duplicate series, one type
// per family. It stops at the first problem, which the error describes.
func (s *MetricSet) Validate(l Limits) error {
	return s.ValidateNames(l, false)
}

// ValidateNames is Validate for a collector. When utf8Names is true, its
// metric_name_validation_scheme is utf8: a metric or label name is then any
// UTF-8 that is not only blanks (UTF8Name), as Prometheus 3 accepts. Such a
// name is escaped, if at all, when the answer is written, as the scraper
// asks (exporter/nameescaping.go).
func (s *MetricSet) ValidateNames(l Limits, utf8Names bool) error {
	if l.MaxMetrics > 0 && len(s.Metrics) > l.MaxMetrics {
		return MetricCountError(len(s.Metrics), l.MaxMetrics)
	}
	return s.validate(l, newSeriesSet(s.Metrics), utf8Names)
}

// UTF8Name reports whether name is a name under metric_name_validation_scheme
// utf8: valid UTF-8, not empty and not blanks alone, which no one means as a
// name.
func UTF8Name(name string) bool {
	return utf8.ValidString(name) && strings.TrimSpace(name) != ""
}

// takesName reports whether a metric name (metric true) or a label name is
// one a series may have: a classic name, or under utf8 any UTF8Name.
func takesName(name string, metric, utf8Names bool) bool {
	return classicName(name, metric) || utf8Names && UTF8Name(name)
}

// The values of a collector's metric_name_validation_scheme, Prometheus's
// names for them: legacy, the default, holds metric and label names to the
// classic characters, and utf8 takes any UTF-8.
const (
	NameValidationLegacy = "legacy"
	NameValidationUTF8   = "utf8"
)

// validate is Validate past the count of series, with the set that tells
// duplicate series, which a test makes itself to give it a weak hash.
func (s *MetricSet) validate(l Limits, seen *seriesSet, utf8Names bool) error {
	types := map[string]MetricType{}
	for i := range s.Metrics {
		m := &s.Metrics[i]
		if !takesName(m.Name, true, utf8Names) {
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
			return nameErrorf("metric %q has type %s but %s; only a histogram read from Prometheus exposition or counted by a rule's histogram block has buckets, and metric() and the other rules make gauges, counters and untyped series", shownName(m.Name), m.Type, map[bool]string{true: "buckets", false: "no buckets"}[m.Histogram != nil])
		case (m.Type == SummaryMetricType) != (m.Summary != nil):
			return nameErrorf("metric %q has type %s but %s; only a summary read from Prometheus exposition has quantiles, and metric() and the rules make gauges, counters and untyped series", shownName(m.Name), m.Type, map[bool]string{true: "quantiles", false: "no quantiles"}[m.Summary != nil])
		}
		// Prometheus accepts infinities and NaN, so no value check applies here.
		if len(m.Labels) > l.MaxLabelsPerMetric && l.MaxLabelsPerMetric > 0 {
			return Errorf("metric %q has %d labels, more than limits.max_labels_per_metric %d; drop labels it does not need or raise limits.max_labels_per_metric", shownName(m.Name), Size(len(m.Labels)), l.MaxLabelsPerMetric)
		}
		// le and quantile, the labels of a histogram's buckets and a
		// summary's quantiles, are label names too. A limit under the longer
		// one, 8 bytes, is checked against them. Any other limit costs one
		// comparison.
		if l.MaxLabelNameLength > 0 && l.MaxLabelNameLength < len("quantile") {
			if own := seriesOwnLabel(m); len(own) > l.MaxLabelNameLength {
				return nameErrorf("metric %q is a %s, whose %s carry the label %s, which is longer than limits.max_label_name_length %d; raise limits.max_label_name_length", shownName(m.Name), m.Type, map[string]string{"le": "buckets", "quantile": "quantiles"}[own], own, l.MaxLabelNameLength)
			}
		}
		var labels uint64
		for k, v := range m.Labels {
			// What labelFailure refuses, asked here without a call: a
			// series that passes pays for the questions alone.
			if !takesName(k, false, utf8Names) || ReservedLabelName(k) || k == seriesOwnLabel(m) || len(k) > l.MaxLabelNameLength && l.MaxLabelNameLength > 0 || l.MaxLabelValueLength > 0 && len(v) > l.MaxLabelValueLength {
				return firstLabelFailure(m, &l, utf8Names)
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

// checkDerivedNames refuses a family named like a series of a histogram or a
// summary. A histogram foo is written as foo_bucket, foo_sum and foo_count,
// and a summary foo as foo, foo_sum and foo_count. A gauge foo_count next to
// either would be written as a second family of that name with another type,
// which Prometheus refuses.
func checkDerivedNames(types map[string]MetricType) error {
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var suffixes []string
		switch types[name] {
		case HistogramMetricType:
			suffixes = []string{"_bucket", "_sum", "_count"}
		case SummaryMetricType:
			suffixes = []string{"_sum", "_count"}
		}
		for _, suffix := range suffixes {
			if other, clash := types[name+suffix]; clash {
				return fmt.Errorf("metric %q (%s) clashes with the %s %q, which is written as series of that name; rename one", name+suffix, other, types[name], name)
			}
		}
	}
	return nil
}

// Number reads a decoded value as a number: any numeric type, a string
// holding one, or a boolean as 1 or 0. Its error shows the value in readable
// form (ShowValue), never in Go syntax. Go syntax printed a whole object,
// such as map[a:[1 2]], into the logs and the scrape's error.
func Number(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case json.Number:
		return parseNumber(string(x))
	case *big.Int:
		// gojq's integers beyond int64, as tonumber or arithmetic make them.
		f, _ := new(big.Float).SetInt(x).Float64()
		return f, nil
	case string:
		return parseNumber(strings.TrimSpace(x))
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case nil:
		return 0, errors.New("value is null, not a number")
	default:
		return 0, fmt.Errorf("value is %s, not a number; select a number inside it", ShowValue(v))
	}
}

// parseNumber reads text as a number.
func parseNumber(text string) (float64, error) {
	f, err := ParseFloat(text)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, strconv.ErrRange):
		return 0, fmt.Errorf("value %s is beyond the range of a 64-bit float", QuoteValue(text))
	default:
		return 0, fmt.Errorf("value %s is not a number; map text to numbers with value_map", QuoteValue(text))
	}
}

// ParseFloat reads the text of a value in a response as a number, as
// strconv.ParseFloat does and with its errors. The exception is two forms
// that only Go uses to write a number: digits separated by underscores,
// 1_000, and hexadecimal floating-point, 0x1p-2. Read as numbers, a cell
// holding an identifier such as 1_000 was exported as 1000. The Prometheus
// decoder refuses both forms as well (decode/promparse.go). They are not
// numbers and fail with strconv.ErrSyntax, whether strconv would read them
// or find them out of range.
//
// In a text that strconv reads or finds out of range, an underscore can only
// be a digit separator and an x only the mark of a hexadecimal number. So
// looking for these two letters is enough, and it is the only cost beyond
// strconv's own parsing.
func ParseFloat(text string) (float64, error) {
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return f, err
	}
	for i := 0; i < len(text); i++ {
		// Digits, signs and the point come before X, so they cost one
		// comparison each.
		if c := text[i]; c >= 'X' && (c == '_' || c == 'x' || c == 'X') {
			return 0, strconv.ErrSyntax
		}
	}
	return f, err
}

// maxQuotedValue is how much of a value an error quotes.
const maxQuotedValue = 64

// maxShownName is how much of a metric's or a label's name the error of a
// set that cannot be exposed quotes: the default
// limits.max_metric_name_length, so a name within that is quoted whole.
const maxShownName = 200

// shownName returns a metric or label name as the error of a set that cannot
// be exposed quotes it with %q (Validate). A name no longer than maxShownName
// is the name itself, which %q quotes whole as before. A longer name is its
// first maxShownName bytes, cut at a character boundary and quoted, followed
// by its length, as Quoted shows a value. The target or a script can make a
// name as long as the response, and the error says after it what is wrong
// with it and which limit to raise: `invalid metric
// name "..."... (10485561 bytes): longer than limits.max_metric_name_length
// 200`.
//
// A name within the bound costs the error the same as before: it is the
// same string, in the same error fmt.Errorf made of it before (nameErrorf).
func shownName(name string) any {
	if len(name) <= maxShownName {
		return name
	}
	head := HeadOf(name, maxShownName)
	return shownStart(strconv.Quote(name[:head]), head, len(name))
}

// ShownName returns a metric or label name as a log line shows it in an
// attribute. It follows the rule by which the error of a set that cannot be
// exposed quotes a name (shownName), so one rule holds wherever a name is
// shown. It has no quotes: they belong to the error, and an attribute value
// never had them. A name no longer than maxShownName is returned as is, the
// same string. A longer one is cut to its first maxShownName bytes, at a
// character boundary, followed by its length (CutMark).
//
// A line may name a metric before the set is validated, as the warning about
// repaired UTF-8 names the first metric repaired. The name is then as long
// as the target or a script made it, and not yet held to
// limits.max_metric_name_length, which the operator may raise anyway. The
// cut name is newly made and holds nothing of the name.
func ShownName(name string) string {
	if len(name) <= maxShownName {
		return name
	}
	return name[:HeadOf(name, maxShownName)] + CutMark(len(name), false)
}

// nameErrorf is fmt.Errorf for the error of a set that cannot be exposed,
// when its text quotes a name (shownName). It returns the error fmt.Errorf
// makes, unless a name was cut. Then it returns the error Errorf makes,
// which reads the same but is recognised with the mark in place of the
// name's length. So a target whose over-long name grows from one scrape to
// the next fails the same way in the log on each.
func nameErrorf(format string, args ...any) error {
	for _, arg := range args {
		if _, cut := arg.(QuotedValue); cut {
			// A copy, which the error keeps: the arguments themselves
			// are then kept by nothing, and an error of names within
			// the bound is made without them leaving the stack.
			return Errorf(format, slices.Clone(args)...)
		}
	}
	return fmt.Errorf(format, args...)
}

// QuoteValue quotes text for an error message. Longer text is cut to its
// first 64 bytes, at a character boundary, and followed by its full length.
// A response that does not match a rule can be a whole page, which an error
// that is logged and returned to the scraper should not carry.
func QuoteValue(text string) string {
	if len(text) <= maxQuotedValue {
		return strconv.Quote(text)
	}
	cut := maxQuotedValue
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes)", strconv.Quote(text[:cut]), len(text))
}

// ShowValue names a decoded value for an error message: text quoted
// (QuoteValue), a number or a boolean as written, null as null, and an object
// or an array by its kind and size rather than its content, such as "an
// object with 2 keys" or "an array of 3 items".
func ShowValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return QuoteValue(x)
	case json.Number:
		return QuoteValue(string(x))
	case bool, float64, float32, int, int64, uint64, *big.Int:
		return fmt.Sprint(x)
	case map[string]any:
		return counted("an object with", len(x), "key")
	case map[any]any:
		return counted("an object with", len(x), "key")
	case []any:
		return counted("an array of", len(x), "item")
	default:
		return fmt.Sprintf("a value of type %T", x)
	}
}

func counted(what string, n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%s %d %s", what, n, noun)
}

// MaxWholeNumberDigits is the longest whole number, sign included, that is
// read as a number. A longer one is kept as its text, with every digit.
// Reading digits into a *big.Int takes time that grows with the square of
// their count. A million digits, which fit within the default size limit,
// took two seconds that no deadline could interrupt. No identifier or count
// is thousands of digits long.
const MaxWholeNumberDigits = 4096

// WholeNumberText reports whether s is a whole number as JSON writes one:
// digits, with a minus sign before them or not.
func WholeNumberText(s string) bool {
	if len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Normalize rewrites decoded JSON or YAML in place into the shapes the
// transforms expect. gojq, the jq and yq engine, handles all of them: maps
// keyed by string; a whole number as an int, or as a *big.Int beyond int64,
// so an ID of any length keeps every digit; any other json.Number as a
// float64, or as its text when it is not one or is a whole number of more
// digits than MaxWholeNumberDigits; and a time.Time, which YAML makes of
// what looks like a timestamp and gojq cannot handle, as RFC 3339 text.
//
// The json decoder makes these shapes itself while it reads a body, without
// a second pass here (internal/decode/jsonvalue.go). A test compares its
// output with what Normalize makes of encoding/json's values. A change to
// the number rules here must be made there too.
func Normalize(v any) any {
	switch x := v.(type) {
	case map[any]any:
		m := map[string]any{}
		for k, v := range x {
			m[fmt.Sprint(k)] = Normalize(v)
		}
		return m
	case map[string]any:
		for k, v := range x {
			x[k] = Normalize(v)
		}
		return x
	case []any:
		for i, v := range x {
			x[i] = Normalize(v)
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		if WholeNumberText(string(x)) {
			if len(x) > MaxWholeNumberDigits {
				return string(x)
			}
			i, _ := new(big.Int).SetString(string(x), 10)
			return i
		}
		if n, err := x.Float64(); err == nil {
			return n
		}
		return string(x)
	case uint64:
		if x <= math.MaxInt64 {
			return int(x)
		}
		return new(big.Int).SetUint64(x)
	case uint:
		return Normalize(uint64(x))
	case int64:
		return int(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return x
	}
}

// CloneMetricSet returns a deep copy of in, which the copy's user may change
// without affecting in.
func CloneMetricSet(in MetricSet) MetricSet {
	out := MetricSet{Metrics: make([]Metric, 0, len(in.Metrics))}
	for _, metric := range in.Metrics {
		out.Metrics = append(out.Metrics, CloneMetric(metric))
	}
	return out
}

// CloneMetric returns a deep copy of in.
func CloneMetric(in Metric) Metric {
	out := in
	if in.Labels != nil {
		out.Labels = CloneLabels(in.Labels)
	}
	if in.Timestamp != nil {
		timestamp := *in.Timestamp
		out.Timestamp = &timestamp
	}
	if in.Histogram != nil {
		histogram := *in.Histogram
		histogram.Buckets = append([]Bucket(nil), in.Histogram.Buckets...)
		if in.Histogram.Exponential != nil {
			histogram.Exponential = in.Histogram.Exponential.AtMostScale(in.Histogram.Exponential.Scale)
		}
		out.Histogram = &histogram
	}
	if in.Summary != nil {
		summary := *in.Summary
		summary.Quantiles = append([]Quantile(nil), in.Summary.Quantiles...)
		out.Summary = &summary
	}
	return out
}

// CloneLabels returns a copy of in, never nil.
func CloneLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SortedKeys returns the keys of in in sorted order.
func SortedKeys[V any](in map[string]V) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// ScrapeTimestamp reports a registered but never scraped request as 0 rather
// than as the zero instant, which would otherwise appear as a timestamp far in
// the past and read as a very stale scrape.
func ScrapeTimestamp(at time.Time) float64 {
	if at.IsZero() {
		return 0
	}
	return float64(at.UnixNano()) / float64(time.Second)
}

// SanitizeUTF8 replaces what is not valid UTF-8 in label values and help text
// with U+FFFD, and returns how many values it changed and the first series
// changed. A metric's labels are copied before a change, since a transform may
// share one map among metrics.
func SanitizeUTF8(set *MetricSet) (uint64, string) {
	if set == nil {
		return 0, ""
	}
	var changed uint64
	first := ""
	note := func(name string) {
		changed++
		if first == "" {
			first = name
		}
	}
	for i := range set.Metrics {
		m := &set.Metrics[i]
		if !utf8.ValidString(m.Help) {
			m.Help = strings.ToValidUTF8(m.Help, "�")
			note(m.Name)
		}
		copied := false
		for k, v := range m.Labels {
			if utf8.ValidString(v) {
				continue
			}
			if !copied {
				m.Labels = CloneLabels(m.Labels)
				copied = true
			}
			m.Labels[k] = strings.ToValidUTF8(v, "�")
			note(m.Name)
		}
	}
	return changed, first
}

// labelFailure returns why a label of a series cannot be exposed, or nil
// when it can: an invalid name, a name over limits.max_label_name_length, or
// a value over limits.max_label_value_length. A name's length is checked
// after it is found to be a valid name, as for a metric name.
func labelFailure(m *Metric, k, v string, l *Limits, utf8Names bool) error {
	if !takesName(k, false, utf8Names) {
		if k != "" && utf8.ValidString(k) {
			return nameErrorf("metric %q has label %q, which is not a classic Prometheus label name; set the collector's name_escaping to underscores or values to export it escaped", shownName(m.Name), shownName(k))
		}
		return nameErrorf("metric %q has invalid label name %q", shownName(m.Name), shownName(k))
	}
	if l.MaxLabelNameLength > 0 && len(k) > l.MaxLabelNameLength {
		return labelNameLengthError(m, k, l.MaxLabelNameLength)
	}
	if ReservedLabelName(k) {
		return nameErrorf("metric %q has %s", shownName(m.Name), reservedLabelError(k))
	}
	// A histogram's buckets are distinguished by le and a summary's
	// quantiles by quantile. A series with its own label of that name
	// would be written with it on _sum and _count, which a parser reads
	// as a bucket or quantile without a bound, and twice on every bucket.
	if own := seriesOwnLabel(m); k == own {
		return nameErrorf("metric %q is a %s and has a label %s of its own, which its %s carry; name the label something else", shownName(m.Name), m.Type, own, map[string]string{"le": "buckets", "quantile": "quantiles"}[own])
	}
	if l.MaxLabelValueLength > 0 && len(v) > l.MaxLabelValueLength {
		return Errorf("metric %q label %q value is %d bytes, longer than limits.max_label_value_length %d; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length", shownName(m.Name), shownName(k), Size(len(v)), l.MaxLabelValueLength)
	}
	return nil
}

// labelNameLengthError is the failure of a series with a label named k that
// is longer than limits.max_label_name_length (limit). The target or a
// script can make the name as long as the response, so it is shown like a
// metric name over its limit: whole within 200 bytes, and beyond that by its
// start and its length (shownName).
func labelNameLengthError(m *Metric, k string, limit int) error {
	return nameErrorf("metric %q label name %q is longer than limits.max_label_name_length %d; rename the label with transform.rename_labels, or raise limits.max_label_name_length", shownName(m.Name), shownName(k), limit)
}

// firstLabelFailure returns the failure of the first failing label of a
// series, ordered by label name. The labels are a map, iterated in no fixed
// order. If two labels fail, one scrape could name one and the next scrape
// the other, and the log would take each for a new failure
// (SameFailureText). So once a label fails, the labels are checked again in
// order. A series that passes never reaches this step.
func firstLabelFailure(m *Metric, l *Limits, utf8Names bool) error {
	names := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := labelFailure(m, k, m.Labels[k], l, utf8Names); err != nil {
			return err
		}
	}
	return nil
}
