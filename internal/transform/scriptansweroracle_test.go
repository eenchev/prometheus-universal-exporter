package transform

import (
	"context"
	"errors"
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// How a script's answer was read before a label a script gave as the empty
// string was left off its series, and before a help, a type or labels of
// another kind than a series has failed a pre-script of a prometheus
// transform (python.go): a python transform's metric (formerPythonMetric)
// and the series a pre-script left for a prometheus transform
// (formerPrometheusFromPython), each as it was. They are the oracle of
// scriptanswerdiff_test.go.

// formerPythonMetric is pythonMetric.metric as it was.
func formerPythonMetric(m pythonMetric) (model.Metric, error) {
	name, ok := m.Name.(string)
	if !ok {
		return model.Metric{}, fmt.Errorf("metric name %s is not a string", showScriptValue(m.Name))
	}
	out := model.Metric{Name: name, Type: model.GaugeMetricType}
	// None for the help or the type is none given, as in metric(...).
	switch help := m.Help.(type) {
	case nil:
	case string:
		out.Help = help
	default:
		return out, fmt.Errorf("metric %q help %s is not a string", name, showScriptValue(m.Help))
	}
	switch kind := m.Type.(type) {
	case nil:
	case string:
		if kind != "" {
			out.Type = model.MetricType(kind)
		}
	default:
		return out, fmt.Errorf(`metric %q type %s is not a string; give "gauge", "counter" or "untyped"`, name, showScriptValue(m.Type))
	}
	switch labels := m.Labels.(type) {
	case nil:
	case map[string]any:
		if len(labels) > 0 {
			out.Labels = make(map[string]string, len(labels))
		}
		for label, value := range labels {
			// None leaves the label out, as metric(...) does.
			if value == nil {
				continue
			}
			text, err := labelText(pythonFloats(value))
			if err != nil {
				label, err = firstUnreadableLabel(labels, pythonFloats)
				return out, fmt.Errorf("metric %q label %q %w", name, label, err)
			}
			out.Labels[label] = text
		}
	default:
		return out, fmt.Errorf("metric %q labels are %s, not a mapping of label names to values", name, showScriptValue(m.Labels))
	}
	value, err := pythonNumber(m.Value)
	if m.Value == nil {
		return out, fmt.Errorf("metric %q value %w", name, err)
	}
	if err != nil {
		return out, fmt.Errorf("metric %q value %s %w", name, model.ShowValue(m.Value), err)
	}
	out.Value = value
	if m.Timestamp != nil {
		at, err := pythonNumber(m.Timestamp)
		if err != nil {
			return out, fmt.Errorf("metric %q timestamp %s is not a number of milliseconds", name, model.ShowValue(m.Timestamp))
		}
		ms, err := timestampMillis(at)
		if err != nil {
			return out, fmt.Errorf("metric %q timestamp %w", name, err)
		}
		out.Timestamp = &ms
	}
	return out, nil
}

// formerPythonSeries is the series of a python transform's answer as they
// were made of the maps encoding/json read it into (oraclePythonAnswer).
func formerPythonSeries(ctx context.Context, out *pythonOutput) (*model.MetricSet, error) {
	if err := takeSeriesN(ctx, len(out.Metrics)); err != nil {
		return nil, err
	}
	set := &model.MetricSet{Metrics: make([]model.Metric, 0, len(out.Metrics))}
	for i, raw := range out.Metrics {
		emitted, err := pythonMetricFrom(i, raw)
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", err), model.ErrScriptFailed)
		}
		metric, err := formerPythonMetric(emitted)
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", err), model.ErrScriptFailed)
		}
		set.Metrics = append(set.Metrics, metric)
	}
	return set, nil
}

// formerPrometheusFromPython is prometheusFromPython as it was.
func formerPrometheusFromPython(data any) (model.MetricSet, error) {
	document, ok := data.(map[string]any)
	list, listed := document["metrics"].([]any)
	if !ok || !listed {
		return model.MetricSet{}, errors.New(`a pre-script of a prometheus transform must leave data as {"metrics": [...]}, the series it was given, changed or not`)
	}
	set := model.MetricSet{Metrics: make([]model.Metric, 0, len(list))}
	for i, raw := range list {
		series, ok := raw.(map[string]any)
		if !ok {
			return model.MetricSet{}, model.Errorf("data[\"metrics\"][%d] is not a mapping", model.Position(i))
		}
		m, err := formerPrometheusSeries(series)
		if err != nil {
			return model.MetricSet{}, model.Errorf("data[\"metrics\"][%d]: %w", model.Position(i), err)
		}
		set.Metrics = append(set.Metrics, m)
	}
	return set, nil
}

// formerPrometheusSeries is prometheusSeries as it was.
func formerPrometheusSeries(series map[string]any) (model.Metric, error) {
	name, _ := series["name"].(string)
	if name == "" {
		return model.Metric{}, errors.New("has no name")
	}
	m := model.Metric{Name: name, Type: model.UntypedMetricType}
	if kind, ok := series["type"].(string); ok && kind != "" {
		m.Type = model.MetricType(kind)
	}
	m.Help, _ = series["help"].(string)
	if labels, ok := series["labels"].(map[string]any); ok && len(labels) > 0 {
		m.Labels = make(map[string]string, len(labels))
		for key, value := range labels {
			if value == nil {
				continue
			}
			text, err := labelText(value)
			if err != nil {
				key, err = firstUnreadableLabel(labels, func(value any) any { return value })
				return m, fmt.Errorf("%s label %q %w", name, key, err)
			}
			m.Labels[key] = text
		}
	}
	if at, ok := series["timestamp"]; ok && at != nil {
		value, err := pythonNumber(at)
		if err != nil {
			return m, fmt.Errorf("%s timestamp %s is not a number of milliseconds", name, model.ShowValue(at))
		}
		ms, err := timestampMillis(value)
		if err != nil {
			return m, fmt.Errorf("%s timestamp %w", name, err)
		}
		m.Timestamp = &ms
	}
	number := func(key string) (float64, error) {
		value, err := pythonNumber(series[key])
		if err != nil || series[key] == nil {
			return 0, fmt.Errorf("%s %s %s is not a number", name, key, model.ShowValue(series[key]))
		}
		return value, nil
	}
	// sumAndCount reads the series' sum and count, and whether it has
	// neither: a key that is missing or None.
	sumAndCount := func() (sum float64, n uint64, noSum, noCount bool, err error) {
		if noSum = series["sum"] == nil; !noSum {
			if sum, err = number("sum"); err != nil {
				return 0, 0, false, false, err
			}
		}
		if noCount = series["count"] == nil; !noCount {
			total, err := number("count")
			if err != nil {
				return 0, 0, false, false, err
			}
			if n, err = observationCount(total); err != nil {
				return 0, 0, false, false, fmt.Errorf("%s count %w", name, err)
			}
		}
		return sum, n, noSum, noCount, nil
	}
	switch m.Type {
	case model.HistogramMetricType:
		buckets, _ := series["buckets"].([]any)
		sum, n, noSum, noCount, err := sumAndCount()
		if err != nil {
			return m, err
		}
		m.Histogram = &model.Histogram{Sum: sum, Count: n, NoSum: noSum, NoCount: noCount}
		for _, raw := range buckets {
			b, _ := raw.(map[string]any)
			le, errLe := pythonNumber(b["le"])
			c, errCount := pythonNumber(b["count"])
			if b == nil || b["le"] == nil || errLe != nil || errCount != nil {
				return m, fmt.Errorf("%s has a bucket that is not {\"le\": <number>, \"count\": <number>}", name)
			}
			cumulative, err := observationCount(c)
			if err != nil {
				return m, fmt.Errorf("%s bucket count %w", name, err)
			}
			m.Histogram.Buckets = append(m.Histogram.Buckets, model.Bucket{UpperBound: le, CumulativeCount: cumulative})
		}
		if len(buckets) == 0 && noSum && noCount {
			return m, emptySeriesError("histogram", name, "buckets", series)
		}
		if err := m.Histogram.Settle(); err != nil {
			return m, fmt.Errorf("the histogram %s %w", name, err)
		}
	case model.SummaryMetricType:
		quantiles, _ := series["quantiles"].([]any)
		sum, n, noSum, noCount, err := sumAndCount()
		if err != nil {
			return m, err
		}
		m.Summary = &model.Summary{Sum: sum, Count: n, NoSum: noSum, NoCount: noCount}
		for _, raw := range quantiles {
			q, _ := raw.(map[string]any)
			quantile, errQ := pythonNumber(q["quantile"])
			value, errV := pythonNumber(q["value"])
			if q == nil || q["quantile"] == nil || errQ != nil || errV != nil {
				return m, fmt.Errorf("%s has a quantile that is not {\"quantile\": <number>, \"value\": <number>}", name)
			}
			m.Summary.Quantiles = append(m.Summary.Quantiles, model.Quantile{Quantile: quantile, Value: value})
		}
		if len(quantiles) == 0 && noSum && noCount {
			return m, emptySeriesError("summary", name, "quantiles", series)
		}
		if err := m.Summary.Settle(); err != nil {
			return m, fmt.Errorf("the summary %s %w", name, err)
		}
	default:
		value, err := number("value")
		if err != nil {
			return m, err
		}
		m.Value = value
	}
	return m, nil
}
