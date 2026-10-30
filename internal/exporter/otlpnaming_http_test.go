//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// otlpReceiver is an OTLP endpoint that takes every export and hands each
// request's payload to the test.
func otlpReceiver(t *testing.T) (string, <-chan otlpPayload) {
	t.Helper()
	received := make(chan otlpPayload, 16)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload otlpPayload
		if err := json.Unmarshal(readOTLPBody(t, r), &payload); err != nil {
			t.Errorf("payload: %v", err)
		}
		received <- payload
	}))
	t.Cleanup(endpoint.Close)
	return endpoint.URL + "/v1/metrics", received
}

// attributeText is a list of attributes as name=value, in their order.
func attributeText(attributes []otlpAttribute) string {
	var parts []string
	for _, a := range attributes {
		parts = append(parts, a.Key+"="+a.Value.StringValue)
	}
	return strings.Join(parts, ",")
}

// Static targets with otlp.labels_as_resource and otlp.instance_id, scraped
// and exported to an endpoint: a target's labels named there are its
// resource's, not its points', on its own series and its health series
// alike, and instance_id makes its name its resource's service.instance.id.
// Targets whose resources are alike in every attribute are one resource,
// as before; one with instance_id, or with another value of a label moved,
// is a resource of its own. The static targets endpoint keeps the labels.
func TestStaticTargetLabelsAsResourceAreExportedOnTheResource(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	endpoint, received := otlpReceiver(t)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint)}
	moved := model.TargetOTLPConfig{LabelsAsResource: []string{"city"}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "berlin_a", Collector: "text", Target: target.URL, ExportViaOTLP: true, Labels: map[string]string{"city": "berlin", "rack": "1"}, OTLP: moved},
		{Name: "berlin_b", Collector: "text", Target: target.URL, ExportViaOTLP: true, Labels: map[string]string{"city": "berlin", "rack": "2"}, OTLP: moved},
		{Name: "paris", Collector: "text", Target: target.URL, ExportViaOTLP: true, Labels: map[string]string{"city": "paris", "rack": "1"}, OTLP: model.TargetOTLPConfig{LabelsAsResource: []string{"city"}, InstanceID: true}},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 0)
	server.exportOTLP(context.Background(), time.Minute)
	var payload otlpPayload
	select {
	case payload = <-received:
	case <-time.After(30 * time.Second):
		t.Fatal("no OTLP payload was delivered")
	}
	// Each resource's attributes, and the series of each of its metrics
	// of a target, as the attributes of their points.
	got := map[string][]string{}
	for _, resource := range payload.ResourceMetrics {
		key := attributeText(resource.Resource.Attributes)
		for _, m := range resource.ScopeMetrics[0].Metrics {
			if m.Gauge == nil || !strings.HasPrefix(m.Name, "demo_value") && !strings.HasPrefix(m.Name, "http_exporter_target_up") {
				continue
			}
			for _, p := range m.Gauge.DataPoints {
				got[key] = append(got[key], m.Name+"{"+strings.ReplaceAll(attributeText(p.Attributes), target.URL, "URL")+"}")
			}
		}
	}
	want := map[string][]string{
		"service.name=prometheus-universal-exporter,city=berlin,deployment.environment=test": {
			"demo_value{rack=1,static_target=berlin_a}", "demo_value{rack=2,static_target=berlin_b}",
			"http_exporter_target_up{collector=text,rack=1,static_target=berlin_a,target=URL}", "http_exporter_target_up{collector=text,rack=2,static_target=berlin_b,target=URL}",
		},
		"service.name=prometheus-universal-exporter,city=paris,deployment.environment=test,service.instance.id=paris": {
			"demo_value{rack=1,static_target=paris}", "http_exporter_target_up{collector=text,rack=1,static_target=paris,target=URL}",
		},
	}
	for key := range got {
		// The exporter-wide resource, with the exporter's own metrics.
		if !strings.Contains(key, "city=") {
			delete(got, key)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the export is\n%q\nwant\n%q", got, want)
	}
	if exposition := getStaticTargets(t, server, "/static-targets"); !strings.Contains(exposition, `city="berlin"`) || !strings.Contains(exposition, `city="paris"`) {
		t.Errorf("the static targets endpoint lost the labels:\n%s", exposition)
	}
}

// otlp.metric_names: otel names the exporter's own metrics as it names the
// targets': a counter without its _total, and a gauge of seconds with the
// unit s.
func TestOTelMetricNamesNameTheExportersOwnMetrics(t *testing.T) {
	endpoint, received := otlpReceiver(t)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint)}
	cfg.OTLP.MetricNames = model.OTLPMetricNamesOTel
	server := newStaticServer(t, cfg, nil)
	server.exportOTLP(context.Background(), time.Minute)
	var payload otlpPayload
	select {
	case payload = <-received:
	case <-time.After(30 * time.Second):
		t.Fatal("no OTLP payload was delivered")
	}
	shapes := map[string]string{}
	for _, resource := range payload.ResourceMetrics {
		for _, m := range resource.ScopeMetrics[0].Metrics {
			shapes[m.Name] = otlpKindNames[otlpKind(m)] + " " + m.Unit
			if strings.HasSuffix(m.Name, "_total") || strings.HasSuffix(m.Name, "_seconds") || strings.HasSuffix(m.Name, "_bytes") {
				t.Errorf("the exporter's own %s keeps its suffix", m.Name)
			}
		}
	}
	for name, want := range map[string]string{
		"http_exporter_otlp_points_dropped":  "sum ",
		"http_exporter_otlp_export_duration": "gauge s",
	} {
		if shapes[name] != want {
			t.Errorf("%s is exported as %q, want %q; the export has %v", name, shapes[name], want, shapes)
		}
	}
}
