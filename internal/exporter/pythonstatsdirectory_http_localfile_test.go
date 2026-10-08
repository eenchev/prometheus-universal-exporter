//go:build !select_request_types || (request_type_http && request_type_localfile)

package exporter

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A probe of a directory runs its collector's script once for every file,
// all in the statistics the trip took (collectDirectory): one that read its
// collector before a reload removed it, and reads its files when the
// collector is back, counts every one of those runs under no collector's
// name, and keeps nothing under it. Handed no statistics, the runs would
// count under the name, for the collector added again. The first probe of
// that one counts its own three runs.
func TestTheScriptsOfADirectoryCountInTheStatisticsOfTheirTrip(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		testutil.WriteIn(t, root, name, "value=1\n")
	}
	kept := pythonScripted("kept", workerScript, "")
	directory := "  - name: gone\n    request: {type: localfile, root: " + root + ", files: [\"*.txt\"]}\n    limits: {script_timeout: 1m}\n    transform:\n      type: python\n      script: |\n        metric(name=\"v\", value=1)\n"
	both := pythonDocument(kept, directory)
	r := newUninterpreted(t, both, "")
	reached, resume := holdFirstProbeOf(t, "gone")
	old := probeAsync(context.Background(), r.server, "/probe?collector=gone", nil)
	<-reached
	r.reloadTo(pythonDocument(kept))
	r.reloadTo(both)
	resume()
	if outcome := <-old; outcome.code != http.StatusOK || strings.Count(outcome.body, "localfile_scrape_error{file=") != 3 {
		t.Fatalf("the probe of the directory was answered %d: %s", outcome.code, outcome.body)
	}
	probeConfigReadHook.Store(nil)
	answer := workerMetrics(t, r)
	pythonSeriesFromZero(t, "after the probe that read the removed collector", answer, "gone", 0, nil)
	if got := seriesValue(t, answer, "http_exporter_python_pool_worker_start_failures_total"); got != 3 {
		t.Errorf("the pool counts %v runs, want the 3 files'", got)
	}
	if got := transform.PythonWorkers().StatsKept(); got != 0 {
		t.Errorf("the pool keeps the statistics of %d collectors, and no collector there is has run a script", got)
	}
	if got := probeOnce(t, r.server, "/probe?collector=gone", nil); got.Code != http.StatusOK {
		t.Fatalf("the probe of the collector added again was answered %d: %s", got.Code, got.Body)
	}
	if got := failedStartsOf(t, workerMetrics(t, r), "gone"); got != 3 {
		t.Errorf("the collector added again counts %v runs after its first probe, want the 3 files'", got)
	}
}
