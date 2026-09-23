package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
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
	OK      bool     `json:"ok"`
	Error   string   `json:"error"`
	Metrics []Metric `json:"metrics"`
	Data    any      `json:"data"`
	Log     string   `json:"log"`
}

// executePython runs a python transform in one of the collector's workers
// (pythonworker.go) and returns the metrics it emitted.
func executePython(ctx context.Context, pythonPath, script string, d *Decoded, r *HTTPResponse, c *Collector) (*MetricSet, error) {
	out, err := runPython(ctx, pythonPath, "metrics", "transform", script, d, r, c)
	if err != nil {
		return nil, err
	}
	return &MetricSet{Metrics: out.Metrics}, nil
}

// executePythonPreScript runs a pre-script and returns the data it left.
func executePythonPreScript(ctx context.Context, pythonPath, script string, d *Decoded, r *HTTPResponse, c *Collector) (any, error) {
	out, err := runPython(ctx, pythonPath, "data", "pre-script", script, d, r, c)
	if err != nil {
		return nil, err
	}
	return normalize(out.Data), nil
}

func runPython(ctx context.Context, pythonPath, mode, what, script string, d *Decoded, r *HTTPResponse, c *Collector) (*pythonOutput, error) {
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
		return nil, markError(err, errScriptFailed)
	}
	line, elapsed, err := pythonWorkers().run(ctx, pythonWorkerSpec(pythonPath, c), payload, timeout)
	if timer := scriptTimerFrom(ctx); timer != nil && elapsed > 0 {
		timer.add(elapsed)
	}
	out, err := pythonResult(c, what, timeout, line, err)
	return out, markError(err, errScriptFailed)
}

// pythonResult reads a worker's answer, counting how the run ended.
func pythonResult(c *Collector, what string, timeout time.Duration, line []byte, err error) (*pythonOutput, error) {
	switch {
	case errors.Is(err, errPythonTimeout):
		pythonWorkers().recordRun(c.Name, pythonRunTimeout)
		return nil, fmt.Errorf("python %s timed out after %s: %w", what, timeout, context.DeadlineExceeded)
	case errors.Is(err, errPythonOutputTooLarge):
		pythonWorkers().recordRun(c.Name, pythonRunOutputLimit)
		return nil, fmt.Errorf("python %s output exceeds limit", what)
	case err != nil:
		pythonWorkers().recordRun(c.Name, pythonRunFailed)
		return nil, fmt.Errorf("python %s failed: %w", what, err)
	}
	var out pythonOutput
	if err := json.Unmarshal(line, &out); err != nil {
		pythonWorkers().recordRun(c.Name, pythonRunFailed)
		return nil, fmt.Errorf("python %s output: %w", what, err)
	}
	if !out.OK {
		pythonWorkers().recordRun(c.Name, pythonRunScriptError)
		return nil, fmt.Errorf("python %s failed: %s", what, strings.TrimSpace(out.Error))
	}
	pythonWorkers().recordRun(c.Name, pythonRunOK)
	return &out, nil
}

// A probe reports how long its Python ran in
// http_exporter_script_duration_seconds. The scripts run deep inside the
// transform, so the probe hands them a timer through the context, and they add
// the time each call to a worker took: the pre-script and the python transform
// together, without starting an interpreter.
type scriptTimer struct {
	mu    sync.Mutex
	total time.Duration
	ran   bool
}

type scriptTimerKey struct{}

// withScriptTimer returns a context carrying a fresh timer.
func withScriptTimer(ctx context.Context) (context.Context, *scriptTimer) {
	timer := &scriptTimer{}
	return context.WithValue(ctx, scriptTimerKey{}, timer), timer
}

func scriptTimerFrom(ctx context.Context) *scriptTimer {
	timer, _ := ctx.Value(scriptTimerKey{}).(*scriptTimer)
	return timer
}

func (t *scriptTimer) add(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += d
	t.ran = true
}

// seconds reports the time the scripts took, and whether any ran.
func (t *scriptTimer) seconds() (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total.Seconds(), t.ran
}

func pythonScriptData(d *Decoded) any {
	if d.Kind == "html" || d.Kind == "xml" {
		return string(d.Raw)
	}
	return d.Data
}

// recordScriptDuration keeps the duration of a probe's Python, when it ran
// any, on the collector and the request.
func recordScriptDuration(rec statsRecorder, timer *scriptTimer) {
	if seconds, ran := timer.seconds(); ran {
		rec.update(func(x *serverStats) { x.lastScriptDuration = seconds })
	}
}
