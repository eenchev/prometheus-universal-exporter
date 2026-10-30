package exporter

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// otlpGoldenResources are the resources of an export with one metric of
// every kind, each with a time of its own, so the export's JSON is the same
// at every run: a probe's under the exporter-wide resource, and a static
// target's under a resource of its own.
func otlpGoldenResources(cfg model.OTLPConfig) []otlpResourceSet {
	at := int64(1700000000000)
	stamp := func() *int64 { v := at; return &v }
	probe := model.MetricSet{Metrics: []model.Metric{
		{Name: "queue_depth", Help: "Jobs waiting.", Type: model.GaugeMetricType, Value: 3, Labels: map[string]string{"queue": "a"}, Timestamp: stamp()},
		{Name: "jobs_processed_total", Help: "Jobs done.", Type: model.CounterMetricType, Value: 7, Labels: map[string]string{"queue": "a"}, Timestamp: stamp()},
		{Name: "request_latency_seconds", Type: model.HistogramMetricType, Labels: map[string]string{"route": "/"}, Timestamp: stamp(), Histogram: &model.Histogram{Count: 4, Sum: 1.5, Buckets: []model.Bucket{{UpperBound: 0.5, CumulativeCount: 2}, {UpperBound: 1, CumulativeCount: 3}}}},
		{Name: "rpc_duration_milliseconds", Type: model.SummaryMetricType, Timestamp: stamp(), Summary: &model.Summary{Count: 2, Sum: 30, Quantiles: []model.Quantile{{Quantile: 0.5, Value: 12}}}},
		{Name: "transfer_bytes_per_second", Type: model.GaugeMetricType, Value: 1024, Timestamp: stamp()},
	}}
	target := model.StaticTarget{Name: "eu", ExportViaOTLP: true, Labels: map[string]string{"city": "berlin"}}
	static := model.MetricSet{Metrics: []model.Metric{
		{Name: "temperature_celsius", Type: model.GaugeMetricType, Value: 21, Labels: map[string]string{"city": "berlin", "static_target": "eu"}, Timestamp: stamp()},
	}}
	return []otlpResourceSet{
		{Identity: defaultResourceIdentity(cfg), Set: probe, seqs: []int64{1, 2, 3, 4, 5}},
		{Identity: targetResource(&target, cfg), Set: static, seqs: []int64{6}},
	}
}

// otlpGoldenJSON is the export of otlpGoldenResources and the head of a
// log export, as an export sends them without metric_names, scope or a
// target's labels_as_resource and instance_id.
const otlpGoldenJSON = `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"prometheus-universal-exporter"}},{"key":"deployment.environment","value":{"stringValue":"test"}}]},"scopeMetrics":[{"scope":{"name":"prometheus-universal-exporter"},"metrics":[{"name":"queue_depth","description":"Jobs waiting.","gauge":{"dataPoints":[{"attributes":[{"key":"queue","value":{"stringValue":"a"}}],"timeUnixNano":"1700000000000000000","asDouble":3}]}},{"name":"jobs_processed_total","description":"Jobs done.","sum":{"dataPoints":[{"attributes":[{"key":"queue","value":{"stringValue":"a"}}],"startTimeUnixNano":"1700000000000000000","timeUnixNano":"1700000000000000000","asDouble":7}],"aggregationTemporality":"AGGREGATION_TEMPORALITY_CUMULATIVE","isMonotonic":true}},{"name":"request_latency_seconds","histogram":{"dataPoints":[{"attributes":[{"key":"route","value":{"stringValue":"/"}}],"startTimeUnixNano":"1700000000000000000","timeUnixNano":"1700000000000000000","count":"4","sum":1.5,"bucketCounts":["2","1","1"],"explicitBounds":[0.5,1]}],"aggregationTemporality":"AGGREGATION_TEMPORALITY_CUMULATIVE"}},{"name":"rpc_duration_milliseconds","summary":{"dataPoints":[{"startTimeUnixNano":"1700000000000000000","timeUnixNano":"1700000000000000000","count":"2","sum":30,"quantileValues":[{"quantile":0.5,"value":12}]}]}},{"name":"transfer_bytes_per_second","gauge":{"dataPoints":[{"timeUnixNano":"1700000000000000000","asDouble":1024}]}}]}]},{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"prometheus-universal-exporter"}},{"key":"deployment.environment","value":{"stringValue":"test"}}]},"scopeMetrics":[{"scope":{"name":"prometheus-universal-exporter"},"metrics":[{"name":"temperature_celsius","gauge":{"dataPoints":[{"attributes":[{"key":"city","value":{"stringValue":"berlin"}},{"key":"static_target","value":{"stringValue":"eu"}}],"timeUnixNano":"1700000000000000000","asDouble":21}]}}]}]}]}
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"prometheus-universal-exporter"}},{"key":"deployment.environment","value":{"stringValue":"test"}}]},"scopeLogs":[{"scope":{"name":"prometheus-universal-exporter"},"logRecords":[{}]}]}]}`

// otlpExportJSON is the JSON of the one request an export of resources
// under cfg is, and of the one request of a log export of a record {}.
func otlpExportJSON(t *testing.T, cfg model.OTLPConfig, resources []otlpResourceSet) string {
	t.Helper()
	server := NewServer(config.NewManager(&model.Config{OTLP: cfg}, "", slog.Default()), "python3", slog.Default())
	requests, err := server.otlpRequestsOf(cfg, resources)
	if err != nil || len(requests.bodies) != 1 {
		t.Fatalf("the export is %d requests, error %v; want one", len(requests.bodies), err)
	}
	logs, err := otlpLogRequests(cfg, [][]byte{[]byte("{}")})
	if err != nil || len(logs) != 1 {
		t.Fatalf("the log export is %d requests, error %v; want one", len(logs), err)
	}
	return string(requests.bodies[0]) + "\n" + string(logs[0].body)
}

// otlpGoldenConfig is otlpConfig without compression, so a body is its JSON,
// as the configuration's validation fills it in after change, if any.
func otlpGoldenConfig(t *testing.T, change func(*model.OTLPConfig)) model.OTLPConfig {
	t.Helper()
	registerFixtureType(t)
	whole := &model.Config{Collectors: []model.Collector{fixtureCollector()}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	whole.OTLP.Compression = model.OTLPCompressionNone
	if change != nil {
		change(&whole.OTLP)
	}
	if err := config.Validate(whole); err != nil {
		t.Fatal(err)
	}
	return whole.OTLP
}

// An export whose configuration leaves out metric_names and scope, of a
// static target that sets neither labels_as_resource nor instance_id, is
// byte for byte what it was before they were keys: names as the exposition
// writes them and no unit, the scope prometheus-universal-exporter without
// a version, and a target's labels on its points. metric_names: prometheus
// written out is the same export.
func TestOTLPExportIsUnchangedWithoutTheNamingAndScopeSettings(t *testing.T) {
	cfg := otlpGoldenConfig(t, nil)
	if got := otlpExportJSON(t, cfg, otlpGoldenResources(cfg)); got != otlpGoldenJSON {
		t.Errorf("the export changed:\n%s\nwant\n%s", got, otlpGoldenJSON)
	}
}

// otlpMetricShapes are the metrics of an export's JSON, each as its name,
// its kind and its unit, by resource.
func otlpMetricShapes(t *testing.T, body string) [][]string {
	t.Helper()
	var payload otlpPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, resource := range payload.ResourceMetrics {
		var metrics []string
		for _, m := range resource.ScopeMetrics[0].Metrics {
			metrics = append(metrics, m.Name+" "+otlpKindNames[otlpKind(m)]+" "+m.Unit)
		}
		out = append(out, metrics)
	}
	return out
}

// With otlp.metric_names: otel, a counter's name loses its _total, and a
// name that ends in a unit — a gauge's, a counter's after its _total, a
// histogram's or a summary's family name — loses it to the metric's unit,
// as the OpenTelemetry Collector's Prometheus translator names a metric of
// the unit, read backwards; bytes_per_second is By/s. A gauge keeps a
// _total, which only a counter's is.
func TestOTLPExportNamesMetricsAsOpenTelemetryDoes(t *testing.T) {
	cfg := otlpGoldenConfig(t, func(o *model.OTLPConfig) { o.MetricNames = model.OTLPMetricNamesOTel })
	resources := otlpGoldenResources(cfg)
	resources[0].Set.Metrics = append(resources[0].Set.Metrics,
		model.Metric{Name: "errors_total", Type: model.GaugeMetricType, Value: 1},
		model.Metric{Name: "bytes_sent_bytes_total", Type: model.CounterMetricType, Value: 1},
	)
	resources[0].seqs = append(resources[0].seqs, 7, 8)
	body, _, _ := strings.Cut(otlpExportJSON(t, cfg, resources), "\n")
	want := [][]string{
		{"queue_depth gauge ", "jobs_processed sum ", "request_latency histogram s", "rpc_duration summary ms", "transfer gauge By/s", "errors_total gauge ", "bytes_sent sum By"},
		{"temperature gauge Cel"},
	}
	if got := otlpMetricShapes(t, body); !reflect.DeepEqual(got, want) {
		t.Errorf("the metrics are\n%q\nwant\n%q", got, want)
	}
}

// The table of names otlp.metric_names: otel exports, each as the name, the
// unit, and the kind of the metric it is a name of. A suffix is a whole
// word, after the last _ and with something before it; a _per_ unit is one
// of a unit and of time.
func TestOTelMetricNames(t *testing.T) {
	for _, c := range []struct {
		name       string
		kind       int
		want, unit string
	}{
		{"http_requests_total", otlpKindSum, "http_requests", ""},
		{"process_cpu_seconds_total", otlpKindSum, "process_cpu", "s"},
		{"process_cpu_seconds_total", otlpKindGauge, "process_cpu_seconds_total", ""},
		{"up", otlpKindGauge, "up", ""},
		{"total", otlpKindSum, "total", ""},
		{"_total", otlpKindSum, "_total", ""},
		{"seconds", otlpKindGauge, "seconds", ""},
		{"x_seconds", otlpKindHistogram, "x", "s"},
		{"x_milliseconds", otlpKindSummary, "x", "ms"},
		{"x_microseconds", otlpKindGauge, "x", "us"},
		{"x_nanoseconds", otlpKindGauge, "x", "ns"},
		{"uptime_minutes", otlpKindGauge, "uptime", "min"},
		{"uptime_hours", otlpKindGauge, "uptime", "h"},
		{"uptime_days", otlpKindGauge, "uptime", "d"},
		{"memory_bytes", otlpKindGauge, "memory", "By"},
		{"memory_kibibytes", otlpKindGauge, "memory", "KiBy"},
		{"memory_mebibytes", otlpKindGauge, "memory", "MiBy"},
		{"memory_gibibytes", otlpKindGauge, "memory", "GiBy"},
		{"memory_tibibytes", otlpKindGauge, "memory", "TiBy"},
		{"disk_kilobytes", otlpKindGauge, "disk", "KBy"},
		{"disk_megabytes", otlpKindGauge, "disk", "MBy"},
		{"disk_gigabytes", otlpKindGauge, "disk", "GBy"},
		{"disk_terabytes", otlpKindGauge, "disk", "TBy"},
		{"cache_hit_ratio", otlpKindGauge, "cache_hit", "1"},
		{"cpu_percent", otlpKindGauge, "cpu", "%"},
		{"room_celsius", otlpKindGauge, "room", "Cel"},
		{"distance_meters", otlpKindGauge, "distance", "m"},
		{"supply_volts", otlpKindGauge, "supply", "V"},
		{"supply_amperes", otlpKindGauge, "supply", "A"},
		{"energy_joules_total", otlpKindSum, "energy", "J"},
		{"power_watts", otlpKindGauge, "power", "W"},
		{"weight_grams", otlpKindGauge, "weight", "g"},
		{"clock_hertz", otlpKindGauge, "clock", "Hz"},
		{"transfer_bytes_per_second", otlpKindGauge, "transfer", "By/s"},
		{"transfer_bytes_per_minute", otlpKindGauge, "transfer", "By/min"},
		{"travel_meters_per_hour", otlpKindGauge, "travel", "m/h"},
		{"growth_bytes_per_day", otlpKindGauge, "growth", "By/d"},
		{"requests_per_second", otlpKindGauge, "requests_per_second", ""},
		{"bytes_per_second", otlpKindGauge, "bytes_per_second", ""},
		{"hits_ratio_per_second", otlpKindGauge, "hits_ratio_per_second", ""},
		{"xseconds", otlpKindGauge, "xseconds", ""},
		{"seconds_x", otlpKindGauge, "seconds_x", ""},
		{"x_seconds_", otlpKindGauge, "x_seconds_", ""},
		{"job:latency_seconds:mean", otlpKindGauge, "job:latency_seconds:mean", ""},
	} {
		name, unit := otlpNameOf(c.name, c.kind, true)
		if name != c.want || unit != c.unit {
			t.Errorf("%s, a %s, is exported as %q of unit %q, want %q of unit %q", c.name, otlpKindNames[c.kind], name, unit, c.want, c.unit)
		}
		if name, unit := otlpNameOf(c.name, c.kind, false); name != c.name || unit != "" {
			t.Errorf("%s under metric_names: prometheus is %q of unit %q", c.name, name, unit)
		}
	}
}

// A family exported as gauges under the names of its samples, as a
// counter that is NaN is, keeps those names under otlp.metric_names: otel:
// they are the samples the exposition writes, not a family named by
// OpenTelemetry, and have no unit.
func TestOTelMetricNamesLeaveAFamilyExportedAsGaugesAsWritten(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "retries_seconds_total", Type: model.CounterMetricType, Value: math.NaN()},
		{Name: "wait_seconds", Type: model.HistogramMetricType, Histogram: &model.Histogram{Count: 1, Sum: -1, Buckets: []model.Bucket{{UpperBound: -1, CumulativeCount: 1}}}},
	}}
	out, _ := otlpMetricsOf(otlpResourceSet{Set: set, otelNames: true}, "1", nil)
	var got []string
	for _, m := range out {
		got = append(got, m.Name+" "+otlpKindNames[otlpKind(m)]+" "+m.Unit)
	}
	want := []string{"retries_seconds_total gauge ", "wait_seconds_bucket gauge ", "wait_seconds_sum gauge ", "wait_seconds_count gauge "}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the metrics are %q, want %q", got, want)
	}
}

// Names otlp.metric_names: otel makes one are one name to the rule of two
// writers of one name (otlpMetricsOf): a gauge jobs of one probe and a
// counter jobs_total of another are the name jobs of two kinds, and a gauge
// latency and a gauge latency_seconds the name latency of two units. The
// shape written last is exported, the other left out and reported; under
// metric_names: prometheus the four are four names and none is left out.
func TestOTelMetricNamesThatCollideAreOneNameOfTwoShapes(t *testing.T) {
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "jobs", Type: model.GaugeMetricType, Value: 1},
		{Name: "latency_seconds", Type: model.GaugeMetricType, Value: 2},
		{Name: "jobs_total", Type: model.CounterMetricType, Value: 3},
		{Name: "latency", Type: model.GaugeMetricType, Value: 4},
	}}
	resource := otlpResourceSet{Set: set, seqs: []int64{1, 2, 3, 4}}
	if out, clashes := otlpMetricsOf(resource, "1", nil); len(out) != 4 || len(clashes) != 0 {
		t.Fatalf("under metric_names: prometheus the export has %d metrics and the clashes %+v, want 4 and none", len(out), clashes)
	}
	resource.otelNames = true
	out, clashes := otlpMetricsOf(resource, "1", nil)
	var got []string
	for _, m := range out {
		got = append(got, m.Name+" "+otlpKindNames[otlpKind(m)]+" "+m.Unit)
	}
	if want := []string{"jobs sum ", "latency gauge "}; !reflect.DeepEqual(got, want) {
		t.Errorf("the metrics are %q, want %q", got, want)
	}
	want := []otlpNameClash{
		{name: "jobs", kept: otlpShape{kind: otlpKindSum}, leftOut: []otlpShape{{kind: otlpKindGauge}}, points: 1},
		{name: "latency", kept: otlpShape{kind: otlpKindGauge}, leftOut: []otlpShape{{kind: otlpKindGauge, unit: "s"}}, points: 1},
	}
	if !reflect.DeepEqual(clashes, want) {
		t.Errorf("the clashes are %+v, want %+v", clashes, want)
	}
	var log bytes.Buffer
	server := NewServer(config.NewManager(&model.Config{}, "", slog.Default()), "python3", slog.Default())
	server.logger = timelessLogger(&log, true)
	server.logOTLPNameClashes(otlpResourceIdentity{ServiceName: "svc"}, "resource", clashes)
	if !strings.Contains(log.String(), `"metric":"latency","kind":"gauge","left_out_kind":"gauge of unit s"`) {
		t.Errorf("the clash of units is logged as\n%s", log.String())
	}
}

// otlp.scope names the instrumentation scope of the export's metrics and of
// its log records, with its version.
func TestOTLPScopeNamesTheMetricsAndTheLogRecordsScope(t *testing.T) {
	cfg := otlpGoldenConfig(t, func(o *model.OTLPConfig) {
		o.Scope = &model.OTLPScopeConfig{Name: "acme.io/edge", Version: "1.2.3"}
	})
	metrics, logs, _ := strings.Cut(otlpExportJSON(t, cfg, otlpGoldenResources(cfg)), "\n")
	const scope = `"scope":{"name":"acme.io/edge","version":"1.2.3"}`
	if strings.Count(metrics, scope) != 2 || strings.Contains(metrics, `"scope":{"name":"`+model.DefaultOTLPScopeName) {
		t.Errorf("the metrics are not under the scope:\n%s", metrics)
	}
	if !strings.Contains(logs, scope) {
		t.Errorf("the log records are not under the scope:\n%s", logs)
	}
	// A scope without a version has none, as the default has none.
	cfg.Scope.Version = ""
	if metrics, logs, _ := strings.Cut(otlpExportJSON(t, cfg, otlpGoldenResources(cfg)), "\n"); !strings.Contains(metrics, `"scope":{"name":"acme.io/edge"}`) || !strings.Contains(logs, `"scope":{"name":"acme.io/edge"}`) {
		t.Errorf("a scope without a version is sent as\n%s\n%s", metrics, logs)
	}
}

// A static target's labels of otlp.labels_as_resource are attributes of its
// resource, and otlp.instance_id makes its name the resource's
// service.instance.id. On its points a label moved so is left out where it
// has the target's value, and kept where the series' own label of the name
// has another, which the resource does not say. The set the endpoint serves
// is not changed.
func TestStaticTargetLabelsAsResourceMoveFromThePointsToTheResource(t *testing.T) {
	cfg := otlpGoldenConfig(t, nil)
	target := model.StaticTarget{Name: "eu", ExportViaOTLP: true, Labels: map[string]string{"city": "berlin", "team": "a", "empty": ""}, OTLP: model.TargetOTLPConfig{LabelsAsResource: []string{"city", "empty"}, InstanceID: true}}
	identity := targetResource(&target, cfg)
	want := map[string]string{"deployment.environment": "test", "city": "berlin", "service.instance.id": "eu"}
	if !reflect.DeepEqual(identity.Attributes, want) {
		t.Errorf("the resource's attributes are %v, want %v", identity.Attributes, want)
	}
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "a", Labels: map[string]string{"city": "berlin", "team": "a", "static_target": "eu"}},
		{Name: "b", Labels: map[string]string{"city": "paris", "team": "a", "static_target": "eu"}},
	}}
	out := withoutResourceLabels(set, target)
	if got := []map[string]string{out.Metrics[0].Labels, out.Metrics[1].Labels}; !reflect.DeepEqual(got, []map[string]string{
		{"team": "a", "static_target": "eu"},
		{"city": "paris", "team": "a", "static_target": "eu"},
	}) {
		t.Errorf("the points' labels are %v", got)
	}
	if set.Metrics[0].Labels["city"] != "berlin" {
		t.Errorf("the set the endpoint serves lost its label: %v", set.Metrics[0].Labels)
	}
	// Without either key the resource and the points are as they were.
	plain := model.StaticTarget{Name: "eu", ExportViaOTLP: true, Labels: target.Labels}
	if got := targetResource(&plain, cfg); !reflect.DeepEqual(got, defaultResourceIdentity(cfg)) {
		t.Errorf("a target without the keys has the resource %+v", got)
	}
	if got := withoutResourceLabels(set, plain); !reflect.DeepEqual(got, set) {
		t.Errorf("a target without the keys has the points %+v", got)
	}
}
