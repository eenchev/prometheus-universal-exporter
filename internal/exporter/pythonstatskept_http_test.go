//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// Over reloads that each give a Python collector another name, the collector
// probed and its self-metrics read under every name it has, the worker pool
// keeps the statistics of the one collector there is and of none of the
// names it had: what it keeps does not grow with the reloads. Nothing is
// counted under a former name, asking what is counted under one keeps
// nothing for it, and every run is still counted for the pool as a whole.
// What is kept is looked at after every rename, so ten show it: that it does
// not grow over two hundred is held at the pool, where a rename costs no
// reload (transform/pythonstats_test.go).
func TestRenamingACollectorOverReloadsKeepsNoWorkerStatisticsOfItsFormerNames(t *testing.T) {
	const renames = 10
	name := func(i int) string { return fmt.Sprintf("renamed_%d", i) }
	document := func(i int) string {
		return pythonDocument(pythonScripted(name(i), "metric(name=\"v\", value=1)", ""))
	}
	r := newUninterpreted(t, document(0), "")
	target := textTarget(t, "value=42\n")
	pool := transform.PythonWorkers()
	for i := range renames {
		if got := probeOnce(t, r.server, probePath(name(i), target.URL, ""), nil); !strings.Contains(got.Body.String(), "the interpreter did not start") {
			t.Fatalf("the probe of %s did not run its script: %d %s", name(i), got.Code, got.Body)
		}
		if got := seriesValue(t, selfMetrics(t, r.server), `http_exporter_python_worker_start_failures_total{collector="`+name(i)+`"}`); got != 1 {
			t.Fatalf("%s counts %v workers that failed to start, want its probe's", name(i), got)
		}
		if got := pool.StatsKept(); got != 1 {
			t.Fatalf("the pool keeps the statistics of %d collectors after %d renames, want those of the one there is", got, i)
		}
		r.reloadTo(document(i + 1))
		if got := pool.StatsKept(); got != 0 {
			t.Fatalf("the pool keeps the statistics of %d collectors when the collector has another name, after %d renames", got, i+1)
		}
	}
	for i := range renames {
		if got := pool.Snapshot(name(i)); got.StartFailures != 0 || len(got.Runs) != 0 {
			t.Fatalf("%+v is counted under the former name %s", got, name(i))
		}
	}
	if got := pool.StatsKept(); got != 0 {
		t.Errorf("the pool keeps the statistics of %d collectors after it was asked about the names the collector had", got)
	}
	if got := seriesValue(t, selfMetrics(t, r.server), "http_exporter_python_pool_worker_start_failures_total"); got != float64(renames) {
		t.Errorf("the pool counts %v workers that failed to start, want the %d of the probes", got, renames)
	}
}
