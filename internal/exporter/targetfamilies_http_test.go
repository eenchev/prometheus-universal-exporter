//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const familiesTargetBody = "# TYPE build info\n# HELP build Build information.\nbuild_info{version=\"1.2\"} 1\n" +
	"# TYPE mode stateset\nmode{mode=\"on\"} 1\nmode{mode=\"off\"} 0\n" +
	"# TYPE wait_seconds counter\n# UNIT wait_seconds seconds\nwait_seconds_total 3\n" +
	"# TYPE size_bytes histogram\n# UNIT size_bytes bytes\nsize_bytes_bucket{le=\"+Inf\"} 1\nsize_bytes_sum 9\nsize_bytes_count 1\n" +
	"# TYPE queue_depth gauge\nqueue_depth 4\n# EOF\n"

// End to end: a target's info and stateset families, and its families'
// units, are written as the target wrote them in a probe's OpenMetrics
// answer, which a strict parser takes; the text format has them as gauges,
// without units, as before. An info renamed by a rule to a name without
// _info, and a family a rule scales, are written as gauges, without a unit.
func TestATargetsInfoStateSetAndUnitsThroughAProbe(t *testing.T) {
	target := exemplarTarget(t, familiesTargetBody)
	changed := passthrough("changed", "", "")
	scale := 1000.0
	changed.Metrics = []model.MetricRule{{Name: "build", Expression: "^build_info$"}, {Name: "wait_ms_total", Expression: "^wait_seconds_total$", Scale: &scale}, {Expression: "^mode$"}}
	server := verboseServer(t, false, passthrough("plain", "", ""), changed)
	server.logger = testutil.QuietLogger(t)
	probe := func(collector, accept string) string {
		r := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), http.Header{"Accept": {accept}})
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		return r.Body.String()
	}
	want := "# TYPE build info\n# HELP build Build information.\nbuild_info{version=\"1.2\"} 1\n" +
		"# TYPE mode stateset\nmode{mode=\"on\"} 1\nmode{mode=\"off\"} 0\n" +
		"# TYPE wait_seconds counter\n# UNIT wait_seconds seconds\nwait_seconds_total 3\n" +
		"# TYPE size_bytes histogram\n# UNIT size_bytes bytes\nsize_bytes_bucket{le=\"+Inf\"} 1\nsize_bytes_sum 9\nsize_bytes_count 1\n" +
		"# TYPE queue_depth gauge\nqueue_depth 4\n# EOF\n"
	got := probe("plain", "application/openmetrics-text")
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if err := strictOpenMetricsError(got); err != nil {
		t.Errorf("a strict parser refuses the answer: %v", err)
	}
	if text := probe("plain", "text/plain"); !strings.Contains(text, "# TYPE build_info gauge\n") || !strings.Contains(text, "# TYPE mode gauge\n") || strings.Contains(text, "UNIT") {
		t.Errorf("the text format:\n%s", text)
	}
	got = probe("changed", "application/openmetrics-text")
	for _, want := range []string{"# TYPE build gauge\n# HELP build Build information.\nbuild{version=\"1.2\"} 1\n", "# TYPE wait_ms counter\nwait_ms_total 3000\n", "# TYPE mode stateset\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
}

// A family is written as the info or the stateset it was read as only while
// every series of it can be one: of the value 1, or of 0 or 1 with its state
// label; else, or beside a gauge of the info's name without _info, it is a
// gauge. A family's UNIT is written while its series agree on it and its
// name ends in it.
func TestAnInfoOrAStateSetFallsBackToAGauge(t *testing.T) {
	info := &model.OpenMetricsFamily{Kind: model.GaugeKindInfo}
	stateset := &model.OpenMetricsFamily{Kind: model.GaugeKindStateSet}
	seconds := &model.OpenMetricsFamily{Unit: "seconds"}
	gauge := func(name string, value float64, kind *model.OpenMetricsFamily, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Type: model.GaugeMetricType, Value: value, Labels: labels, OpenMetrics: kind}
	}
	got := string(appendOpenMetrics(nil, &model.MetricSet{Metrics: []model.Metric{
		gauge("a_info", 2, info, nil),
		gauge("b_info", 1, info, nil), gauge("b", 5, nil, nil),
		gauge("c_info", 1, info, nil),
		gauge("s", 2, stateset, map[string]string{"s": "x"}),
		gauge("t", 1, stateset, map[string]string{"other": "x"}),
		gauge("u", 1, stateset, map[string]string{"u": "x"}),
		gauge("d_seconds", 1, seconds, map[string]string{"x": "1"}), gauge("d_seconds", 2, nil, map[string]string{"x": "2"}),
		gauge("e_seconds", 1, seconds, nil),
	}}))
	for _, want := range []string{
		"# TYPE a_info gauge\n", "# TYPE b_info gauge\n", "# TYPE b gauge\n", "# TYPE c info\nc_info 1\n",
		"# TYPE s gauge\n", "# TYPE t gauge\n", "# TYPE u stateset\n",
		"# TYPE d_seconds gauge\nd_seconds{x=\"1\"}", "# TYPE e_seconds gauge\n# UNIT e_seconds seconds\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if err := strictOpenMetricsError(got); err != nil {
		t.Errorf("a strict parser refuses the answer: %v", err)
	}
}

// OTLP gives a series the unit its target's UNIT gave it, as a UCUM code
// where it is one of the units a name's suffix stands for, and else as it
// was written; under otlp.metric_names: otel the name's own suffix comes
// first.
func TestOTLPCarriesATargetsUnit(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "wait_seconds", Type: model.GaugeMetricType, Value: 1, OpenMetrics: &model.OpenMetricsFamily{Unit: "seconds"}},
		{Name: "jobs_requests", Type: model.GaugeMetricType, Value: 2, OpenMetrics: &model.OpenMetricsFamily{Unit: "requests"}},
		{Name: "plain", Type: model.GaugeMetricType, Value: 3},
	}}
	units := map[string]string{}
	for _, m := range otlpMetrics(set, "1", nil) {
		units[m.Name] = m.Unit
	}
	if units["wait_seconds"] != "s" || units["jobs_requests"] != "requests" || units["plain"] != "" {
		t.Errorf("units %v", units)
	}
	out, _ := otlpPoints(set, 0, true, "1", nil, nil)
	units = map[string]string{}
	for _, m := range out {
		units[m.Name] = m.Unit
	}
	if units["wait"] != "s" || units["jobs_requests"] != "requests" {
		t.Errorf("otel units %v", units)
	}
}
