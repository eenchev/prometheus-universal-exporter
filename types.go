package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type MetricType string

const (
	GaugeMetricType     MetricType = "gauge"
	CounterMetricType   MetricType = "counter"
	HistogramMetricType MetricType = "histogram"
	SummaryMetricType   MetricType = "summary"
	UntypedMetricType   MetricType = "untyped"
)

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

type Histogram struct {
	Buckets []Bucket
	Sum     float64
	Count   uint64
}
type Bucket struct {
	UpperBound      float64
	CumulativeCount uint64
}
type Summary struct {
	Quantiles []Quantile
	Sum       float64
	Count     uint64
}
type Quantile struct {
	Quantile float64
	Value    float64
}
type MetricSet struct{ Metrics []Metric }

var metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

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

func (s *MetricSet) Validate(l Limits) error {
	seen := map[string]struct{}{}
	types := map[string]MetricType{}
	if l.MaxMetrics > 0 && len(s.Metrics) > l.MaxMetrics {
		return fmt.Errorf("metric count %d exceeds limit %d", len(s.Metrics), l.MaxMetrics)
	}
	for i := range s.Metrics {
		m := &s.Metrics[i]
		if !metricNameRE.MatchString(m.Name) || len(m.Name) > l.MaxMetricNameLength && l.MaxMetricNameLength > 0 {
			return fmt.Errorf("invalid metric name %q", m.Name)
		}
		switch m.Type {
		case GaugeMetricType, CounterMetricType, HistogramMetricType, SummaryMetricType, UntypedMetricType:
		default:
			return fmt.Errorf("metric %q has invalid type %q", m.Name, m.Type)
		}
		if math.IsInf(m.Value, 0) || math.IsNaN(m.Value) { /* Prometheus accepts these values. */
		}
		if len(m.Labels) > l.MaxLabelsPerMetric && l.MaxLabelsPerMetric > 0 {
			return fmt.Errorf("metric %q has too many labels", m.Name)
		}
		for k, v := range m.Labels {
			if !labelNameRE.MatchString(k) {
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

func metricFromValue(v any, fallback *MetricRule) (Metric, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return Metric{}, fmt.Errorf("metric result must be an object, got %T", v)
	}
	m := Metric{Type: GaugeMetricType, Labels: map[string]string{}}
	if fallback != nil {
		m.Name, m.Type, m.Help = fallback.Name, fallback.Type, fallback.Description
	}
	if x, ok := obj["name"].(string); ok {
		m.Name = x
	}
	if x, ok := obj["type"].(string); ok {
		m.Type = MetricType(x)
	}
	if x, ok := obj["help"].(string); ok {
		m.Help = x
	}
	if x, ok := obj["labels"].(map[string]any); ok {
		for k, v := range x {
			m.Labels[k] = fmt.Sprint(v)
		}
	}
	if x, ok := obj["labels"].(map[string]string); ok {
		m.Labels = x
	}
	if x, ok := obj["value"]; ok {
		n, err := number(x)
		if err != nil {
			return m, err
		}
		m.Value = n
	} else {
		return m, fmt.Errorf("metric %q has no numeric value", m.Name)
	}
	if m.Name == "" {
		return m, fmt.Errorf("metric result has no name")
	}
	return m, nil
}

func number(v any) (float64, error) {
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

func normalize(v any) any {
	switch x := v.(type) {
	case map[any]any:
		m := map[string]any{}
		for k, v := range x {
			m[fmt.Sprint(k)] = normalize(v)
		}
		return m
	case map[string]any:
		for k, v := range x {
			x[k] = normalize(v)
		}
		return x
	case []any:
		for i, v := range x {
			x[i] = normalize(v)
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
