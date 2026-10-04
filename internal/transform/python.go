package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// pythonInput is one request to a worker. The body goes to it once: a
// script's response.text is its response.body, and so is its data when the
// decoder gives the script the body as text, so the worker makes the three
// one string (DataIsBody) rather than being sent, and parsing, a copy for
// each. A large body would otherwise cost three times its size to hand over.
type pythonInput struct {
	Mode       string         `json:"mode"`
	Script     string         `json:"script"`
	Data       any            `json:"data"`
	DataIsBody bool           `json:"data_is_body,omitempty"`
	Response   pythonResponse `json:"response"`
	Target     string         `json:"target"`
	Collector  string         `json:"collector"`
}

type pythonResponse struct {
	StatusCode any                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
}

// pythonOutput is one answer from a worker: the metrics a transform emitted or
// the data a pre-script left, or the error the script raised. A script can
// append anything to metrics, so each is read as whatever it is (pythonMetric).
type pythonOutput struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Metrics []any  `json:"metrics"`
	Data    any    `json:"data"`
	Log     string `json:"log"`
	// read says readPythonAnswer read the answer (pythonanswer.go), which
	// leaves no Metrics: series are the metrics as the series they stand
	// for, up to the first that is none, whose error seriesErr is, and
	// count is how many the script emitted. normalized says Data is what
	// model.Normalize makes of it already.
	read       bool
	series     []model.Metric
	seriesErr  error
	count      int
	normalized bool
}

// pythonMetric is a metric as a script emitted it. metric(...) makes its
// name, type and help text, its value a float, its labels text and its
// timestamp whole milliseconds, but a script can also append to metrics
// itself, so each is read as whatever it is, and checked here as metric(...)
// checks it: what is wrong is reported naming the metric and the argument
// rather than as the JSON decoder's error about a field of this struct.
type pythonMetric struct {
	Name      any
	Help      any
	Type      any
	Value     any
	Labels    any
	Timestamp any
}

// pythonMetricFrom reads one entry of a script's metrics, which must be a
// mapping, as metric(...) appends.
func pythonMetricFrom(index int, raw any) (pythonMetric, error) {
	entry, ok := raw.(map[string]any)
	if !ok {
		return pythonMetric{}, model.Errorf("metrics[%d] is %s, not a metric; call metric(...), or append a mapping with a name and a value", model.Position(index), showScriptValue(raw))
	}
	return pythonMetric{Name: entry["name"], Help: entry["help"], Type: entry["type"], Value: entry["value"], Labels: entry["labels"], Timestamp: entry["timestamp"]}, nil
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
// token Python's json reads; nonFiniteJSONMarker is how each starts.
var nonFiniteJSONMarker = []byte(`"\u0000pue-nonfinite:`)

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

// timestampMillis is a timestamp in whole milliseconds, refused when it is
// not finite or is beyond what an int64 of milliseconds holds, where the
// conversion would give a meaningless number.
func timestampMillis(at float64) (int64, error) {
	if math.IsNaN(at) || math.IsInf(at, 0) || at >= math.MaxInt64 || at < math.MinInt64 {
		return 0, fmt.Errorf("%v is not a number of milliseconds an exposition can carry", at)
	}
	return int64(at), nil
}

// observationCount is a histogram's or summary's count, or a bucket's, which
// is a whole number of observations from 0 to 2^64-1.
func observationCount(v float64) (uint64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= math.MaxUint64 || v != math.Trunc(v) {
		return 0, fmt.Errorf("%v is not a count of observations, a whole number from 0", v)
	}
	return uint64(v), nil
}

// pythonNumber reads a value a script gave as a number.
func pythonNumber(v any) (float64, error) {
	switch n := v.(type) {
	case nil:
		return 0, errors.New("is None, not a number")
	case float64:
		return n, nil
	case json.Number, int, *big.Int:
		// A worker's answer is read with its numbers as json.Number, and a
		// pre-script's data normalized to int or *big.Int.
		return model.Number(n)
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
	return pythonSeries(ctx, out)
}

// pythonSeries is the metrics of a python transform's answer as series,
// counted against limits.max_metrics.
func pythonSeries(ctx context.Context, out *pythonOutput) (*model.MetricSet, error) {
	if out.read {
		// Read into series already, and counted (pythonanswer.go): a script
		// that emitted too many fails as one past limits.max_metrics before
		// what is wrong with one of its metrics is said, as below.
		if err := takeSeriesN(ctx, out.count); err != nil {
			return nil, err
		}
		if out.seriesErr != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", out.seriesErr), model.ErrScriptFailed)
		}
		if out.series == nil {
			out.series = []model.Metric{}
		}
		return &model.MetricSet{Metrics: out.series}, nil
	}
	// The answer is bounded by limits.max_output_bytes already; counted
	// before its metrics are converted, a script that emitted too many fails
	// as one past limits.max_metrics, not as whatever the conversion finds.
	if err := takeSeriesN(ctx, len(out.Metrics)); err != nil {
		return nil, err
	}
	set := &model.MetricSet{Metrics: make([]model.Metric, 0, len(out.Metrics))}
	for i, raw := range out.Metrics {
		emitted, err := pythonMetricFrom(i, raw)
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", err), model.ErrScriptFailed)
		}
		metric, err := emitted.metric()
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python transform: %w", err), model.ErrScriptFailed)
		}
		set.Metrics = append(set.Metrics, metric)
	}
	return set, nil
}

// prometheusFromPython reads back what a pre-script of a prometheus transform
// left in data: the {"metrics": [...]} document pythonPrometheusData gave it,
// each series a mapping of name, type, help, labels and timestamp, and value,
// or buckets, sum and count for a histogram, quantiles, sum and count for a
// summary. A histogram or summary without sum or count, or with None for it,
// has none, as one the target wrote without it, and is checked as one read
// from the target is (model.Histogram.Settle): a histogram's count and the
// +Inf entry of its buckets are each read as the script left them, also when
// they differ or when it left neither. One left with nothing at all, no
// buckets or quantiles, no sum and no count, is refused (emptySeriesError).
func prometheusFromPython(data any) (model.MetricSet, error) {
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
		m, err := prometheusSeries(series)
		if err != nil {
			return model.MetricSet{}, model.Errorf("data[\"metrics\"][%d]: %w", model.Position(i), err)
		}
		set.Metrics = append(set.Metrics, m)
	}
	return set, nil
}

func prometheusSeries(series map[string]any) (model.Metric, error) {
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

// emptySeriesError is the error of a histogram or a summary a pre-script left
// with nothing: none of its parts, its buckets or quantiles, no sum and no
// count. The decoder never reads such a series from a target, and it would be
// exported as a TYPE line with no sample under it. It is what a misspelled
// key leaves, "bucket" for "buckets", so the error says which keys such a
// series has and which this one has, the first of them when they are many.
func emptySeriesError(kind, name, parts string, series map[string]any) error {
	const shown = 12
	keys := model.SortedKeys(series)
	more := ""
	if len(keys) > shown {
		more = fmt.Sprintf(" and %d more", len(keys)-shown)
		keys = keys[:shown]
	}
	for i, key := range keys {
		keys[i] = model.QuoteValue(key)
	}
	return fmt.Errorf("the %[1]s %[2]s has no %[3]s, no sum and no count; a %[1]s series has the keys %[3]q, \"sum\" and \"count\", and this one has the keys %[4]s%[5]s", kind, name, parts, strings.Join(keys, ", "), more)
}

// executePythonPreScript runs a pre-script and returns the data it left.
func executePythonPreScript(ctx context.Context, pythonPath, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (any, error) {
	out, err := runPython(ctx, pythonPath, "data", "pre-script", script, d, r, c)
	if err != nil {
		return nil, err
	}
	return pythonData(out), nil
}

// pythonData is what a pre-script's answer says it left in data.
func pythonData(out *pythonOutput) any {
	if out.normalized {
		return pythonFloats(out.Data)
	}
	return pythonFloats(model.Normalize(out.Data))
}

func runPython(ctx context.Context, pythonPath, mode, what, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (*pythonOutput, error) {
	if pythonPath == "" {
		pythonPath = "python3"
	}
	timeout := time.Duration(c.Limits.ScriptTimeout)
	if timeout <= 0 {
		timeout = 100 * time.Millisecond
	}
	// The request is written into a buffer kept for the next one, which is
	// free again once the worker has been handed it.
	encoder := pythonEncoders.Get().(*pythonEncoder)
	payload, err := encoder.request(mode, script, d, r, c)
	if err != nil {
		return nil, model.MarkError(err, model.ErrScriptFailed)
	}
	line, ran, err := PythonWorkers().run(ctx, pythonWorkerSpec(pythonPath, c), payload, timeout)
	if cap(encoder.buf) <= pythonEncoderKept {
		pythonEncoders.Put(encoder)
	}
	if timer := scriptTimerFrom(ctx); timer != nil && ran > 0 {
		timer.add(ran)
	}
	out, err := pythonResult(c, what, timeout, line, err)
	if err == nil && out.Log != "" {
		// What the script printed, its first 4 KiB, for whoever debugs it;
		// it counts against max_output_bytes only as far as that. A debug
		// probe's report shows it (WithRuleLogger).
		ruleLogger(ctx).Debug("python "+what+" printed", "collector", c.Name, "output", out.Log)
	}
	return out, model.MarkError(err, model.ErrScriptFailed)
}

// pythonRequest is the request line that has a worker run script: the
// response, with its body once, and the data, unless it is the body
// (pythonrequest.go).
func pythonRequest(mode, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) ([]byte, error) {
	return new(pythonEncoder).request(mode, script, d, r, c)
}

// dataIsBody reports whether a script's data is the response's body as text,
// as it is for the text, html and xml decoders unless a pre-script changed
// it, so that it is sent once.
func dataIsBody(data any, body string) bool {
	text, ok := data.(string)
	return ok && text == body
}

// showScriptValue names a value a script gave for an error, as
// model.ShowValue does, with a number as the script wrote it: a worker's
// answer is read with its numbers as json.Number, which ShowValue quotes.
func showScriptValue(v any) string {
	if n, ok := v.(json.Number); ok {
		return string(n)
	}
	return model.ShowValue(v)
}

// pythonResult reads a worker's answer, counting how the run ended.
func pythonResult(c *model.Collector, what string, timeout time.Duration, line []byte, err error) (*pythonOutput, error) {
	var deadline pythonDeadlineError
	switch {
	case errors.Is(err, errPythonTimeout):
		PythonWorkers().recordRun(c.Name, pythonRunTimeout)
		return nil, fmt.Errorf("python %s timed out after %s: %w", what, timeout, context.DeadlineExceeded)
	case errors.As(err, &deadline):
		// The probe's deadline, not limits.script_timeout, ended the run:
		// said and counted as that, so nobody raises a script_timeout the
		// script never reached.
		PythonWorkers().recordRun(c.Name, pythonRunDeadline)
		if !deadline.started {
			return nil, fmt.Errorf("python %s did not run: its probe or scrape ran out of time while the response was handed to the worker: %w", what, context.DeadlineExceeded)
		}
		return nil, model.Errorf("python %s was stopped after %s because its probe or scrape ran out of time, not because of limits.script_timeout (%s): %w", what, model.Elapsed(deadline.ran.Round(time.Millisecond)), timeout, context.DeadlineExceeded)
	case errors.Is(err, errPythonOutputTooLarge):
		PythonWorkers().recordRun(c.Name, pythonRunOutputLimit)
		return nil, fmt.Errorf("python %s output exceeds limit", what)
	case err != nil:
		PythonWorkers().recordRun(c.Name, pythonRunFailed)
		return nil, fmt.Errorf("python %s failed: %w", what, err)
	}
	// An answer as a worker writes it is read without encoding/json
	// (pythonanswer.go); any other line is read as every line was.
	out, read := readPythonAnswer(line)
	if !read {
		// Numbers are read as the JSON decoder reads a response's: as
		// json.Number, which model.Normalize makes an int, or a *big.Int
		// past int64, so an ID a pre-script passes through keeps every
		// digit rather than being rounded to the nearest float64 above
		// 2^53.
		out = &pythonOutput{}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(out); err != nil {
			PythonWorkers().recordRun(c.Name, pythonRunFailed)
			return nil, fmt.Errorf("python %s output: %w", what, err)
		}
	}
	if !out.OK {
		PythonWorkers().recordRun(c.Name, pythonRunScriptError)
		return nil, fmt.Errorf("python %s failed: %s", what, strings.TrimSpace(out.Error))
	}
	PythonWorkers().recordRun(c.Name, pythonRunOK)
	return out, nil
}

// ScriptTimer adds up how long a probe's Python ran.
//
// A probe reports how long its Python ran in
// http_exporter_script_duration_seconds. The scripts run deep inside the
// transform, so the probe hands them a timer through the context, and they add
// the time each script ran, as limits.script_timeout measures it: the
// pre-script and the python transform together, without starting an
// interpreter or handing it the response.
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
// summary's quantiles, sum and count — and its timestamp when it has one. A
// histogram or summary the target wrote without a _sum or a _count has no
// sum or count key. A histogram's +Inf bucket is among its buckets with the
// count the target gave it, which its count key need not be the same as
// (model.Histogram).
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
			series["buckets"] = buckets
			if !m.Histogram.NoSum {
				series["sum"] = m.Histogram.Sum
			}
			if !m.Histogram.NoCount {
				series["count"] = float64(m.Histogram.Count)
			}
		case m.Summary != nil:
			quantiles := make([]any, 0, len(m.Summary.Quantiles))
			for _, q := range m.Summary.Quantiles {
				quantiles = append(quantiles, map[string]any{"quantile": q.Quantile, "value": q.Value})
			}
			series["quantiles"] = quantiles
			if !m.Summary.NoSum {
				series["sum"] = m.Summary.Sum
			}
			if !m.Summary.NoCount {
				series["count"] = float64(m.Summary.Count)
			}
		default:
			series["value"] = m.Value
		}
		metrics = append(metrics, series)
	}
	return map[string]any{"metrics": metrics}
}

// firstUnreadableLabel is the first, by name, of a series' labels a script
// gave whose value is no text, and why it is none. The labels are a map,
// gone through in no order: of two that are no text the failure named
// whichever came first, another on the next scrape, which the log took for a
// new failure each time (model.SameFailureText). So once one fails they are
// gone through again in the order of their names; read is what the value is
// read as before it is made text.
func firstUnreadableLabel(labels map[string]any, read func(any) any) (string, error) {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if labels[name] == nil {
			continue
		}
		if _, err := labelText(read(labels[name])); err != nil {
			return name, err
		}
	}
	return "", nil
}
