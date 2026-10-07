//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The keys of the OTLP export (otlpKey), from a target's answer and a
// targets file to the endpoint: what a probe and a static target's scrape
// queue is exported apart whatever bytes its labels and its resource hold.

// exportedLines are the points of the one export the server makes now, a
// line each (otlpLines), without the start times, which are the scrape's.
func exportedLines(t *testing.T, server *Server, endpoint *otlpEndpoint) []string {
	t.Helper()
	server.exportOTLP(context.Background(), time.Minute)
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if len(endpoint.bodies) != 1 {
		t.Fatalf("%d exports reached the endpoint, want 1", len(endpoint.bodies))
	}
	lines := otlpLines(t, endpoint.bodies[0])
	for i, line := range lines {
		lines[i], _, _ = strings.Cut(line, " since ")
	}
	return lines
}

// A JSON answer's text may hold a NUL, and a label made of it holds it: a
// probe that reads a series whose label is 1, a NUL and b=2, and one whose
// labels are a=1 and b=2, answers with both and exports both. The first was
// never exported: the second took its place in the queue.
func TestSeriesAProbeReadsWithANULInALabelAreAllExported(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"a":"1\u0000b=2","n":5},{"a":"1","b":"2","n":100},{"b":"3","n":7}]`))
	}))
	t.Cleanup(target.Close)
	c := model.Collector{
		Name: "rows", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "m_total", Type: model.CounterMetricType, Items: ".[]", Expression: ".n", Labels: []model.LabelRule{{Name: "a", Expression: `.a // ""`}, {Name: "b", Expression: `.b // ""`}}}},
	}
	endpoint := newOTLPEndpoint(t)
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{c}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}, nil)
	server.logger = testutil.QuietLogger(t)
	answer := probeOnce(t, server, "/probe?collector=rows&target="+url.QueryEscape(target.URL), nil)
	if want := "# TYPE m_total counter\nm_total{a=\"1\x00b=2\"} 5\nm_total{a=\"1\",b=\"2\"} 100\nm_total{b=\"3\"} 7\n"; answer.Code != http.StatusOK || answer.Body.String() != want {
		t.Fatalf("the probe is answered %d:\n%q\nwant\n%q", answer.Code, answer.Body.String(), want)
	}
	const resource = `[service.name="prometheus-universal-exporter" deployment.environment="test"] `
	exported := slices.DeleteFunc(exportedLines(t, server, endpoint), func(line string) bool { return !strings.Contains(line, "] m_total{") })
	wantLines(t, "the probe's series in the export", exported,
		resource+`m_total{a="1",b="2"} 100`,
		resource+`m_total{a="1\x00b=2"} 5`,
		resource+`m_total{b="3"} 7`)
}

// Two static targets whose resources differ in where an attribute's name
// ends — a=b of the value c, which a targets file may name, and a of the
// value b=c — are exported under a resource each, with their own series
// and health. They were one resource: the series and the health of both
// went out under the attributes of the one scraped first.
func TestStaticTargetsWhoseResourcesWereJoinedAlikeAreExportedApart(t *testing.T) {
	one, two := textTarget(t, "value=1\n"), textTarget(t, "value=2\n")
	endpoint := newOTLPEndpoint(t)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{ExportViaOTLP: true, Name: "one", Collector: "text", Target: one.URL, OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"a=b": "c"}}},
		{ExportViaOTLP: true, Name: "two", Collector: "text", Target: two.URL, OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"a": "b=c"}}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 0)
	const (
		ofOne = `[service.name="prometheus-universal-exporter" a=b="c" deployment.environment="test"] `
		ofTwo = `[service.name="prometheus-universal-exporter" a="b=c" deployment.environment="test"] `
	)
	var values []string
	health := map[string]int{}
	for _, line := range exportedLines(t, server, endpoint) {
		resource, series, _ := strings.Cut(line, "] ")
		resource += "] "
		switch {
		case strings.HasPrefix(series, "demo_value{"):
			values = append(values, line)
		case resource == ofOne && strings.Contains(series, `static_target="one"`), resource == ofTwo && strings.Contains(series, `static_target="two"`):
			health[resource]++
		case resource == ofOne || resource == ofTwo:
			t.Errorf("a target's resource holds %s", line)
		}
	}
	wantLines(t, "the targets' series in the export", values,
		ofTwo+`demo_value{static_target="two"} 2`,
		ofOne+`demo_value{static_target="one"} 1`)
	if health[ofOne] == 0 || health[ofOne] != health[ofTwo] {
		t.Errorf("the export holds %d health points of the one target and %d of the other, under their resources", health[ofOne], health[ofTwo])
	}
}
