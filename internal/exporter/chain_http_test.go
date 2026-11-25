//go:build !select_request_types || request_type_http

package exporter

import (
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

// jenkinsTarget answers a Jenkins-like list of jobs and each job's last
// build, web's with 404.
func jenkinsTarget(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/json":
			_, _ = w.Write([]byte(`{"jobs":[{"name":"api"},{"name":"web"},{"name":"db"}]}`))
		case "/job/api/lastBuild/api/json":
			_, _ = w.Write([]byte(`{"duration":81000}`))
		case "/job/db/lastBuild/api/json":
			_, _ = w.Write([]byte(`{"duration":5000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// jenkinsCollector fans out over the jobs of jenkinsTarget under answer:
// all, with on_item_error as given, reading each build's duration and the
// first answer's status from $steps.
func jenkinsCollector(name, onItemError string, reportFailures bool) model.Collector {
	return model.Collector{
		Name: name,
		Request: model.RequestConfig{Type: fetch.RequestTypeHTTP, Path: "/api/json", Own: fetch.HTTPConfig{
			Extract: map[string]model.ChainExtract{"jobs": {JSON: "[.jobs[].name]"}},
			Then:    []model.ChainStep{{Name: "last_build", ForEach: "jobs", Path: "/job/{{each}}/lastBuild/api/json", OnItemError: onItemError}},
		}},
		Answer:         model.ChainAnswerAll,
		ReportFailures: reportFailures,
		Transform:      model.TransformConfig{Type: "jq"},
		Limits:         model.Limits{MaxResponseBytes: 1 << 20},
		Metrics: []model.MetricRule{
			{Name: "jenkins_last_build_seconds", Type: model.GaugeMetricType, Items: ".last_build[] | select(.body)", Expression: ".body.duration / 1000", Labels: []model.LabelRule{{Name: "job", Expression: ".each"}}},
			{Name: "jenkins_first_status", Type: model.GaugeMetricType, Expression: "$steps.first.status"},
			{Name: "jenkins_builds_asked", Type: model.GaugeMetricType, Expression: "$steps.last_build | length"},
		},
	}
}

// A probe of a chained collector reads the document of every answer and
// $steps; the chain's self-metrics count each step by its name. An item
// that fails under skip is counted and logged once, summed; under fail it
// fails the probe in the http_status stage, its error and log line naming
// the step and the item, and report_failures labels the failed stage with
// the step.
func TestAProbeOfAChain(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := jenkinsTarget(t)
	server := modeServer(t, jenkinsCollector("skipping", model.ChainItemSkip, false), jenkinsCollector("failing", "", false), jenkinsCollector("reporting", "", true))
	probeURL := func(collector string) string {
		return "/probe?collector=" + collector + "&target=" + url.QueryEscape(target.URL)
	}

	r := probeOnce(t, server, probeURL("skipping"), nil)
	body := r.Body.String()
	if r.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", r.Code, body)
	}
	for _, want := range []string{`jenkins_last_build_seconds{job="api"} 81`, `jenkins_last_build_seconds{job="db"} 5`, "jenkins_first_status 200", "jenkins_builds_asked 3"} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, `job="web"`) {
		t.Errorf("the skipped item made a series:\n%s", body)
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_chain_requests_total{collector="skipping",step="first"} 1`,
		`http_exporter_chain_requests_total{collector="skipping",step="last_build"} 3`,
		`http_exporter_chain_step_failures_total{collector="skipping",step="last_build"} 1`,
		`http_exporter_chain_step_failures_total{collector="skipping",step="first"} 0`,
		`http_exporter_chain_items{collector="skipping",step="last_build"} 3`,
		`http_exporter_chain_items_cut_total{collector="skipping",step="last_build"} 0`,
		`http_exporter_scrape_http_status_code{collector="skipping"} 200`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics", want)
		}
	}
	if strings.Contains(metrics, `http_exporter_chain_items{collector="skipping",step="first"}`) {
		t.Error("a step that does not fan out has a series of items")
	}
	if strings.Contains(metrics, "http_exporter_chain_step_duration_seconds") {
		t.Error("the step durations are there without verbose mode")
	}
	if n := strings.Count(logs.String(), `step \"last_build\": 1 of 3 items failed and were skipped`); n != 1 {
		t.Errorf("%d lines summing the skipped items:\n%s", n, logs)
	}

	r = probeOnce(t, server, probeURL("failing"), nil)
	if want := `collector failing http_status failed: step "last_build" item 2 of 3 ("web"): received HTTP status 404`; r.Code != http.StatusBadGateway || !strings.HasPrefix(r.Body.String(), want) {
		t.Fatalf("answered %d: %s\nwant %s", r.Code, r.Body, want)
	}
	if !strings.Contains(logs.String(), `"step":"last_build","item":2,"items":3`) {
		t.Errorf("the failure was not logged with its step and item:\n%s", logs)
	}

	r = probeOnce(t, server, probeURL("reporting"), nil)
	if want := `http_exporter_probe_failed_stage{stage="http_status",step="last_build"} 1`; r.Code != http.StatusOK || !strings.Contains(r.Body.String(), want+"\n") {
		t.Fatalf("answered %d, without %s: %s", r.Code, want, r.Body)
	}
}

// A Python script reads the steps of a chain as steps, and verbose mode adds
// a histogram of each step's duration, by its name.
func TestAChainsStepsInPythonAndItsVerboseHistogram(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := jenkinsTarget(t)
	collector := jenkinsCollector("scripted", model.ChainItemSkip, false)
	collector.Transform = model.TransformConfig{Type: "python", Script: "metric('jenkins_first_status', value=steps['first']['status'])\nmetric('jenkins_builds_asked', value=len(steps['last_build']))"}
	collector.Metrics = nil
	collector.Limits.ScriptTimeout = model.Duration(time.Minute)
	server := verboseServer(t, true, collector)
	r := probeOnce(t, server, "/probe?collector=scripted&target="+url.QueryEscape(target.URL), nil)
	for _, want := range []string{"jenkins_first_status 200", "jenkins_builds_asked 3"} {
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), want+"\n") {
			t.Fatalf("answered %d without %q: %s\n%s", r.Code, want, r.Body, logs)
		}
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_chain_step_duration_seconds_count{collector="scripted",step="first"} 1`,
		`http_exporter_chain_step_duration_seconds_count{collector="scripted",step="last_build"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics", want)
		}
	}
}

// Each request of a chain is a CLIENT span with the step it was made for,
// pue.chain.step, and a fan-out item's with its place, pue.chain.item.
func TestEachRequestOfAChainIsASpanOfItsStep(t *testing.T) {
	target := jenkinsTarget(t)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Hour, func(c *model.Config) {
		c.OTLP.Traces.SampleRatio = 1
		c.Collectors = append(c.Collectors, jenkinsCollector("traced", model.ChainItemSkip, false))
	}, nil)
	if answer := probeOnce(t, server, "/probe?collector=traced&target="+url.QueryEscape(target.URL), nil); answer.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", answer.Code, answer.Body)
	}
	exportTracesNow(server)
	var steps []string
	for _, span := range receiver.spans(t) {
		if span.Kind != otlpSpanKindClient {
			continue
		}
		attrs := spanAttributes(span.otlpSpan)
		steps = append(steps, attrs["pue.chain.step"]+" "+attrs["pue.chain.item"])
	}
	slices.Sort(steps)
	if want := []string{"first ", "last_build 1", "last_build 2", "last_build 3"}; !slices.Equal(steps, want) {
		t.Fatalf("the requests' spans are %v, want %v", steps, want)
	}
}
