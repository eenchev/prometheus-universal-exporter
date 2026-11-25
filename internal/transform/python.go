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

// pythonInput is one request to a worker. The body is sent once. A script's
// response.text is its response.body, and so is its data when the decoder
// gives the script the body as text. So the worker makes the three one
// string (DataIsBody) instead of receiving and parsing a copy of each.
// Otherwise a large body would cost three times its size to hand over.
type pythonInput struct {
	Mode       string         `json:"mode"`
	Script     string         `json:"script"`
	Data       any            `json:"data"`
	DataIsBody bool           `json:"data_is_body,omitempty"`
	Response   pythonResponse `json:"response"`
	Target     string         `json:"target"`
	Collector  string         `json:"collector"`
	// Params are the probe's parameters by name; left out when there is
	// none.
	Params map[string]string `json:"params,omitempty"`
	// ScrapeTime and LastSuccess are the scrape's clock in seconds since
	// the epoch, each left out when there is none.
	ScrapeTime  any `json:"scrape_time,omitempty"`
	LastSuccess any `json:"last_success,omitempty"`
}

type pythonResponse struct {
	StatusCode any                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	// Timings, TLS, URL, Redirects, HTTPVersion and RemoteIP are
	// response.timings, response.tls, response.url, response.redirects,
	// response.http_version and response.remote_ip, jq's $timings, $tls,
	// $url, $redirects, $http_version and $remote_ip (fetch.HTTPTrip).
	Timings     any `json:"timings"`
	TLS         any `json:"tls"`
	URL         any `json:"url"`
	Redirects   any `json:"redirects"`
	HTTPVersion any `json:"http_version"`
	RemoteIP    any `json:"remote_ip"`
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
	// State is the state the script left, as JSON, for a collector with
	// transform.state (pythonstate.go); "" when the worker wrote none.
	State string `json:"state"`
	// Shown is the error of a failed script as the worker cut it, used
	// instead of Error. That is an error of more than 16,384 characters, or
	// one whose line does not fit in limits.max_output_bytes, shown as the
	// exporter would have shown it (shownByWorker).
	Shown []any `json:"shown"`
	// read says readPythonAnswer read the answer (pythonanswer.go), which
	// leaves Metrics empty. series holds the metrics as the series they
	// stand for, up to the first that is not one; seriesErr is that one's
	// error. count is how many the script emitted. normalized says Data is
	// already what model.Normalize makes of it.
	read       bool
	series     []model.Metric
	seriesErr  error
	count      int
	normalized bool
}

// pythonMetric is a metric as a script emitted it. metric(...) makes its
// name, type and help text, its value a float, its labels text and its
// timestamp whole milliseconds. But a script can also append to metrics
// itself. So each field is read as whatever it is, and checked here as
// metric(...) checks it. An error names the metric and the argument,
// instead of being the JSON decoder's error about a field of this struct.
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
// exporter exchange JSON. Each side writes such a float as a string no data
// contains: NUL, a marker, the value, NUL. The other side reads it back as
// the float. Towards the worker, the marker is replaced in the encoded line
// with NaN, Infinity or -Infinity, which Python's json reads as floats.
// Coming back, the worker writes the marker and pythonFloats turns it into
// the float.
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
			// An empty value leaves the label out too, as a rule's
			// expression label does (missingRequiredLabel): Prometheus
			// reads such a label as none.
			if text == "" {
				continue
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
	set, err := pythonSeries(ctx, out)
	if err == nil {
		shareHelp(set.Metrics)
	}
	return set, err
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

// shareHelp gives each series a script made without a help text the help
// of the first series of its name that had one. A metric's help belongs to
// its family and is written once, with the first series. A script might set
// it only on a later series, for example when it makes the first one in a
// separate branch. That help was meant for the metric, but was silently
// dropped.
func shareHelp(series []model.Metric) {
	missing, said := false, false
	for i := range series {
		if series[i].Help == "" {
			missing = true
		} else {
			said = true
		}
	}
	if !missing || !said {
		return
	}
	helps := map[string]string{}
	for i := range series {
		if _, known := helps[series[i].Name]; !known && series[i].Help != "" {
			helps[series[i].Name] = series[i].Help
		}
	}
	for i := range series {
		if series[i].Help == "" {
			series[i].Help = helps[series[i].Name]
		}
	}
}

// prometheusFromPython reads back what a pre-script of a prometheus
// transform left in data: the {"metrics": [...]} document from
// pythonPrometheusData. Each series is a mapping of name, type, help,
// labels and timestamp, plus value, or buckets, sum and count for a
// histogram, or quantiles, sum and count for a summary. A histogram or
// summary without sum or count, or with None for it, has none, like one
// the target wrote without it. It is checked like one read from the target
// (model.Histogram.Settle). A histogram's count and the +Inf entry of its
// buckets are each read as the script left them, even when they differ or
// when both are missing. A series left with nothing at all (no buckets or
// quantiles, no sum and no count) is refused (emptySeriesError).
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
	// A type, help or labels of the wrong kind is the script's mistake, like
	// a value of the wrong kind. Read as not given, the series was exported
	// untyped, without its help, or without its labels, which made it a
	// different series. None means not given.
	switch kind := series["type"].(type) {
	case nil:
	case string:
		if kind != "" {
			m.Type = model.MetricType(kind)
		}
	default:
		return m, fmt.Errorf(`%s type %s is not a string; give "gauge", "counter", "untyped", "histogram" or "summary"`, name, model.ShowValue(kind))
	}
	switch help := series["help"].(type) {
	case nil:
	case string:
		m.Help = help
	default:
		return m, fmt.Errorf("%s help %s is not a string", name, model.ShowValue(help))
	}
	switch labels := series["labels"].(type) {
	case nil:
	case map[string]any:
		if len(labels) > 0 {
			m.Labels = make(map[string]string, len(labels))
		}
		for key, value := range labels {
			if value == nil {
				continue
			}
			text, err := labelText(value)
			if err != nil {
				key, err = firstUnreadableLabel(labels, func(value any) any { return value })
				return m, fmt.Errorf("%s label %q %w", name, key, err)
			}
			// An empty value leaves the label off, as None does and as a
			// rule's expression label does (missingRequiredLabel).
			if text == "" {
				continue
			}
			m.Labels[key] = text
		}
	default:
		return m, fmt.Errorf("%s labels are %s, not a mapping of label names to values", name, model.ShowValue(labels))
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

// emptySeriesError is the error for a histogram or summary that a
// pre-script left with nothing: no buckets or quantiles, no sum and no
// count. The decoder never reads such a series from a target, and it would
// be exported as a TYPE line with no sample under it. A misspelled key
// causes it, such as "bucket" for "buckets". So the error lists the keys
// such a series has and the keys this one has, the first few if there are
// many.
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
	pool := PythonWorkers()
	// A test's pool may leave every script longer (SetLeastScriptTimeout).
	timeout = max(timeout, time.Duration(pool.leastScriptTimeout.Load()))
	// The request is written into a buffer kept for the next one, which is
	// free again once the worker has been handed it.
	encoder := pythonEncoders.Get().(*pythonEncoder)
	encoder.params = pythonParams(ctx)
	encoder.clock, _ = ctx.Value(scrapeClockKey{}).(scrapeClock)
	payload, err := encoder.request(mode, script, d, r, c)
	// The encoder is kept for the next request, of another probe.
	encoder.params = nil
	if errors.Is(err, errPythonRequestTooLarge) {
		return nil, model.MarkError(fmt.Errorf("python %s did not run: %w", what, err), model.ErrScriptFailed)
	}
	if err != nil {
		return nil, model.MarkError(err, model.ErrScriptFailed)
	}
	// The run is counted once in the statistics it takes here, however it
	// ends and whatever a reload does to its collector meanwhile. These are
	// the statistics its trip carries, or, for a run without any, those
	// kept under the collector's name (PythonPool.statsOf).
	timer := scriptTimerFrom(ctx)
	spec := pythonWorkerSpec(pythonPath, c)
	spec.stats = pool.statsOf(timer.carried(), c.Name)
	// The state the script is given, which is no part of the request as
	// the encoder writes it: it is put before the brace that ends it.
	stateKey := pythonStateKey{collector: c.Name, what: what, target: r.Target, params: pythonStateParams(pythonParams(ctx))}
	if c.Transform.State {
		payload = append(payload[:len(payload)-1], `,"state":`...)
		payload = append(payload, pythonStates.get(stateKey)...)
		payload = append(payload, '}')
	}
	var peak int64
	runCtx := ctx
	if memoryMeasured(ctx) {
		runCtx = context.WithValue(ctx, workerPeakKey{}, &peak)
	}
	line, ran, err := pool.run(runCtx, spec, payload, timeout)
	if peak > 0 {
		// The worker's whole life is in it, the runs before this one too:
		// a worker is kept between scrapes.
		ruleLogger(ctx).Debug("python "+what+" memory", "collector", c.Name, "worker_peak_bytes", peak, "limit_bytes", int64(c.Limits.MaxScriptMemory))
	}
	if cap(encoder.buf) <= pythonEncoderKept {
		pythonEncoders.Put(encoder)
	}
	if timer != nil && ran > 0 {
		timer.add(ran)
	}
	out, err := countedPythonResult(spec.stats, c, what, timeout, line, err)
	// What a script that ran to its end left in state is kept for its
	// next run, unless the run is a debug probe's (WithMemoryMeasured is
	// what marks one), which changes nothing of what a scrape finds.
	if err == nil && out != nil && c.Transform.State && out.State != "" {
		if err = checkPythonState(c, what, out.State); err == nil && !memoryMeasured(ctx) {
			pythonStates.put(stateKey, out.State)
		}
	}
	if out != nil && out.Log != "" {
		// Keep what the script printed and logged for whoever debugs it:
		// the first 4 KiB of printed output and twenty log lines. It counts
		// against max_output_bytes only up to that. A debug probe's report
		// shows it (WithRuleLogger). A failed script has both with its
		// failure, if the failure's line had room. Of the printed output
		// that is the last 1,024 characters, written at warn. They are what
		// was printed just before the failure, and are wanted without a
		// second run at debug.
		printed, logged := pythonLogLines(out.Log)
		logger := ruleLogger(ctx)
		switch {
		case printed != "" && err != nil:
			// It goes with the failure (PrintedBeforeFailure), which is
			// logged once and then sparingly while it repeats. A separate
			// line was written at every scrape of a script that fails
			// every time. A script under development that prints its input
			// prints a token, and this reaches the log of an exporter that
			// never asked for script output. So anything that looks like a
			// credential is masked, as in any text a failure carries.
			err = &failedAfterPrinting{error: err, printed: fetch.RedactText(printed)}
		case printed != "":
			logger.Debug("python "+what+" printed", "collector", c.Name, "output", printed)
		}
		for _, line := range logged {
			// With the target, which says where among a collector's
			// targets the line is of, as the logs show one: without
			// what it carries of a credential.
			if r != nil && r.Target != "" {
				logger.Log(ctx, line.level, "python "+what+" logged", "collector", c.Name, "target", fetch.DisplayTarget(c, r.Target), "message", line.text)
				continue
			}
			logger.Log(ctx, line.level, "python "+what+" logged", "collector", c.Name, "message", line.text)
		}
	}
	if err != nil {
		out = nil
	}
	return out, model.MarkError(err, model.ErrScriptFailed)
}

// failedAfterPrinting is a script's failure together with the last part of
// what the script printed before it. That output is not part of the
// failure's text, because failures are told apart by their text, and the
// printed output differs from one scrape to the next.
type failedAfterPrinting struct {
	error
	printed string
}

func (e *failedAfterPrinting) Unwrap() error { return e.error }

// PrintedBeforeFailure is the last 1,024 characters a Python script printed
// before it failed with err, with anything that looks like a credential
// masked. It is "" for a script that printed nothing and for an error that
// is not a script failure. Whoever logs the failure writes it next to the
// failure.
func PrintedBeforeFailure(err error) string {
	var failed *failedAfterPrinting
	if errors.As(err, &failed) {
		return failed.printed
	}
	return ""
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

// countedPythonResult reads a worker's answer, counting how the run ended in
// st, the statistics the run took when it began.
func countedPythonResult(st *PythonStats, c *model.Collector, what string, timeout time.Duration, line []byte, err error) (*pythonOutput, error) {
	pool := st.pool
	var deadline pythonDeadlineError
	switch {
	case errors.Is(err, errPythonTimeout):
		pool.recordRun(st, pythonRunTimeout)
		return nil, fmt.Errorf("python %s timed out after %s: %w", what, timeout, context.DeadlineExceeded)
	case errors.As(err, &deadline):
		// The probe's deadline, not limits.script_timeout, ended the run:
		// said and counted as that, so nobody raises a script_timeout the
		// script never reached.
		pool.recordRun(st, pythonRunDeadline)
		if !deadline.started {
			return nil, fmt.Errorf("python %s did not run: its probe or scrape ran out of time while the response was handed to the worker: %w", what, context.DeadlineExceeded)
		}
		return nil, model.Errorf("python %s was stopped after %s because its probe or scrape ran out of time, not because of limits.script_timeout (%s): %w", what, model.Elapsed(deadline.ran.Round(time.Millisecond)), timeout, context.DeadlineExceeded)
	case errors.Is(err, errPythonOutputTooLarge):
		pool.recordRun(st, pythonRunOutputLimit)
		return nil, fmt.Errorf("python %s output exceeds limit", what)
	case err != nil:
		pool.recordRun(st, pythonRunFailed)
		return nil, fmt.Errorf("python %s failed: %w", what, err)
	}
	// An answer as a worker writes it is read without encoding/json
	// (pythonanswer.go); any other line is read as every line was.
	out, read := readPythonAnswer(line)
	if !read {
		// Numbers are read the same way the JSON decoder reads a
		// response's: as json.Number. model.Normalize makes that an int, or
		// a *big.Int past int64. So an ID a pre-script passes through keeps
		// every digit instead of being rounded to the nearest float64 above
		// 2^53.
		out = &pythonOutput{}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(out); err != nil {
			pool.recordRun(st, pythonRunFailed)
			return nil, fmt.Errorf("python %s output: %w", what, err)
		}
	}
	if !out.OK {
		pool.recordRun(st, pythonRunScriptError)
		// A traceback longer than an error holds is shown by its end: the
		// exception's own line and the frames before it (scripterror.go).
		// The worker already does this where the whole might not fit the
		// output limit.
		// The answer goes back with the failure, for what the script
		// logged before it failed (runPython).
		if out.Shown != nil {
			return out, model.Errorf("python %s failed: %s", what, shownByWorker(out.Shown))
		}
		return out, model.Errorf("python %s failed: %s", what, shownScriptError(strings.TrimSpace(out.Error)))
	}
	pool.recordRun(st, pythonRunOK)
	return out, nil
}

// ScriptTimer adds up how long a probe's Python ran.
//
// A probe reports how long its Python ran in
// http_exporter_script_duration_seconds. The scripts run deep inside the
// transform, so the probe passes them a timer through the context. Each
// script adds its run time as limits.script_timeout measures it: the
// pre-script and the python transform together, excluding interpreter
// start and handing over the response.
//
// The timer also carries, the same way and for the same scripts, the
// worker statistics that the probe's runs count in (PythonStats). The probe
// took them for the collector it read. A run finds them through the timer
// and does not look them up by its collector's name, which a reload may
// have given to another collector since.
type ScriptTimer struct {
	mu    sync.Mutex
	total time.Duration
	ran   bool
	stats *PythonStats
}

type scriptTimerKey struct{}

// WithScriptTimer returns a context carrying a fresh timer, and with it
// stats, the worker statistics the runs under the context count in; nil for
// runs that count in those kept under their collector's name.
func WithScriptTimer(ctx context.Context, stats *PythonStats) (context.Context, *ScriptTimer) {
	timer := &ScriptTimer{stats: stats}
	return context.WithValue(ctx, scriptTimerKey{}, timer), timer
}

// carried is the worker statistics the timer carries, nil for no timer and
// for one that carries none. They do not change once the timer is made.
func (t *ScriptTimer) carried() *PythonStats {
	if t == nil {
		return nil
	}
	return t.stats
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

// pythonScriptData is what a script gets as data, by decoder:
//   - JSON, YAML, graphite, NDJSON, logfmt, TOML, INI and InfluxDB line
//     protocol: the parsed document;
//   - CSV: rows, as mappings by header, or lists without one;
//   - text, HTML and XML: the text;
//   - Prometheus exposition: {"metrics": [...]}. Each series is a mapping
//     of its name, type, help, labels and value (or a histogram's buckets,
//     sum and count, or a summary's quantiles, sum and count), plus its
//     timestamp when it has one.
//
// A histogram or summary the target wrote without a _sum or a _count has
// no sum or count key. A histogram's +Inf bucket is among its buckets with
// the count the target gave it, which may differ from its count key
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

// firstUnreadableLabel returns the first, by name, of a series' labels from
// a script whose value is not text, and why. The labels are a map with no
// order. With two such labels, the failure named whichever came first, and
// another one on the next scrape. The log then took it for a new failure
// each time (model.SameFailureText). So once one fails, they are checked
// again in name order. read is what the value is read as before it is
// made text.
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
