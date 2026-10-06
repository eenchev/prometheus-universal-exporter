//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// End to end, a pre-script of a prometheus transform that leaves a series'
// help, type or labels as something a series does not have there fails the
// probe as a timestamp of another kind always did: the answer names the
// series and the key, and the scrape is counted once as a script's failure
// and once as a failed transform. The series used to be exported without
// its help, untyped, or without its labels. A type that is text and is no
// type is refused where every series is checked, as it was, which is no
// failure of the script.
func TestAPreScriptSeriesOfAnotherKindFailsTheProbeAsAScriptFailure(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, "# HELP up_thing Help.\n# TYPE up_thing gauge\nup_thing{a=\"b\"} 1\n")
	scripted := func(name, script string) model.Collector {
		return model.Collector{
			Name: name, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "prometheus"},
			Transform: model.TransformConfig{Type: "prometheus", PreScript: script}, Limits: model.Limits{ScriptTimeout: model.Duration(time.Minute)},
		}
	}
	server := verboseServer(t, false,
		scripted("kind_help", `data["metrics"][0]["help"] = 5`),
		scripted("kind_type", `data["metrics"][0]["type"] = ["counter"]`),
		scripted("kind_labels", `data["metrics"][0]["labels"] = [["a", "b"]]`),
		scripted("kind_timestamp", `data["metrics"][0]["timestamp"] = [1, 2, 3]`),
		scripted("kind_none", `data["metrics"][0]["help"] = None`),
		scripted("kind_no_type", `data["metrics"][0]["type"] = "counterr"`),
	)
	for collector, want := range map[string]string{
		"kind_help":      "collector kind_help transform failed: python pre-script: data[\"metrics\"][0]: up_thing help 5 is not a string\n",
		"kind_type":      "collector kind_type transform failed: python pre-script: data[\"metrics\"][0]: up_thing type an array of 1 item is not a string; give \"gauge\", \"counter\", \"untyped\", \"histogram\" or \"summary\"\n",
		"kind_labels":    "collector kind_labels transform failed: python pre-script: data[\"metrics\"][0]: up_thing labels are an array of 1 item, not a mapping of label names to values\n",
		"kind_timestamp": "collector kind_timestamp transform failed: python pre-script: data[\"metrics\"][0]: up_thing timestamp an array of 3 items is not a number of milliseconds\n",
		"kind_none":      "# TYPE up_thing gauge\nup_thing{a=\"b\"} 1\n",
		"kind_no_type":   "metric \"up_thing\" has invalid type \"counterr\"\n",
	} {
		answer := probeOnce(t, server, probePath(collector, target.URL, ""), nil)
		if (answer.Code == http.StatusOK) != (collector == "kind_none") || !strings.HasSuffix(answer.Body.String(), want) || collector != "kind_no_type" && answer.Body.String() != want {
			t.Errorf("%s: answered %d %q, want %q", collector, answer.Code, answer.Body, want)
		}
	}
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_script_errors_total{collector="kind_help"}`:         1,
		`http_exporter_script_errors_total{collector="kind_type"}`:         1,
		`http_exporter_script_errors_total{collector="kind_labels"}`:       1,
		`http_exporter_script_errors_total{collector="kind_timestamp"}`:    1,
		`http_exporter_script_errors_total{collector="kind_none"}`:         0,
		`http_exporter_script_errors_total{collector="kind_no_type"}`:      0,
		`http_exporter_transform_errors_total{collector="kind_help"}`:      1,
		`http_exporter_transform_errors_total{collector="kind_type"}`:      1,
		`http_exporter_transform_errors_total{collector="kind_labels"}`:    1,
		`http_exporter_transform_errors_total{collector="kind_timestamp"}`: 1,
		`http_exporter_transform_errors_total{collector="kind_none"}`:      0,
		`http_exporter_transform_errors_total{collector="kind_no_type"}`:   0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

// End to end, a label a script gives as the empty string is in no answer: a
// python transform's metric(...) and an entry it appends by hand, and a
// series a pre-script leaves a prometheus transform, were answered with
// l="" and exported over OTLP with the attribute. They are answered with
// the labels that have a value, and queued for OTLP with those. Two series
// a script tells apart by such a label alone are refused as the duplicates
// they are to Prometheus, as they were.
func TestALabelAScriptGivesEmptyIsInNoAnswer(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, "# TYPE up_thing gauge\nup_thing{a=\"b\"} 1\n")
	pre := model.Collector{
		Name: "empty_pre", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "prometheus"},
		Transform: model.TransformConfig{Type: "prometheus", PreScript: `data["metrics"][0]["labels"]["l"] = ""`}, Limits: model.Limits{ScriptTimeout: model.Duration(time.Minute)},
	}
	cfg := &model.Config{OTLP: otlpConfig("http://collector.invalid/v1/metrics"), Collectors: []model.Collector{
		pythonCollector("empty_metric", `metric("m", "gauge", 1, {"l": "", "k": "v"})`),
		pythonCollector("empty_entry", `metrics.append({"name": "m", "value": 1, "labels": {"l": ""}})`),
		pre,
		pythonCollector("empty_twice", `metric("m", "gauge", 1, {"l": ""}); metric("m", "gauge", 2)`),
	}}
	server := newStaticServer(t, cfg, nil)
	for collector, want := range map[string]string{
		"empty_metric": "# TYPE m gauge\nm{k=\"v\"} 1\n",
		"empty_entry":  "# TYPE m gauge\nm 1\n",
		"empty_pre":    "# TYPE up_thing gauge\nup_thing{a=\"b\"} 1\n",
	} {
		answer := probeOnce(t, server, probePath(collector, target.URL, ""), nil)
		if answer.Code != http.StatusOK || answer.Body.String() != want {
			t.Errorf("%s: answered %d %q, want %q", collector, answer.Code, answer.Body, want)
		}
	}
	exported := 0
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			exported++
			for name, value := range m.Labels {
				if value == "" {
					t.Errorf("the OTLP export carries %s with the label %s empty: %v", m.Name, name, m.Labels)
				}
			}
		}
	}
	if exported != 3 {
		t.Errorf("%d series were queued for OTLP, want the three", exported)
	}
	twice := probeOnce(t, server, probePath("empty_twice", target.URL, ""), nil)
	if twice.Code == http.StatusOK || !strings.HasSuffix(twice.Body.String(), "duplicate metric series \"m\"\n") {
		t.Errorf("two series told apart by an empty label: answered %d %q, want the duplicate refused", twice.Code, twice.Body)
	}
}
