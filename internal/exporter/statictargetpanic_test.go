package exporter

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A static target scrape runs on the scrape loop's own goroutine, where a panic
// would end the process. It is recovered, logged with its stack, and the
// scrape ends as failed, as a probe's panic does; the next target is scraped
// as usual.
func TestAPanickingStaticScrapeFailsOnlyThatScrape(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	fetch.RequestTypes["panics"] = &fetch.RequestType{
		Name:         "panics",
		Fields:       []string{"path"},
		TargetFields: []string{"path"},
		Validate:     func(*model.Collector) error { return nil },
		Fetch: func(context.Context, string, *model.Collector, fetch.RequestOverrides, http.Header) (*fetch.HTTPResponse, error) {
			panic("boom")
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "panics") })
	registerFixtureType(t)

	broken := testutil.Collector("broken", "text")
	broken.Request = model.RequestConfig{Type: "panics", Path: "/data"}
	cfg := &model.Config{Collectors: []model.Collector{broken, fixtureCollector()}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "broken", Collector: "broken", Target: "panics://data"}, {ExportViaOTLP: true, Name: "fine", Collector: "fixed", Target: "fixture://data"}}}
	server := newStaticServer(t, cfg, file)

	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	up := map[string]float64{}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			if m.Name == "http_exporter_target_up" {
				up[m.Labels["static_target"]] = m.Value
			}
		}
	}
	if len(up) != 2 || up["broken"] != 0 || up["fine"] != 1 {
		t.Fatalf("http_exporter_target_up=%v, want broken 0 and fine 1", up)
	}
	if text := logs.String(); !strings.Contains(text, "static target scrape panicked") || !strings.Contains(text, "boom") || !strings.Contains(text, "stack") {
		t.Fatalf("the panic was not logged with its stack:\n%s", text)
	}
}
