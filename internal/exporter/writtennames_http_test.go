//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// namesTarget answers /metrics with one series under a UTF-8 metric name and
// a UTF-8 label name, as a Prometheus 3 target may, /classic with the same
// under classic names, and /json with the same as a JSON document.
func namesTarget(t *testing.T) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("# TYPE \"http.server.duration\" gauge\n{\"http.server.duration\",\"service.name\"=\"api\"} 0.25\n"))
		case "/classic":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("# TYPE http_server_duration gauge\nhttp_server_duration{service_name=\"api\"} 0.25\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"duration": 0.25, "service": "api"}`))
		}
	}))
	t.Cleanup(target.Close)
	return target
}

// namedCollectors are collectors that each export the one series of
// namesTarget: target passes on what the target names, and the others name
// it themselves, each as its transform does, metric being the metric's name
// and label the label's. A python rule only names the script's series, to
// cut its label. With the classic names the two that read the target's own
// names read them from /classic.
func namedCollectors(escaping, metric, label string) string {
	keys := ""
	if escaping != "" {
		keys = "    name_escaping: " + escaping + "\n"
	}
	// The rule that exports a classic metric under metric reads the label
	// under label too, and takes the one it read off where that is another.
	named, remove := "/metrics", "      remove_labels: [service_name]\n"
	if label == "service_name" {
		named, remove = "/classic", ""
	}
	collector := func(name, path, rest string) string {
		return "  - name: " + name + "\n" + keys + "    request:\n      type: http\n      path: " + path + "\n" + rest
	}
	return "collectors:\n" +
		collector("target", named, "    transform:\n      type: prometheus\n") +
		collector("jq_rule", "/json", "    transform:\n      type: jq\n    metrics:\n      - name: "+metric+"\n        expression: .duration\n        labels:\n          - name: "+label+"\n            expression: .service\n") +
		collector("prometheus_rule", named, "    transform:\n      type: prometheus\n    metrics:\n      - name: "+metric+"\n") +
		collector("prometheus_renamed", "/classic", "    transform:\n      type: prometheus\n"+remove+"    metrics:\n      - name: "+metric+"\n        expression: '^http_server_duration$'\n        labels:\n          - name: "+label+"\n            expression: service_name\n") +
		collector("python_rule", "/json", "    transform:\n      type: python\n      script: |\n        metric(name=\""+metric+"\", value=data[\"duration\"], labels={\""+label+"\": data[\"service\"]})\n    metrics:\n      - name: "+metric+"\n        labels:\n          - name: "+label+"\n            expression: unread\n            truncate: true\n")
}

// namedServer is a server of the configuration document, exporting over
// OTLP to endpoint when one is given.
func namedServer(t *testing.T, document, endpoint string) *Server {
	t.Helper()
	if endpoint != "" {
		document = "otlp:\n  enabled: true\n  endpoint: " + endpoint + "/v1/metrics\n" + document
	}
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
	if err != nil {
		t.Fatalf("%v\n%s", err, document)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

// A rule's name, and its labels' names, that are not classic are exported
// under name_escaping underscores and values as each exports a target's
// same names, by the one place that escapes every transform's series: the
// answer of a jq collector whose rule is named http.server.duration, with a
// label service.name, of a prometheus collector whose rule passes on the
// target's metric of that name, of one whose rule exports a classic metric
// under that name, and of a python collector whose rule names the script's
// series of that name to cut its label, is each, byte for byte, the answer
// of a pass-through of a target that gives those names itself, in the text
// format and in OpenMetrics; and each queues for OTLP the series the
// pass-through queues, which is exported under the escaped names. The
// configuration was refused at the load, for each of those rules, under
// every name_escaping.
func TestARulesNameIsExportedAsATargetsSameNameIs(t *testing.T) {
	testutil.CaptureLogs(t)
	target := namesTarget(t)
	for escaping, want := range map[string]struct{ metric, label string }{
		"underscores": {"http_server_duration", "service_name"},
		"values":      {"U__http_2e_server_2e_duration", "U__service_2e_name"},
	} {
		endpoint := newOTLPEndpoint(t)
		server := namedServer(t, namedCollectors(escaping, "http.server.duration", "service.name"), endpoint.server.URL)
		answer := func(collector, accept string) string {
			t.Helper()
			header := http.Header{}
			if accept != "" {
				header.Set("Accept", accept)
			}
			r := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), header)
			if r.Code != http.StatusOK {
				t.Fatalf("name_escaping %s, %s: answered %d: %s", escaping, collector, r.Code, r.Body)
			}
			return r.Body.String()
		}
		const openMetrics = "application/openmetrics-text; version=1.0.0"
		text, open := answer("target", ""), answer("target", openMetrics)
		if wantText := fmt.Sprintf("# TYPE %s gauge\n%s{%s=\"api\"} 0.25\n", want.metric, want.metric, want.label); text != wantText || open != wantText+"# EOF\n" {
			t.Fatalf("name_escaping %s: the pass-through answers\n%s\nand in OpenMetrics\n%s\nwant\n%s", escaping, text, open, wantText)
		}
		queued := func() model.MetricSet {
			t.Helper()
			resources := server.drainOTLP()
			if len(resources) != 1 {
				t.Fatalf("name_escaping %s: %d resources are queued for OTLP", escaping, len(resources))
			}
			return resources[0].Set
		}
		answer("target", "")
		reference := queued()
		if m := metricByName(reference, want.metric); m == nil || !reflect.DeepEqual(m.Labels, map[string]string{want.label: "api"}) {
			t.Fatalf("name_escaping %s: the pass-through queues %+v for OTLP", escaping, reference.Metrics)
		}
		for _, collector := range []string{"jq_rule", "prometheus_rule", "prometheus_renamed", "python_rule"} {
			if got := answer(collector, ""); got != text {
				t.Errorf("name_escaping %s, %s: the answer is\n%s\nwant the pass-through's\n%s", escaping, collector, got, text)
			}
			if got := answer(collector, openMetrics); got != open {
				t.Errorf("name_escaping %s, %s: the OpenMetrics answer is\n%s\nwant the pass-through's\n%s", escaping, collector, got, open)
			}
			set := queued()
			if len(set.Metrics) != 1 || set.Metrics[0].Name != want.metric || !reflect.DeepEqual(set.Metrics[0].Labels, reference.Metrics[0].Labels) || set.Metrics[0].Value != 0.25 {
				t.Errorf("name_escaping %s, %s: queued for OTLP %+v\nwant the pass-through's %+v", escaping, collector, set.Metrics, reference.Metrics)
			}
		}
		// What goes over the wire is the series under the escaped names,
		// and no metric of the name as it is written, which the export
		// has as a value alone: the rule's, among its failures.
		answer("jq_rule", "")
		server.exportOTLP(context.Background(), time.Minute)
		if exported := `{"name":"` + want.metric + `","gauge":{"dataPoints":[{"attributes":[{"key":"` + want.label + `","value":{"stringValue":"api"}}],`; endpoint.count() != 1 || !strings.Contains(string(endpoint.bodies[0]), exported) || strings.Contains(string(endpoint.bodies[0]), `"name":"http.server.duration"`) {
			t.Errorf("name_escaping %s: the export of a jq rule's series has no %s:\n%s", escaping, exported, endpoint.bodies)
		}
	}

	// Under fail, the default, the name could only fail every scrape, as
	// the target's does, so the load refuses it, and says what the scrape's
	// refusal says.
	for _, escaping := range []string{"", "fail"} {
		_, err := config.Load(testutil.WriteFile(t, "config.yaml", namedCollectors(escaping, "http.server.duration", "service.name")))
		if err == nil {
			t.Fatalf("name_escaping %q: the rules loaded", escaping)
		}
		for _, want := range []string{
			`collector "jq_rule" metric "http.server.duration" has invalid label name "service.name"; set the collector's name_escaping to underscores or values to export it escaped`,
			`collector "prometheus_rule" metric "http.server.duration": "http.server.duration" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped; a pattern to match the target's metric names by is a prometheus rule's expression, not its name`,
			`collector "prometheus_renamed" metric "http.server.duration" has invalid label name "service.name"; set the collector's name_escaping to underscores or values to export it escaped`,
			`collector "python_rule" metric "http.server.duration" has invalid label name "service.name"; set the collector's name_escaping to underscores or values to export it escaped`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("name_escaping %q: the load says\n%v\nwant %s", escaping, err, want)
			}
		}
		server := namedServer(t, "collectors:\n  - name: target\n    request:\n      type: http\n      path: /metrics\n    transform:\n      type: prometheus\n", "")
		if r := probeOnce(t, server, "/probe?collector=target&target="+url.QueryEscape(target.URL), nil); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), `metric name "http.server.duration" is not a classic Prometheus name; set the collector's name_escaping to underscores or values to export it escaped`) {
			t.Errorf("the pass-through of a collector without name_escaping answered %d: %s", r.Code, r.Body)
		}
	}
}

// Rules whose names are all classic are exported as they were under every
// name_escaping: the four collectors of classic names answer, each of them,
// what the pass-through of a target of classic names answers, the same bytes
// with name_escaping left out, fail, underscores and values, in the text
// format and in OpenMetrics, and queue the same series for OTLP.
func TestNameEscapingChangesNothingOfRulesOfClassicNames(t *testing.T) {
	testutil.CaptureLogs(t)
	target := namesTarget(t)
	const wantText = "# TYPE http_server_duration gauge\nhttp_server_duration{service_name=\"api\"} 0.25\n"
	for _, escaping := range []string{"", "fail", "underscores", "values"} {
		server := namedServer(t, namedCollectors(escaping, "http_server_duration", "service_name"), "http://collector.invalid")
		for _, collector := range []string{"target", "jq_rule", "prometheus_rule", "prometheus_renamed", "python_rule"} {
			for accept, want := range map[string]string{"": wantText, "application/openmetrics-text; version=1.0.0": wantText + "# EOF\n"} {
				header := http.Header{}
				if accept != "" {
					header.Set("Accept", accept)
				}
				r := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), header)
				if r.Code != http.StatusOK || r.Body.String() != want {
					t.Errorf("name_escaping %q, %s, Accept %q: answered %d\n%s\nwant\n%s", escaping, collector, accept, r.Code, r.Body, want)
				}
			}
			resources := server.drainOTLP()
			if len(resources) != 1 || len(resources[0].Set.Metrics) != 1 || resources[0].Set.Metrics[0].Name != "http_server_duration" || !reflect.DeepEqual(resources[0].Set.Metrics[0].Labels, map[string]string{"service_name": "api"}) {
				t.Errorf("name_escaping %q, %s: queued for OTLP %+v", escaping, collector, resources)
			}
		}
	}
}

// What tells of a rule tells of it by its name as the configuration writes
// it, under which its author finds it: the log of a rule that failed, the
// probe's error under error_mode fail, the rule's failures among the
// self-metrics and the list of rules that carried on in a debug probe's
// report. The report's lists of series and of the rules that gave none are
// of the names the series are exported under, with the prefix and escaped,
// as the series a probe would have served are: a rule that gave its series
// is not listed as one that gave none because its name is written
// otherwise than its series is exported, which a rule of a collector with a
// metrics_prefix was, under every name.
func TestARuleOfAnEscapedNameIsToldOfByItsNameAsWritten(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := namesTarget(t)
	const rules = "    transform:\n      type: jq\n    metrics:\n      - name: http.server.duration\n        expression: .duration\n      - name: http.client.duration\n        expression: .absent\n      - name: queue_depth\n        expression: .absent\n        required: false\n"
	document := "collectors:\n" +
		"  - name: plain\n    name_escaping: underscores\n    request:\n      type: http\n      path: /json\n" + rules +
		"  - name: prefixed\n    name_escaping: values\n    metrics_prefix: otel\n    request:\n      type: http\n      path: /json\n" + rules +
		"  - name: strict\n    name_escaping: underscores\n    request:\n      type: http\n      path: /json\n" + strings.Replace(rules, "        expression: .absent\n      - name: queue_depth", "        expression: .absent\n        error_mode: fail\n      - name: queue_depth", 1) +
		"  - name: classic\n    metrics_prefix: otel\n    request:\n      type: http\n      path: /json\n    transform:\n      type: jq\n    metrics:\n      - name: duration\n        expression: .duration\n      - name: queue_depth\n        expression: .absent\n        required: false\n"
	document += "  - name: mapped\n    name_escaping: underscores\n    request:\n      type: http\n      path: /json\n    transform:\n      type: jq\n    metrics:\n      - name: http.server.duration\n        expression: .duration\n        labels:\n          - name: service.name\n            expression: .service\n            value_map: {api: API}\n" +
		"  - name: cut\n    name_escaping: values\n    limits:\n      max_label_value_length: 7\n    request:\n      type: http\n      path: /json\n    transform:\n      type: python\n      script: |\n        metric(name=\"http.server.duration\", value=data[\"duration\"], labels={\"service.name\": \"a\" * 40})\n    metrics:\n      - name: http.server.duration\n        labels:\n          - name: service.name\n            expression: unread\n            truncate: true\n"
	server := namedServer(t, document, "")
	server.SetProbeDebug(true)
	query := "&target=" + url.QueryEscape(target.URL)

	if r := probeOnce(t, server, "/probe?collector=plain"+query, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "http_server_duration 0.25\n") {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	if !strings.Contains(logs.String(), `"msg":"metric extraction failed","collector":"plain",`) || !strings.Contains(logs.String(), `"metric":"http.client.duration","error_mode":"log"`) {
		t.Errorf("the failure of the rule is logged as\n%s", logs)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_rule_failures_total{collector="plain",metric="http.client.duration"}`); got != 1 {
		t.Errorf("the rule's failures among the self-metrics: %v", got)
	}
	if r := probeOnce(t, server, "/probe?collector=strict"+query, nil); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), `"collector":"strict","metric":"http.client.duration"`) {
		t.Errorf("under error_mode fail the probe answered %d: %s", r.Code, r.Body)
	}
	// What a rule says of its series' labels finds them by the names as
	// they are written, before they are escaped: a label's value_map, and
	// the cut of a label of a python script's series.
	for collector, want := range map[string]string{"mapped": `http_server_duration{service_name="API"} 0.25`, "cut": `U__http_2e_server_2e_duration{U__service_2e_name="aaaa…"} 0.25`} {
		if r := probeOnce(t, server, "/probe?collector="+collector+query, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), want+"\n") {
			t.Errorf("%s answered %d: %s\nwant %s", collector, r.Code, r.Body, want)
		}
	}
	for collector, want := range map[string][]string{
		"plain":    {"Series by metric\n    http_server_duration: 1\n", "Rules that gave no series: http_client_duration, queue_depth\n", "    http.client.duration: 1 failed"},
		"prefixed": {"Series by metric\n    U__otel__http_2e_server_2e_duration: 1\n", "Rules that gave no series: U__otel__http_2e_client_2e_duration, otel_queue_depth\n", "    http.client.duration: 1 failed"},
		"classic":  {"Series by metric\n    otel_duration: 1\n", "Rules that gave no series: otel_queue_depth\n"},
	} {
		report := debugProbeGet(t, server, "collector="+collector+"&debug=true"+query).Body.String()
		assertContains(t, report, want...)
	}
}
