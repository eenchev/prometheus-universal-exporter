package repository

import (
	"strings"
	"testing"
)

// The exporter serves the static targets' status page at the static
// targets path followed by /status, and refuses to start with the
// self-metrics there: the chart refuses those values when it renders,
// rather than render a Deployment that cannot start.
func TestTheChartRefusesTheSelfMetricsOnTheStatusPage(t *testing.T) {
	helm := requireHelm(t)
	for _, args := range [][]string{
		{"--set", "selfMetrics.path=/static-targets/status"},
		{"--set", "staticTargets.path=/static", "--set", "selfMetrics.path=/static/status"},
	} {
		out, ok := helmTemplate(t, helm, chartDir, args...)
		if ok || !strings.Contains(out, "the static targets' status page") {
			t.Errorf("rendering with %v succeeded or failed otherwise:\n%s", args, out)
		}
	}
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "selfMetrics.path=/static-targets/state"); !ok {
		t.Errorf("a self-metrics path beside the status page failed rendering:\n%s", out)
	}
}
