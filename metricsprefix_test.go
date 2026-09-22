package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
		c := testCollector("prefixed", "text")
		c.MetricsPrefix = prefix
		if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
			t.Errorf("%q: %v", prefix, err)
		}
	}
	for _, prefix := range invalid {
		c := testCollector("prefixed", "text")
		c.MetricsPrefix = prefix
		err := (&Config{Collectors: []Collector{c}}).Validate()
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
	for _, config := range []string{"", `metrics_prefix: ""`} {
		c := loadPrefixedCollector(t, config)
		set := transformText(t, &c, "value=7\n")
		if len(set.Metrics) != 1 || set.Metrics[0].Name != "demo_value" {
			t.Fatalf("%q: metrics=%+v", config, set.Metrics)
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

func loadPrefixedCollector(t *testing.T, line string) Collector {
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
	cfg, err := LoadConfig(writeFile(t, "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Collectors[0]
}

func transformText(t *testing.T, c *Collector, body string) *MetricSet {
	t.Helper()
	r := &HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"text/plain"}}}
	d, err := decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Every transform's output is prefixed, since the prefix is applied once,
// after the transform, whichever it is.
func TestMetricsPrefixAppliesToEveryTransform(t *testing.T) {
	gauge := func(name, expression string, labels ...LabelRule) []MetricRule {
		return []MetricRule{{Name: name, Type: GaugeMetricType, Expression: expression, Labels: labels}}
	}
	tests := []struct {
		name        string
		contentType string
		body        string
		transform   TransformConfig
		metrics     []MetricRule
		want        []string
	}{
		{"jq", "application/json", `{"value": 3}`, TransformConfig{Type: "jq"}, gauge("demo_value", ".value"), []string{"acme_demo_value"}},
		{"yq", "application/yaml", "value: 3\n", TransformConfig{Type: "yq"}, gauge("demo_value", ".value"), []string{"acme_demo_value"}},
		{"regex", "text/plain", "value=3\n", TransformConfig{Type: "regex"}, gauge("demo_value", `value=(\d+)`), []string{"acme_demo_value"}},
		{"csv", "text/csv", "server,cpu\nweb01,3\n", TransformConfig{Type: "csv"}, gauge("server_cpu", "cpu"), []string{"acme_server_cpu"}},
		{"css", "text/html", `<p class="v">3</p>`, TransformConfig{Type: "css"}, gauge("demo_value", "p.v"), []string{"acme_demo_value"}},
		{"xpath", "application/xml", `<r><v>3</v></r>`, TransformConfig{Type: "xpath"}, gauge("demo_value", "//v"), []string{"acme_demo_value"}},
		// A prometheus transform passes source metrics through by their own
		// names, and a histogram keeps its family: the exposition adds _bucket,
		// _sum and _count to the prefixed name.
		{"prometheus passthrough", "text/plain; version=0.0.4",
			"# TYPE upstream_requests_total counter\nupstream_requests_total 5\n# TYPE upstream_latency_seconds histogram\nupstream_latency_seconds_bucket{le=\"+Inf\"} 1\nupstream_latency_seconds_sum 0.2\nupstream_latency_seconds_count 1\n",
			TransformConfig{Type: "prometheus"}, nil, []string{"acme_upstream_requests_total", "acme_upstream_latency_seconds"}},
		// ... and a rule that renames a source metric is prefixed as renamed.
		{"prometheus rename", "text/plain; version=0.0.4", "# TYPE upstream_requests_total counter\nupstream_requests_total 5\n",
			TransformConfig{Type: "prometheus"}, []MetricRule{{Name: "requests_total", Type: CounterMetricType, Expression: "^upstream_requests_total$"}}, []string{"acme_requests_total"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := Collector{Name: "prefixed", MetricsPrefix: "acme", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: test.transform, Metrics: test.metrics, Limits: Limits{MaxMetrics: 10}}
			if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
				t.Fatal(err)
			}
			r := &HTTPResponse{Body: []byte(test.body), Headers: http.Header{"Content-Type": {test.contentType}}}
			d, err := decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			set, err := transform(context.Background(), d, r, &c, "python3")
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
	c := Collector{
		Name: "prefixed", MetricsPrefix: "acme", Request: RequestConfig{Type: RequestTypeHTTP},
		Response:  ResponseConfig{Format: "text"},
		Transform: TransformConfig{Type: "python", Script: `metric(name="script_value", value=4)`},
		Metrics:   []MetricRule{}, Limits: Limits{MaxMetrics: 10, ScriptTimeout: Duration(5 * time.Second)},
	}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	set := transformText(t, &c, "anything")
	if len(set.Metrics) != 1 || set.Metrics[0].Name != "acme_script_value" || set.Metrics[0].Value != 4 {
		t.Fatalf("metrics=%+v", set.Metrics)
	}
}

// End to end through /probe: HELP and TYPE lines carry the prefixed name, a
// histogram's series are the prefixed family, other collectors are untouched,
// and the exporter's own metrics are never prefixed.
func TestMetricsPrefixOnTheProbeResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# HELP upstream_latency_seconds Latency\n# TYPE upstream_latency_seconds histogram\nupstream_latency_seconds_bucket{le=\"1\"} 2\nupstream_latency_seconds_bucket{le=\"+Inf\"} 3\nupstream_latency_seconds_sum 1.5\nupstream_latency_seconds_count 3\n"))
	}))
	defer upstream.Close()

	prefixed := Collector{Name: "prefixed", MetricsPrefix: "acme", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: TransformConfig{Type: "prometheus"}}
	plain := Collector{Name: "plain", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: TransformConfig{Type: "prometheus"}}
	cfg := &Config{Collectors: []Collector{prefixed, plain}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", slog.Default()), "python3", slog.Default())

	body := probeOnce(t, server, "/probe?collector=prefixed&target="+url.QueryEscape(upstream.URL), nil).Body.String()
	for _, want := range []string{
		"# HELP acme_upstream_latency_seconds Latency\n",
		"# TYPE acme_upstream_latency_seconds histogram\n",
		`acme_upstream_latency_seconds_bucket{le="1"} 2` + "\n",
		`acme_upstream_latency_seconds_bucket{le="+Inf"} 3` + "\n",
		"acme_upstream_latency_seconds_sum 1.5\n",
		"acme_upstream_latency_seconds_count 3\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\nupstream_latency_seconds") || strings.HasPrefix(body, "upstream_latency_seconds") {
		t.Errorf("an unprefixed series leaked:\n%s", body)
	}

	body = probeOnce(t, server, "/probe?collector=plain&target="+url.QueryEscape(upstream.URL), nil).Body.String()
	if !strings.Contains(body, "upstream_latency_seconds_count 3\n") || strings.Contains(body, "acme_") {
		t.Errorf("a collector without a prefix was prefixed:\n%s", body)
	}

	self := selfMetrics(t, server)
	if strings.Contains(self, "acme_") || !strings.Contains(self, "http_exporter_") {
		t.Errorf("the exporter's own metrics must not be prefixed:\n%s", firstLines(self, 20))
	}
}

// OTLP export sees the same names as /probe, for probes and scheduled targets
// alike; a scheduled target's health metrics are the exporter's and stay
// unprefixed.
func TestMetricsPrefixOnOTLPExport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer upstream.Close()

	c := testCollector("text", "text")
	c.MetricsPrefix = "acme"
	cfg := &Config{Collectors: []Collector{c}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "eu", Collector: "text", Target: upstream.URL}}}
	server := newScheduledServer(t, cfg, file)

	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	resources := server.drainOTLP()
	if len(resources) != 1 {
		t.Fatalf("resources=%+v", resources)
	}
	if m := metricByName(resources[0].Set, "acme_demo_value"); m == nil || m.Value != 42 {
		t.Fatalf("scheduled metrics=%+v", resources[0].Set.Metrics)
	}
	if metricByName(resources[0].Set, "demo_value") != nil {
		t.Fatal("the unprefixed name was exported too")
	}
	if metricByName(resources[0].Set, "http_exporter_target_up") == nil {
		t.Fatal("the health metric must keep its own name")
	}

	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(upstream.URL), nil)
	resources = server.drainOTLP()
	if len(resources) != 1 || metricByName(resources[0].Set, "acme_demo_value") == nil {
		t.Fatalf("probe metrics queued for OTLP=%+v", resources)
	}
}

// A declared name that would be too long once prefixed is refused at startup;
// a name produced at scrape time is checked when the scrape happens.
func TestMetricsPrefixRespectsTheNameLengthLimit(t *testing.T) {
	c := testCollector("prefixed", "text")
	c.MetricsPrefix = "acme"
	c.Limits.MaxMetricNameLength = len("acme_demo_value") - 1
	err := (&Config{Collectors: []Collector{c}}).Validate()
	if err == nil || !strings.Contains(err.Error(), `metric "demo_value" is exported as "acme_demo_value", which is longer than limits.max_metric_name_length 14`) {
		t.Fatalf("err=%v", err)
	}

	c = testCollector("prefixed", "text")
	c.MetricsPrefix = strings.Repeat("a", 199)
	err = (&Config{Collectors: []Collector{c}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "leaves no room for a metric name") {
		t.Fatalf("err=%v", err)
	}

	// Exactly at the limit is fine.
	c = testCollector("prefixed", "text")
	c.MetricsPrefix = "acme"
	c.Limits.MaxMetricNameLength = len("acme_demo_value")
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}

	// At scrape time, the prefixed name goes through the same limit check as
	// every exported name.
	set := &MetricSet{Metrics: []Metric{{Name: "script_value", Type: GaugeMetricType, Value: 1}}}
	applyMetricsPrefix(set, "acme")
	if err := set.Validate(Limits{MaxMetricNameLength: len("acme_script_value") - 1}); err == nil {
		t.Fatal("a prefixed name over the limit passed validation")
	}
}

// The response cache is keyed by the collector's definition, so changing the
// prefix on reload never serves metrics cached under the old names.
func TestChangingTheMetricsPrefixChangesTheCacheKey(t *testing.T) {
	a := testCollector("prefixed", "text")
	b := a
	b.MetricsPrefix = "acme"
	if collectorFingerprint(&a) == collectorFingerprint(&b) {
		t.Fatal("the prefix is not part of the cache key")
	}
}

// The shipped configurations document the key; the reference example uses it.
func TestTheExampleConfigurationShowsMetricsPrefix(t *testing.T) {
	cfg, err := LoadConfig("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfg.Collectors {
		if c.MetricsPrefix != "" {
			return
		}
	}
	raw, _ := yaml.Marshal(cfg.Collectors[0])
	t.Fatalf("config.example.yaml has no collector with metrics_prefix; first collector:\n%s", raw)
}

// --dry-run reports an invalid prefix as a failed configuration, as startup
// would refuse it.
func TestDryRunReportsAnInvalidMetricsPrefix(t *testing.T) {
	path := writeFile(t, "config.yaml", `collectors:
  - name: prefixed
    metrics_prefix: grafana_
    request:
      type: http
    transform:
      type: regex
    metrics:
      - name: demo_value
        description: A value
        type: gauge
        error_mode: log
        expression: 'value=(\d+)'
`)
	out := runCheckCLI(t, "--config.file="+path)
	if out.code != 1 || out.report.Status != "failed" {
		t.Fatalf("exit=%d status=%s", out.code, out.report.Status)
	}
	result := out.result(t, "config")
	if result.Status != checkFailed || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], `invalid metrics_prefix "grafana_"`) {
		t.Fatalf("config check=%+v", result)
	}
}
