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
type Metric struct {
	Name      string            `json:"name"`
	Help      string            `json:"help,omitempty"`
	Type      MetricType        `json:"type"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	Timestamp *int64            `json:"timestamp,omitempty"`
	Histogram *Histogram        `json:"-"`
	Summary   *Summary          `json:"-"`
}

// Histogram is the value of a histogram series. NoSum and NoCount say that
// the source it was read from gave no _sum or no _count sample, so that none
// is written for it: a histogram made here, whose zero value has both, always
// has them.
//
// A histogram's +Inf bucket counts every observation, as its _count does, so
// the two are one number written twice, and a histogram holds each as its
// source wrote it: the +Inf bucket among Buckets, the _count in Count. A
// source may write only one of them, and the other is then read from it
// (InfBucket, Settle). It may also write two that differ, as a target does
// that counts an observation in its buckets and in its count without a lock
// and is read between the two; both are then kept and written as they were
// read, since nothing says which of them is right. And it may write neither,
// which leaves a histogram without a +Inf bucket.
type Histogram struct {
	Buckets []Bucket
	Sum     float64
	Count   uint64
	NoSum   bool
	NoCount bool
}

// InfBucket is the count of the histogram's +Inf bucket, and whether it has
// one: the bucket of that bound among Buckets, or, for a histogram without
// one, its _count, which a +Inf bucket is then written from. A histogram with
// neither has none. The +Inf bucket is nearly always the last, which is
// looked at first.
func (h *Histogram) InfBucket() (count uint64, ok bool) {
	for i := len(h.Buckets) - 1; i >= 0; i-- {
		if math.IsInf(h.Buckets[i].UpperBound, 1) {
			return h.Buckets[i].CumulativeCount, true
		}
	}
	return h.Count, !h.NoCount
}

// Settle checks a histogram read from a source and settles its count. Two
// buckets with one upper bound, such as le="1" and le="1.0", are an error, as
// two samples of one series are. Without a _count (NoCount) the count is the
// +Inf bucket's, which counts every observation. A +Inf bucket and a _count
// that differ are no error, and neither is a histogram without either
// (Histogram): one such series must not fail a whole scrape. The error does
// not name the series, which its caller knows.
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

// duplicateFloat reports a value that is twice among the n that at gives.
// Two NaN count as the same value, since both are written as NaN. Values in
// ascending order, as buckets and quantiles nearly always are written, hold
// none, which one pass over them shows; only others are copied and sorted.
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
// matching [a-zA-Z_:][a-zA-Z0-9_:]*. It is checked for every series of every
// scrape, so it is a loop over the bytes rather than a regular expression,
// which cost a quarter of a large scrape's time.
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

// ReservedLabelName reports whether name is one Prometheus keeps for itself:
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

// seriesSet is the series of a set Validate has seen so far, which tells a
// series seen twice. To Prometheus a series is its name and its labels, in
// whatever order, and a label with an empty value is no label at all, so
// m{a=""} and m are one series.
//
// Validate looks at every series of every scrape, so seeing one is shaped to
// cost little and to allocate nothing. A series is kept by a hash rather than
// by a key written out: its name and the name and value of each label are
// hashed where they lie, and the labels' hashes are added up, which comes to
// the same sum in any order, so nothing is sorted and nothing copied. The
// first series with a hash stands for it in byHash. A series whose hash is
// taken is compared, label by label, with the one holding it, and only that
// comparison says they are the same series: two different series with one
// hash cost a comparison, and the second then goes into others, where the few
// series that share a hash with a different, earlier one are kept in a list
// and compared in turn. A hash therefore never makes a duplicate of two
// different series, nor hides a real one.
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

// addHashed is add for a caller that went through the labels of metrics[i]
// itself, as Validate does to check them, and added up the labelHash of each
// one with a value: going through a map a second time costs more than the
// hashing does.
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
// making series at the first one past the limit, so count is then limit+1:
// the scrape has at least that many, and how many more is not worth the
// memory of finding out.
func MetricCountError(count, limit int) error {
	return MarkError(fmt.Errorf("metric count %d exceeds limit %d", count, limit), ErrLimitExceeded)
}

// Validate checks the set against the collector's limits and the rules of
// the exposition format: valid names and types, no duplicate series, one type
// per family. It stops at the first problem, which the error describes.
func (s *MetricSet) Validate(l Limits) error {
	if l.MaxMetrics > 0 && len(s.Metrics) > l.MaxMetrics {
		return MetricCountError(len(s.Metrics), l.MaxMetrics)
	}
	return s.validate(l, newSeriesSet(s.Metrics))
}

// validate is Validate past the count of series, with the set that tells
// duplicate series, which a test makes itself to give it a weak hash.
func (s *MetricSet) validate(l Limits, seen *seriesSet) error {
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

// checkDerivedNames refuses a family named like a series of a histogram or a
// summary: a histogram foo is written as foo_bucket, foo_sum and foo_count,
// and a summary foo as foo, foo_sum and foo_count, so a gauge foo_count next
// to either would be written as a second family of that name, of another
// type, which Prometheus refuses.
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
// holding one, or a boolean as 1 or 0. Its error names the value as a person
// reads it (ShowValue), never in Go's syntax, which printed an object in full,
// as map[a:[1 2]], into the logs and the scrape's error.
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
	f, err := strconv.ParseFloat(text, 64)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, strconv.ErrRange):
		return 0, fmt.Errorf("value %s is beyond the range of a 64-bit float", QuoteValue(text))
	default:
		return 0, fmt.Errorf("value %s is not a number; map text to numbers with value_map", QuoteValue(text))
	}
}

// maxQuotedValue is how much of a value an error quotes.
const maxQuotedValue = 64

// QuoteValue quotes text for an error message, cut to its first 64 bytes, at
// a character boundary, when it is longer, with its full length after it. A
// response that is not what a rule expected can be a whole page, which an
// error, logged and returned to the scraper, should not carry.
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

// MaxWholeNumberDigits is the longest whole number read as a number, sign
// included: a longer one is kept as its text, every digit of it. Reading
// digits into a *big.Int takes time that grows with the square of their
// number — a million of them, which a response within the default size limit
// can hold, took two seconds that no deadline could interrupt — and no
// identifier or count is thousands of digits long.
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
// transforms expect, all of which gojq, the jq and yq engine, handles: maps
// keyed by string; a whole number as an int, or a *big.Int beyond int64, so
// an ID of any length keeps every digit; any other json.Number as a float64,
// or as its text when it is not one, or is a whole number of more digits
// than MaxWholeNumberDigits; and a time.Time, which YAML makes of what reads
// as a timestamp and gojq cannot handle, as RFC 3339 text.
//
// The json decoder makes these shapes itself as it reads a body, without a
// second pass here (internal/decode/jsonvalue.go), and a test compares what
// it makes with what Normalize makes of encoding/json's values: a change to
// the rules for numbers here is a change to make there too.
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
