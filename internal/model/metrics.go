package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"math/big"
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

// Histogram is the value of a histogram series.
type Histogram struct {
	Buckets []Bucket
	Sum     float64
	Count   uint64
}

// Bucket is one bucket of a histogram: how many observations were at most
// UpperBound.
type Bucket struct {
	UpperBound      float64
	CumulativeCount uint64
}

// Summary is the value of a summary series.
type Summary struct {
	Quantiles []Quantile
	Sum       float64
	Count     uint64
}

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

// appendSeriesKey appends the key identifying the series m is to Prometheus
// to key, and reports whether m has a label with an empty value. Prometheus
// reads such a label as no label at all, so m{a=""} and m are one series and
// have one key. names is scratch space for the label names, returned for the
// next call to reuse.
func (m *Metric) appendSeriesKey(key []byte, names []string) ([]byte, []string, bool) {
	names = names[:0]
	empty := false
	for k, v := range m.Labels {
		if v == "" {
			empty = true
			continue
		}
		names = append(names, k)
	}
	slices.Sort(names)
	key = append(key, m.Name...)
	for _, k := range names {
		key = append(key, '\xff')
		key = append(key, k...)
		key = append(key, '=')
		key = append(key, m.Labels[k]...)
	}
	return key, names, empty
}

// seriesSet is the series of a set Validate has seen so far. It keeps each
// series by a hash of its key, the first series with that hash standing for
// it, so seeing a series allocates nothing: a series whose hash is taken is
// told apart from the one holding it by building that one's key again, and
// only a series that really differs from it goes by its key in full.
type seriesSet struct {
	metrics []Metric
	hash    func([]byte) uint64
	byHash  map[uint64]int
	// others holds series whose hash a different series took first, and
	// whether each has a label with an empty value.
	others     map[string]bool
	key, other []byte
	names      []string
}

func newSeriesSet(metrics []Metric) *seriesSet {
	seed := maphash.MakeSeed()
	return &seriesSet{
		metrics: metrics,
		hash:    func(key []byte) uint64 { return maphash.Bytes(seed, key) },
		byHash:  make(map[uint64]int, len(metrics)),
	}
}

// add adds metrics[i]. When the series was seen already, it reports so, and
// whether either of the two has a label with an empty value.
func (s *seriesSet) add(i int) (duplicate, empty bool) {
	s.key, s.names, empty = s.metrics[i].appendSeriesKey(s.key[:0], s.names)
	h := s.hash(s.key)
	first, taken := s.byHash[h]
	if !taken {
		s.byHash[h] = i
		return false, false
	}
	var firstEmpty bool
	s.other, s.names, firstEmpty = s.metrics[first].appendSeriesKey(s.other[:0], s.names)
	if bytes.Equal(s.key, s.other) {
		return true, empty || firstEmpty
	}
	if earlierEmpty, seen := s.others[string(s.key)]; seen {
		return true, empty || earlierEmpty
	}
	if s.others == nil {
		s.others = map[string]bool{}
	}
	s.others[string(s.key)] = empty
	return false, false
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
	seen := newSeriesSet(s.Metrics)
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
			if l.MaxLabelValueLength > 0 && len(v) > l.MaxLabelValueLength {
				return fmt.Errorf("metric %q label %q is too long", m.Name, k)
			}
		}
		if l.MaxHelpLength > 0 && len(m.Help) > l.MaxHelpLength {
			return fmt.Errorf("metric %q help is too long", m.Name)
		}
		if prior, ok := types[m.Name]; ok && prior != m.Type {
			return fmt.Errorf("metric %q has inconsistent types", m.Name)
		}
		types[m.Name] = m.Type
		if duplicate, empty := seen.add(i); duplicate {
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

// Normalize rewrites decoded JSON or YAML in place into the shapes the
// transforms expect, all of which gojq, the jq and yq engine, handles: maps
// keyed by string; a whole number as an int, or a *big.Int beyond int64, so
// an ID of any length keeps every digit; any other json.Number as a float64,
// or as its text when it is not one; and a time.Time, which YAML makes of
// what reads as a timestamp and gojq cannot handle, as RFC 3339 text.
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
		if i, ok := new(big.Int).SetString(string(x), 10); ok {
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
