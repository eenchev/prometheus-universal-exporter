package exporter

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The series read from runtime/metrics keep the types client_golang gives
// them: a _total is a counter, everything else a gauge.
func TestRuntimeSeriesKeepTheirTypes(t *testing.T) {
	for _, series := range runtimeSeries {
		want := model.GaugeMetricType
		if strings.HasSuffix(series.name, "_total") {
			want = model.CounterMetricType
		}
		if series.typ != want {
			t.Errorf("%s has type %v, want %v", series.name, series.typ, want)
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
