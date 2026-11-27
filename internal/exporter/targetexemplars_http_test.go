//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// exemplarTarget answers body as OpenMetrics.
func exemplarTarget(t *testing.T, body string) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(target.Close)
	return target
}

const exemplarTargetBody = "# TYPE jobs counter\n" +
	"jobs_total 3 # {trace_id=\"0123456789ABCDEF0123456789abcdef\",span_id=\"0123456789abcdef\"} 1 1700000000.5\n" +
	"# TYPE plain counter\nplain_total 4 # {user=\"a\\\"b\"} 2\n" +
	"# TYPE latency histogram\n" +
	"latency_bucket{le=\"0.5\"} 1 # {trace_id=\"t1\"} 0.25\n" +
	"latency_bucket{le=\"+Inf\"} 2 # {trace_id=\"t2\"} 9 1700000001\n" +
	"latency_sum 9.25\nlatency_count 2\n" +
	"# TYPE temperature gauge\ntemperature 20\n# EOF\n"

// End to end: a target's exemplars, on its counters and its histogram's
// buckets, the +Inf bucket's included, are on the series of a probe that
// answers OpenMetrics, with their labels escaped and without a timestamp the
// target did not write, and its labels in order of name; the text format has none; and a counter a rule
// scales has none, its exemplar being of the value the target wrote.
func TestATargetsExemplarsThroughAProbe(t *testing.T) {
	target := exemplarTarget(t, exemplarTargetBody)
	scaled := passthrough("scaled", "", "")
	scale := 2.0
	scaled.Metrics = []model.MetricRule{{Name: "jobs_total", Scale: &scale}}
	server := verboseServer(t, false, passthrough("plain", "", ""), scaled)
	server.logger = testutil.QuietLogger(t)
	probe := func(collector, accept string) string {
		r := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), http.Header{"Accept": {accept}})
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		return r.Body.String()
	}
	want := "# TYPE jobs counter\njobs_total 3 # {span_id=\"0123456789abcdef\",trace_id=\"0123456789ABCDEF0123456789abcdef\"} 1 1700000000.5\n" +
		"# TYPE plain counter\nplain_total 4 # {user=\"a\\\"b\"} 2\n" +
		"# TYPE latency histogram\nlatency_bucket{le=\"0.5\"} 1 # {trace_id=\"t1\"} 0.25\nlatency_bucket{le=\"+Inf\"} 2 # {trace_id=\"t2\"} 9 1700000001\nlatency_sum 9.25\nlatency_count 2\n" +
		"# TYPE temperature gauge\ntemperature 20\n# EOF\n"
	if got := probe("plain", "application/openmetrics-text"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := probe("plain", "text/plain"); strings.Contains(got, " # {") {
		t.Errorf("the text format has exemplars:\n%s", got)
	}
	if got := probe("scaled", "application/openmetrics-text"); !strings.Contains(got, "\njobs_total 6\n") {
		t.Errorf("the scaled counter:\n%s", got)
	}
}

// OTLP carries a target's exemplars on a sum's point and a histogram's: a
// trace_id or span_id label that is an id as its trace or span, lower case,
// any other label as a filtered attribute, and the point's time for an
// exemplar without one. A gauge's exemplar is not carried.
func TestOTLPCarriesATargetsExemplars(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "jobs_total", Type: model.CounterMetricType, Value: 3, Exemplar: &model.Exemplar{Value: 1, Time: at, Labels: map[string]string{
			"trace_id": "0123456789ABCDEF0123456789abcdef", "span_id": "0123456789abcdef", "user": "a",
		}}},
		{Name: "temperature", Type: model.GaugeMetricType, Value: 20, Exemplar: &model.Exemplar{Value: 1, Labels: map[string]string{"user": "a"}}},
		{Name: "latency", Type: model.HistogramMetricType, Histogram: &model.Histogram{Count: 2, Sum: 9.25, Buckets: []model.Bucket{
			{UpperBound: 0.5, CumulativeCount: 1, Exemplar: &model.Exemplar{Value: 0.25, Labels: map[string]string{"trace_id": "t1"}}},
		}}},
	}}
	out, err := json.Marshal(otlpMetrics(set, "5", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"exemplars":[{"filteredAttributes":[{"key":"user","value":{"stringValue":"a"}}],"timeUnixNano":"1700000000000000000","asDouble":1,"spanId":"0123456789abcdef","traceId":"0123456789abcdef0123456789abcdef"}]`,
		`"exemplars":[{"filteredAttributes":[{"key":"trace_id","value":{"stringValue":"t1"}}],"timeUnixNano":"5","asDouble":0.25}]`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %s in\n%s", want, out)
		}
	}
	if n := strings.Count(string(out), `"exemplars"`); n != 2 {
		t.Errorf("%d points with exemplars, want 2:\n%s", n, out)
	}
}

// A static target's exemplars are on its series on /static-targets in
// OpenMetrics, beside its static_target label.
func TestAStaticTargetsExemplarsAreServed(t *testing.T) {
	target := exemplarTarget(t, exemplarTargetBody)
	cfg := &model.Config{Collectors: []model.Collector{passthrough("plain", "", "")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "app", Collector: "plain", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 0)
	r := probeOnce(t, server, "/static-targets", http.Header{"Accept": {"application/openmetrics-text"}})
	for _, want := range []string{
		"\nplain_total{static_target=\"app\"} 4 # {user=\"a\\\"b\"} 2\n",
		"\nlatency_bucket{le=\"+Inf\",static_target=\"app\"} 2 # {trace_id=\"t2\"} 9 1700000001\n",
	} {
		if !strings.Contains(r.Body.String(), want) {
			t.Errorf("no %q in\n%s", want, r.Body.String())
		}
	}
}
