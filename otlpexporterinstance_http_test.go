//go:build !select_request_types || request_type_http

package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// otlpMetricsReceiver is a collector that keeps the bodies of the metrics
// exports it is sent, gunzipped, one after another.
func otlpMetricsReceiver(t *testing.T) (*httptest.Server, *syncBuffer) {
	t.Helper()
	bodies := &syncBuffer{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		if r.URL.Path == "/v1/metrics" {
			_, _ = bodies.Write(append(body, '\n'))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	return collector, bodies
}

// exporterInstanceAttribute is the attribute as an export's JSON carries it.
func exporterInstanceAttribute(value string) string {
	return `{"key":"pue.exporter.instance","value":{"stringValue":"` + value + `"}}`
}

// Outside allocator mode the exporter's resource is named by the host name,
// read when the configuration loads, which the start's line says.
func TestTheExporterInstanceIsTheHostName(t *testing.T) {
	tier.Slow(t)
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("the host name cannot be read: %v", err)
	}
	collector, bodies := otlpMetricsReceiver(t)
	p := startExporterProcess(t, "otlp:\n  enabled: true\n  endpoint: "+collector.URL+"/v1/metrics\n  interval: 1s\n  exporter_instance:\n    enabled: true\n", childOptions{})
	testutil.WaitFor(t, "an export naming the host", func() bool { return strings.Contains(bodies.String(), exporterInstanceAttribute(host)) })
	if logs := p.logs.String(); !strings.Contains(logs, `"otlp_exporter_instance_attribute":"pue.exporter.instance","otlp_exporter_instance":"`+host+`"`) {
		t.Errorf("the start's line does not name the exporter instance:\n%s", logs)
	}
}

// In allocator mode the resource is named by the --allocator.member name,
// whatever the host name: the bundle's configuration is loaded with it, as
// the status page shows. (The export itself follows the configuration in
// force, which TestEveryOTLPResourceNamesTheExporterInstance covers; its
// first one after the first bundle waits for the interval of the
// configuration the replica awaited with, 30s.)
func TestTheExporterInstanceIsTheAllocatorMember(t *testing.T) {
	collector, _ := otlpMetricsReceiver(t)
	a := newAllocatorStandIn(t)
	p := startAllocatorExporter(t, "--allocator.url="+a.server.URL, "--allocator.cache-dir="+t.TempDir(), "--allocator.member=pod-7", "--allocator.advertise=10.0.0.7:8080")
	a.serve(map[string]string{"config.yaml": "otlp:\n  enabled: true\n  endpoint: " + collector.URL + "/v1/metrics\n  interval: 1s\n  exporter_instance:\n    enabled: true\n" + allocatorTestConfig})
	var body string
	testutil.WaitFor(t, "the bundle in force", func() bool {
		var code int
		code, body = p.get(t, "/static-targets/status?format=json")
		return code == http.StatusOK
	})
	if !strings.Contains(body, `"exporter_instance":{"attribute":"pue.exporter.instance","value":"pod-7"}`) {
		t.Errorf("the status page does not name the member:\n%s", body)
	}
}

// --check resolves the value as startup does, the --allocator.member name in
// allocator mode, and its report says what the resource will carry.
func TestTheCheckReportsTheExporterInstance(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", "otlp:\n  enabled: true\n  endpoint: http://collector.invalid:4318/v1/metrics\n  exporter_instance: {enabled: true}\n"+testutil.MinimalConfig)
	report := checkStartup(checkInputs{ConfigFile: path, ExporterInstance: "pod-7"})
	if len(report.Checks) == 0 || report.Checks[0].Check != "config" || report.Checks[0].Status != checkOK {
		t.Fatalf("report %+v", report)
	}
	if got, _ := report.Checks[0].Details["otlp_exporter_instance"].(map[string]string); got["attribute"] != "pue.exporter.instance" || got["value"] != "pod-7" {
		t.Errorf("the config check's details: %v", report.Checks[0].Details)
	}
	if _, listed := checkStartup(checkInputs{ConfigFile: testutil.WriteFile(t, "plain.yaml", testutil.MinimalConfig)}).Checks[0].Details["otlp_exporter_instance"]; listed {
		t.Error("a configuration without the block lists an exporter instance")
	}
}
