package transform

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

const reviewExposition = "# HELP req_total Requests.\n# TYPE req_total counter\nreq_total{code=\"200\"} 5\n# TYPE lat histogram\nlat_bucket{le=\"1\"} 1\nlat_bucket{le=\"+Inf\"} 2\nlat_sum 3\nlat_count 2\n# TYPE q summary\nq{quantile=\"0.9\"} 7\nq_sum 1\nq_count 1\n"

// A pre-script of a prometheus transform gets the series as
// {"metrics": [...]} and what it leaves is read back into series.
func TestAPrometheusPreScriptIsReadBack(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "pre", Decoder: model.DecoderConfig{Type: "prometheus"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "prometheus", PreScript: `
data["metrics"] = [s for s in data["metrics"] if s["name"] != "q"]
for s in data["metrics"]:
    if s["name"] == "req_total":
        s["value"] = s["value"] * 2
        s["labels"]["env"] = "prod"
`}}
	set, err := runBody(t, c, "text/plain", reviewExposition)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(set)
	req, lat := got["req_total"], got["lat"]
	if len(set.Metrics) != 2 || req.Value != 10 || req.Labels["env"] != "prod" || req.Type != model.CounterMetricType || req.Help != "Requests." {
		t.Fatalf("%+v", set.Metrics)
	}
	if lat.Histogram == nil || len(lat.Histogram.Buckets) != 2 || lat.Histogram.Count != 2 || lat.Histogram.Sum != 3 {
		t.Fatalf("histogram %+v", lat.Histogram)
	}
	for script, want := range map[string]string{
		`data = [1, 2]`:                                         `must leave data as {"metrics": [...]}`,
		`data["metrics"][0]["value"] = "many"`:                  `req_total value many is not a number`,
		`data["metrics"][1]["buckets"] = [{"le": "x"}]`:         `has a bucket that is not`,
		`data["metrics"][0]["timestamp"] = 1e30`:                `timestamp 1e+30 is not a number of milliseconds`,
		`data["metrics"].append({"type": "gauge", "value": 1})`: `has no name`,
	} {
		bad := c
		bad.Transform.PreScript = script
		if _, err := runBody(t, bad, "text/plain", reviewExposition); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", script, err, want)
		}
	}
}

// A histogram or summary keeps its type: a rule cannot give it another, nor
// another series its type; and a series typed as one must have its shape.
func TestAHistogramKeepsItsType(t *testing.T) {
	c := model.Collector{Name: "types", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
		Metrics: []model.MetricRule{{Expression: "^lat$", Type: model.CounterMetricType, ErrorMode: model.ErrorModeFail}}}
	if _, err := runBody(t, c, "text/plain", reviewExposition); err == nil || !strings.Contains(err.Error(), "a histogram or summary keeps its own type") {
		t.Fatalf("%v", err)
	}
	c.Metrics = []model.MetricRule{{Expression: "^req_total$", Type: model.HistogramMetricType, ErrorMode: model.ErrorModeFail}}
	if _, err := runBody(t, c, "text/plain", reviewExposition); err == nil || !strings.Contains(err.Error(), "no other series can become one") {
		t.Fatalf("%v", err)
	}
	for _, m := range []model.Metric{
		{Name: "h", Type: model.HistogramMetricType, Value: 3},
		{Name: "s", Type: model.SummaryMetricType, Value: 3},
		{Name: "g", Type: model.GaugeMetricType, Histogram: &model.Histogram{}},
	} {
		set := model.MetricSet{Metrics: []model.Metric{m}}
		if err := set.Validate(model.Limits{}); err == nil {
			t.Errorf("%+v was valid", m)
		}
	}
}

// Integers beyond int64, which jq makes big, are numbers.
func TestBigIntegersAreNumbers(t *testing.T) {
	c := model.Collector{Name: "big", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{
			{Name: "bytes", Type: model.GaugeMetricType, Expression: ".bytes | tonumber", ErrorMode: model.ErrorModeFail},
			{Name: "sum", Type: model.GaugeMetricType, Expression: "9223372036854775807 + 1", ErrorMode: model.ErrorModeFail},
		}}
	set, err := runBody(t, c, "application/json", `{"bytes": "12345678901234567890"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := byName(set); got["bytes"].Value != 12345678901234567890 || got["sum"].Value != 9223372036854775808 {
		t.Fatalf("%+v", set.Metrics)
	}
}

// What a script prints is kept to its first 4 KiB, logged at debug level,
// and does not use up max_output_bytes.
func TestPrintedOutputIsCappedAndLogged(t *testing.T) {
	requirePython(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	c := workerCollector("printer", `
print("x" * 2000000)
metric(name="a", value=1)
`)
	c.Limits.MaxOutputBytes = 64 << 10
	set, err := runWorkerScript(t, c)
	if err != nil || len(set.Metrics) != 1 {
		t.Fatalf("%v %+v", err, set)
	}
	if !strings.Contains(logs.String(), "python transform printed") || !strings.Contains(logs.String(), "more characters") {
		t.Fatalf("logs: %.300s", logs.String())
	}
}

// A pre-script may leave numbers and None in CSV rows: a number is written as
// the other transforms write one, and None leaves the label out.
func TestCSVLabelsFromAPreScript(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "csvpre", Decoder: model.DecoderConfig{Type: "csv"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "csv", PreScript: `
for row in data:
    row["id"] = 1234567
    row["zone"] = None
`},
		Metrics: []model.MetricRule{{Name: "cpu", Type: model.GaugeMetricType, Expression: "cpu", Labels: []model.LabelRule{{Name: "id", Expression: "id"}, {Name: "zone", Expression: "zone"}}}}}
	set, err := runBody(t, c, "text/csv", "cpu\n0.5\n")
	if err != nil {
		t.Fatal(err)
	}
	if labels := set.Metrics[0].Labels; labels["id"] != "1234567" || len(labels) != 1 {
		t.Fatalf("labels %v", labels)
	}
}

// Metrics a script appends itself are checked as metric(...) checks them.
func TestAppendedMetricsAreChecked(t *testing.T) {
	requirePython(t)
	set, err := runWorkerScript(t, workerCollector("appended", `
metrics.append({"name": "a", "value": 1, "labels": {"code": 200, "gone": None, "ratio": 0.5}})
`))
	if err != nil {
		t.Fatal(err)
	}
	if m := set.Metrics[0]; m.Type != model.GaugeMetricType || m.Labels["code"] != "200" || m.Labels["ratio"] != "0.5" || len(m.Labels) != 2 {
		t.Fatalf("%+v", m)
	}
	for script, want := range map[string]string{
		`metrics.append({"name": "x", "value": None})`:                       `metric "x" value is None, not a number`,
		`metrics.append({"name": "x", "value": 1, "labels": {"a": [1, 2]}})`: `metric "x" label "a" is an array`,
		`metrics.append({"name": "x", "value": 1, "timestamp": 1e30})`:       `metric "x" timestamp 1e+30 is not a number of milliseconds`,
		`metric(name="x", value=1, timestamp=1e22)`:                          `timestamp 1e+22 is not a number of milliseconds`,
	} {
		if _, err := runWorkerScript(t, workerCollector("bad", script)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", script, err, want)
		}
	}
}
