// Package testutil holds the helpers the tests of several packages share:
// temporary files, a minimal collector and configuration, captured logs and
// polling. It is imported only by tests.
package testutil

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// MinimalConfig is a configuration with one regex collector, demo.
const MinimalConfig = `collectors:
  - name: demo
    request:
      type: http
      path: /status
    transform:
      type: regex
    metrics:
      - name: demo_value
        expression: 'value=(\d+)'
`

// WriteIn writes a file under dir, creating its directories.
func WriteIn(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// WriteFile writes a file in a fresh temporary directory and returns its path.
func WriteFile(t *testing.T, name, body string) string {
	t.Helper()
	path := t.TempDir() + "/" + name
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Collector is an http collector named name that reads value=<n> into
// demo_value with a regex, fails on every error, and accepts responses of up to
// 1 KiB.
func Collector(name, format string) model.Collector {
	return model.Collector{Name: name, Request: model.RequestConfig{Type: "http", Method: "GET"}, Decoder: model.DecoderConfig{Type: format}, Transform: model.TransformConfig{Type: "regex"}, Metrics: []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: `value=(\d+)`}}, ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"}, Limits: model.Limits{MaxResponseBytes: 1024}}
}

// CollectorYAML is one regex collector named name, indented as an item of a
// top-level collectors list.
func CollectorYAML(name string) string {
	return `  - name: ` + name + `
    request:
      type: http
      path: /status
    transform:
      type: regex
    metrics:
      - name: ` + name + `_value
        expression: 'value=(\d+)'
`
}

// CollectorsDocument is a collectors list of the named collectors.
func CollectorsDocument(names ...string) string {
	var b strings.Builder
	b.WriteString("collectors:\n")
	for _, name := range names {
		b.WriteString(CollectorYAML(name))
	}
	return b.String()
}

// CollectorNames lists the names of a configuration's collectors, in order.
func CollectorNames(c *model.Config) []string {
	names := make([]string, 0, len(c.Collectors))
	for _, x := range c.Collectors {
		names = append(names, x.Name)
	}
	return names
}

// CaptureLogs sends slog's default logger to a JSON buffer until the test
// ends, and returns the buffer.
//
// Anything consuming these logs parses them, so one differently shaped line is
// not a cosmetic problem: it is a line the consumer drops or chokes on. The
// failure this guards against is a log written through slog's default logger
// rather than the exporter's own, which without newLogger installing the
// default would come out as `2026/09/18 21:43:35 ERROR ...` in the middle of an
// otherwise JSON stream.
func CaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var out bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return &out
}

// QuietLogger captures the logs of a test and returns the default logger
// that writes them.
func QuietLogger(t *testing.T) *slog.Logger {
	t.Helper()
	CaptureLogs(t)
	return slog.Default()
}

// AssertJSONLines fails unless every line is a JSON object carrying the fields
// slog's JSON handler produces.
func AssertJSONLines(t *testing.T, out *bytes.Buffer, wantLines int) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("this log line is not JSON:\n%s\n%v", line, err)
		}
		for _, field := range []string{"time", "level", "msg"} {
			if _, ok := record[field]; !ok {
				t.Errorf("log line %q has no %q field", line, field)
			}
		}
		records = append(records, record)
	}
	if len(records) != wantLines {
		t.Fatalf("captured %d log lines, want %d:\n%s", len(records), wantLines, out.String())
	}
	return records
}

// FirstLines is the first n lines of s, for failure messages.
func FirstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}

// WaitFor polls done until it is true, and fails the test after a few
// seconds, naming what it waited for.
func WaitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
