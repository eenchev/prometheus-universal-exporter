//go:build !select_request_types || request_type_http

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// lintConfig has a counter not named _total, a gauge in milliseconds, and a
// metric name two collectors make with another type each, and passes every
// check.
const lintConfig = `collectors:
  - name: app
    request: {type: http}
    decoder: {type: json}
    transform: {type: jq}
    metrics:
      - name: jobs_done
        type: counter
        expression: .jobs
      - name: queue_depth
        expression: .queue
  - name: legacy
    request: {type: http}
    decoder: {type: json}
    transform: {type: jq}
    metrics:
      - name: queue_depth
        type: counter
        expression: .queue
      - name: poll_latency_ms
        expression: .latency
`

// --dry-run lists the metric lint's advice under details.lint of the config
// check, by collector, rule and metric, and logs each finding at WARN, and
// the check passes all the same: exit 0 and status ok. A collector's
// lint.ignore takes its rules' advice out of the list and the log.
func TestDryRunListsTheMetricLintWithoutFailing(t *testing.T) {
	out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", lintConfig))
	if out.code != 0 || out.report.Status != checkOK {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
	type shown struct{ collector, rule, metric string }
	listed := func(out checkRun) []shown {
		t.Helper()
		var got []shown
		findings, _ := out.result(t, "config").Details["lint"].([]any)
		for _, f := range findings {
			f := f.(map[string]any)
			if message, _ := f["message"].(string); message == "" {
				t.Fatalf("a finding without a message: %v", f)
			}
			got = append(got, shown{f["collector"].(string), f["rule"].(string), f["metric"].(string)})
		}
		return got
	}
	want := []shown{
		{"app", "counter-total", "jobs_done"},
		{"app", "name-clash", "queue_depth"},
		{"legacy", "counter-total", "queue_depth"},
		{"legacy", "unit-suffix", "poll_latency_ms"},
		{"legacy", "name-clash", "queue_depth"},
	}
	if got := listed(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("details.lint lists %v, want %v\n%s", got, want, out.stdout)
	}
	logged := 0
	for _, record := range out.logs {
		if record["msg"] == "metric lint" {
			logged++
			if record["level"] != "WARN" || record["collector"] == nil || record["rule"] == nil || record["metric"] == nil || record["advice"] == nil {
				t.Fatalf("record=%v", record)
			}
		}
	}
	if logged != len(want) {
		t.Fatalf("logged %d findings, want %d:\n%s", logged, len(want), out.stderr)
	}

	silenced := strings.Replace(lintConfig, "  - name: legacy\n", "  - name: legacy\n    lint:\n      ignore: [name-clash, unit-suffix]\n", 1)
	out = runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", silenced))
	want = []shown{{"app", "counter-total", "jobs_done"}, {"app", "name-clash", "queue_depth"}, {"legacy", "counter-total", "queue_depth"}}
	if got := listed(out); out.code != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("exit=%d, details.lint lists %v, want %v", out.code, got, want)
	}

	// A configuration without advice lists none.
	out = runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig))
	if _, ok := out.result(t, "config").Details["lint"]; ok || out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}

	// Nor does advice make a failing check pass, or change how it fails.
	broken := lintConfig + "  - name: broken\n    request: {type: http}\n    transform: {type: nope}\n"
	if out := runCheckCLI(t, "--config.file="+testutil.WriteFile(t, "config.yaml", broken)); out.code != 1 || out.report.Status != checkFailed {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}
