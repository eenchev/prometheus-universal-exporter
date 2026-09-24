package transform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

type pythonInput struct {
	Mode      string         `json:"mode"`
	Script    string         `json:"script"`
	Data      any            `json:"data"`
	Response  pythonResponse `json:"response"`
	Target    string         `json:"target"`
	Collector string         `json:"collector"`
}

type pythonResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	Text       string              `json:"text"`
}

// pythonOutput is one answer from a worker: the metrics a transform emitted or
// the data a pre-script left, or the error the script raised.
type pythonOutput struct {
	OK      bool           `json:"ok"`
	Error   string         `json:"error"`
	Metrics []pythonMetric `json:"metrics"`
	Data    any            `json:"data"`
	Log     string         `json:"log"`
}

// pythonMetric is a metric as a script emitted it. metric(...) makes its
// value a float and its timestamp whole milliseconds, but a script can also
// append to metrics itself, so both are read as whatever they are.
type pythonMetric struct {
	Name      string            `json:"name"`
	Help      string            `json:"help"`
	Type      model.MetricType  `json:"type"`
	Value     any               `json:"value"`
	Labels    map[string]string `json:"labels"`
	Timestamp any               `json:"timestamp"`
}

// A float that is NaN or infinite has no JSON form, and the worker and the
// exporter exchange JSON. Each side writes one as a string no data holds —
// NUL, a marker, the value, NUL — and the other reads it back as the float.
// Going to the worker, the marker is replaced in the encoded line with NaN,
// Infinity or -Infinity, which Python's json reads as floats; coming back,
// the worker writes the marker, and pythonFloats turns it into the float.
const nonFiniteMarker = "\x00pue-nonfinite:"

var nonFiniteValues = map[string]float64{
	nonFiniteMarker + "NaN\x00":  math.NaN(),
	nonFiniteMarker + "+Inf\x00": math.Inf(1),
	nonFiniteMarker + "-Inf\x00": math.Inf(-1),
}

// nonFiniteJSON replaces each marker, as json.Marshal writes it, with the
// token Python's json reads.
var nonFiniteJSON = strings.NewReplacer(
	`"\u0000pue-nonfinite:NaN\u0000"`, "NaN",
	`"\u0000pue-nonfinite:+Inf\u0000"`, "Infinity",
	`"\u0000pue-nonfinite:-Inf\u0000"`, "-Infinity",
)

// withNonFiniteMarkers is data with every NaN or infinite float a marker.
func withNonFiniteMarkers(data any) any {
	switch v := data.(type) {
	case float64:
		switch {
		case math.IsNaN(v):
			return nonFiniteMarker + "NaN\x00"
		case math.IsInf(v, 1):
			return nonFiniteMarker + "+Inf\x00"
		case math.IsInf(v, -1):
			return nonFiniteMarker + "-Inf\x00"
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[key] = withNonFiniteMarkers(value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = withNonFiniteMarkers(value)
		}
		return out
	}
	return data
}

// pythonFloats is what a worker answered with its markers floats again.
func pythonFloats(data any) any {
	switch v := data.(type) {
	case string:
		if f, ok := nonFiniteValues[v]; ok {
			return f
		}
	case map[string]any:
		for key, value := range v {
			v[key] = pythonFloats(value)
		}
	case []any:
		for i, value := range v {
			v[i] = pythonFloats(value)
		}
	}
	return data
}

// metric is the emitted metric as the exporter's: its value a float, a
// numeric string read as one, and its timestamp whole milliseconds.
func (m pythonMetric) metric() (model.Metric, error) {
	out := model.Metric{Name: m.Name, Help: m.Help, Type: m.Type, Labels: m.Labels}
	value, err := pythonNumber(m.Value)
	if err != nil {
		return out, fmt.Errorf("metric %q value %v %w", m.Name, m.Value, err)
	}
	out.Value = value
	if m.Timestamp != nil {
		at, err := pythonNumber(m.Timestamp)
		if err != nil || math.IsNaN(at) || math.IsInf(at, 0) {
			return out, fmt.Errorf("metric %q timestamp %v is not a number of milliseconds", m.Name, m.Timestamp)
		}
		ms := int64(at)
		out.Timestamp = &ms
	}
	return out, nil
}

// pythonNumber reads a value a script gave as a number.
func pythonNumber(v any) (float64, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		if f, ok := nonFiniteValues[n]; ok {
			return f, nil
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f, nil
		}
	}
	return 0, errors.New("is not a number")
}

// executePython runs a python transform in one of the collector's workers
// (pythonworker.go) and returns the metrics it emitted.
func executePython(ctx context.Context, pythonPath, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (*model.MetricSet, error) {
	out, err := runPython(ctx, pythonPath, "metrics", "transform", script, d, r, c)
	if err != nil {
		return nil, err
	}
	set := &model.MetricSet{Metrics: make([]model.Metric, 0, len(out.Metrics))}
	for _, emitted := range out.Metrics {
		metric, err := emitted.metric()
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", err), model.ErrScriptFailed)
		}
		set.Metrics = append(set.Metrics, metric)
	}
	return set, nil
}

// executePythonPreScript runs a pre-script and returns the data it left.
func executePythonPreScript(ctx context.Context, pythonPath, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (any, error) {
	out, err := runPython(ctx, pythonPath, "data", "pre-script", script, d, r, c)
	if err != nil {
		return nil, err
	}
	return pythonFloats(model.Normalize(out.Data)), nil
}

func runPython(ctx context.Context, pythonPath, mode, what, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (*pythonOutput, error) {
	if pythonPath == "" {
		pythonPath = "python3"
	}
	timeout := time.Duration(c.Limits.ScriptTimeout)
	if timeout <= 0 {
		timeout = 100 * time.Millisecond
	}
	input := pythonInput{Mode: mode, Script: script, Data: withNonFiniteMarkers(pythonScriptData(d)), Target: r.Target, Collector: c.Name, Response: pythonResponse{StatusCode: r.StatusCode, Headers: r.Headers, Body: string(r.Body), Text: string(r.Body)}}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, model.MarkError(err, model.ErrScriptFailed)
	}
	payload = []byte(nonFiniteJSON.Replace(string(payload)))
	line, elapsed, err := PythonWorkers().run(ctx, pythonWorkerSpec(pythonPath, c), payload, timeout)
	if timer := scriptTimerFrom(ctx); timer != nil && elapsed > 0 {
		timer.add(elapsed)
	}
	out, err := pythonResult(c, what, timeout, line, err)
	return out, model.MarkError(err, model.ErrScriptFailed)
}

// pythonResult reads a worker's answer, counting how the run ended.
func pythonResult(c *model.Collector, what string, timeout time.Duration, line []byte, err error) (*pythonOutput, error) {
	switch {
	case errors.Is(err, errPythonTimeout):
		PythonWorkers().recordRun(c.Name, pythonRunTimeout)
		return nil, fmt.Errorf("python %s timed out after %s: %w", what, timeout, context.DeadlineExceeded)
	case errors.Is(err, errPythonOutputTooLarge):
		PythonWorkers().recordRun(c.Name, pythonRunOutputLimit)
		return nil, fmt.Errorf("python %s output exceeds limit", what)
	case err != nil:
		PythonWorkers().recordRun(c.Name, pythonRunFailed)
		return nil, fmt.Errorf("python %s failed: %w", what, err)
	}
	var out pythonOutput
	if err := json.Unmarshal(line, &out); err != nil {
		PythonWorkers().recordRun(c.Name, pythonRunFailed)
		return nil, fmt.Errorf("python %s output: %w", what, err)
	}
	if !out.OK {
		PythonWorkers().recordRun(c.Name, pythonRunScriptError)
		return nil, fmt.Errorf("python %s failed: %s", what, strings.TrimSpace(out.Error))
	}
	PythonWorkers().recordRun(c.Name, pythonRunOK)
	return &out, nil
}

// ScriptTimer adds up how long a probe's Python ran.
//
// A probe reports how long its Python ran in
// http_exporter_script_duration_seconds. The scripts run deep inside the
// transform, so the probe hands them a timer through the context, and they add
// the time each call to a worker took: the pre-script and the python transform
// together, without starting an interpreter.
type ScriptTimer struct {
	mu    sync.Mutex
	total time.Duration
	ran   bool
}

type scriptTimerKey struct{}

// WithScriptTimer returns a context carrying a fresh timer.
func WithScriptTimer(ctx context.Context) (context.Context, *ScriptTimer) {
	timer := &ScriptTimer{}
	return context.WithValue(ctx, scriptTimerKey{}, timer), timer
}

func scriptTimerFrom(ctx context.Context) *ScriptTimer {
	timer, _ := ctx.Value(scriptTimerKey{}).(*ScriptTimer)
	return timer
}

func (t *ScriptTimer) add(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += d
	t.ran = true
}

// Seconds reports the time the scripts took, and whether any ran.
func (t *ScriptTimer) Seconds() (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total.Seconds(), t.ran
}

// pythonScriptData is what a script gets as data, by decoder: the parsed
// document of JSON, YAML and graphite; CSV rows, mappings by header or lists
// without one; the text of text, HTML and XML; and, for Prometheus
// exposition, {"metrics": [...]}, each series a mapping of its name, type,
// help, labels and value — or a histogram's buckets, sum and count, a
// summary's quantiles, sum and count — and its timestamp when it has one.
func pythonScriptData(d *decode.Decoded) any {
	switch d.Kind {
	case "html", "xml":
		return string(d.Raw)
	case "prometheus":
		if set, ok := d.Data.(model.MetricSet); ok {
			return pythonPrometheusData(set)
		}
	}
	return d.Data
}

// pythonPrometheusData is a Prometheus scrape as a script reads it.
func pythonPrometheusData(set model.MetricSet) map[string]any {
	metrics := make([]any, 0, len(set.Metrics))
	for _, m := range set.Metrics {
		labels := make(map[string]any, len(m.Labels))
		for name, value := range m.Labels {
			labels[name] = value
		}
		series := map[string]any{"name": m.Name, "type": string(m.Type), "help": m.Help, "labels": labels}
		if m.Timestamp != nil {
			series["timestamp"] = float64(*m.Timestamp)
		}
		switch {
		case m.Histogram != nil:
			buckets := make([]any, 0, len(m.Histogram.Buckets))
			for _, b := range m.Histogram.Buckets {
				buckets = append(buckets, map[string]any{"le": b.UpperBound, "count": float64(b.CumulativeCount)})
			}
			series["buckets"], series["sum"], series["count"] = buckets, m.Histogram.Sum, float64(m.Histogram.Count)
		case m.Summary != nil:
			quantiles := make([]any, 0, len(m.Summary.Quantiles))
			for _, q := range m.Summary.Quantiles {
				quantiles = append(quantiles, map[string]any{"quantile": q.Quantile, "value": q.Value})
			}
			series["quantiles"], series["sum"], series["count"] = quantiles, m.Summary.Sum, float64(m.Summary.Count)
		default:
			series["value"] = m.Value
		}
		metrics = append(metrics, series)
	}
	return map[string]any{"metrics": metrics}
}
