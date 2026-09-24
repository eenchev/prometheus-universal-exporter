package transform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Metrics []model.Metric `json:"metrics"`
	Data    any            `json:"data"`
	Log     string         `json:"log"`
}

// ExecutePython runs a python transform in one of the collector's workers
// (pythonworker.go) and returns the metrics it emitted.
func ExecutePython(ctx context.Context, pythonPath, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (*model.MetricSet, error) {
	out, err := runPython(ctx, pythonPath, "metrics", "transform", script, d, r, c)
	if err != nil {
		return nil, err
	}
	return &model.MetricSet{Metrics: out.Metrics}, nil
}

// executePythonPreScript runs a pre-script and returns the data it left.
func executePythonPreScript(ctx context.Context, pythonPath, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (any, error) {
	out, err := runPython(ctx, pythonPath, "data", "pre-script", script, d, r, c)
	if err != nil {
		return nil, err
	}
	return model.Normalize(out.Data), nil
}

func runPython(ctx context.Context, pythonPath, mode, what, script string, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector) (*pythonOutput, error) {
	if pythonPath == "" {
		pythonPath = "python3"
	}
	timeout := time.Duration(c.Limits.ScriptTimeout)
	if timeout <= 0 {
		timeout = 100 * time.Millisecond
	}
	input := pythonInput{Mode: mode, Script: script, Data: pythonScriptData(d), Target: r.Target, Collector: c.Name, Response: pythonResponse{StatusCode: r.StatusCode, Headers: r.Headers, Body: string(r.Body), Text: string(r.Body)}}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, model.MarkError(err, model.ErrScriptFailed)
	}
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
		PythonWorkers().recordRun(c.Name, PythonRunFailed)
		return nil, fmt.Errorf("python %s failed: %w", what, err)
	}
	var out pythonOutput
	if err := json.Unmarshal(line, &out); err != nil {
		PythonWorkers().recordRun(c.Name, PythonRunFailed)
		return nil, fmt.Errorf("python %s output: %w", what, err)
	}
	if !out.OK {
		PythonWorkers().recordRun(c.Name, PythonRunScriptError)
		return nil, fmt.Errorf("python %s failed: %s", what, strings.TrimSpace(out.Error))
	}
	PythonWorkers().recordRun(c.Name, PythonRunOK)
	return &out, nil
}

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

func pythonScriptData(d *decode.Decoded) any {
	if d.Kind == "html" || d.Kind == "xml" {
		return string(d.Raw)
	}
	return d.Data
}
