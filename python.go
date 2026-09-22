package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		return nil, err
	}
	line, err := pythonWorkers.run(ctx, pythonWorkerSpec(pythonPath, c), payload, timeout)
	switch {
	case errors.Is(err, errPythonTimeout):
		pythonWorkers.recordRun(c.Name, pythonRunTimeout)
		return nil, fmt.Errorf("python %s timed out after %s: %w", what, timeout, context.DeadlineExceeded)
	case errors.Is(err, errPythonOutputTooLarge):
		pythonWorkers.recordRun(c.Name, pythonRunOutputLimit)
		return nil, fmt.Errorf("python %s output exceeds limit", what)
	case err != nil:
		pythonWorkers.recordRun(c.Name, pythonRunFailed)
		return nil, fmt.Errorf("python %s failed: %w", what, err)
	}
	var out pythonOutput
	if err := json.Unmarshal(line, &out); err != nil {
		pythonWorkers.recordRun(c.Name, pythonRunFailed)
		return nil, fmt.Errorf("python %s output: %w", what, err)
	}
	if !out.OK {
		pythonWorkers.recordRun(c.Name, pythonRunScriptError)
		return nil, fmt.Errorf("python %s failed: %s", what, strings.TrimSpace(out.Error))
	}
	pythonWorkers.recordRun(c.Name, pythonRunOK)
	return &out, nil
}

func pythonScriptData(d *Decoded) any {
	if d.Kind == "html" || d.Kind == "xml" {
		return string(d.Raw)
	}
	return d.Data
}
