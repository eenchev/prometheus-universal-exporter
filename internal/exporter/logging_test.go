package exporter

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A logger obtained from newLogger and slog's default must be the same
// handler, so it cannot matter which one a given call site reaches for.
func TestTheDefaultLoggerIsTheExportersLogger(t *testing.T) {
	out := testutil.CaptureLogs(t)
	slog.Default().Info("through the default logger", "which", "default")
	slog.Info("through the package function", "which", "package")
	records := testutil.AssertJSONLines(t, out, 2)
	for _, record := range records {
		if record["which"] == nil {
			t.Fatalf("attributes were dropped: %v", record)
		}
	}
}

// HELP is what a reader sees in Grafana's metric browser or in `curl` output,
// so sixteen identical lines are as good as none. This keeps every family
// described, distinctly, and keeps the descriptor list in step with what is
// actually exposed.
func TestEverySelfMetricHasItsOwnDescription(t *testing.T) {
	seen := map[string]string{}
	for _, d := range selfMetricDescriptors {
		if strings.TrimSpace(d.Help) == "" {
			t.Errorf("%s has no description", d.Name)
			continue
		}
		if strings.EqualFold(strings.TrimSpace(d.Help), "Exporter self metric.") {
			t.Errorf("%s still carries the placeholder description", d.Name)
		}
		if !strings.HasSuffix(d.Help, ".") {
			t.Errorf("%s: %q should read as a sentence", d.Name, d.Help)
		}
		if other, duplicate := seen[d.Help]; duplicate {
			t.Errorf("%s and %s share the description %q", other, d.Name, d.Help)
		}
		seen[d.Help] = d.Name
		if selfMetricHelp[d.Name] != d.Help {
			t.Errorf("%s: the indexed help does not match the descriptor", d.Name)
		}
	}
	if len(selfMetricDescriptors) != len(selfMetricNames()) {
		t.Fatal("the name list and the descriptors disagree")
	}
}
