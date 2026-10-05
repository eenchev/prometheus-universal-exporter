//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// End to end: a histogram passed through from a scheduled Prometheus target,
// the verbose scrape-time histogram and the GC summary all arrive at the
// collector with their data.
func TestHistogramsAndSummariesReachTheOTLPEndpoint(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# TYPE latency_seconds histogram\nlatency_seconds_bucket{le=\"0.5\"} 3\nlatency_seconds_bucket{le=\"+Inf\"} 4\nlatency_seconds_sum 1.25\nlatency_seconds_count 4\n"))
	}))
	defer target.Close()
	received := make(chan []byte, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- readOTLPBody(t, r)
	}))
	defer endpoint.Close()

	c := testutil.Collector("passthrough", "prometheus")
	c.Transform = model.TransformConfig{Type: "prometheus"}
	c.Metrics = nil
	cfg := &model.Config{Collectors: []model.Collector{c}, OTLP: otlpConfig(endpoint.URL + "/v1/metrics"), Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true, ResourceMetrics: true}}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "scheduled", Collector: "passthrough", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	probeOnce(t, server, "/probe?collector=passthrough&target="+url.QueryEscape(target.URL), nil)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	server.exportOTLP(context.Background(), 5*time.Second)

	var payload otlpPayload
	select {
	case body := <-received:
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%v\n%s", err, body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("nothing was exported")
	}
	found := map[string]otlpMetric{}
	for _, resource := range payload.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, m := range scope.Metrics {
				found[m.Name] = m
			}
		}
	}
	passed := found["latency_seconds"]
	if passed.Histogram == nil || passed.Histogram.DataPoints[0].Count != "4" || !reflect.DeepEqual(passed.Histogram.DataPoints[0].BucketCounts, []string{"3", "1"}) {
		t.Fatalf("the passed-through histogram arrived as %+v", passed)
	}
	scrape := found["http_exporter_collector_scrape_duration_seconds"]
	if scrape.Histogram == nil || scrape.Histogram.DataPoints[0].Count != "2" {
		t.Fatalf("the scrape-time histogram arrived as %+v", scrape)
	}
	var total int
	for _, count := range scrape.Histogram.DataPoints[0].BucketCounts {
		n, _ := json.Number(count).Int64()
		total += int(n)
	}
	if total != 2 {
		t.Fatalf("bucket counts add up to %d, want the count 2", total)
	}
	if gc := found["go_gc_duration_seconds"]; gc.Summary == nil {
		t.Fatalf("the GC summary arrived as %+v", gc)
	}
}
