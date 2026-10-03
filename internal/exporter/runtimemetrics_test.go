package exporter

import (
	"runtime"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The CPU classes come from runtime/metrics by name. A Go release that drops or
// renames one must make the series disappear rather than publish a zero.
func TestCPUClassMetricsComeFromTheRuntime(t *testing.T) {
	published := map[string]bool{}
	for _, metric := range cpuClassMetrics() {
		published[metric.Name] = true
	}
	if len(published) == 0 {
		t.Skip("this Go release publishes none of the CPU classes")
	}
	for _, want := range cpuClassSamples {
		if !published[want.metricName] {
			t.Logf("%s is not published by %s", want.metricName, runtime.Version())
		}
	}
	// Whatever is published must be a counter: these only ever grow.
	for _, metric := range cpuClassMetrics() {
		if metric.Type != model.CounterMetricType {
			t.Errorf("%s has type %v, want a counter", metric.Name, metric.Type)
		}
	}
}

// /proc/self/stat cannot be split on spaces: the second field is the executable
// name in parentheses and may contain them.
func TestStatFieldsHandleACommandNameWithSpaces(t *testing.T) {
	tests := []struct {
		name  string
		stat  string
		pid   string
		comm  string
		third string
	}{
		{"plain", "42 (exporter) S 1 42", "42", "exporter", "S"},
		{"spaces", "42 (my exporter) S 1 42", "42", "my exporter", "S"},
		{"parenthesis inside", "42 (odd (name)) S 1 42", "42", "odd (name)", "S"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := statFields(test.stat)
			if len(fields) < 3 {
				t.Fatalf("parsed %v", fields)
			}
			if fields[0] != test.pid || fields[1] != test.comm || fields[2] != test.third {
				t.Fatalf("parsed %q as %q / %q / %q", test.stat, fields[0], fields[1], fields[2])
			}
		})
	}
	if fields := statFields("no parentheses here"); fields != nil {
		t.Fatalf("a malformed line should parse to nothing, got %v", fields)
	}
}
