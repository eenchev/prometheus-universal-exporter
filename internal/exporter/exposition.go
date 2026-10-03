package exporter

import (
	"cmp"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// probeError is the body of a probe that failed because a metric rule with
// error_mode fail could not produce its value. Prometheus only looks at the
// status code, which already fails the scrape; the body is for the person who
// runs the probe by hand to find out why, so it names the collector, the rule
// and the error rather than leaving them to be dug out of the exporter's log.
type probeError struct {
	Status    string `json:"status"`
	Stage     string `json:"stage"`
	Collector string `json:"collector"`
	Metric    string `json:"metric,omitempty"`
	Target    string `json:"target,omitempty"`
	Error     string `json:"error"`
}

// writeProbeError writes a probe failure as a JSON object. Status is always
// "error", so a client can test one field without knowing the others.
func writeProbeError(w http.ResponseWriter, code int, body probeError) {
	body.Status = "error"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// expositionContentType is the text format's type, with the charset its
// label values and help are written in, so a browser opening an endpoint
// shows them as they are.
const expositionContentType = "text/plain; version=0.0.4; charset=utf-8"

// expositionBuffers keeps the buffers answers are rendered into, so a busy
// endpoint does not allocate one, and grow it, for every answer. A buffer
// that grew past maxPooledExposition is left to the garbage collector rather
// than kept for ever for one large answer.
var expositionBuffers = sync.Pool{New: func() any { b := make([]byte, 0, 16<<10); return &b }}

const maxPooledExposition = 4 << 20

// writeMetricSet answers r with s, in the format r's Accept header prefers
// (negotiateFormat): the Prometheus text format, or OpenMetrics. Label values
// and help come from scraped targets, so the answer carries text a target
// chose; it is served as plain text with nosniff, so a browser shows it as
// text and never renders it as HTML. r may be nil, which answers in the text
// format.
func writeMetricSet(w http.ResponseWriter, r *http.Request, s *model.MetricSet) {
	format := formatText
	if r != nil {
		format = negotiateFormat(r.Header.Get("Accept"))
	}
	w.Header().Set("Content-Type", format.contentType())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The answer depends on Accept, so a cache in between must not give one
	// client's format to another.
	w.Header().Add("Vary", "Accept")
	pooled := expositionBuffers.Get().(*[]byte)
	b := format.appendMetricSet((*pooled)[:0], s)
	_, _ = w.Write(b) //nolint:gosec // G705: text/plain with nosniff, never rendered as HTML
	if cap(b) <= maxPooledExposition {
		*pooled = b[:0]
		expositionBuffers.Put(pooled)
	}
}

// appendMetricSet appends the exposition text of a metric set to b, so the
// verbose self-metrics are rendered by exactly the same code as collector
// output. It writes with strconv's append functions rather than fmt, and
// keeps one slice for sorting every series' label names, so rendering
// allocates little beyond the text itself.
func appendMetricSet(b []byte, s *model.MetricSet) []byte {
	var e expositionWriter
	described := map[string]bool{}
	last := ""
	for _, m := range s.Metrics {
		if m.Name != last && !described[m.Name] {
			if m.Help != "" {
				b = append(b, "# HELP "...)
				b = append(b, m.Name...)
				b = append(b, ' ')
				b = appendEscaped(b, m.Help, false)
				b = append(b, '\n')
			}
			b = append(b, "# TYPE "...)
			b = append(b, m.Name...)
			b = append(b, ' ')
			b = append(b, m.Type...)
			b = append(b, '\n')
			described[m.Name] = true
		}
		last = m.Name
		switch {
		case m.Histogram != nil:
			b = e.appendHistogram(b, m)
		case m.Summary != nil:
			b = e.appendSummary(b, m)
		default:
			b = e.appendSample(b, m.Name, "", m.Labels, "", "", m.Value, m)
		}
	}
	return b
}

// expositionWriter holds what rendering reuses from series to series.
type expositionWriter struct {
	keys      []string
	buckets   []model.Bucket
	quantiles []model.Quantile
}

// appendSample writes one sample line: name and suffix, the labels with
// extraName set to extraValue when extraName is not empty, the value and the
// metric's timestamp.
func (e *expositionWriter) appendSample(b []byte, name, suffix string, labels map[string]string, extraName, extraValue string, value float64, m model.Metric) []byte {
	b = append(b, name...)
	b = append(b, suffix...)
	b = e.appendLabels(b, labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendFloat(b, value, 'g', -1, 64)
	return appendTimestamp(b, m)
}

// appendCount is appendSample for a count, written as an integer.
func (e *expositionWriter) appendCount(b []byte, name, suffix string, labels map[string]string, extraName, extraValue string, count uint64, m model.Metric) []byte {
	b = append(b, name...)
	b = append(b, suffix...)
	b = e.appendLabels(b, labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendUint(b, count, 10)
	return appendTimestamp(b, m)
}

// appendLabels writes {name="value",...} in name order, with extraName, a
// histogram's le or a summary's quantile, set to extraValue over any label
// of that name, and nothing for no labels.
func (e *expositionWriter) appendLabels(b []byte, labels map[string]string, extraName, extraValue string) []byte {
	keys := e.keys[:0]
	for k := range labels {
		if k != extraName {
			keys = append(keys, k)
		}
	}
	if extraName != "" {
		keys = append(keys, extraName)
	}
	e.keys = keys
	if len(keys) == 0 {
		return b
	}
	slices.Sort(keys)
	b = append(b, '{')
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		value := labels[k]
		if extraName != "" && k == extraName {
			value = extraValue
		}
		b = append(b, k...)
		b = append(b, '=', '"')
		b = appendEscaped(b, value, true)
		b = append(b, '"')
	}
	return append(b, '}')
}

// appendEscaped writes text with backslashes and newlines escaped, and
// double quotes too in a label value.
func appendEscaped(b []byte, text string, quotes bool) []byte {
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\\':
			b = append(b, '\\', '\\')
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '"' && quotes:
			b = append(b, '\\', '"')
		default:
			b = append(b, c)
		}
	}
	return b
}

// appendHistogram writes a histogram's lines: its buckets, in ascending order
// of their bounds whatever order the source had them in (ascendingBuckets),
// and its _sum and _count unless the source it was read from had none
// (model.Histogram), so a histogram goes out as it came in rather than with a
// sum or a count of 0 it never had. The +Inf bucket and the _count are each
// written with the number the source gave it, also when the two differ; a
// histogram that has a _count and no +Inf bucket is given the bucket, and one
// with neither is written without both.
func (e *expositionWriter) appendHistogram(b []byte, m model.Metric) []byte {
	for _, x := range e.ascendingBuckets(m.Histogram.Buckets) {
		// The +Inf bucket is written below, last and once, whether the
		// histogram carries one of its own or is given it from its count.
		if math.IsInf(x.UpperBound, 1) {
			continue
		}
		b = e.appendCount(b, m.Name, "_bucket", m.Labels, "le", strconv.FormatFloat(x.UpperBound, 'g', -1, 64), x.CumulativeCount, m)
	}
	if inf, has := m.Histogram.InfBucket(); has {
		b = e.appendCount(b, m.Name, "_bucket", m.Labels, "le", "+Inf", inf, m)
	}
	if !m.Histogram.NoSum {
		b = e.appendSample(b, m.Name, "_sum", m.Labels, "", "", m.Histogram.Sum, m)
	}
	if !m.Histogram.NoCount {
		b = e.appendCount(b, m.Name, "_count", m.Labels, "", "", m.Histogram.Count, m)
	}
	return b
}

// appendTimestamp ends a sample line of m: its own timestamp, in
// milliseconds after a space, if it has one, and the newline. Every line of
// a histogram or a summary carries it, as every line of a plain sample does.
func appendTimestamp(b []byte, m model.Metric) []byte {
	if m.Timestamp != nil {
		b = append(b, ' ')
		b = strconv.AppendInt(b, *m.Timestamp, 10)
	}
	return append(b, '\n')
}

// ascendingBuckets returns a histogram's buckets in ascending order of their
// upper bounds, which is the order both formats are written in: a target may
// write its buckets in any order, the Prometheus text format asking for none,
// while OpenMetrics, and a reader that takes the first bucket a value fits
// in, need them ascending. A bound that is not a number comes first, and
// +Inf, which is written from the count, last. Buckets nearly always arrive
// in order, and are then returned as they are; others are sorted in the
// writer's own copy, the metric set being shared with the cache.
func (e *expositionWriter) ascendingBuckets(buckets []model.Bucket) []model.Bucket {
	if slices.IsSortedFunc(buckets, compareBuckets) {
		return buckets
	}
	e.buckets = append(e.buckets[:0], buckets...)
	slices.SortStableFunc(e.buckets, compareBuckets)
	return e.buckets
}

// ascendingQuantiles returns a summary's quantiles in ascending order, as
// ascendingBuckets does a histogram's buckets.
func (e *expositionWriter) ascendingQuantiles(quantiles []model.Quantile) []model.Quantile {
	if slices.IsSortedFunc(quantiles, compareQuantiles) {
		return quantiles
	}
	e.quantiles = append(e.quantiles[:0], quantiles...)
	slices.SortStableFunc(e.quantiles, compareQuantiles)
	return e.quantiles
}

func compareBuckets(a, b model.Bucket) int { return cmp.Compare(a.UpperBound, b.UpperBound) }

func compareQuantiles(a, b model.Quantile) int { return cmp.Compare(a.Quantile, b.Quantile) }

// typedValues reports whether the values of m are ones a series of its type
// may have in OpenMetrics, and in OTLP, which follows it. The Prometheus text
// format asks nothing of them, so a target may write, and the exporter
// passes on, what a strict OpenMetrics parser refuses, failing the scrape:
//
//   - a counter that is NaN or negative;
//   - a histogram whose _sum is NaN or negative, or is there beside a bucket
//     with a negative bound, which makes it no counter; one with a bound that
//     is not a number; one whose bucket counts fall from one bound to the
//     next higher, or to its +Inf bucket's; one whose +Inf bucket and _count
//     differ, as a target read between its updating the one and the other
//     writes them (model.Histogram); and one with neither of the two, which
//     has no +Inf bucket;
//   - a summary whose _sum is NaN or negative, with a quantile that is not
//     within 0 to 1, or with a negative value for a quantile. NaN is a
//     quantile's value when nothing was observed, and is allowed.
//
// A gauge and an untyped series may have any value, and a count, being
// unsigned, is never NaN or negative. A family with such a series is written
// as unknown in OpenMetrics (planOpenMetrics) and exported as gauges over
// OTLP (otlpMetrics), with the same series and values, rather than left out
// or refused.
func typedValues(m model.Metric) bool {
	switch {
	case m.Histogram != nil:
		h := m.Histogram
		if !h.NoSum && !counts(h.Sum) {
			return false
		}
		buckets := h.Buckets
		if !slices.IsSortedFunc(buckets, compareBuckets) {
			buckets = slices.Clone(buckets)
			slices.SortStableFunc(buckets, compareBuckets)
		}
		inf, has := h.InfBucket()
		if !has || !h.NoCount && inf != h.Count {
			return false
		}
		var below uint64
		for _, b := range buckets {
			switch {
			case math.IsInf(b.UpperBound, 1):
				// Written last, and checked below.
				continue
			case math.IsNaN(b.UpperBound), b.UpperBound < 0 && !h.NoSum, b.CumulativeCount < below:
				return false
			}
			below = b.CumulativeCount
		}
		return below <= inf
	case m.Summary != nil:
		s := m.Summary
		if !s.NoSum && !counts(s.Sum) {
			return false
		}
		for _, q := range s.Quantiles {
			if !(q.Quantile >= 0 && q.Quantile <= 1) || q.Value < 0 {
				return false
			}
		}
		return true
	case m.Type == model.CounterMetricType:
		return counts(m.Value)
	}
	return true
}

// counts reports whether v is a value a counter may have: not NaN and not
// negative.
func counts(v float64) bool { return v >= 0 }

// appendSummary writes a summary's lines: its quantiles, in ascending order,
// and its _sum and _count unless the source it was read from had none.
func (e *expositionWriter) appendSummary(b []byte, m model.Metric) []byte {
	for _, x := range e.ascendingQuantiles(m.Summary.Quantiles) {
		b = e.appendSample(b, m.Name, "", m.Labels, "quantile", strconv.FormatFloat(x.Quantile, 'g', -1, 64), x.Value, m)
	}
	if !m.Summary.NoSum {
		b = e.appendSample(b, m.Name, "_sum", m.Labels, "", "", m.Summary.Sum, m)
	}
	if !m.Summary.NoCount {
		b = e.appendCount(b, m.Name, "_count", m.Labels, "", "", m.Summary.Count, m)
	}
	return b
}

// expositionFormat is the format an answer is written in.
type expositionFormat struct {
	// openMetrics chooses OpenMetrics over the Prometheus text format, and
	// version is the OpenMetrics version the client asked for.
	openMetrics bool
	version     string
}

var formatText = expositionFormat{}

// openMetricsVersion is the OpenMetrics version answered when a client names
// none. 0.0.1, which Prometheus also asks for, is the same text.
const openMetricsVersion = "1.0.0"

func (f expositionFormat) contentType() string {
	if f.openMetrics {
		return "application/openmetrics-text; version=" + f.version + "; charset=utf-8"
	}
	return expositionContentType
}

func (f expositionFormat) appendMetricSet(b []byte, s *model.MetricSet) []byte {
	if f.openMetrics {
		return appendOpenMetrics(b, s)
	}
	return appendMetricSet(b, s)
}

// negotiateFormat chooses the format an Accept header prefers: OpenMetrics
// when it gives application/openmetrics-text, of version 1.0.0 or 0.0.1 or
// none, a higher quality than any type the text format answers (text/plain,
// text/*, */*). Prometheus asks for OpenMetrics first. The text format is
// the answer to anything else: no Accept, a browser's, a tie, or only types
// the exporter does not write, such as Prometheus' protobuf format.
func negotiateFormat(accept string) expositionFormat {
	bestOM, bestText, omVersion := -1.0, -1.0, ""
	for _, entry := range strings.Split(accept, ",") {
		mediaType, params := parseMediaRange(entry)
		q := 1.0
		if raw, ok := params["q"]; ok {
			parsed, err := strconv.ParseFloat(raw, 64)
			if err != nil || parsed < 0 || parsed > 1 {
				continue
			}
			q = parsed
		}
		if q == 0 {
			continue
		}
		switch mediaType {
		case "application/openmetrics-text":
			version := params["version"]
			if version != "" && version != "1.0.0" && version != "0.0.1" {
				continue
			}
			if version == "" {
				version = openMetricsVersion
			}
			if q > bestOM || (q == bestOM && version == openMetricsVersion) {
				bestOM, omVersion = q, version
			}
		case "text/plain", "text/*", "*/*":
			bestText = max(bestText, q)
		}
	}
	if bestOM > bestText {
		return expositionFormat{openMetrics: true, version: omVersion}
	}
	return formatText
}

// parseMediaRange splits one entry of an Accept header into its media type,
// lower-cased, and its parameters, names lower-cased and values unquoted.
func parseMediaRange(entry string) (string, map[string]string) {
	parts := strings.Split(entry, ";")
	params := map[string]string{}
	for _, part := range parts[1:] {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(name))] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return strings.ToLower(strings.TrimSpace(parts[0])), params
}

// appendOpenMetrics appends s in the OpenMetrics text format 1.0.0. It
// differs from the Prometheus text format where OpenMetrics does:
//
//   - A family's series are written together, in the order the family first
//     appears, since OpenMetrics does not allow a family to be interleaved
//     with another.
//   - A counter family is named without _total, and its samples with it: a
//     counter named requests is written as the family requests with samples
//     requests_total, as one named requests_total is (openMetricsCounter).
//   - untyped is written as unknown.
//   - HELP escapes double quotes, as label values do.
//   - A timestamp is in seconds rather than milliseconds.
//   - le and quantile values are written as floats, 1.0 rather than 1.
//   - The answer ends with # EOF.
//   - No two families claim the same name (planOpenMetrics): where they
//     would, the families involved are written as unknown under the names
//     their samples have in the text format.
//   - A family with values OpenMetrics does not allow its type, such as a
//     negative counter (typedValues), is written as unknown in the same way.
//
// In both formats a histogram's buckets and a summary's quantiles are
// written in ascending order (ascendingBuckets).
//
// A _created sample follows a counter, a histogram or a summary series that
// has a creation time (model.Metric.Created), in the family that keeps its
// type; one written as unknown has no such sample. Only the exporter's own
// series have that time, and only when the self-metrics are asked to write
// it (selfcreated.go): the exporter reads counters from targets and does not
// know when they started, and OpenMetrics leaves _created out when it is not
// known.
func appendOpenMetrics(b []byte, s *model.MetricSet) []byte {
	var e expositionWriter
	for _, f := range planOpenMetrics(s) {
		b = append(b, "# TYPE "...)
		b = append(b, f.name...)
		b = append(b, ' ')
		b = append(b, f.typ...)
		b = append(b, '\n')
		if f.help != "" {
			b = append(b, "# HELP "...)
			b = append(b, f.name...)
			b = append(b, ' ')
			b = appendEscaped(b, f.help, true)
			b = append(b, '\n')
		}
		for _, p := range f.parts {
			m := s.Metrics[p.index]
			switch {
			case p.kind == omBuckets && m.Histogram != nil:
				b = e.appendOpenMetricsBuckets(b, f.name, m)
			case p.kind == omSum && m.Histogram != nil:
				b = e.appendOpenMetricsSample(b, f.name, m.Labels, "", "", m.Histogram.Sum, m)
			case p.kind == omCount && m.Histogram != nil:
				b = e.appendOpenMetricsCount(b, f.name, m.Labels, "", "", m.Histogram.Count, m)
			case p.kind == omQuantiles && m.Summary != nil:
				b = e.appendOpenMetricsQuantiles(b, f.name, m)
			case p.kind == omSum && m.Summary != nil:
				b = e.appendOpenMetricsSample(b, f.name, m.Labels, "", "", m.Summary.Sum, m)
			case p.kind == omCount && m.Summary != nil:
				b = e.appendOpenMetricsCount(b, f.name, m.Labels, "", "", m.Summary.Count, m)
			case m.Histogram != nil:
				b = e.appendOpenMetricsHistogram(b, f.name, m)
			case m.Summary != nil:
				b = e.appendOpenMetricsSummary(b, f.name, m)
			default:
				b = e.appendOpenMetricsSample(b, f.sample, m.Labels, "", "", m.Value, m)
			}
			if m.Created != 0 && p.kind == omWhole {
				b = e.appendOpenMetricsCreated(b, f, m)
			}
		}
	}
	return append(b, "# EOF\n"...)
}

// omPartKind is which lines of a series an OpenMetrics family carries.
type omPartKind uint8

const (
	// omWhole is every line of the series: its value, or all the lines of a
	// histogram or a summary.
	omWhole omPartKind = iota
	// omBuckets, omSum, omCount and omQuantiles are one kind of line of a
	// histogram or a summary written as unknown, each in a family of its
	// own named as the lines are.
	omBuckets
	omSum
	omCount
	omQuantiles
)

// omPart is the lines of one series of the metric set that a family carries.
type omPart struct {
	index int
	kind  omPartKind
}

// omFamily is one family of an OpenMetrics answer: its name and type, its
// help, the name of a plain value's sample (name_total for a counter, the
// family name otherwise) and the series it carries.
type omFamily struct {
	name, typ, help, sample string
	parts                   []omPart
}

// planOpenMetrics groups the series of s into OpenMetrics families.
//
// OpenMetrics names a counter family without _total and gives a histogram's
// and a summary's samples suffixes, and every family claims its name and the
// names of the samples its type may have: a counter foo claims foo,
// foo_total and foo_created, a histogram foo claims foo, foo_bucket,
// foo_count, foo_sum and foo_created, a summary foo claims foo, foo_count,
// foo_sum and foo_created, and a gauge or an unknown family only its name. A
// name claimed twice is invalid OpenMetrics, and Prometheus fails the scrape:
// the counter foo_total and the gauge foo, which the text format writes side
// by side, would both be the family foo.
//
// So a family is written in its natural form (above) unless a name it would
// claim is claimed by another family, and then as unknown under the names
// its samples have in the text format, which keeps every sample name as the
// text format has it. Counters give way first: a counter whose claims meet
// another family's is written as the unknown family of its own name, whose
// one sample has that name. Then a histogram or summary whose claims still
// meet another family's is written as one unknown family for each of its
// sample names (foo_bucket, foo_sum and foo_count, or foo, foo_sum and
// foo_count). Gauges and untyped series claim only their own names and keep
// their form. Families written as unknown claim only their sample names,
// which differ from family to family except where the text format itself
// writes two families' samples under one name (the gauge foo_count beside
// the histogram foo); those share one unknown family.
//
// A counter not named _total in the text format is still written with
// _total in its natural form, as OpenMetrics requires of a counter's
// samples. One that has no natural form, the counter named _total, whose
// family would have no name, is written as unknown under its own name from
// the start.
//
// A histogram or summary read without a _sum or a _count (model.Histogram)
// has no such line in either form, and a family that would hold only such
// lines is not written. OpenMetrics has a histogram's _sum and _count
// together or not at all, so a histogram with one of the two, as a target
// may write it in the text format, is no OpenMetrics histogram, and its
// family is written as unknown families from the start as well.
//
// So is every family with a series whose values OpenMetrics does not allow
// its type (typedValues): a counter that is NaN or negative, a histogram
// whose bucket counts fall or whose +Inf bucket is not its _count, a summary
// with a quantile of 1.5. The text format allows them, a target may write
// them, and a strict OpenMetrics parser refuses the whole answer over one of
// them. Written as unknown, the family keeps every series and value, and
// loses only a type it does not meet. That is decided for the family, not
// the series, since a family has one type; a histogram with a label of its
// own named le, whose _sum and _count would carry it and so be no part of
// the series its buckets are, is treated the same. These families are
// unknown before any gives way, so the names they claim are their samples'
// alone and the planning above holds.
func planOpenMetrics(s *model.MetricSet) []*omFamily {
	type textFamily struct {
		name    string
		typ     model.MetricType
		help    string
		indexes []int
		unknown bool
	}
	var (
		order  []*textFamily
		byName = map[string]*textFamily{}
	)
	for i, m := range s.Metrics {
		f := byName[m.Name]
		if f == nil {
			f = &textFamily{name: m.Name, typ: m.Type, help: m.Help}
			if _, _, _, natural := openMetricsCounter(m.Name); m.Type == model.CounterMetricType && !natural {
				f.unknown = true
			}
			byName[m.Name] = f
			order = append(order, f)
		}
		if h := m.Histogram; h != nil && (h.NoSum != h.NoCount || !h.NoSum && hasLabel(m, "le")) {
			f.unknown = true
		}
		if !typedValues(m) {
			f.unknown = true
		}
		f.indexes = append(f.indexes, i)
	}
	claims := func(f *textFamily) []string {
		n := f.name
		switch {
		case f.unknown && f.typ == model.HistogramMetricType:
			return []string{n + "_bucket", n + "_sum", n + "_count"}
		case f.unknown && f.typ == model.SummaryMetricType:
			return []string{n, n + "_sum", n + "_count"}
		case f.unknown:
			return []string{n}
		case f.typ == model.CounterMetricType:
			family, sample, created, _ := openMetricsCounter(n)
			return []string{family, sample, created}
		case f.typ == model.HistogramMetricType:
			return []string{n, n + "_bucket", n + "_count", n + "_sum", n + "_created"}
		case f.typ == model.SummaryMetricType:
			return []string{n, n + "_count", n + "_sum", n + "_created"}
		}
		return []string{n}
	}
	contested := func() map[string]int {
		claimed := map[string]int{}
		for _, f := range order {
			for _, c := range claims(f) {
				claimed[c]++
			}
		}
		return claimed
	}
	giveWay := func(types ...model.MetricType) {
		claimed := contested()
		for _, f := range order {
			if !slices.Contains(types, f.typ) {
				continue
			}
			for _, c := range claims(f) {
				if claimed[c] > 1 {
					f.unknown = true
					break
				}
			}
		}
	}
	giveWay(model.CounterMetricType)
	giveWay(model.HistogramMetricType, model.SummaryMetricType)

	var (
		families []*omFamily
		byFamily = map[string]*omFamily{}
	)
	add := func(name, typ, sample string, f *textFamily, kind omPartKind) {
		out := byFamily[name]
		if out == nil {
			out = &omFamily{name: name, typ: typ, sample: sample, help: f.help}
			byFamily[name] = out
			families = append(families, out)
		} else {
			// Only unknown families written under one sample name meet
			// here; together they are unknown.
			out.typ = "unknown"
			if out.help == "" {
				out.help = f.help
			}
		}
		for _, i := range f.indexes {
			out.parts = append(out.parts, omPart{index: i, kind: kind})
		}
	}
	// addLines is add for one kind of line of a histogram or a summary
	// written as unknown, which only the series that have the line are part
	// of; with none, there is no family.
	addLines := func(name string, f *textFamily, kind omPartKind) {
		with := *f
		with.indexes = nil
		for _, i := range f.indexes {
			if hasOpenMetricsLine(s.Metrics[i], kind) {
				with.indexes = append(with.indexes, i)
			}
		}
		if len(with.indexes) > 0 {
			add(name, "unknown", name, &with, kind)
		}
	}
	for _, f := range order {
		switch {
		case f.unknown && f.typ == model.HistogramMetricType:
			addLines(f.name+"_bucket", f, omBuckets)
			addLines(f.name+"_sum", f, omSum)
			addLines(f.name+"_count", f, omCount)
		case f.unknown && f.typ == model.SummaryMetricType:
			addLines(f.name, f, omQuantiles)
			addLines(f.name+"_sum", f, omSum)
			addLines(f.name+"_count", f, omCount)
		case f.unknown:
			add(f.name, "unknown", f.name, f, omWhole)
		case f.typ == model.CounterMetricType:
			family, sample, _, _ := openMetricsCounter(f.name)
			add(family, "counter", sample, f, omWhole)
		default:
			add(f.name, openMetricsType(f.typ), f.name, f, omWhole)
		}
	}
	return families
}

// hasLabel reports whether a series has a label of its own by a name.
func hasLabel(m model.Metric, name string) bool {
	_, has := m.Labels[name]
	return has
}

// hasOpenMetricsLine reports whether a histogram or a summary has the kind
// of line: its _sum or _count, which the source it was read from may not have
// had, a quantile, or a bucket, which a histogram read with neither a +Inf
// bucket nor a _count may have none of.
func hasOpenMetricsLine(m model.Metric, kind omPartKind) bool {
	switch {
	case kind == omBuckets && m.Histogram != nil:
		_, inf := m.Histogram.InfBucket()
		return inf || len(m.Histogram.Buckets) > 0
	case kind == omSum && m.Histogram != nil:
		return !m.Histogram.NoSum
	case kind == omCount && m.Histogram != nil:
		return !m.Histogram.NoCount
	case kind == omSum && m.Summary != nil:
		return !m.Summary.NoSum
	case kind == omCount && m.Summary != nil:
		return !m.Summary.NoCount
	case kind == omQuantiles && m.Summary != nil:
		return len(m.Summary.Quantiles) > 0
	}
	return true
}

// openMetricsCounter is the natural OpenMetrics form of the counter the text
// format names name: its family, named without _total, its sample, named
// with it, and the _created sample the family also claims. natural is false
// for a counter that has no such form: the one named _total, whose family
// would have no name.
//
// The suffix is the plain text _total whatever the name is, also for one
// escaped by name_escaping values (transform/nameescaping.go), where the
// _total of the original name reads __total: my.requests_total, exported as
// U__my_2e_requests__total, is the family U__my_2e_requests_ with the sample
// U__my_2e_requests__total, and my.errors, exported as U__my_2e_errors, is
// the family U__my_2e_errors with the sample U__my_2e_errors_total. That is
// how Prometheus writes an escaped counter, and it is the one form in which a
// reader of OpenMetrics, which knows a counter's sample as its family and
// _total, finds the sample in its family: a suffix written in escaped form,
// __total, gave a strict parser, and this exporter's own, a counter family
// without samples and a sample of no family beside it.
func openMetricsCounter(name string) (family, sample, created string, natural bool) {
	family = strings.TrimSuffix(name, "_total")
	if family == "" {
		return name, name, name, false
	}
	return family, family + "_total", family + "_created", true
}

func openMetricsType(t model.MetricType) string {
	if t == model.UntypedMetricType || t == "" {
		return "unknown"
	}
	return string(t)
}

func (e *expositionWriter) appendOpenMetricsSample(b []byte, name string, labels map[string]string, extraName, extraValue string, value float64, m model.Metric) []byte {
	b = append(b, name...)
	b = e.appendLabels(b, labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendFloat(b, value, 'g', -1, 64)
	return appendOpenMetricsTimestamp(b, m)
}

func (e *expositionWriter) appendOpenMetricsCount(b []byte, name string, labels map[string]string, extraName, extraValue string, count uint64, m model.Metric) []byte {
	b = append(b, name...)
	b = e.appendLabels(b, labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendUint(b, count, 10)
	return appendOpenMetricsTimestamp(b, m)
}

func (e *expositionWriter) appendOpenMetricsHistogram(b []byte, family string, m model.Metric) []byte {
	b = e.appendOpenMetricsBuckets(b, family+"_bucket", m)
	if !m.Histogram.NoSum {
		b = e.appendOpenMetricsSample(b, family+"_sum", m.Labels, "", "", m.Histogram.Sum, m)
	}
	if !m.Histogram.NoCount {
		b = e.appendOpenMetricsCount(b, family+"_count", m.Labels, "", "", m.Histogram.Count, m)
	}
	return b
}

// appendOpenMetricsBuckets writes a histogram's bucket lines as samples
// named name: the +Inf bucket last, with its own count (model.Histogram), and
// none for a histogram that has none, which is written as unknown families.
func (e *expositionWriter) appendOpenMetricsBuckets(b []byte, name string, m model.Metric) []byte {
	for _, x := range e.ascendingBuckets(m.Histogram.Buckets) {
		if math.IsInf(x.UpperBound, 1) {
			continue
		}
		b = e.appendOpenMetricsCount(b, name, m.Labels, "le", openMetricsFloat(x.UpperBound), x.CumulativeCount, m)
	}
	if inf, has := m.Histogram.InfBucket(); has {
		b = e.appendOpenMetricsCount(b, name, m.Labels, "le", "+Inf", inf, m)
	}
	return b
}

func (e *expositionWriter) appendOpenMetricsSummary(b []byte, family string, m model.Metric) []byte {
	b = e.appendOpenMetricsQuantiles(b, family, m)
	if !m.Summary.NoSum {
		b = e.appendOpenMetricsSample(b, family+"_sum", m.Labels, "", "", m.Summary.Sum, m)
	}
	if !m.Summary.NoCount {
		b = e.appendOpenMetricsCount(b, family+"_count", m.Labels, "", "", m.Summary.Count, m)
	}
	return b
}

// appendOpenMetricsQuantiles writes a summary's quantile lines as samples
// named name.
func (e *expositionWriter) appendOpenMetricsQuantiles(b []byte, name string, m model.Metric) []byte {
	for _, x := range e.ascendingQuantiles(m.Summary.Quantiles) {
		b = e.appendOpenMetricsSample(b, name, m.Labels, "quantile", openMetricsFloat(x.Quantile), x.Value, m)
	}
	return b
}

// appendOpenMetricsCreated writes the _created sample of a series with a
// creation time, after the series' other samples: the family's name with
// _created, which a counter, a histogram and a summary family claim
// (planOpenMetrics), the series' labels, and the time in seconds. Families of
// other types have no such sample, and nothing is written for them, nor for
// a histogram with a label of its own named le or a summary with one named
// quantile, which model.MetricSet.Validate refuses: the sample would carry
// the label, and so be no part of the series its buckets or quantiles are.
func (e *expositionWriter) appendOpenMetricsCreated(b []byte, f *omFamily, m model.Metric) []byte {
	switch f.typ {
	case "counter", "histogram", "summary":
	default:
		return b
	}
	if own := model.SeriesOwnLabel(m); own != "" && hasLabel(m, own) {
		return b
	}
	b = append(b, f.name...)
	b = append(b, "_created"...)
	b = e.appendLabels(b, m.Labels, "", "")
	b = append(b, ' ')
	b = strconv.AppendFloat(b, float64(m.Created)/1000, 'f', -1, 64)
	return appendOpenMetricsTimestamp(b, m)
}

// openMetricsFloat writes an le or quantile value as OpenMetrics' canonical
// float: 1.0 rather than 1, so every writer gives the same label value.
func openMetricsFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// appendOpenMetricsTimestamp ends a sample line: its timestamp, in seconds,
// if it has one, and the newline.
func appendOpenMetricsTimestamp(b []byte, m model.Metric) []byte {
	if m.Timestamp != nil {
		b = append(b, ' ')
		b = strconv.AppendFloat(b, float64(*m.Timestamp)/1000, 'f', -1, 64)
	}
	return append(b, '\n')
}
