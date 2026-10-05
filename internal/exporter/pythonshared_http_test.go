//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A pre-script that leaves a list in itself twice, or lists that each hold
// the next twice, forty of them, fails its scrape at once with what is
// wrong with data: that it holds itself, or that it is longer than
// limits.max_output_bytes written out. Each took its worker's memory limit
// first, or without one the script's time, and the worker with it. The
// script's worker is there for its next scrape; lists like those as a
// metric's label fail as two numbers there do; and a list that is in data
// twice is read back as two.
func TestDataThatHoldsAListTooOftenFailsTheScrapeAtOnce(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	const doubled = "junk = [1]\nfor _ in range(40):\n    junk = [junk, junk]\n"
	leaving := func(name, preScript, script string) model.Collector {
		c := pythonCollector(name, script)
		c.Transform.PreScript = preScript
		// What ends a worker that goes through all the data is written as.
		c.Limits.MaxScriptMemory = 256 << 20
		return c
	}
	server := verboseServer(t, false,
		leaving("shared_itself", "data = []\ndata.append(data)\ndata.append(data)\n", `metric("n", value=1)`),
		leaving("shared_doubled", doubled+"data = junk\n", `metric("n", value=1)`),
		leaving("shared_label", "", doubled+`metrics.append({"name": "m", "value": 1, "labels": {"l": junk}})`),
		leaving("shared_pair", "pair = [1, 2]\ndata = [pair, pair]\n", `metric("n", value=len(data), labels={"first": str(data[0]), "second": str(data[1]), "one": str(data[0] is data[1])})`),
	)
	for collector, want := range map[string]string{
		"shared_itself":  "python pre-script failed: RecursionError: data is nested more than 10000 deep, or a list or a dict in it holds itself; the exporter reads what a script leaves in data nested 10000 deep at most, as deep as it decodes a response",
		"shared_doubled": "python pre-script failed: OverflowError: what the script left in data is longer than limits.max_output_bytes (1048576 bytes) written out, a list or a dict that is there more than once being written each time; leave less there, or raise limits.max_output_bytes",
		"shared_label":   `python transform: metric "m" label "l" is an array of 2 values, not a single value; select one, or join them with join(",")`,
	} {
		// Twice: the worker that answered the first is there for the second.
		for range 2 {
			answer := probeOnce(t, server, probePath(collector, target.URL, ""), nil)
			if answer.Code == http.StatusOK || answer.Body.String() != "collector "+collector+" transform failed: "+want+"\n" {
				t.Fatalf("%s: answered %d %.600q, want the failure %q", collector, answer.Code, answer.Body, want)
			}
		}
	}
	for range 2 {
		if answer := probeOnce(t, server, probePath("shared_pair", target.URL, ""), nil); answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `n{first="[1, 2]",one="False",second="[1, 2]"} 2`+"\n") {
			t.Fatalf("a list that is in data twice: answered %d %q, want it read back as two lists", answer.Code, answer.Body)
		}
	}
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_script_errors_total{collector="shared_itself"}`:  2,
		`http_exporter_script_errors_total{collector="shared_doubled"}`: 2,
		`http_exporter_script_errors_total{collector="shared_label"}`:   2,
		`http_exporter_script_errors_total{collector="shared_pair"}`:    0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	for _, collector := range []string{"shared_itself", "shared_doubled", "shared_label", "shared_pair"} {
		if workers := transform.PythonWorkers().Snapshot(collector); workers.Starts != 1 || len(workers.Stops) != 0 {
			t.Errorf("%s: %d workers started and %v stopped, want its one worker to have answered every scrape", collector, workers.Starts, workers.Stops)
		}
	}
}
