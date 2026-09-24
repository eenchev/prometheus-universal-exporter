package model

import (
	"encoding/json"
	"fmt"
	"regexp"
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

// MetricNameRE matches a classic Prometheus metric name.
var MetricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// LabelNameRE matches a classic Prometheus label name.
var LabelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (m Metric) seriesKey() string {
	keys := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(m.Name)
	for _, k := range keys {
		b.WriteByte('\xff')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m.Labels[k])
	}
	return b.String()
}

// Validate checks the set against the collector's limits and the rules of
// the exposition format: valid names and types, no duplicate series, one type
// per family. It stops at the first problem, which the error describes.
func (s *MetricSet) Validate(l Limits) error {
	seen := map[string]struct{}{}
	types := map[string]MetricType{}
	if l.MaxMetrics > 0 && len(s.Metrics) > l.MaxMetrics {
		return fmt.Errorf("metric count %d exceeds limit %d", len(s.Metrics), l.MaxMetrics)
	}
	for i := range s.Metrics {
		m := &s.Metrics[i]
		if !MetricNameRE.MatchString(m.Name) {
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
		// Prometheus accepts infinities and NaN, so no value check applies here.
		if len(m.Labels) > l.MaxLabelsPerMetric && l.MaxLabelsPerMetric > 0 {
			return fmt.Errorf("metric %q has too many labels", m.Name)
		}
		for k, v := range m.Labels {
			if !LabelNameRE.MatchString(k) {
				if k != "" && utf8.ValidString(k) {
					return fmt.Errorf("metric %q has label %q, which is not a classic Prometheus label name; set the collector's name_escaping to underscores or values to export it escaped", m.Name, k)
				}
				return fmt.Errorf("metric %q has invalid label name %q", m.Name, k)
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
		if _, ok := seen[m.seriesKey()]; ok {
			return fmt.Errorf("duplicate metric series %q", m.Name)
		}
		seen[m.seriesKey()] = struct{}{}
	}
	return nil
}

// Number reads a decoded value as a number: any numeric type, a string
// holding one, or a boolean as 1 or 0.
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
		return x.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("%v is not numeric", v)
	}
}

// Normalize rewrites decoded JSON or YAML in place into the shapes the
// transforms expect: maps keyed by string, and a json.Number as a float64,
// or as its text when it is not one.
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
		if n, err := x.Float64(); err == nil {
			return n
		}
		return string(x)
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
