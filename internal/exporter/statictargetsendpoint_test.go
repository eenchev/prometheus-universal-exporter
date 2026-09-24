package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The static targets endpoint (statictargetsendpoint.go).

func getStaticTargets(t *testing.T, server *Server, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s answered %d: %s", path, recorder.Code, recorder.Body)
	}
	if err := parseExposition(recorder.Body.Bytes()); err != nil {
		t.Fatalf("%s is not a valid exposition: %v\n%s", path, err, recorder.Body)
	}
	return recorder.Body.String()
}

// Every target's latest result is served on one endpoint, each series
// labelled with its target, with the target's health metrics, and without
// OTLP export being involved at all.
func TestTheStaticTargetsEndpointServesEveryTarget(t *testing.T) {
	up := textTarget(t, "value=42\n")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: up.URL, Labels: map[string]string{"region": "eu"}},
		{Name: "us", Collector: "text", Target: up.URL, Labels: map[string]string{"region": "us"}},
		{Name: "broken", Collector: "text", Target: down.URL},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)

	if body := getStaticTargets(t, server, "/static-targets"); body != "" {
		t.Fatalf("before any scrape the endpoint served:\n%s", body)
	}
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	body := getStaticTargets(t, server, "/static-targets")
	for series, want := range map[string]float64{
		`demo_value{region="eu",static_target="eu"}`:                                                       42,
		`demo_value{region="us",static_target="us"}`:                                                       42,
		`http_exporter_target_up{collector="text",region="eu",static_target="eu",target="` + up.URL + `"}`: 1,
		`http_exporter_target_up{collector="text",static_target="broken",target="` + down.URL + `"}`:       0,
	} {
		if got := metricValue(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if strings.Count(body, "# TYPE demo_value ") != 1 || strings.Count(body, "# TYPE http_exporter_target_up ") != 1 {
		t.Errorf("a family's series are not together under one TYPE:\n%s", body)
	}
	if strings.Contains(body, `demo_value{static_target="broken"`) {
		t.Errorf("the failed target exported metrics:\n%s", body)
	}
	// None of the targets sets export_via_otlp: nothing waits for OTLP.
	if _, ok := pendingValue(server, "demo_value"); ok {
		t.Error("a target without export_via_otlp was queued for OTLP")
	}
}

// With export_via_otlp a target is queued for OTLP as well as served; without
// it, only served.
func TestExportViaOTLPAlsoQueuesTheTargetForOTLP(t *testing.T) {
	target := textTarget(t, "value=7\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "exported", Collector: "text", Target: target.URL, ExportViaOTLP: true, Labels: map[string]string{"via": "otlp"}},
		{Name: "served", Collector: "text", Target: target.URL, Labels: map[string]string{"via": "endpoint"}},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)

	body := getStaticTargets(t, server, "/static-targets")
	for _, name := range []string{"exported", "served"} {
		if !strings.Contains(body, `static_target="`+name+`"`) {
			t.Errorf("%s is not on the endpoint:\n%s", name, body)
		}
	}
	var queued []string
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			if m.Name == "demo_value" {
				queued = append(queued, m.Labels["via"])
			}
		}
	}
	if len(queued) != 1 || queued[0] != "otlp" {
		t.Fatalf("queued for OTLP: %v, want only the target with export_via_otlp", queued)
	}
}

// A family one target exports with another type than an earlier one is left
// out for that target, and logged; everything else of both is served.
func TestAFamilyTypeClashLeavesOutTheLaterTarget(t *testing.T) {
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}, nil)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	merged := server.mergeStaticTargets([]namedSet{
		{name: "a", set: model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.GaugeMetricType, Value: 1}}}},
		{name: "b", set: model.MetricSet{Metrics: []model.Metric{{Name: "shared", Type: model.CounterMetricType, Value: 2}, {Name: "own", Type: model.GaugeMetricType, Value: 3}}}},
	})
	var got []string
	for _, m := range merged.Metrics {
		got = append(got, m.Name+"/"+m.Labels["static_target"])
	}
	if strings.Join(got, " ") != "shared/a own/b" {
		t.Fatalf("merged %v, want a's shared and b's own", got)
	}
	if !strings.Contains(logs.String(), "static target metric left out of the static targets endpoint") || !strings.Contains(logs.String(), `"type_in_use":"gauge"`) {
		t.Fatalf("the clash was not logged:\n%s", logs)
	}
}

// A target removed from the file leaves the endpoint with the reload.
func TestARemovedTargetLeavesTheEndpoint(t *testing.T) {
	target := textTarget(t, "value=1\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	both := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "kept", Collector: "text", Target: target.URL},
		{Name: "removed", Collector: "text", Target: target.URL},
	}}
	server := newStaticServer(t, cfg, both)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	if body := getStaticTargets(t, server, "/static-targets"); !strings.Contains(body, `static_target="removed"`) {
		t.Fatalf("before the reload:\n%s", body)
	}
	one := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: both.Targets[:1]}
	if err := config.ValidateStaticTargets(one); err != nil {
		t.Fatal(err)
	}
	server.manager.SetTargets("", one)
	body := getStaticTargets(t, server, "/static-targets")
	if strings.Contains(body, `static_target="removed"`) || !strings.Contains(body, `static_target="kept"`) {
		t.Fatalf("after the reload:\n%s", body)
	}
}

// The endpoint is at --web.static-targets-path, protected like the
// self-metrics, and read with GET or HEAD only.
func TestTheStaticTargetsEndpointPathAndProtection(t *testing.T) {
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		Web:        model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "u", Password: "p"}},
	}
	server := newStaticServer(t, cfg, nil)
	server.SetStaticTargetsPath("/targets")
	handler := server.Handler()
	for path, want := range map[string]int{"/targets": http.StatusUnauthorized, "/static-targets": http.StatusNotFound} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != want {
			t.Errorf("%s answered %d, want %d", path, recorder.Code, want)
		}
	}
	for method, want := range map[string]int{http.MethodGet: http.StatusOK, http.MethodHead: http.StatusOK, http.MethodPost: http.StatusMethodNotAllowed} {
		request := httptest.NewRequest(method, "/targets", nil)
		request.SetBasicAuth("u", "p")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Errorf("%s /targets answered %d, want %d", method, recorder.Code, want)
		}
	}
}

func TestStaticTargetsPathIsChecked(t *testing.T) {
	for path, want := range map[string]string{"/static-targets": "/static-targets", "targets": "/targets", "/a/b": "/a/b"} {
		if got, err := StaticTargetsPath(path, "/self-metrics"); err != nil || got != want {
			t.Errorf("StaticTargetsPath(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"", "/", "/probe", "/collectors", "/self-metrics", "/x/", "/{a}"} {
		if _, err := StaticTargetsPath(path, "/self-metrics"); err == nil {
			t.Errorf("StaticTargetsPath(%q) was accepted", path)
		}
	}
}
