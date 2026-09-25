package exporter

import (
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
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

// writeMetricSet answers with the exposition text of s. Label values and help
// come from scraped targets, so the answer carries text a target chose; it is
// served as text/plain with nosniff, so a browser shows it as text and never
// renders it as HTML.
func writeMetricSet(w http.ResponseWriter, s *model.MetricSet) {
	w.Header().Set("Content-Type", expositionContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	pooled := expositionBuffers.Get().(*[]byte)
	b := appendMetricSet((*pooled)[:0], s)
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
	keys []string
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

func (e *expositionWriter) appendHistogram(b []byte, m model.Metric) []byte {
	for _, x := range m.Histogram.Buckets {
		// The +Inf bucket is written below from the count. A histogram decoded
		// from a Prometheus source carries its own +Inf bucket, which written
		// here as well would be a duplicate series.
		if math.IsInf(x.UpperBound, 1) {
			continue
		}
		b = e.appendCount(b, m.Name, "_bucket", m.Labels, "le", strconv.FormatFloat(x.UpperBound, 'g', -1, 64), x.CumulativeCount, m)
	}
	b = e.appendCount(b, m.Name, "_bucket", m.Labels, "le", "+Inf", m.Histogram.Count, m)
	b = e.appendSample(b, m.Name, "_sum", m.Labels, "", "", m.Histogram.Sum, m)
	return e.appendCount(b, m.Name, "_count", m.Labels, "", "", m.Histogram.Count, m)
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

func (e *expositionWriter) appendSummary(b []byte, m model.Metric) []byte {
	for _, x := range m.Summary.Quantiles {
		b = e.appendSample(b, m.Name, "", m.Labels, "quantile", strconv.FormatFloat(x.Quantile, 'g', -1, 64), x.Value, m)
	}
	b = e.appendSample(b, m.Name, "_sum", m.Labels, "", "", m.Summary.Sum, m)
	return e.appendCount(b, m.Name, "_count", m.Labels, "", "", m.Summary.Count, m)
}
