package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// pendingPoints lists the points waiting for export, as name, labels and value.
func pendingPoints(server *Server) []model.Metric {
	server.otlpMu.Lock()
	defer server.otlpMu.Unlock()
	var out []model.Metric
	for _, batch := range server.otlpPending {
		for _, pending := range batch.metrics {
			out = append(out, pending.metric)
		}
	}
	return out
}

// Probes of two targets answering the same series are one point, the later
// probe's, unless otlp.probe_attributes keeps them apart with collector and
// target attributes; a label of the series' own by either name is kept.
func TestOTLPProbeAttributes(t *testing.T) {
	one, two := textTarget(t, "value=1\n"), textTarget(t, "value=2\n")
	for _, attributes := range []bool{false, true} {
		otlp := otlpConfig("http://collector.invalid/v1/metrics")
		otlp.ProbeAttributes = attributes
		server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlp}, nil)
		server.logger = testutil.QuietLogger(t)
		for _, target := range []string{one.URL, two.URL} {
			if recorder := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(target), nil); recorder.Code != http.StatusOK {
				t.Fatalf("%d %s", recorder.Code, recorder.Body)
			}
		}
		points := pendingPoints(server)
		if !attributes {
			if len(points) != 1 || points[0].Value != 2 || len(points[0].Labels) != 0 {
				t.Fatalf("without probe_attributes: %+v", points)
			}
			continue
		}
		if len(points) != 2 {
			t.Fatalf("with probe_attributes: %+v", points)
		}
		byTarget := map[string]float64{}
		for _, p := range points {
			if p.Labels["collector"] != "text" {
				t.Fatalf("point %+v has no collector attribute", p)
			}
			byTarget[p.Labels["target"]] = p.Value
		}
		if byTarget[one.URL] != 1 || byTarget[two.URL] != 2 {
			t.Fatalf("points by target %v", byTarget)
		}
	}
	// A label of the series' own named target is kept.
	otlp := otlpConfig("http://collector.invalid/v1/metrics")
	otlp.ProbeAttributes = true
	server := newStaticServer(t, &model.Config{OTLP: otlp, Collectors: []model.Collector{testutil.Collector("text", "text")}}, nil)
	server.queueProbeOTLP(model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"target": "own"}}}}, "text", "http://t")
	if points := pendingPoints(server); len(points) != 1 || points[0].Labels["target"] != "own" || points[0].Labels["collector"] != "text" {
		t.Fatalf("%+v", points)
	}
}

// A point goes out with the time it was queued, not the time of the export,
// and keeps it through a retry.
func TestOTLPPointsKeepTheTimeTheyWereQueued(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable, http.StatusOK)
	server := otlpServer(t, endpoint.server.URL)
	before := time.Now()
	queueProbeMetric(server, "queued", 1)
	after := time.Now()
	time.Sleep(50 * time.Millisecond)
	server.exportOTLP(t.Context(), time.Second)
	endpoint.mu.Lock()
	body := string(endpoint.bodies[len(endpoint.bodies)-1])
	endpoint.mu.Unlock()
	var payload otlpPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range payload.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				if metric.Name != "queued" {
					continue
				}
				found = true
				at, _ := strconv.ParseInt(metric.Gauge.DataPoints[0].TimeUnixNano, 10, 64)
				if at < before.Truncate(time.Millisecond).UnixNano() || at > after.UnixNano() {
					t.Fatalf("point at %s, queued between %s and %s", time.Unix(0, at), before, after)
				}
			}
		}
	}
	if !found {
		t.Fatalf("the point was not exported:\n%s", body)
	}
}

// The start times remembered are bounded by twice otlp.max_pending_points:
// past it, the series seen least recently is forgotten, and starts again
// when it comes back; one seen again recently is kept.
func TestOTLPStartTimesAreBounded(t *testing.T) {
	starts := newOTLPStartTimes()
	starts.bound(2)
	if starts.max != 4 {
		t.Fatalf("remembers %d series for max_pending_points 2, want 4", starts.max)
	}
	start := starts.forResource("r")
	counter := func(name string) model.Metric {
		return model.Metric{Name: name, Type: model.CounterMetricType, Value: 1}
	}
	for i, name := range []string{"a", "b", "c", "d"} {
		start(counter(name), strconv.Itoa(i))
	}
	// a is seen again, so b is now the least recently seen.
	if got := start(counter("a"), "10"); got != "0" {
		t.Fatalf("a: %s", got)
	}
	start(counter("e"), "11")
	if n := len(starts.series); n != 4 {
		t.Fatalf("%d series remembered, want 4", n)
	}
	if got := start(counter("a"), "12"); got != "0" {
		t.Fatalf("a, seen recently, was forgotten: starts at %s", got)
	}
	if got := start(counter("b"), "13"); got != "13" {
		t.Fatalf("b, seen least recently, was not forgotten: starts at %s", got)
	}
	if n := len(starts.series); n != 4 || starts.recent.Len() != 4 {
		t.Fatalf("%d series remembered (%d in order), want 4", n, starts.recent.Len())
	}
	// An export bounds them by the configured otlp.max_pending_points.
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(endpoint.Close)
	otlp := otlpConfig(endpoint.URL)
	otlp.MaxPendingPoints = 3
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlp}, nil)
	var set model.MetricSet
	for i := range 10 {
		set.Metrics = append(set.Metrics, counter("c"+strconv.Itoa(i)))
	}
	if _, _, err := server.pushOTLP(context.Background(), otlp, []otlpResourceSet{{Identity: defaultResourceIdentity(otlp), Set: set}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := len(server.otlpStarts.series); n != 6 {
		t.Fatalf("an export of 10 counters with max_pending_points 3 left %d start times, want 6", n)
	}
	// Without otlp.max_pending_points, its default applies.
	starts.bound(0)
	if want := 2 * model.DefaultOTLPMaxPendingPoints; starts.max != want {
		t.Fatalf("remembers %d series by default, want %d", starts.max, want)
	}
}

// A cumulative point starts when its series was first exported, and again
// after a reset, when its count went down; a gauge has no start.
func TestOTLPStartTimes(t *testing.T) {
	starts := newOTLPStartTimes()
	start := starts.forResource("r")
	counter := func(v float64) model.Metric {
		return model.Metric{Name: "c_total", Type: model.CounterMetricType, Value: v}
	}
	if got := start(counter(5), "100"); got != "100" {
		t.Fatalf("first: %s", got)
	}
	if got := start(counter(7), "200"); got != "100" {
		t.Fatalf("growing: %s", got)
	}
	if got := start(counter(2), "300"); got != "300" {
		t.Fatalf("after a reset: %s", got)
	}
	if got := starts.forResource("other")(counter(9), "400"); got != "400" {
		t.Fatalf("another resource: %s", got)
	}
	histogram := model.Metric{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Count: 3, Sum: 1}}
	if got := start(histogram, "500"); got != "500" {
		t.Fatalf("histogram: %s", got)
	}
	histogram.Histogram = &model.Histogram{Count: 4, Sum: 2}
	if got := start(histogram, "600"); got != "500" {
		t.Fatalf("histogram growing: %s", got)
	}
	// Forgotten after an hour unseen.
	starts.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if got := start(counter(8), "700"); got != "700" {
		t.Fatalf("after an hour unseen: %s", got)
	}
	raw, _ := json.Marshal(otlpMetrics(model.MetricSet{Metrics: []model.Metric{counter(1), {Name: "g", Type: model.GaugeMetricType, Value: 1}}}, "900", newOTLPStartTimes().forResource("r")))
	text := string(raw)
	if strings.Count(text, `"startTimeUnixNano":"900"`) != 1 {
		t.Fatalf("one start, the counter's: %s", text)
	}
}
