//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A list where a metric has one value fails a scrape as the script's
// failure, answered and counted the same, whether the list holds three
// numbers, a list nested five thousand deep, or itself: the answer names
// the metric and the label, and says what stands there by its kind and how
// many items it has. The deep one and the one that holds itself ended the
// scrape with "RecursionError: maximum recursion depth exceeded", which
// names nothing. Each script's worker is there for its next scrape, and a
// list as deep under a key no metric has leaves the metric what it is.
func TestAListNestedDeepInAMetricFailsTheScrapeAsAShortOneDoes(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	const deep = "junk = 1\nfor _ in range(5000):\n    junk = [junk]\n"
	server := verboseServer(t, false,
		pythonCollector("flat_short", `metrics.append({"name": "m", "value": 1, "labels": {"l": [1, 2, 3]}})`),
		pythonCollector("flat_deep", deep+`metrics.append({"name": "m", "value": 1, "labels": {"l": [junk, 2, 3]}})`),
		pythonCollector("flat_itself", "junk = [1, 2]\njunk.append(junk)\n"+`metrics.append({"name": "m", "value": 1, "labels": {"l": junk}})`),
		pythonCollector("flat_argument", deep+`metric("m", value=1, help=[junk, 2, 3])`),
		pythonCollector("flat_key", deep+`metrics.append({"name": "m", "value": 1, "mine": [junk, 2, 3]})`),
	)
	const label = `python transform: metric "m" label "l" is an array of 3 values, not a single value; select one, or join them with join(",")`
	short := probeOnce(t, server, probePath("flat_short", target.URL, ""), nil)
	if short.Code == http.StatusOK || short.Body.String() != "collector flat_short transform failed: "+label+"\n" {
		t.Fatalf("a label that is a list of three: answered %d %q, want it to say %q", short.Code, short.Body, label)
	}
	for collector, want := range map[string]string{
		"flat_deep":     label,
		"flat_itself":   label,
		"flat_argument": "ValueError: metric 'm' help a list of 3 items is not a string",
	} {
		// Twice: the worker that answered the first is there for the second.
		for range 2 {
			answer := probeOnce(t, server, probePath(collector, target.URL, ""), nil)
			if answer.Code != short.Code || !strings.Contains(answer.Body.String(), want) || strings.Contains(answer.Body.String(), "RecursionError") {
				t.Fatalf("%s: answered %d %q, want %d and %q", collector, answer.Code, answer.Body, short.Code, want)
			}
			// The answer names the collector and the stage that failed.
			if got := strings.Replace(answer.Body.String(), collector, "flat_short", 1); collector != "flat_argument" && got != short.Body.String() {
				t.Fatalf("%s: answered %q, want the answer of the list of three, %q", collector, answer.Body, short.Body)
			}
		}
	}
	for range 2 {
		if answer := probeOnce(t, server, probePath("flat_key", target.URL, ""), nil); answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), "m 1\n") {
			t.Fatalf("a list nested deep under a key of the script's own: answered %d %q, want the metric", answer.Code, answer.Body)
		}
	}
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_script_errors_total{collector="flat_short"}`:       1,
		`http_exporter_script_errors_total{collector="flat_deep"}`:        2,
		`http_exporter_script_errors_total{collector="flat_itself"}`:      2,
		`http_exporter_script_errors_total{collector="flat_argument"}`:    2,
		`http_exporter_script_errors_total{collector="flat_key"}`:         0,
		`http_exporter_transform_errors_total{collector="flat_short"}`:    1,
		`http_exporter_transform_errors_total{collector="flat_deep"}`:     2,
		`http_exporter_transform_errors_total{collector="flat_itself"}`:   2,
		`http_exporter_transform_errors_total{collector="flat_argument"}`: 2,
		`http_exporter_transform_errors_total{collector="flat_key"}`:      0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	for _, collector := range []string{"flat_short", "flat_deep", "flat_itself", "flat_argument", "flat_key"} {
		if workers := transform.PythonWorkers().Snapshot(collector); workers.Starts != 1 || len(workers.Stops) != 0 {
			t.Errorf("%s: %d workers started and %v stopped, want its one worker to have answered every scrape", collector, workers.Starts, workers.Stops)
		}
	}
}
