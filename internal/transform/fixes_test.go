package transform

import (
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// runBody decodes body with c's decoder, the Content-Type given, and
// transforms it.
func runBody(t *testing.T, c model.Collector, contentType, body string) (*model.MetricSet, error) {
	t.Helper()
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{"Content-Type": {contentType}}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	return Transform(t.Context(), d, r, &c, "python3")
}

func byName(set *model.MetricSet) map[string]model.Metric {
	out := map[string]model.Metric{}
	for _, m := range set.Metrics {
		out[m.Name] = m
	}
	return out
}

// A prometheus rule without a type keeps the type of each series it passes
// through; one with a type gives it.
func TestAPrometheusRuleKeepsTheSourceType(t *testing.T) {
	body := "# TYPE req_total counter\nreq_total 5\n# TYPE req_seconds histogram\nreq_seconds_bucket{le=\"1\"} 1\nreq_seconds_bucket{le=\"+Inf\"} 2\nreq_seconds_sum 3\nreq_seconds_count 2\n# TYPE up gauge\nup 1\n"
	c := model.Collector{Name: "p", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
		Metrics: []model.MetricRule{{Expression: "^req"}, {Expression: "^up$", Type: model.UntypedMetricType}}}
	set, err := runBody(t, c, "text/plain", body)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(set)
	if got["req_total"].Type != model.CounterMetricType || got["req_seconds"].Type != model.HistogramMetricType || got["req_seconds"].Histogram == nil || got["up"].Type != model.UntypedMetricType {
		t.Fatalf("types %+v", got)
	}
}

// NaN and the infinities reach a script as floats and come back from it as
// floats, in data and in metric(...), where Go's JSON has no form for them.
func TestPythonCarriesNonFiniteNumbers(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "nan", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "python", Script: `
for s in data["metrics"]:
    metric(name="copy_" + s["name"], value=s["value"])
metric(name="made_nan", value=float("nan"))
metric(name="made_inf", value=float("-inf"))
`}}
	set, err := runBody(t, c, "text/plain", "a NaN\nb +Inf\nc 1\n")
	if err != nil {
		t.Fatal(err)
	}
	got := byName(set)
	if !math.IsNaN(got["copy_a"].Value) || !math.IsInf(got["copy_b"].Value, 1) || got["copy_c"].Value != 1 || !math.IsNaN(got["made_nan"].Value) || !math.IsInf(got["made_inf"].Value, -1) {
		t.Fatalf("%+v", set.Metrics)
	}
	// A pre-script's data carries them back too.
	c = model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "json"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "jq", PreScript: `data = {"v": float("inf"), "n": data["n"]}`},
		Metrics:   []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: ".v"}, {Name: "n", Type: model.GaugeMetricType, Expression: ".n"}}}
	set, err = runBody(t, c, "application/json", `{"n": 2}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := byName(set); !math.IsInf(got["v"].Value, 1) || got["n"].Value != 2 {
		t.Fatalf("%+v", set.Metrics)
	}
}

// metric(...) takes what the other transforms take: a numeric string, a
// bool, a float timestamp in milliseconds; anything else fails naming the
// metric. A metric appended to metrics by hand is read the same way.
func TestPythonMetricValues(t *testing.T) {
	requirePython(t)
	c := workerCollector("values", `
import time
metric(name="s", value=" 12 ")
metric(name="b", value=True)
metric(name="t", value=1, timestamp=1727000000123.9)
metrics.append({"name": "raw", "type": "gauge", "value": "7", "labels": {}, "help": "", "timestamp": 1727000000000.5})
`)
	set, err := runWorkerScript(t, c)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(set)
	if got["s"].Value != 12 || got["b"].Value != 1 || got["t"].Timestamp == nil || *got["t"].Timestamp != 1727000000123 || got["raw"].Value != 7 || *got["raw"].Timestamp != 1727000000000 {
		t.Fatalf("%+v", set.Metrics)
	}
	for script, want := range map[string]string{
		`metric(name="x", value="twelve")`:                              "metric 'x' value 'twelve' is not a number",
		`metric(name="x", value=None)`:                                  "metric 'x' value None is not a number",
		`metric(name="x", value=1, timestamp=float("nan"))`:             "metric 'x' timestamp is not a number of milliseconds",
		`metrics.append({"name": "x", "type": "gauge", "value": "no"})`: `metric "x" value no is not a number`,
	} {
		if _, err := runWorkerScript(t, workerCollector("bad", script)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v, want %q", script, err, want)
		}
	}
}

// A script reading Prometheus exposition gets each series as a mapping,
// a histogram's buckets and a summary's quantiles included.
func TestPythonGetsPrometheusSeries(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "prom", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "python", Script: `
for s in data["metrics"]:
    if s["type"] == "histogram":
        metric(name="h_buckets", value=len(s["buckets"]))
        metric(name="h_first_le", value=s["buckets"][0]["le"])
        metric(name="h_inf", value=1 if s["buckets"][-1]["le"] == float("inf") else 0)
        metric(name="h_count", value=s["count"])
    elif s["type"] == "summary":
        metric(name="q", value=s["quantiles"][0]["value"])
    else:
        metric(name="v_" + s["name"], value=s["value"], labels=s["labels"], timestamp=s.get("timestamp"))
`}}
	body := "# TYPE h histogram\nh_bucket{le=\"0.5\"} 1\nh_bucket{le=\"+Inf\"} 3\nh_sum 2\nh_count 3\n# TYPE s summary\ns{quantile=\"0.9\"} 7\ns_sum 1\ns_count 1\n# TYPE g gauge\ng{job=\"x\"} 4 1727000000000\n"
	set, err := runBody(t, c, "text/plain", body)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(set)
	if got["h_buckets"].Value != 2 || got["h_first_le"].Value != 0.5 || got["h_inf"].Value != 1 || got["h_count"].Value != 3 || got["q"].Value != 7 || got["v_g"].Value != 4 || got["v_g"].Labels["job"] != "x" || *got["v_g"].Timestamp != 1727000000000 {
		t.Fatalf("%+v", set.Metrics)
	}
}

// Without a header row, a csv rule names its columns by number, from 1.
func TestCSVColumnsByNumber(t *testing.T) {
	header := false
	c := model.Collector{Name: "csv", Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: &header}}, Transform: model.TransformConfig{Type: "csv"},
		Metrics: []model.MetricRule{{Name: "cpu", Type: model.GaugeMetricType, Expression: "2", Labels: []model.LabelRule{{Name: "host", Expression: "1"}}}}}
	set, err := runBody(t, c, "text/csv", "web01,0.5\nweb02,0.7\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 || set.Metrics[0].Labels["host"] != "web01" || set.Metrics[1].Value != 0.7 {
		t.Fatalf("%+v", set.Metrics)
	}
}

// An XPath expression that computes a value makes one series of it; a label
// may compute its text.
func TestXPathComputedValues(t *testing.T) {
	c := model.Collector{Name: "x", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
		Metrics: []model.MetricRule{
			{Name: "jobs", Type: model.GaugeMetricType, Expression: "count(//job)", Labels: []model.LabelRule{{Name: "site", Expression: "normalize-space(/status/@site)"}}},
			{Name: "size", Type: model.GaugeMetricType, Expression: "sum(//job/@size)"},
			{Name: "healthy", Type: model.GaugeMetricType, Expression: "/status/@state = 'ok'"},
			{Name: "load", Type: model.GaugeMetricType, Expression: "string(/status/@load)"},
			{Name: "per_job", Type: model.GaugeMetricType, Expression: "//job/@size", Labels: []model.LabelRule{{Name: "name", Expression: "normalize-space(../@name)"}}},
		}}
	set, err := runBody(t, c, "application/xml", `<status site="  eu  west " state="ok" load="0.25"><job name=" a " size="2"/><job name="b" size="3"/></status>`)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(set)
	if got["jobs"].Value != 2 || got["jobs"].Labels["site"] != "eu west" || got["size"].Value != 5 || got["healthy"].Value != 1 || got["load"].Value != 0.25 {
		t.Fatalf("%+v", set.Metrics)
	}
	names := map[string]bool{}
	for _, m := range set.Metrics {
		if m.Name == "per_job" {
			names[m.Labels["name"]] = true
		}
	}
	if !names["a"] || !names["b"] {
		t.Fatalf("per job labels %v", names)
	}
	// A number that is not one is NaN: the rule's missing value.
	c.Metrics = []model.MetricRule{{Name: "bad", Type: model.GaugeMetricType, Expression: "number(//job/@name)", ErrorMode: model.ErrorModeFail}}
	if _, err := runBody(t, c, "application/xml", `<status><job name="a"/></status>`); err == nil || !strings.Contains(err.Error(), `computed NaN, not a number`) {
		t.Fatalf("err=%v", err)
	}
}

// truncate: true applies to a label rename_labels renames, and to the labels
// of a prometheus rule without a name.
func TestTruncationBeforeRenames(t *testing.T) {
	long := strings.Repeat("x", 40)
	c := model.Collector{Name: "t", Decoder: model.DecoderConfig{Type: "json"}, Limits: model.Limits{MaxLabelValueLength: 10},
		Transform: model.TransformConfig{Type: "jq", RenameLabels: map[string]string{"desc": "description"}},
		Metrics:   []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: ".v", Labels: []model.LabelRule{{Name: "desc", Expression: ".d", Truncate: true}}}}}
	set, err := runBody(t, c, "application/json", `{"v":1,"d":"`+long+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Metrics[0].Labels["description"]; len(got) > 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("label %q", got)
	}
	c = model.Collector{Name: "p", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"}, Limits: model.Limits{MaxLabelValueLength: 10},
		Metrics: []model.MetricRule{{Expression: "^up$", Labels: []model.LabelRule{{Name: "note", Expression: "note", Truncate: true}}}}}
	set, err = runBody(t, c, "text/plain", `up{note="`+long+`"} 1`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Metrics[0].Labels["note"]; len(got) > 10 {
		t.Fatalf("label %q", got)
	}
}
