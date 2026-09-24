//go:build !select_request_types || request_type_graphite

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// fakeGraphite answers /render as graphite-web does, with points just
// written, and records the targets each request asked for.
type fakeGraphite struct {
	mu      sync.Mutex
	targets [][]string
}

func (g *fakeGraphite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/render" || r.URL.Query().Get("format") != "json" {
		http.Error(w, "not a render request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.targets = append(g.targets, r.URL.Query()["target"])
	g.mu.Unlock()
	now := time.Now().Unix()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `[
	  {"target": "app.web01.requests", "tags": {"name": "app.web01.requests"}, "datapoints": [[40, %[1]d], [42, %[2]d], [null, %[3]d]]},
	  {"target": "app.web02.requests", "tags": {"name": "app.web02.requests"}, "datapoints": [[7, %[2]d]]},
	  {"target": "app.web03.requests", "tags": {"name": "app.web03.requests"}, "datapoints": [[null, %[2]d]]},
	  {"target": "app.old.requests", "tags": {"name": "app.old.requests"}, "datapoints": [[1, %[4]d]]},
	  {"target": "cpu.load;env=staging", "tags": {"name": "cpu.load", "env": "staging"}, "datapoints": [[0.5, %[2]d]]}
	]`, now-120, now-60, now, now-3600)
}

func (g *fakeGraphite) asked() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]string(nil), g.targets...)
}

func graphiteExporter(t *testing.T, yaml string) *Server {
	t.Helper()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yaml))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
}

const graphiteExporterConfig = `collectors:
  - name: graphite_app
    request:
      type: graphite
      targets:
        - app.*.requests
        - "seriesByTag('name=cpu.load', 'env={{param_env:prod}}')"
    response:
      graphite:
        max_age: 10m
    transform:
      type: jq
    metrics:
      - name: app_requests
        items: .series[] | select(.segments[0] == "app")
        expression: .value
        labels:
          - name: host
            expression: .segments[1]
      - name: cpu_load
        items: .series[] | select(.path == "cpu.load")
        expression: .value
        labels:
          - name: env
            expression: .tags.env
`

// A probe asks Graphite for the collector's expressions, placeholders
// filled from the probe, and maps the series with jq: the newest value of
// each, a series of nulls and one older than max_age left out.
func TestAGraphiteCollectorProbe(t *testing.T) {
	graphite := &fakeGraphite{}
	upstream := httptest.NewServer(graphite)
	defer upstream.Close()
	server := graphiteExporter(t, graphiteExporterConfig)
	recorder := probeOnce(t, server, "/probe?collector=graphite_app&param_env=staging&target="+url.QueryEscape(upstream.URL), nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, body)
	}
	for _, want := range []string{`app_requests{host="web01"} 42`, `app_requests{host="web02"} 7`, `cpu_load{env="staging"} 0.5`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	for _, absent := range []string{"web03", `host="old"`} {
		if strings.Contains(body, absent) {
			t.Errorf("%s is exported:\n%s", absent, body)
		}
	}
	asked := graphite.asked()
	if len(asked) != 1 || strings.Join(asked[0], " | ") != "app.*.requests | seriesByTag('name=cpu.load', 'env=staging')" {
		t.Fatalf("asked %q", asked)
	}
	// The series of nulls and the one older than max_age are counted.
	if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_decoder_series_left_out_total{collector="graphite_app"} 2`) {
		t.Fatalf("%s", metrics)
	}
	// A value that could change the expression is refused before Graphite
	// is asked; so is a probe parameter of http's alone.
	for query, want := range map[string]string{
		"param_env=" + url.QueryEscape("x'),sumSeries('y"): "lands inside a Graphite expression",
		"method=POST": `request.type is "graphite"`,
	} {
		recorder = probeOnce(t, server, "/probe?collector=graphite_app&target="+url.QueryEscape(upstream.URL)+"&"+query, nil)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%s: %d %s", query, recorder.Code, recorder.Body)
		}
	}
	if len(graphite.asked()) != 1 {
		t.Fatalf("a refused probe reached Graphite: %q", graphite.asked())
	}
}

// A Python script reads the same series.
func TestAGraphiteCollectorWithPython(t *testing.T) {
	requirePython(t)
	upstream := httptest.NewServer(&fakeGraphite{})
	defer upstream.Close()
	server := graphiteExporter(t, `collectors:
  - name: graphite_py
    request:
      type: graphite
      targets: [app.*.requests]
    transform:
      type: python
      script: |
        for s in data["series"]:
            if s["segments"][0] == "app":
                metric(name="app_requests_max", value=max(v for v, _ in s["points"]), labels={"host": s["segments"][1]})
`)
	recorder := probeOnce(t, server, "/probe?collector=graphite_py&target="+url.QueryEscape(upstream.URL), nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `app_requests_max{host="web01"} 42`) || !strings.Contains(recorder.Body.String(), `app_requests_max{host="old"} 1`) {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
}

// A static target brings its own expressions, and is scraped with them.
func TestAGraphiteStaticTarget(t *testing.T) {
	graphite := &fakeGraphite{}
	upstream := httptest.NewServer(graphite)
	defer upstream.Close()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", graphiteExporterConfig))
	if err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{
		Name: "db", Collector: "graphite_app", Target: upstream.URL,
		Request: model.TargetRequestConfig{Targets: []string{"app.web0*.requests"}, From: "-1h"},
	}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(t.Context(), 10*time.Second)
	asked := graphite.asked()
	if len(asked) != 1 || strings.Join(asked[0], " | ") != "app.web0*.requests" {
		t.Fatalf("asked %q", asked)
	}
	body := getStaticTargets(t, server, "/static-targets")
	if !strings.Contains(body, `app_requests{host="web01",static_target="db"} 42`) {
		t.Fatalf("%s", body)
	}
}

// The collectors page says what a graphite collector's target is.
func TestTheCollectorsPageShowsAGraphiteTarget(t *testing.T) {
	server := graphiteExporter(t, graphiteExporterConfig)
	recorder := probeOnce(t, server, "/collectors", nil)
	if !strings.Contains(recorder.Body.String(), "http://graphite:8080") {
		t.Fatalf("%s", recorder.Body)
	}
}

// A static target's own targets and window are part of its cache key: two
// targets of one caching collector on one server, asking for different
// series, never read each other's result, nor does a probe of the collector.
func TestGraphiteStaticTargetsCacheApart(t *testing.T) {
	graphite := &fakeGraphite{}
	upstream := httptest.NewServer(graphite)
	defer upstream.Close()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.Replace(graphiteExporterConfig, "    transform:\n", "    cache:\n      ttl: 1h\n    transform:\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "web", Collector: "graphite_app", Target: upstream.URL, Request: model.TargetRequestConfig{Targets: []string{"app.web*.requests"}}},
		{Name: "db", Collector: "graphite_app", Target: upstream.URL, Request: model.TargetRequestConfig{Targets: []string{"db.*.requests"}}},
		{Name: "db_hour", Collector: "graphite_app", Target: upstream.URL, Request: model.TargetRequestConfig{Targets: []string{"db.*.requests"}, From: "-1h"}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(t.Context(), 10*time.Second)
	probeOnce(t, server, "/probe?collector=graphite_app&target="+url.QueryEscape(upstream.URL), nil)
	var asked []string
	for _, targets := range graphite.asked() {
		asked = append(asked, strings.Join(targets, " | "))
	}
	sort.Strings(asked)
	if want := []string{"app.*.requests | seriesByTag('name=cpu.load', 'env=prod')", "app.web*.requests", "db.*.requests", "db.*.requests"}; !slices.Equal(asked, want) {
		t.Fatalf("asked %q, want %q", asked, want)
	}
	// Scraped again, every one is answered from the cache.
	server.scrapeStaticTargets(t.Context(), 10*time.Second)
	if len(graphite.asked()) != 4 {
		t.Fatalf("asked again: %q", graphite.asked())
	}
}

// A static target's from and until are the probe parameters a probe would
// send for the same window, so the two share cache entries; its targets,
// which no probe can send, are kept apart.
func TestAGraphiteStaticTargetsCacheQuery(t *testing.T) {
	target := &model.StaticTarget{Target: "http://graphite:8080", Collector: "graphite_app", Request: model.TargetRequestConfig{From: " -1h ", Until: "-1min", Targets: []string{"a.b"}}}
	if q := targetCacheQuery(target); q.Get("from") != "-1h" || q.Get("until") != "-1min" || q.Has("targets") {
		t.Fatalf("query %v", q)
	}
	if own := targetOwnRequest(target); !slices.Equal(own, []string{"targets", "a.b"}) {
		t.Fatalf("own %q", own)
	}
	if own := targetOwnRequest(&model.StaticTarget{Request: model.TargetRequestConfig{From: "-1h"}}); own != nil {
		t.Fatalf("own %q", own)
	}
}
