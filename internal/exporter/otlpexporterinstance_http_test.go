//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// otlp.exporter_instance: the attribute naming the replica, on every
// resource the export sends, of metrics and of log records, and on the
// debug page's and the status page's OTLP sections.

// withExporterInstance switches otlp.exporter_instance on with the value
// replica-1 and the attribute written as attribute, "" for the default.
func withExporterInstance(attribute string) func(*model.OTLPConfig) {
	return func(o *model.OTLPConfig) {
		o.ExporterInstance = model.OTLPExporterInstanceConfig{Enabled: true, Attribute: attribute, Value: "replica-1"}
	}
}

// Every resource of an export carries the attribute: the exporter-wide one,
// of a probe's series, of a static target without an otlp block of its own
// and of the exporter's own metrics, and that of a static target with its
// own service name, which is still a resource of its own. The attribute is
// pue.exporter.instance unless the block names another. Switched off, no
// resource carries one.
func TestEveryOTLPResourceNamesTheExporterInstance(t *testing.T) {
	for _, test := range []struct{ attribute, written string }{{"", "pue.exporter.instance"}, {"k8s.pod.name", "k8s.pod.name"}, {"off", ""}} {
		target := textTarget(t, "value=3\n")
		endpoint := newOTLPEndpoint(t)
		cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}
		if test.attribute != "off" {
			withExporterInstance(test.attribute)(&cfg.OTLP)
		}
		file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
			{ExportViaOTLP: true, Name: "plain", Collector: "text", Target: target.URL},
			{ExportViaOTLP: true, Name: "own", Collector: "text", Target: target.URL, OTLP: model.TargetOTLPConfig{ServiceName: "legacy"}},
		}}
		server := newStaticServer(t, cfg, file)
		server.logger = testutil.QuietLogger(t)
		if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(target.URL), nil); answer.Code != http.StatusOK {
			t.Fatalf("the probe is answered %d", answer.Code)
		}
		server.scrapeStaticTargets(context.Background(), 0)
		var (
			exporterWide = `[service.name="prometheus-universal-exporter" deployment.environment="test"] `
			ofOwn        = `[service.name="legacy" deployment.environment="test"] `
		)
		if test.written != "" {
			exporterWide = `[service.name="prometheus-universal-exporter" deployment.environment="test" ` + test.written + `="replica-1"] `
			ofOwn = `[service.name="legacy" deployment.environment="test" ` + test.written + `="replica-1"] `
		}
		var values []string
		own := false
		for _, line := range exportedLines(t, server, endpoint) {
			if !strings.HasPrefix(line, exporterWide) && !strings.HasPrefix(line, ofOwn) {
				t.Errorf("%s: a point of another resource: %s", test.written, line)
			}
			switch {
			case strings.Contains(line, "] demo_value{"):
				values = append(values, line)
			case strings.HasPrefix(line, exporterWide+"http_exporter_"):
				own = true
			}
		}
		slices.Sort(values)
		wantLines(t, test.written+": the series of the probe and the targets", values,
			ofOwn+`demo_value{static_target="own"} 3`,
			exporterWide+`demo_value{static_target="plain"} 3`,
			exporterWide+`demo_value{} 3`)
		if !own {
			t.Errorf("%s: the export has none of the exporter's own metrics under its resource", test.written)
		}
	}
}

// The log records of the exporter's own log go under the exporter-wide
// resource, which carries the attribute too.
func TestTheExportersLogRecordsNameTheExporterInstance(t *testing.T) {
	receiver := newLogReceiver(t)
	server, logger, _ := logExportServer(t, receiver, withExporterInstance(""))
	logger.Info("a line to export")
	exportLogsNow(server)
	requests := receiver.logRequests(t)
	if len(requests) != 1 || len(requests[0].ResourceLogs) != 1 {
		t.Fatalf("%d log requests", len(requests))
	}
	var keys []string
	for _, a := range requests[0].ResourceLogs[0].Resource.Attributes {
		keys = append(keys, a.Key+"="+a.Value.StringValue)
	}
	if want := []string{"service.name=prometheus-universal-exporter", "deployment.environment=test", "pue.exporter.instance=replica-1"}; !slices.Equal(keys, want) {
		t.Errorf("the log records' resource is %v, want %v", keys, want)
	}
}

// The debug page's OTLP tab shows the resource as the export makes it,
// with the attribute and a note saying what it is, and its request bodies,
// of the metrics and of the log records, carry it.
func TestTheOTLPTabShowsTheExporterInstance(t *testing.T) {
	receiver := newLogReceiver(t)
	server, _, _ := logExportServer(t, receiver, withExporterInstance(""))
	server.SetDebug(DebugEdit)
	failing := "name: failing\nrequest: {type: http}\ndecoder: {type: json}\ntransform: {type: jq}\nmetrics:\n  - {name: up, type: gauge, expression: .state}\n  - {name: down, type: gauge, expression: .other, error_mode: log}\n"
	otlp := postRun(t, server, pasted(failing, `{"state": 1, "other": "x"}`), http.StatusOK).OTLP
	var resource []string
	for _, a := range otlp.Resource {
		resource = append(resource, a.Key+"="+a.Value)
	}
	if want := []string{"service.name=prometheus-universal-exporter", "deployment.environment=test", "pue.exporter.instance=replica-1"}; !slices.Equal(resource, want) {
		t.Errorf("the tab's resource is %v, want %v", resource, want)
	}
	requireContains(t, strings.Join(otlp.ResourceNotes, "\n"), "pue.exporter.instance names this exporter replica, on every resource it sends (otlp.exporter_instance).")
	attribute := "\"key\": \"pue.exporter.instance\",\n"
	if !strings.Contains(otlp.Body, attribute) || !strings.Contains(otlp.Body, `"stringValue": "replica-1"`) {
		t.Errorf("the metrics body has no attribute:\n%s", otlp.Body)
	}
	if otlp.Logs == nil || !strings.Contains(otlp.Logs.Body, attribute) || !strings.Contains(otlp.Logs.Body, `"stringValue": "replica-1"`) {
		t.Errorf("the logs body has no attribute: %+v", otlp.Logs)
	}
}

// The status page's OTLP section shows the attribute and its value in
// force, in the page and in its JSON as exporter_instance, which is left
// out while the block is off (TestTheStatusPagesOTLPJSON).
func TestTheStatusPageShowsTheExporterInstance(t *testing.T) {
	endpoint := newOTLPEndpoint(t)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}
	withExporterInstance("k8s.pod.name")(&cfg.OTLP)
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	if got := string(rawOTLPSection(t, server)["exporter_instance"]); got != `{"attribute":"k8s.pod.name","value":"replica-1"}` {
		t.Errorf("exporter_instance is %s", got)
	}
	requireContains(t, getPage(t, server, "/static-targets/status"), "<tr><th>Exporter instance</th><td><code>k8s.pod.name</code> = replica-1<br>")
	off := otlpServer(t, endpoint.server.URL)
	if _, has := rawOTLPSection(t, off)["exporter_instance"]; has || strings.Contains(getPage(t, off, "/static-targets/status"), "Exporter instance") {
		t.Error("the section shows an exporter instance while the block is off")
	}
}
