//go:build !select_request_types || request_type_localfile

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A directory's file is decoded on its own, and a sample line the decoder
// leaves out of one is counted and logged as a probe's is, the warning
// naming the file: the file is read without the line, and the file beside it
// as before.
func TestASampleLineLeftOutOfADirectoryFileIsCountedAndLoggedWithTheFile(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "jobs.prom", "# TYPE job_seconds histogram\njob_seconds{quantile=\"0.5\"} 2\njob_seconds_bucket{le=\"+Inf\"} 3\njob_seconds_sum 9\njob_seconds_count 3\n")
	testutil.WriteIn(t, root, "node.prom", "node_up 1\n")
	server := fileServer(t, dirCollector("textfiles", root, "*.prom"))
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	result := probeFile(t, server, "collector=textfiles")
	result.must(t, http.StatusOK, `job_seconds_bucket{file="jobs.prom",le="+Inf"} 3`, `job_seconds_sum{file="jobs.prom"} 9`, `node_up{file="node.prom"} 1`)
	if strings.Contains(result.body, "quantile") {
		t.Fatalf("the line was passed on:\n%s", result.body)
	}
	if want := `http_exporter_decoder_lines_skipped_total{collector="textfiles"} 1`; !strings.Contains(selfMetrics(t, server), want+"\n") {
		t.Errorf("the self-metrics lack %s", want)
	}
	text := logs.String()
	if n := strings.Count(text, `"level":"WARN","msg":"sample lines left out"`); n != 1 || !strings.Contains(text, `"file":"jobs.prom","left_out":1,"error":"line 2: expected job_seconds_bucket with an le label, job_seconds_sum or job_seconds_count as a sample of the histogram job_seconds, got job_seconds"`) {
		t.Fatalf("%d warnings:\n%s", n, text)
	}
}
