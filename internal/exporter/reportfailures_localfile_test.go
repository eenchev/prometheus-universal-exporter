//go:build !select_request_types || request_type_localfile

package exporter

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// report_failures is the collector's, whatever its request type: a
// localfile collector whose file is missing answers 200 with success 0 and
// its own fetch stage, file; one whose file cannot be decoded, decode; and
// one that reads its file, its series with success 1.
func TestReportFailuresOfALocalFileCollector(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "up.prom", "up_value 1\n")
	testutil.WriteIn(t, root, "broken.prom", "this is { not prometheus\n")
	missing := fileCollector("missing", root, "missing.prom")
	broken := fileCollector("broken", root, "broken.prom")
	up := fileCollector("up", root, "up.prom")
	missing.ReportFailures, broken.ReportFailures, up.ReportFailures = true, true, true
	collectors := []string{"missing", "broken", "up"}
	server := fileServer(t, missing, broken, up)
	for i, want := range []string{
		"http_exporter_probe_success 0\n# HELP http_exporter_probe_elapsed_seconds",
		"http_exporter_probe_success 0\n",
		"up_value 1\n",
	} {
		got := probeOnce(t, server, "/probe?collector="+collectors[i], nil)
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Errorf("%s: %d %s; want %q", collectors[i], got.Code, got.Body, want)
		}
		stage := map[string]string{"missing": `{stage="file"} 1`, "broken": `{stage="decode"} 1`}[collectors[i]]
		if stage != "" && !strings.Contains(got.Body.String(), "http_exporter_probe_failed_stage"+stage) {
			t.Errorf("%s: %s; want the stage %s", collectors[i], got.Body, stage)
		}
		if stage == "" && !strings.Contains(got.Body.String(), "http_exporter_probe_success 1\n") {
			t.Errorf("%s: %s; want success 1", collectors[i], got.Body)
		}
	}
}
