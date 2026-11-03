//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// probeHistogram is the http_exporter_probe_duration_seconds series of
// collector and kind in the self-metric set, nil when there is none.
func probeHistogram(server *Server, collector, kind string) *model.Histogram {
	for _, m := range server.selfMetricSet().Metrics {
		if m.Name == probeDurationName && m.Labels["collector"] == collector && m.Labels["kind"] == kind {
			return m.Histogram
		}
	}
	return nil
}

// Without verbose self-metrics, every probe of a collector is observed in
// its kind="probe" series, those answered from the cache too, each as long
// as http_exporter_scrape_duration_seconds says; a collector not probed yet
// has its series at zero, and none has a kind="static" series while no
// static target names it. The family is declared once, labelled by
// collector and kind alone, and is in the OTLP self-metric set.
func TestTheProbeDurationHistogramIsAlwaysPublished(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	cached := testutil.Collector("cached", "text")
	cached.Cache.TTL = model.Duration(time.Minute)
	server := verboseServer(t, false, cached, testutil.Collector("idle", "text"))
	for range 3 {
		probeOnce(t, server, "/probe?collector=cached&target="+url.QueryEscape(target.URL), nil)
	}
	exposition := selfMetrics(t, server)
	for _, want := range []string{
		"# TYPE http_exporter_probe_duration_seconds histogram\n",
		`http_exporter_probe_duration_seconds_bucket{collector="cached",kind="probe",le="60"} 3` + "\n",
		`http_exporter_probe_duration_seconds_count{collector="cached",kind="probe"} 3` + "\n",
		`http_exporter_probe_duration_seconds_count{collector="idle",kind="probe"} 0` + "\n",
	} {
		if !strings.Contains(exposition, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(exposition, "# TYPE http_exporter_probe_duration_seconds "); n != 1 {
		t.Errorf("the family is declared %d times", n)
	}
	if strings.Contains(exposition, `kind="static"`) {
		t.Error("a collector no static target names has a static series")
	}
	if strings.Contains(exposition, "http_exporter_collector_scrape_duration_seconds") {
		t.Error("the trip histogram is published without verbose self-metrics")
	}
	h := probeHistogram(server, "cached", "probe")
	if h == nil || h.Count != 3 {
		t.Fatalf("the OTLP self-metric set has %+v", h)
	}
	last := server.statsFor("cached").snapshot().lastDuration
	if last <= 0 || h.Sum < last {
		t.Errorf("the histogram's sum %v is under the last probe's duration %v", h.Sum, last)
	}
	for _, m := range server.selfMetricSet().Metrics {
		if m.Name == probeDurationName && len(m.Labels) != 2 {
			t.Errorf("a series has the labels %v, want collector and kind", m.Labels)
		}
	}
	// A debug probe is no probe of the collector's.
	server.SetDebug(DebugFull)
	probeOnce(t, server, "/probe?collector=idle&debug=true&target="+url.QueryEscape(target.URL), nil)
	if h := probeHistogram(server, "idle", "probe"); h == nil || h.Count != 0 {
		t.Errorf("a debug probe was observed: %+v", h)
	}
}

// A static target scrape is observed in its collector's kind="static"
// series, not in kind="probe", and a collector has that series only while a
// static target names it.
func TestStaticScrapesAreObservedApartFromProbes(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text"), testutil.Collector("other", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 0)
	if h := probeHistogram(server, "text", "static"); h == nil || h.Count != 1 {
		t.Fatalf("the static series is %+v, want one scrape", h)
	}
	if h := probeHistogram(server, "text", "probe"); h == nil || h.Count != 0 {
		t.Fatalf("the probe series is %+v, want none observed", h)
	}
	if h := probeHistogram(server, "other", "static"); h != nil {
		t.Fatalf("a collector no static target names has a static series: %+v", h)
	}
}

// The probe histogram has native buckets in the protobuf answer, schema 3
// at its finest, and is an exponential histogram over OTLP with
// otlp.exponential_histograms, as the trip histogram is.
func TestTheProbeDurationHistogramHasNativeAndExponentialBuckets(t *testing.T) {
	testutil.CaptureLogs(t)
	var raw []byte
	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
	}))
	defer endpoint.Close()
	target := textTarget(t, "value=42\n")
	server := histogramServer(t, func(cfg *model.Config) {
		cfg.Web.SelfMetrics.Verbose = false
		cfg.OTLP = otlpConfig(endpoint.URL + "/v1/metrics")
		cfg.OTLP.Compression = model.OTLPCompressionNone
		cfg.OTLP.ExponentialHistograms = true
	})
	probeOnce(t, server, "/probe?collector=app&target="+url.QueryEscape(target.URL), nil)
	families := protoFamilies(t, selfMetricsAccepting(t, server, prometheusProtobufAccept).Body.Bytes())
	family := families[probeDurationName]
	if family == nil || family[3][0].(uint64) != protoHistogram || len(family[4]) != 1 {
		t.Fatalf("the protobuf family is %v", family)
	}
	h := protoFields(t, protoFields(t, family[4][0].([]byte))[7][0].([]byte))
	if h[1][0].(uint64) != 1 || len(h[3]) != len(scrapeDurationBuckets) {
		t.Fatalf("count %v and %d classic buckets", h[1][0], len(h[3]))
	}
	if schema := protoSint(h[5][0]); schema != 3 || len(h[12]) != 1 {
		t.Fatalf("schema %d with %d spans, want 3 with one", schema, len(h[12]))
	}

	server.exportOTLP(context.Background(), time.Minute)
	var payload struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []map[string]json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	var found map[string]json.RawMessage
	for _, metric := range payload.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		if string(metric["name"]) == strconv.Quote(probeDurationName) {
			found = metric
		}
	}
	if found == nil || found["exponentialHistogram"] == nil || found["histogram"] != nil {
		t.Fatalf("the OTLP family is %v", found)
	}
}

// A slow probe whose trace was exported is its bucket's exemplar in the
// probe histogram, with the trace's id and its root span's; a fast probe,
// whose trace was dropped, makes none.
func TestAnExportedProbeIsItsProbeDurationBucketsExemplar(t *testing.T) {
	receiver := newLogReceiver(t)
	slow := tracedTarget(t, 60*time.Millisecond, http.StatusOK)
	fast := tracedTarget(t, 0, http.StatusOK)
	server := tracesServer(t, receiver, 40*time.Millisecond, nil, nil)
	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(fast.URL), nil)
	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(slow.URL), nil)
	exportTracesNow(server)
	var root *receivedSpan
	spans := receiver.spans(t)
	for i := range spans {
		if spans[i].Name == "probe text" {
			root = &spans[i]
		}
	}
	if root == nil {
		t.Fatalf("no probe span among %+v", spans)
	}
	om := selfMetricsAccepting(t, server, "application/openmetrics-text;version=1.0.0").Body.String()
	exemplar := regexp.MustCompile(`(?m)^http_exporter_probe_duration_seconds_bucket\{collector="text",kind="probe",le="([^"]+)"\} \d+ # \{trace_id="([0-9a-f]{32})",span_id="([0-9a-f]{16})"\} [0-9.e-]+ [0-9.]+$`)
	found := exemplar.FindAllStringSubmatch(om, -1)
	if len(found) != 1 || found[0][2] != root.TraceID || found[0][3] != root.SpanID {
		t.Fatalf("the exemplars are %q, want one of trace %s span %s:\n%s", found, root.TraceID, root.SpanID, om)
	}
	if le, _ := strconv.ParseFloat(found[0][1], 64); le < 0.05 {
		t.Errorf("the exemplar is on the bucket le=%s, under the slow probe's time", found[0][1])
	}
}
