//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector with report_failures may not export a series under a name of
// the probe's own, http_exporter_probe_success,
// http_exporter_probe_elapsed_seconds and http_exporter_probe_failed_stage:
// a rule named so, one that metrics_prefix makes so, and a derived family
// are refused at load, naming the collector, the metric and the name; a
// collector without it takes each.
func TestReportFailuresReservesTheProbeSeriesNames(t *testing.T) {
	collector := func(report, prefix, rule, derived string) string {
		document := "collectors:\n  - name: up\n" + report + prefix + "    request: {type: http}\n    decoder: {type: json}\n    transform: {type: jq}\n    metrics:\n      - name: " + rule + "\n        expression: .x\n"
		if derived != "" {
			document += "    derived:\n      - name: " + derived + "\n        sum: [" + rule + "]\n        by: []\n"
		}
		return document
	}
	for _, tc := range []struct{ prefix, rule, derived, metric, exported string }{
		{"", "http_exporter_probe_success", "", "http_exporter_probe_success", "http_exporter_probe_success"},
		{"", "http_exporter_probe_elapsed_seconds", "", "http_exporter_probe_elapsed_seconds", "http_exporter_probe_elapsed_seconds"},
		{"", "http_exporter_probe_failed_stage", "", "http_exporter_probe_failed_stage", "http_exporter_probe_failed_stage"},
		{"    metrics_prefix: http\n", "exporter_probe_success", "", "exporter_probe_success", "http_exporter_probe_success"},
		{"", "probe_value", "http_exporter_probe_success", "http_exporter_probe_success", "http_exporter_probe_success"},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", collector("    report_failures: true\n", tc.prefix, tc.rule, tc.derived)))
		want := `collector "up" metric "` + tc.metric + `" is exported as ` + tc.exported + `, which is reserved for the series report_failures adds to every probe's answer`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s%s %s: %v, want %q", tc.prefix, tc.rule, tc.derived, err, want)
		}
		if _, err := Load(testutil.WriteFile(t, "config.yaml", collector("", tc.prefix, tc.rule, tc.derived))); err != nil {
			t.Errorf("without report_failures, %s%s %s: %v", tc.prefix, tc.rule, tc.derived, err)
		}
	}
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", collector("    report_failures: true\n", "", "uptime_up", "")))
	if err != nil || !cfg.Collectors[0].ReportFailures {
		t.Errorf("report_failures: true is not read: %v", err)
	}
}
