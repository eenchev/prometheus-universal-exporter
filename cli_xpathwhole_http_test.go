//go:build !select_request_types || request_type_http

package main

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The check refuses an XPath expression the engine would read only the
// start of, as startup does, and says where it stops.
func TestCheckRefusesAnXPathExpressionReadOnlyInPart(t *testing.T) {
	config := testutil.WriteFile(t, "config.yaml", `collectors:
  - name: jobs
    request:
      type: http
      path: /jobs.xml
    transform:
      type: xpath
    metrics:
      - name: job_bytes
        expression: 'sum(//job/@size))'
`)
	check := runCheckCLI(t, "--config.file="+config)
	const want = `collector "jobs" metric "job_bytes" XPath "sum(//job/@size))": the ")" at byte 17 closes nothing`
	if errs := check.result(t, "config").Errors; check.code != 1 || len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), want) {
		t.Fatalf("--dry-run exit=%d, errors %q; want exit 1 and %q", check.code, errs, want)
	}
	if start := runCLI(t, "--config.file="+config); start.code != 1 {
		t.Fatalf("startup exit=%d, want 1: --dry-run and startup disagree", start.code)
	}
}
