//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// enginePanicCollector is an xpath collector with a rule the XPath engine
// panics on once an element has the attribute its predicate asks a number
// of, between two rules that work. on_transform_error is ignore, which a
// rule's failure is not a matter of.
func enginePanicCollector(name, mode string, coalesce *bool) model.Collector {
	c := testutil.Collector(name, "xml")
	c.Transform = model.TransformConfig{Type: "xpath"}
	c.Coalesce = coalesce
	c.Metrics = []model.MetricRule{
		{Name: "cells", Type: model.GaugeMetricType, Expression: "count(//a)"},
		{Name: "marked", Type: model.GaugeMetricType, Expression: "count(//a[contains(@x, 5)])", ErrorMode: mode},
		{Name: "total", Type: model.GaugeMetricType, Expression: "sum(//a)"},
	}
	c.ErrorHandling = model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "ignore"}
	return c
}

// A panic of the XPath engine on what a target answered is the failure of
// the rule it was evaluating, through a probe as through the transform, for
// a collector whose probes are shared and one whose probes are not, and for
// a static target. Under log and ignore the probe answers 200 with the
// series of the other rules, the failure is counted for the rule in the
// self-metrics at every probe, and logged, under log, once for the probes
// that repeat it. Under fail the probe answers 502 as it does any rule's
// failure, naming the metric, and the failure is logged once. Nothing is
// answered 500, no connection is closed, and no stack is logged: before,
// the shared probe answered 500 `probe failed: internal error` with a stack
// at every probe, the other closed the connection, and the static target
// was down, whatever the rule's error_mode.
func TestAnEnginePanicThroughAProbeIsItsRulesFailure(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body := `<r><a>7</a><a>2</a></r>`
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer target.Close()
	alone := false
	var collectors []model.Collector
	for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail} {
		collectors = append(collectors, enginePanicCollector("shared_"+mode, mode, nil), enginePanicCollector("alone_"+mode, mode, &alone))
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "logging", Collector: "shared_log", Target: target.URL},
		{Name: "failing", Collector: "shared_fail", Target: target.URL},
	}}
	server := newStaticServer(t, &model.Config{Collectors: collectors}, file)
	exporter := httptest.NewServer(server.Handler())
	defer exporter.Close()
	get := func(path string) (int, string) {
		t.Helper()
		response, err := http.Get(exporter.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return response.StatusCode, string(answer)
	}
	probeOf := func(collector string) (int, string) {
		t.Helper()
		return get("/probe?collector=" + collector + "&target=" + url.QueryEscape(target.URL))
	}

	// An answer the engine reads: every rule gives its series.
	for _, c := range collectors {
		if code, answer := probeOf(c.Name); code != http.StatusOK || !strings.Contains(answer, "\ncells 2\n") || !strings.Contains(answer, "\nmarked 0\n") || !strings.Contains(answer, "\ntotal 9\n") {
			t.Fatalf("%s over an answer the engine reads: %d %s", c.Name, code, answer)
		}
	}

	// One it panics on, three times over.
	body = `<r><a x="5">7</a><a>2</a></r>`
	const said = `metric "marked" XPath "count(//a[contains(@x, 5)])" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`
	const probes = 3
	for _, c := range collectors {
		for range probes {
			code, answer := probeOf(c.Name)
			if strings.HasSuffix(c.Name, "_fail") {
				var failure map[string]any
				if err := json.Unmarshal([]byte(answer), &failure); err != nil || code != http.StatusBadGateway {
					t.Fatalf("%s: %d %s, want 502 and a JSON error", c.Name, code, answer)
				}
				if failure["stage"] != "metric" || failure["metric"] != "marked" || failure["collector"] != c.Name || failure["error"] != said {
					t.Errorf("%s: the failure %v, want that of the metric marked, saying %q", c.Name, failure, said)
				}
				continue
			}
			if code != http.StatusOK || !strings.Contains(answer, "\ncells 2\n") || !strings.Contains(answer, "\ntotal 9\n") || strings.Contains(answer, "marked") {
				t.Errorf("%s: %d %s, want 200 with the series of the other rules", c.Name, code, answer)
			}
		}
	}

	// The static targets: the one under log is up with the other rules'
	// series, the one under fail is down.
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	code, static := get(server.staticTargetsEndpoint())
	if code != http.StatusOK || !strings.Contains(static, `cells{static_target="logging"} 2`) || !strings.Contains(static, `total{static_target="logging"} 9`) || strings.Contains(static, `{static_target="failing"}`) || strings.Contains(static, "marked") {
		t.Errorf("the static targets answer %d:\n%s", code, static)
	}
	for series, want := range map[string]float64{
		`http_exporter_target_up{collector="shared_log",static_target="logging",target="` + target.URL + `"}`:  1,
		`http_exporter_target_up{collector="shared_fail",static_target="failing",target="` + target.URL + `"}`: 0,
	} {
		if got := seriesValue(t, static, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	// Counted for the rule at every probe that carried on, the static
	// target's among them, and never as a failure of the transform.
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_rule_failures_total{collector="shared_log",metric="marked"}`:    probes + 1,
		`http_exporter_rule_failures_total{collector="alone_log",metric="marked"}`:     probes,
		`http_exporter_rule_failures_total{collector="shared_ignore",metric="marked"}`: probes,
		`http_exporter_rule_failures_total{collector="alone_ignore",metric="marked"}`:  probes,
		`http_exporter_rule_failures_total{collector="shared_log",metric="cells"}`:     0,
		`http_exporter_transform_errors_total{collector="shared_log"}`:                 0,
		`http_exporter_transform_errors_total{collector="alone_ignore"}`:               0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	// The log: one line for each collector under log and under fail and
	// for each static target, the repeats held back, none under ignore, and
	// neither a stack nor an internal error anywhere.
	lines := map[string]int{}
	for _, record := range logRecords(t, logs) {
		text := jsonText(t, record)
		if strings.Contains(text, "goroutine ") || strings.Contains(text, "internal error") || strings.Contains(text, "panic") {
			t.Errorf("the log has a line of a panic: %s", text)
		}
		if !strings.Contains(text, "the XPath engine failed on it") {
			continue
		}
		collector, _ := record["collector"].(string)
		message, _ := record["msg"].(string)
		lines[collector+": "+message]++
		if record["metric"] != "marked" || record["error"] != said {
			t.Errorf("the failure is logged as %s", text)
		}
	}
	for line, want := range map[string]int{
		// The probes' and the static target's.
		"shared_log: metric extraction failed": 2, "alone_log: metric extraction failed": 1,
		"shared_fail: probe failed": 1, "alone_fail: probe failed": 1, "shared_fail: static target scrape failed": 1,
	} {
		if lines[line] != want {
			t.Errorf("%d lines %q, want %d; all of them: %v", lines[line], line, want, lines)
		}
	}
	for line := range lines {
		if strings.Contains(line, "ignore") {
			t.Errorf("a rule under ignore is logged: %v", lines)
		}
	}
}

// The engine's failure is what a rule is logged and counted with when nodes
// of the rule had failed before the engine did. A table has a cell without
// a value on every probe, which the rule `load` under log reports and the
// log holds back as a repeat; then a cell gets the attribute the label's
// predicate stumbles over. The probe still answers 200, now without any
// series of the rule, and the log has a new line that says the engine
// failed: before, the rule was reported with the empty cell as on the days
// before, held back as a repeat, and nothing said its series were gone. The
// rule is counted once at each probe, as one failure of the whole, and its
// missing value only on the probes where that was what failed. Under ignore
// the counts are the same and nothing is logged; under fail the empty cell
// ends the probe before the engine comes to the other.
func TestAnEngineFailureAfterNodesThatFailedIsTheOneLoggedAndCounted(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body := `<r><td h="a">1</td><td h="b"></td><td h="c">2</td><td h="d">3</td></r>`
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer target.Close()
	var collectors []model.Collector
	for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail} {
		c := testutil.Collector("table_"+mode, "xml")
		c.Transform = model.TransformConfig{Type: "xpath"}
		c.Metrics = []model.MetricRule{
			{Name: "cells", Type: model.GaugeMetricType, Expression: "count(//td)"},
			{Name: "load", Type: model.GaugeMetricType, Expression: "//td", ErrorMode: mode, Labels: []model.LabelRule{{Name: "host", Expression: "@h"}, {Name: "marked", Expression: "contains(@x, 5)"}}},
		}
		collectors = append(collectors, c)
	}
	server := newStaticServer(t, &model.Config{Collectors: collectors}, nil)
	exporter := httptest.NewServer(server.Handler())
	defer exporter.Close()
	probeOf := func(collector string) (int, string) {
		t.Helper()
		response, err := http.Get(exporter.URL + "/probe?collector=" + collector + "&target=" + url.QueryEscape(target.URL))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(answer)
	}
	const (
		probes = 2
		blank  = `metric "load" value is missing for node 1: XPath "//td" selected a node without a value`
		engine = `metric "load" label "marked": XPath "contains(@x, 5)" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`
	)
	for day, stumbles := range []bool{false, true} {
		if stumbles {
			body = `<r><td h="a">1</td><td h="b"></td><td h="c" x="5">2</td><td h="d">3</td></r>`
		}
		for _, c := range collectors {
			for range probes {
				code, answer := probeOf(c.Name)
				if strings.HasSuffix(c.Name, "_fail") {
					if code != http.StatusBadGateway || !strings.Contains(answer, "value is missing for node 1") {
						t.Errorf("day %d, %s: %d %s, want 502 for the cell without a value", day, c.Name, code, answer)
					}
					continue
				}
				if code != http.StatusOK || !strings.Contains(answer, "\ncells 4\n") || strings.Contains(answer, `load{host="a"`) == stumbles {
					t.Errorf("day %d, %s: %d %s, want 200, with the rule's series only while the engine reads the table", day, c.Name, code, answer)
				}
			}
		}
	}

	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_rule_failures_total{collector="table_log",metric="load"}`:    2 * probes,
		`http_exporter_rule_failures_total{collector="table_ignore",metric="load"}`: 2 * probes,
		`http_exporter_missing_keys_total{collector="table_log"}`:                   probes,
		`http_exporter_missing_keys_total{collector="table_ignore"}`:                probes,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	// The log, in order: the cell without a value once, its repeat held
	// back, then the engine's failure once, each as one failure of the rule.
	var logged []string
	for _, record := range logRecords(t, logs) {
		if record["msg"] != "metric extraction failed" {
			continue
		}
		if record["collector"] != "table_log" || record["metric"] != "load" || record["failures"] != float64(1) {
			t.Errorf("the line %s, want one failure of the rule under log", jsonText(t, record))
		}
		failure, _ := record["error"].(string)
		logged = append(logged, failure)
	}
	if len(logged) != 2 || logged[0] != blank || logged[1] != engine {
		t.Errorf("the failures logged are %q, want the cell without a value and then the engine's", logged)
	}
}

// A runtime error the engine fails with on a rule under ignore leaves no
// line in the log at the level the exporter runs at, and no stack anywhere a
// scraper or the log's reader sees; a debug probe's report, which has the
// lines of every level, shows the line with the stack, by which a fault of
// the exporter's own can be found. The same failure with other numbers, over
// a text of another length, is logged once under log: its repeat is held
// back.
func TestARuntimeErrorOfTheEngineThroughAProbe(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body := `<r><a>ab</a></r>`
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	}))
	defer target.Close()
	var collectors []model.Collector
	for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore} {
		c := testutil.Collector("cut_"+mode, "xml")
		c.Transform = model.TransformConfig{Type: "xpath"}
		c.Metrics = []model.MetricRule{
			{Name: "cells", Type: model.GaugeMetricType, Expression: "count(//a)"},
			{Name: "cut", Type: model.GaugeMetricType, Expression: "substring(//a, 2, 10)", ErrorMode: mode},
		}
		collectors = append(collectors, c)
	}
	server := newStaticServer(t, &model.Config{Collectors: collectors}, nil)
	probe := func(collector, extra string) string {
		t.Helper()
		response := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL)+extra, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("%s%s: %d %s", collector, extra, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	for _, text := range []string{"ab", "abcde", "abc"} {
		body = `<r><a>` + text + `</a></r>`
		for _, c := range collectors {
			if answer := probe(c.Name, ""); !strings.Contains(answer, "\ncells 1\n") || strings.Contains(answer, "goroutine ") || strings.Contains(answer, "runtime error") {
				t.Errorf("%s over %q: %s", c.Name, text, answer)
			}
		}
	}
	server.SetProbeDebug(true)
	report := probe("cut_ignore", "&debug=true")
	if !strings.Contains(report, "the XPath engine failed with a runtime error") || !strings.Contains(report, "goroutine ") || !strings.Contains(report, "slice bounds out of range [:4] with length 3") {
		t.Errorf("the debug probe's report has no line with the stack:\n%s", report)
	}
	var logged []string
	for _, record := range logRecords(t, logs) {
		text := jsonText(t, record)
		if strings.Contains(text, "goroutine ") || record["collector"] == "cut_ignore" && record["msg"] != "probe debug report served" {
			t.Errorf("the log has the line %s", text)
		}
		if record["msg"] == "metric extraction failed" {
			failure, _ := record["error"].(string)
			logged = append(logged, failure)
		}
	}
	if want := `metric "cut" XPath "substring(//a, 2, 10)" cannot be evaluated: the XPath engine failed on it: runtime error: slice bounds out of range [:3] with length 2`; len(logged) != 1 || logged[0] != want {
		t.Errorf("the failures logged are %q, want %q once", logged, want)
	}
}
