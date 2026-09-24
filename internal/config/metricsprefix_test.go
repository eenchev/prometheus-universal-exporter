package config

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// A collector's metrics_prefix is joined with "_" to every metric it exports.

func TestMetricsPrefixValidation(t *testing.T) {
	valid := []string{"grafana", "vendor_eu", "a", "A1", "grafana_cloud_v2", "Vendor", "x9_2"}
	invalid := []string{
		"_grafana",   // leading underscore; "__" is reserved by Prometheus
		"__grafana",  // reserved
		"grafana_",   // the joining "_" would double it
		"a__b",       // double underscore
		"1grafana",   // no metric name starts with a digit
		"graf-ana",   // not a metric name character
		"graf:ana",   // ":" is reserved for recording rules
		"graf ana",   // space
		"grafanä",    // not ASCII
		" grafana",   // surrounding space
		"grafana\n",  // newline
		"_",          // nothing but a separator
		"grafana__x", // double underscore in the middle
	}
	for _, prefix := range valid {
		c := testutil.Collector("prefixed", "text")
		c.MetricsPrefix = prefix
		if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
			t.Errorf("%q: %v", prefix, err)
		}
	}
	for _, prefix := range invalid {
		c := testutil.Collector("prefixed", "text")
		c.MetricsPrefix = prefix
		err := Validate(&model.Config{Collectors: []model.Collector{c}})
		if err == nil {
			t.Errorf("%q was accepted", prefix)
			continue
		}
		for _, want := range []string{`collector "prefixed"`, "invalid metrics_prefix", "start with a letter", `joined to each metric name with "_"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: error %q should mention %q", prefix, err, want)
			}
		}
	}
}

// An unset prefix, or an empty one, leaves names exactly as declared.
func TestNoMetricsPrefixLeavesNamesAlone(t *testing.T) {
	for _, conf := range []string{"", `metrics_prefix: ""`} {
		c := loadPrefixedCollector(t, conf)
		set := transformText(t, &c, "value=7\n")
		if len(set.Metrics) != 1 || set.Metrics[0].Name != "demo_value" {
			t.Fatalf("%q: metrics=%+v", conf, set.Metrics)
		}
	}
}

// The key is read from YAML, where unknown keys are rejected.
func TestMetricsPrefixIsReadFromTheConfigurationFile(t *testing.T) {
	c := loadPrefixedCollector(t, "metrics_prefix: vendor")
	if c.MetricsPrefix != "vendor" {
		t.Fatalf("metrics_prefix=%q", c.MetricsPrefix)
	}
	set := transformText(t, &c, "value=7\n")
	if len(set.Metrics) != 1 || set.Metrics[0].Name != "vendor_demo_value" || set.Metrics[0].Value != 7 {
		t.Fatalf("metrics=%+v", set.Metrics)
	}
}

func loadPrefixedCollector(t *testing.T, line string) model.Collector {
	t.Helper()
	document := `collectors:
  - name: prefixed
    ` + line + `
    request:
      type: http
    response:
      format: text
    transform:
      type: regex
    metrics:
      - name: demo_value
        description: A value
        type: gauge
        error_mode: log
        expression: 'value=(\d+)'
`
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Collectors[0]
}

func transformText(t *testing.T, c *model.Collector, body string) *model.MetricSet {
	t.Helper()
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"text/plain"}}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform.Transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Every transform's output is prefixed, since the prefix is applied once,
// after the transform, whichever it is.
func TestMetricsPrefixAppliesToEveryTransform(t *testing.T) {
	gauge := func(name, expression string, labels ...model.LabelRule) []model.MetricRule {
		return []model.MetricRule{{Name: name, Type: model.GaugeMetricType, Expression: expression, Labels: labels}}
	}
	tests := []struct {
		name        string
		contentType string
		body        string
		transform   model.TransformConfig
		metrics     []model.MetricRule
		want        []string
	}{
		{"jq", "application/json", `{"value": 3}`, model.TransformConfig{Type: "jq"}, gauge("demo_value", ".value"), []string{"acme_demo_value"}},
		{"yq", "application/yaml", "value: 3\n", model.TransformConfig{Type: "yq"}, gauge("demo_value", ".value"), []string{"acme_demo_value"}},
		{"regex", "text/plain", "value=3\n", model.TransformConfig{Type: "regex"}, gauge("demo_value", `value=(\d+)`), []string{"acme_demo_value"}},
		{"csv", "text/csv", "server,cpu\nweb01,3\n", model.TransformConfig{Type: "csv"}, gauge("server_cpu", "cpu"), []string{"acme_server_cpu"}},
		{"css", "text/html", `<p class="v">3</p>`, model.TransformConfig{Type: "css"}, gauge("demo_value", "p.v"), []string{"acme_demo_value"}},
		{"xpath", "application/xml", `<r><v>3</v></r>`, model.TransformConfig{Type: "xpath"}, gauge("demo_value", "//v"), []string{"acme_demo_value"}},
		// A prometheus transform passes source metrics through by their own
		// names, and a histogram keeps its family: the exposition adds _bucket,
		// _sum and _count to the prefixed name.
		{"prometheus passthrough", "text/plain; version=0.0.4",
			"# TYPE upstream_requests_total counter\nupstream_requests_total 5\n# TYPE upstream_latency_seconds histogram\nupstream_latency_seconds_bucket{le=\"+Inf\"} 1\nupstream_latency_seconds_sum 0.2\nupstream_latency_seconds_count 1\n",
			model.TransformConfig{Type: "prometheus"}, nil, []string{"acme_upstream_requests_total", "acme_upstream_latency_seconds"}},
		// ... and a rule that renames a source metric is prefixed as renamed.
		{"prometheus rename", "text/plain; version=0.0.4", "# TYPE upstream_requests_total counter\nupstream_requests_total 5\n",
			model.TransformConfig{Type: "prometheus"}, []model.MetricRule{{Name: "requests_total", Type: model.CounterMetricType, Expression: "^upstream_requests_total$"}}, []string{"acme_requests_total"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := model.Collector{Name: "prefixed", MetricsPrefix: "acme", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: test.transform, Metrics: test.metrics, Limits: model.Limits{MaxMetrics: 10}}
			if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
				t.Fatal(err)
			}
			r := &fetch.HTTPResponse{Body: []byte(test.body), Headers: http.Header{"Content-Type": {test.contentType}}}
			d, err := decode.Decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			set, err := transform.Transform(context.Background(), d, r, &c, "python3")
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, m := range set.Metrics {
				names = append(names, m.Name)
			}
			if strings.Join(names, ",") != strings.Join(test.want, ",") {
				t.Fatalf("names=%v, want %v", names, test.want)
			}
		})
	}
}

// Names a Python script emits are not known until the scrape, and are prefixed
// like any other.
func TestMetricsPrefixAppliesToPythonMetrics(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
	c := model.Collector{
		Name: "prefixed", MetricsPrefix: "acme", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Response:  model.ResponseConfig{Format: "text"},
		Transform: model.TransformConfig{Type: "python", Script: `metric(name="script_value", value=4)`},
		Metrics:   []model.MetricRule{}, Limits: model.Limits{MaxMetrics: 10, ScriptTimeout: model.Duration(5 * time.Second)},
	}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}
	set := transformText(t, &c, "anything")
	if len(set.Metrics) != 1 || set.Metrics[0].Name != "acme_script_value" || set.Metrics[0].Value != 4 {
		t.Fatalf("metrics=%+v", set.Metrics)
	}
}

// A declared name that would be too long once prefixed is refused at startup;
// a name produced at scrape time is checked when the scrape happens.
func TestMetricsPrefixRespectsTheNameLengthLimit(t *testing.T) {
	c := testutil.Collector("prefixed", "text")
	c.MetricsPrefix = "acme"
	c.Limits.MaxMetricNameLength = len("acme_demo_value") - 1
	err := Validate(&model.Config{Collectors: []model.Collector{c}})
	if err == nil || !strings.Contains(err.Error(), `metric "demo_value" is exported as "acme_demo_value", which is longer than limits.max_metric_name_length 14`) {
		t.Fatalf("err=%v", err)
	}

	c = testutil.Collector("prefixed", "text")
	c.MetricsPrefix = strings.Repeat("a", 199)
	err = Validate(&model.Config{Collectors: []model.Collector{c}})
	if err == nil || !strings.Contains(err.Error(), "leaves no room for a metric name") {
		t.Fatalf("err=%v", err)
	}

	// Exactly at the limit is fine.
	c = testutil.Collector("prefixed", "text")
	c.MetricsPrefix = "acme"
	c.Limits.MaxMetricNameLength = len("acme_demo_value")
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
		t.Fatal(err)
	}

	// At scrape time, the prefixed name goes through the same limit check as
	// every exported name.
	set := &model.MetricSet{Metrics: []model.Metric{{Name: "script_value", Type: model.GaugeMetricType, Value: 1}}}
	transform.ApplyMetricsPrefix(set, "acme")
	if err := set.Validate(model.Limits{MaxMetricNameLength: len("acme_script_value") - 1}); err == nil {
		t.Fatal("a prefixed name over the limit passed validation")
	}
}
