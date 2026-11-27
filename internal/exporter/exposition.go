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
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// probeError is the body of a probe that failed because a metric rule with
// error_mode fail could not produce its value. Prometheus only looks at the
// status code, which already fails the scrape. The body is for a person who
// runs the probe by hand to find out why. So it names the collector, the rule
// and the error, instead of leaving them to be dug out of the exporter's log.
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

// expositionBuffers keeps the buffers that answers are rendered into, so a
// busy endpoint does not allocate and grow a buffer for every answer. A buffer
// that grew past maxPooledExposition is left to the garbage collector, so it
// is not kept forever for one large answer.
var expositionBuffers = sync.Pool{New: func() any { b := make([]byte, 0, 16<<10); return &b }}

const maxPooledExposition = 4 << 20

// writeMetricSet answers r with s, in the format that r's Accept header
// prefers (negotiateFormat): the Prometheus text format or OpenMetrics. Label
// values and help come from scraped targets, so the answer carries text that a
// target chose. It is served as plain text with nosniff, so a browser shows it
// as text and never renders it as HTML. r may be nil; the answer is then in
// the text format.
//
// utf8Names says that the set may hold a non-classic name, from a collector
// whose metric_name_validation_scheme is utf8. Only then are its names
// checked, and such a name is escaped as the scraper asks (nameescaping.go).
// The names of any other set are classic, because they were validated so.
func writeMetricSet(w http.ResponseWriter, r *http.Request, s *model.MetricSet, utf8Names bool) {
	format := formatText
	if r != nil {
		format = negotiateFormat(r.Header.Get("Accept"))
	}
	if !utf8Names || !hasUTF8Names(s) {
		// Classic names alone: no escaping applies, and the answer is
		// written and typed as it always was (nameescaping.go).
		format.escaping = ""
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
// verbose self-metrics are rendered by the same code as collector output. It
// writes with strconv's append functions instead of fmt, and keeps one slice
// for sorting the label names of every series, so rendering allocates little
// beyond the text itself.
//
// The text format wants all lines of a metric in one group, under its HELP
// and TYPE, and nearly every set is already grouped. The series are written in
// the set's order, and a name that differs from the previous one is looked up
// once. If that name was written already, with another metric's series since,
// the set has a metric's series apart. A script may emit them so, a target
// that a pass-through reads may write them so, and two rules of one name with
// another rule between them produce them so. The written text is then dropped
// and the set is written again metric by metric (appendGroupedMetricSet).
// Only such a set pays this cost.
func appendMetricSet(b []byte, s *model.MetricSet) []byte {
	var e expositionWriter
	return e.appendMetricSet(b, s)
}

func (e *expositionWriter) appendMetricSet(b []byte, s *model.MetricSet) []byte {
	described := map[string]bool{}
	start, last := len(b), ""
	for _, m := range s.Metrics {
		if m.Name != last {
			if described[m.Name] {
				return e.appendGroupedMetricSet(b[:start], s)
			}
			b = e.appendFamilyLines(b, m)
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

// appendFamilyLines writes the HELP line, if the metric has help, and the
// TYPE line of the metric m is the first series of.
func (e *expositionWriter) appendFamilyLines(b []byte, m model.Metric) []byte {
	if m.Help != "" {
		b = append(b, "# HELP "...)
		b = e.appendName(b, m.Name)
		b = append(b, ' ')
		b = appendEscaped(b, m.Help, false)
		b = append(b, '\n')
	}
	b = append(b, "# TYPE "...)
	b = e.appendName(b, m.Name)
	b = append(b, ' ')
	b = append(b, m.Type...)
	return append(b, '\n')
}

// appendGroupedMetricSet is appendMetricSet for a set that has a metric's
// series apart. It writes the metrics in the order each first appears in the
// set. Each metric has its series together, in the set's order, under the HELP
// and TYPE of its first one. Nothing is sorted, so a set that has every
// metric's series together is written as appendMetricSet writes it.
func (e *expositionWriter) appendGroupedMetricSet(b []byte, s *model.MetricSet) []byte {
	last := ""
	for _, i := range seriesByFamily(s) {
		m := s.Metrics[i]
		if m.Name != last {
			b = e.appendFamilyLines(b, m)
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

// seriesByFamily is the places of a set's series, metric by metric: the
// metrics in the order each first appears, and the series of each in the
// order the set has them.
func seriesByFamily(s *model.MetricSet) []int {
	// family maps each name to its metric, in the order the names first
	// appear. starts holds where each metric's series begin in the returned
	// order: first the number of its series while they are counted, then the
	// sum of the counts before it.
	family := map[string]int{}
	var starts []int
	for _, m := range s.Metrics {
		f, known := family[m.Name]
		if !known {
			f = len(starts)
			family[m.Name] = f
			starts = append(starts, 0)
		}
		starts[f]++
	}
	total := 0
	for f, count := range starts {
		starts[f] = total
		total += count
	}
	order := make([]int, len(s.Metrics))
	for i, m := range s.Metrics {
		f := family[m.Name]
		order[starts[f]] = i
		starts[f]++
	}
	return order
}

// expositionWriter holds what rendering reuses from series to series.
type expositionWriter struct {
	// quoted writes a name that is not classic in Prometheus 3's quoted
	// syntax, as a scraper asking for escaping=allow-utf-8 reads it
	// (nameescaping.go); without it every name is written as it is.
	quoted    bool
	keys      []string
	buckets   []model.Bucket
	quantiles []model.Quantile
}

// appendSample writes one sample line: name and suffix, the labels with
// extraName set to extraValue when extraName is not empty, the value and the
// metric's timestamp.
func (e *expositionWriter) appendSample(b []byte, name, suffix string, labels map[string]string, extraName, extraValue string, value float64, m model.Metric) []byte {
	b = e.appendHead(b, name, suffix, labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendFloat(b, value, 'g', -1, 64)
	return appendTimestamp(b, m)
}

// appendCount is appendSample for a count, written as an integer.
func (e *expositionWriter) appendCount(b []byte, name, suffix string, labels map[string]string, extraName, extraValue string, count uint64, m model.Metric) []byte {
	b = e.appendHead(b, name, suffix, labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendUint(b, count, 10)
	return appendTimestamp(b, m)
}

// appendHead writes what a sample line has before its value: the name and
// suffix, and the labels. A name that is not classic, written quoted, goes
// inside the braces, first: {"http.server.duration_bucket",le="0.1"}.
func (e *expositionWriter) appendHead(b []byte, name, suffix string, labels map[string]string, extraName, extraValue string) []byte {
	if !e.quoted || model.ValidMetricName(name) {
		b = append(b, name...)
		b = append(b, suffix...)
		return e.appendLabels(b, labels, extraName, extraValue)
	}
	b = append(b, '{', '"')
	b = appendEscaped(b, name, true)
	b = appendEscaped(b, suffix, true)
	b = append(b, '"')
	b = e.appendLabelsAfter(b, labels, extraName, extraValue, true)
	return append(b, '}')
}

// appendName writes a family's name in a HELP or TYPE line: quoted when it
// is not classic and the writer quotes.
func (e *expositionWriter) appendName(b []byte, name string) []byte {
	if !e.quoted || model.ValidMetricName(name) {
		return append(b, name...)
	}
	b = append(b, '"')
	b = appendEscaped(b, name, true)
	return append(b, '"')
}

// appendLabels writes {name="value",...} in name order, with extraName, a
// histogram's le or a summary's quantile, set to extraValue over any label
// of that name, and nothing for no labels.
func (e *expositionWriter) appendLabels(b []byte, labels map[string]string, extraName, extraValue string) []byte {
	return e.appendLabelsAfter(b, labels, extraName, extraValue, false)
}

// appendLabelsAfter is appendLabels, or, after a quoted name inside the
// braces, the labels each after a comma and without the braces.
func (e *expositionWriter) appendLabelsAfter(b []byte, labels map[string]string, extraName, extraValue string, afterName bool) []byte {
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
	if !afterName {
		b = append(b, '{')
	}
	for i, k := range keys {
		if i > 0 || afterName {
			b = append(b, ',')
		}
		value := labels[k]
		if extraName != "" && k == extraName {
			value = extraValue
		}
		if e.quoted && !model.ValidLabelName(k) {
			b = append(b, '"')
			b = appendEscaped(b, k, true)
			b = append(b, '"')
		} else {
			b = append(b, k...)
		}
		b = append(b, '=', '"')
		b = appendEscaped(b, value, true)
		b = append(b, '"')
	}
	if afterName {
		return b
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

// appendHistogram writes a histogram's lines. Its buckets are written in
// ascending order of their bounds, whatever order the source had
// (ascendingBuckets). Its _sum and _count are written unless the source it was
// read from had none (model.Histogram), so a histogram goes out as it came in,
// without a sum or count of 0 it never had. The +Inf bucket and the _count each
// get the number the source gave, even when the two differ. A histogram with a
// _count and no +Inf bucket is given the bucket; one with neither gets neither.
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
// upper bounds, the order both formats are written in. A target may write its
// buckets in any order, since the Prometheus text format requires none. But
// OpenMetrics, and a reader that takes the first bucket a value fits in, need
// them ascending. A bound that is not a number comes first, and +Inf, which is
// written from the count, comes last. Buckets nearly always arrive in order and
// are then returned as they are. Others are sorted in the writer's own copy,
// because the metric set is shared with the cache.
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

// typedValues reports whether the values of m are allowed for a series of its
// type in OpenMetrics, and in OTLP, which follows it. The Prometheus text
// format sets no such rules. So a target may write, and the exporter passes
// on, values that a strict OpenMetrics parser refuses, failing the scrape:
//
//   - a counter that is NaN or negative;
//   - a histogram whose _sum is NaN or negative, or is present beside a bucket
//     with a negative bound (which makes it no counter); one with a bound that
//     is not a number; one whose bucket counts fall from one bound to the next
//     higher one, or to its +Inf bucket's; one whose +Inf bucket and _count
//     differ, as when a target is read between updating the one and the other
//     (model.Histogram); and one with neither of the two, which has no +Inf
//     bucket;
//   - a summary whose _sum is NaN or negative, with a quantile outside 0 to 1,
//     or with a negative value for a quantile. NaN is a quantile's value when
//     nothing was observed, and is allowed.
//
// A gauge and an untyped series may have any value. A count is unsigned, so
// it is never NaN or negative. A family with such a series is written as
// unknown in OpenMetrics (planOpenMetrics) and exported as gauges over OTLP
// (otlpMetrics), with the same series and values, instead of being left out
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
	// escaping is how a name that is not classic is written, as the scraper
	// asked (nameescaping.go): allow-utf-8, underscores, dots or values; ""
	// for an answer of classic names alone, which says none.
	escaping string
}

var formatText = expositionFormat{}

// openMetricsVersion is the OpenMetrics version answered when a client names
// none. 0.0.1, which Prometheus also asks for, is the same text.
const openMetricsVersion = "1.0.0"

func (f expositionFormat) contentType() string {
	contentType := expositionContentType
	if f.openMetrics {
		contentType = "application/openmetrics-text; version=" + f.version + "; charset=utf-8"
	}
	if f.escaping != "" {
		contentType += "; escaping=" + f.escaping
	}
	return contentType
}

func (f expositionFormat) appendMetricSet(b []byte, s *model.MetricSet) []byte {
	var e expositionWriter
	switch f.escaping {
	case "":
	case transform.NameEscapingAllowUTF8:
		e.quoted = true
	default:
		s = escapedSet(s, f.escaping)
	}
	if f.openMetrics {
		return e.appendOpenMetrics(b, s)
	}
	return e.appendMetricSet(b, s)
}

// negotiateFormat chooses the format that an Accept header prefers. It is
// OpenMetrics when the header gives application/openmetrics-text (version
// 1.0.0, 0.0.1 or none) a higher quality than any type the text format
// answers (text/plain, text/*, */*). Prometheus asks for OpenMetrics first.
// Anything else gets the text format: no Accept, a browser's, a tie, or only
// types the exporter does not write, such as Prometheus' protobuf format.
func negotiateFormat(accept string) expositionFormat {
	a := acceptQualities(accept)
	if a.om > a.text {
		return expositionFormat{openMetrics: true, version: a.omVersion, escaping: answerEscaping(a.omEscaping)}
	}
	return expositionFormat{escaping: answerEscaping(a.textEscaping)}
}

// acceptance is what an Accept header asks of the text formats. It holds the
// highest quality given to OpenMetrics of a version the exporter writes, with
// the version and escaping that media range asks for. It also holds the
// highest quality given to a type the text format answers, with that range's
// escaping. A quality is -1 where the header gives none.
type acceptance struct {
	om, text                            float64
	omVersion, omEscaping, textEscaping string
}

// acceptQualities is an Accept header's acceptance.
func acceptQualities(accept string) acceptance {
	a := acceptance{om: -1, text: -1}
	for entry := range strings.SplitSeq(accept, ",") {
		mediaType, params := parseMediaRange(entry)
		q, ok := acceptQuality(params)
		if !ok || q == 0 {
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
			if q > a.om || (q == a.om && version == openMetricsVersion) {
				a.om, a.omVersion, a.omEscaping = q, version, params["escaping"]
			}
		case "text/plain", "text/*", "*/*":
			if q > a.text {
				a.text, a.textEscaping = q, params["escaping"]
			}
		}
	}
	return a
}

// acceptQuality is a media range's q parameter, 1 when it has none; false
// for one that is not a number from 0 to 1, which leaves the range out.
func acceptQuality(params map[string]string) (float64, bool) {
	raw, ok := params["q"]
	if !ok {
		return 1, true
	}
	q, err := strconv.ParseFloat(raw, 64)
	if err != nil || q < 0 || q > 1 {
		return 0, false
	}
	return q, true
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
// A _created sample follows a counter, histogram or summary series that has a
// creation time (model.Metric.Created), in a family that keeps its type. A
// series written as unknown has no such sample. Only the exporter's own series
// have that time, and only when the self-metrics are asked to write it
// (selfcreated.go). The exporter reads counters from targets and does not know
// when they started, and OpenMetrics leaves _created out when it is not known.
func appendOpenMetrics(b []byte, s *model.MetricSet) []byte {
	var e expositionWriter
	return e.appendOpenMetrics(b, s)
}

func (e *expositionWriter) appendOpenMetrics(b []byte, s *model.MetricSet) []byte {
	for _, f := range planOpenMetrics(s) {
		b = append(b, "# TYPE "...)
		b = e.appendName(b, f.name)
		b = append(b, ' ')
		b = append(b, f.typ...)
		b = append(b, '\n')
		if f.help != "" {
			b = append(b, "# HELP "...)
			b = e.appendName(b, f.name)
			b = append(b, ' ')
			b = appendEscaped(b, f.help, true)
			b = append(b, '\n')
		}
		for _, p := range f.parts {
			m := s.Metrics[p.index]
			switch {
			case p.kind == omBuckets && m.Histogram != nil:
				// The buckets of a histogram written as unknown: OpenMetrics
				// gives an unknown family's samples no exemplars.
				b = e.appendOpenMetricsBucketLines(b, f.name, m, false)
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
				// A counter's exemplar follows its sample; no other family
				// of one sample has exemplars in OpenMetrics.
				if f.typ == "counter" {
					b = appendOpenMetricsExemplar(b, m.Exemplar)
				}
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
// OpenMetrics names a counter family without _total, and gives the samples of
// a histogram and a summary suffixes. Every family claims its name and the
// names of the samples its type may have:
//   - a counter foo claims foo, foo_total and foo_created;
//   - a histogram foo claims foo, foo_bucket, foo_count, foo_sum and
//     foo_created;
//   - a summary foo claims foo, foo_count, foo_sum and foo_created;
//   - a gauge or an unknown family claims only its name.
//
// A name claimed twice is invalid OpenMetrics, and Prometheus fails the
// scrape. For example, the counter foo_total and the gauge foo, which the text
// format writes side by side, would both be the family foo.
//
// So a family is written in its natural form (above), unless another family
// claims a name it would claim. It is then written as unknown under the names
// its samples have in the text format, which keeps every sample name as the
// text format has it. Counters give way first: a counter whose claims clash
// with another family's is written as the unknown family of its own name,
// with one sample of that name. Then a histogram or summary whose claims still
// clash is written as one unknown family per sample name (foo_bucket, foo_sum
// and foo_count, or foo, foo_sum and foo_count). Gauges and untyped series
// claim only their own names and keep their form. Families written as unknown
// claim only their sample names. These differ between families, except where
// the text format itself writes the samples of two families under one name
// (the gauge foo_count beside the histogram foo); those share one unknown
// family.
//
// A counter not named _total in the text format is still written with _total
// in its natural form, as OpenMetrics requires of a counter's samples. The
// counter named _total has no natural form, since its family would have no
// name. It is written as unknown under its own name from the start.
//
// A histogram or summary read without a _sum or a _count (model.Histogram)
// has no such line in either form, and a family that would hold only such
// lines is not written. OpenMetrics has a histogram's _sum and _count together
// or not at all. So a histogram with one of the two, as a target may write it
// in the text format, is not an OpenMetrics histogram. Its family is also
// written as unknown families from the start.
//
// The same holds for every family with a series whose values OpenMetrics does
// not allow for its type (typedValues): a counter that is NaN or negative, a
// histogram whose bucket counts fall or whose +Inf bucket is not its _count,
// a summary with a quantile of 1.5. The text format allows them, a target may
// write them, and a strict OpenMetrics parser refuses the whole answer over
// one of them. Written as unknown, the family keeps every series and value,
// and loses only a type it does not meet. This is decided per family, not per
// series, since a family has one type. A histogram with its own label named
// le is treated the same: its _sum and _count would carry that label, and so
// would not be part of the series its buckets are. These families are unknown
// before any family gives way, so they claim only their samples' names, and
// the planning above holds.
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

// hasOpenMetricsLine reports whether a histogram or a summary has the kind of
// line: its _sum or _count (which the source it was read from may not have
// had), a quantile, or a bucket (a histogram read with neither a +Inf bucket
// nor a _count may have none).
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

// openMetricsCounter is the natural OpenMetrics form of the counter that the
// text format names name: its family, named without _total, its sample, named
// with _total, and the _created sample the family also claims. natural is
// false for a counter that has no such form: the one named _total, whose
// family would have no name.
//
// The suffix is the plain text _total whatever the name is. This also holds
// for a name escaped by name_escaping values (transform/nameescaping.go),
// where the _total of the original name reads __total. For example,
// my.requests_total, exported as U__my_2e_requests__total, is the family
// U__my_2e_requests_ with the sample U__my_2e_requests__total. my.errors,
// exported as U__my_2e_errors, is the family U__my_2e_errors with the sample
// U__my_2e_errors_total. This is how Prometheus writes an escaped counter. It
// is also the only form in which an OpenMetrics reader, which knows a
// counter's sample as its family plus _total, finds the sample in its family.
// A suffix written in escaped form, __total, gave a strict parser, and this
// exporter's own, a counter family without samples and beside it a sample of
// no family.
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
	b = e.appendHead(b, name, "", labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendFloat(b, value, 'g', -1, 64)
	return appendOpenMetricsTimestamp(b, m)
}

func (e *expositionWriter) appendOpenMetricsCount(b []byte, name string, labels map[string]string, extraName, extraValue string, count uint64, m model.Metric) []byte {
	b = e.appendHead(b, name, "", labels, extraName, extraValue)
	b = append(b, ' ')
	b = strconv.AppendUint(b, count, 10)
	return appendOpenMetricsTimestamp(b, m)
}

func (e *expositionWriter) appendOpenMetricsHistogram(b []byte, family string, m model.Metric) []byte {
	b = e.appendOpenMetricsBucketLines(b, family+"_bucket", m, true)
	if !m.Histogram.NoSum {
		b = e.appendOpenMetricsSample(b, family+"_sum", m.Labels, "", "", m.Histogram.Sum, m)
	}
	if !m.Histogram.NoCount {
		b = e.appendOpenMetricsCount(b, family+"_count", m.Labels, "", "", m.Histogram.Count, m)
	}
	return b
}

// appendOpenMetricsBucketLines writes a histogram's bucket lines as samples
// named name. The +Inf bucket comes last, with its own count
// (model.Histogram). A histogram that has none gets none, and is written as
// unknown families. With exemplars, a bucket's exemplar follows its count
// (appendOpenMetricsExemplar): the exporter's own durations', or a target's.
// The +Inf bucket's is the histogram's InfExemplar or, for a histogram read
// from a target, the one its +Inf bucket had.
func (e *expositionWriter) appendOpenMetricsBucketLines(b []byte, name string, m model.Metric, exemplars bool) []byte {
	infExemplar := m.Histogram.InfExemplar
	for _, x := range e.ascendingBuckets(m.Histogram.Buckets) {
		if math.IsInf(x.UpperBound, 1) {
			if infExemplar == nil {
				infExemplar = x.Exemplar
			}
			continue
		}
		b = e.appendOpenMetricsCount(b, name, m.Labels, "le", openMetricsFloat(x.UpperBound), x.CumulativeCount, m)
		if exemplars {
			b = appendOpenMetricsExemplar(b, x.Exemplar)
		}
	}
	if inf, has := m.Histogram.InfBucket(); has {
		b = e.appendOpenMetricsCount(b, name, m.Labels, "le", "+Inf", inf, m)
		if exemplars {
			b = appendOpenMetricsExemplar(b, infExemplar)
		}
	}
	return b
}

// appendOpenMetricsExemplar writes x, if there is one, on the line that b ends
// with. OpenMetrics writes an exemplar after a bucket's count or a counter's
// value and timestamp: " # {trace_id="…",span_id="…"} value timestamp", with
// the timestamp in seconds, and none for an exemplar a target wrote without
// one. The exporter's own are 32 and 16 hex digits under their names, 63
// characters, within the 128 an exemplar's labels may have, which a target's
// were held to when it was read. The Prometheus text format has no
// exemplars, so none is written there.
func appendOpenMetricsExemplar(b []byte, x *model.Exemplar) []byte {
	if x == nil {
		return b
	}
	b = b[:len(b)-1]
	b = append(b, ` # {`...)
	for i, pair := range x.Pairs() {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, pair[0]...)
		b = append(b, `="`...)
		b = appendEscaped(b, pair[1], true)
		b = append(b, '"')
	}
	b = append(b, `} `...)
	b = strconv.AppendFloat(b, x.Value, 'g', -1, 64)
	if !x.Time.IsZero() {
		b = append(b, ' ')
		b = strconv.AppendFloat(b, float64(x.Time.UnixMilli())/1000, 'f', -1, 64)
	}
	return append(b, '\n')
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
// creation time, after the series' other samples. The sample has the family's
// name with _created (claimed by counter, histogram and summary families, see
// planOpenMetrics), the series' labels, and the time in seconds. Families of
// other types have no such sample, and nothing is written for them. Nothing is
// written either for a histogram with its own label named le, or a summary
// with one named quantile, which model.MetricSet.Validate refuses: the sample
// would carry the label, and so not be part of the series of its buckets or
// quantiles.
func (e *expositionWriter) appendOpenMetricsCreated(b []byte, f *omFamily, m model.Metric) []byte {
	switch f.typ {
	case "counter", "histogram", "summary":
	default:
		return b
	}
	if own := model.SeriesOwnLabel(m); own != "" && hasLabel(m, own) {
		return b
	}
	b = e.appendHead(b, f.name, "_created", m.Labels, "", "")
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
