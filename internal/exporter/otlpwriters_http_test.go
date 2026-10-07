//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// One writer for each stream of an OTLP export (otlpMetricsOf), from the
// targets' answers to the endpoint: what two probes of a collector, a probe
// and a static target, and a probe of the exporter itself write under one
// resource.

// passedOn is a collector that passes a target's exposition on, every
// series with the type the target gave it.
func passedOn() model.Collector {
	c := testutil.Collector("passthrough", "prometheus")
	c.Transform = model.TransformConfig{Type: "prometheus"}
	c.Metrics = nil
	c.Limits.MaxResponseBytes = 1 << 20
	return c
}

// lastExport is the body of the export the server makes now.
func lastExport(t *testing.T, server *Server, endpoint *otlpEndpoint) []byte {
	t.Helper()
	server.exportOTLP(context.Background(), time.Minute)
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if len(endpoint.bodies) == 0 {
		t.Fatal("no export reached the endpoint")
	}
	return endpoint.bodies[len(endpoint.bodies)-1]
}

// writersLines are the points of an export's body whose names are among
// the names, a line each without its resource: the kind, the series and
// its value.
func writersLines(t *testing.T, body []byte, names ...string) []string {
	t.Helper()
	var lines []string
	for _, p := range otlpStreams(t, body) {
		for _, name := range names {
			if p.name == name {
				lines = append(lines, p.kind+" "+p.name+p.attributes+" "+p.value)
			}
		}
	}
	return lines
}

// Two probes of one collector, of two targets, are two writers under the
// exporter's resource. One target's histogram has a +Inf bucket and a
// _count that differ, so it is exported as gauges under the names of its
// samples; the other has a gauge h_bucket with the le of a bucket, a
// counter of the name the first has a gauge of, and a gauge where the first
// has an untyped series. The export after both probes has each stream once,
// as the target probed last wrote it, and the name of two kinds as the kind
// it wrote, which is logged once; after the first target is probed again,
// as that one wrote them, and the log has a repeat. The export held the
// bucket twice, the name as a gauge and a sum, and the untyped series and
// the gauge as two points of one.
func TestTwoProbesOfOneStreamAreExportedAsTheLater(t *testing.T) {
	one := textTarget(t, "# TYPE h histogram\nh_bucket{le=\"1\"} 3\nh_bucket{le=\"+Inf\"} 4\nh_sum 1.5\nh_count 5\n# TYPE m gauge\nm 1\nu 1\n")
	two := textTarget(t, "# TYPE h_bucket gauge\nh_bucket{le=\"1\"} 9\n# TYPE m counter\nm 2\n# TYPE u gauge\nu 2\n")
	endpoint := newOTLPEndpoint(t)
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{passedOn()}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}, nil)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	probe := func(target *httptest.Server) {
		t.Helper()
		if answer := probeOnce(t, server, "/probe?collector=passthrough&target="+url.QueryEscape(target.URL), nil); answer.Code != http.StatusOK {
			t.Fatalf("the probe is answered %d: %s", answer.Code, answer.Body)
		}
	}
	names := []string{"h_bucket", "h_sum", "h_count", "m", "u"}
	probe(one)
	probe(two)
	sent := lastExport(t, server, endpoint)
	wantLines(t, "the export after the two probes", writersLines(t, sent, names...),
		`gauge h_bucket{le="+Inf"} 4`,
		`gauge h_bucket{le="1"} 9`,
		`gauge h_sum{} 1.5`,
		`gauge h_count{} 5`,
		`sum m{} 2`,
		`gauge u{} 2`)
	oneWriterEach(t, "the export after the two probes", sent, true)
	logged := nameClashLines(t, logs)
	if len(logged) != 1 || logged[0]["level"] != "WARN" || logged[0]["metric"] != "m" || logged[0]["kind"] != "sum" || logged[0]["left_out_kind"] != "gauge" || logged[0]["service_name"] != "prometheus-universal-exporter" {
		t.Errorf("the export after the two probes logged, of names exported as one kind of two:\n%s", logs)
	}
	logs.Reset()
	probe(two)
	probe(one)
	sent = lastExport(t, server, endpoint)
	wantLines(t, "the export after the first target is probed last", writersLines(t, sent, names...),
		`gauge h_bucket{le="1"} 3`,
		`gauge h_bucket{le="+Inf"} 4`,
		`gauge h_sum{} 1.5`,
		`gauge h_count{} 5`,
		`gauge m{} 1`,
		`gauge u{} 1`)
	oneWriterEach(t, "the export after the first target is probed last", sent, true)
	if logged = nameClashLines(t, logs); len(logged) != 0 {
		t.Errorf("the name is logged again at info level while its two kinds go on:\n%s", logs)
	}
}

// One probe's answer has every stream once: a target that writes a series
// twice, a name with two types, a gauge named as a sample of its histogram
// or a bucket twice is no answer, and nothing of it is queued for an
// export. Only two answers under one resource make two writers.
func TestOneProbesAnswerHasNoStreamTwice(t *testing.T) {
	for name, body := range map[string]string{
		"a series twice":                                       "# TYPE m gauge\nm 1\nm 2\n",
		"a name of two types":                                  "# TYPE m gauge\nm{a=\"1\"} 1\n# TYPE m counter\nm{a=\"2\"} 2\n",
		"a gauge named as its histogram's count":               "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 4\nh_sum 1\nh_count 4\n# TYPE h_count gauge\nh_count{a=\"1\"} 4\n",
		"a bucket twice":                                       "# TYPE h histogram\nh_bucket{le=\"1\"} 3\nh_bucket{le=\"1.0\"} 3\nh_bucket{le=\"+Inf\"} 4\nh_sum 1\nh_count 4\n",
		"a gauge with the le of a bucket beside its histogram": "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 4\nh_sum 1\nh_count 5\n# TYPE h_bucket gauge\nh_bucket{le=\"1\"} 4\n",
	} {
		target := textTarget(t, body)
		server := newStaticServer(t, &model.Config{Collectors: []model.Collector{passedOn()}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}, nil)
		server.logger = testutil.QuietLogger(t)
		answer := probeOnce(t, server, "/probe?collector=passthrough&target="+url.QueryEscape(target.URL), nil)
		if points := pendingPoints(server); answer.Code == http.StatusOK || len(points) != 0 {
			t.Errorf("%s: the probe is answered %d and %d points wait for an export:\n%s", name, answer.Code, len(points), answer.Body)
		}
	}
}

// A static target without a resource of its own is exported under the
// exporter's, where a probe writes too: its gauge and a probe's counter of
// that name are one name there, exported as the kind written last and
// logged, while a static target with a resource of its own keeps its gauge
// beside them.
func TestAStaticTargetAndAProbeUnderOneResourceAreTwoWriters(t *testing.T) {
	gauge := textTarget(t, "# TYPE m gauge\nm 1\n")
	counter := textTarget(t, "# TYPE m counter\nm 2\n")
	endpoint := newOTLPEndpoint(t)
	cfg := &model.Config{Collectors: []model.Collector{passedOn()}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{ExportViaOTLP: true, Name: "shared", Collector: "passthrough", Target: gauge.URL},
		{ExportViaOTLP: true, Name: "apart", Collector: "passthrough", Target: gauge.URL, OTLP: model.TargetOTLPConfig{ServiceName: "legacy"}},
	}}
	server := newStaticServer(t, cfg, file)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	server.scrapeStaticTargets(context.Background(), 0)
	if answer := probeOnce(t, server, "/probe?collector=passthrough&target="+url.QueryEscape(counter.URL), nil); answer.Code != http.StatusOK {
		t.Fatalf("the probe is answered %d: %s", answer.Code, answer.Body)
	}
	sent := lastExport(t, server, endpoint)
	var points []string
	for _, p := range otlpStreams(t, sent) {
		if p.name == "m" {
			resource, _, _ := strings.Cut(p.resource, " ")
			points = append(points, resource+" "+p.kind+" "+p.name+p.attributes+" "+p.value)
		}
	}
	wantLines(t, "the points of the name in the export", points,
		`[service.name="legacy" gauge m{static_target="apart"} 1`,
		`[service.name="prometheus-universal-exporter" sum m{} 2`)
	oneWriterEach(t, "the export", sent, true)
	if logged := nameClashLines(t, logs); len(logged) != 1 || logged[0]["metric"] != "m" || logged[0]["kind"] != "sum" || logged[0]["left_out_points"] != float64(1) {
		t.Errorf("the export logged, of names exported as one kind of two:\n%s", logs)
	}
}

// A collector that scrapes the exporter's own metrics answers with series
// that are the exporter's own streams under its resource. The export after
// such a probe has each of them once, as the exporter has it at the export:
// it held every one of them twice, the probe's beside the exporter's own.
func TestAProbeOfTheExporterItselfIsNotExportedBesideItsOwnMetrics(t *testing.T) {
	endpoint := newOTLPEndpoint(t)
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{passedOn()}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}, nil)
	server.logger = testutil.QuietLogger(t)
	itself := httptest.NewServer(server.Handler())
	t.Cleanup(itself.Close)
	if answer := probeOnce(t, server, "/probe?collector=passthrough&target="+url.QueryEscape(itself.URL+server.selfMetricsEndpoint()), nil); answer.Code != http.StatusOK {
		t.Fatalf("the probe is answered %d: %s", answer.Code, answer.Body)
	}
	own := map[string]bool{}
	for _, m := range server.selfMetricSet().Metrics {
		own[m.Name] = true
	}
	queued := 0
	for _, m := range pendingPoints(server) {
		if own[m.Name] {
			queued++
		}
	}
	sent := lastExport(t, server, endpoint)
	oneWriterEach(t, "the export", sent, true)
	exported := 0
	for _, p := range otlpStreams(t, sent) {
		if own[p.name] {
			exported++
		}
	}
	// The probe itself made series of the collector that the answer it
	// read did not have yet: those are in the export once as well.
	if want := len(server.selfMetricSet().Metrics); queued < 20 || exported != want {
		t.Errorf("the probe queued %d series of the exporter's own, and the export has %d points of its %d own series", queued, exported, want)
	}
}

// The exporter's own metrics, in every mode, beside what probes and a
// static target of ordinary names queued under its resource, are no two
// writers of a stream: each family's series follow one another, so the
// export is made once (otlpPoints), with nothing gone through again, at
// every interval of an exporter whose metrics are named apart.
func TestAnExportOfTheExportersOwnMetricsAndProbesIsMadeOnce(t *testing.T) {
	one, two := textTarget(t, "value=1\n"), textTarget(t, "# TYPE jobs_total counter\njobs_total{queue=\"a\"} 4\njobs_total{queue=\"b\"} 5\n# TYPE depth gauge\ndepth 2\n")
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text"), passedOn()},
		OTLP:       otlpConfig("http://collector.invalid/v1/metrics"),
		Web:        model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true, ResourceMetrics: true}},
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "scheduled", Collector: "text", Target: one.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	for _, probe := range []string{"/probe?collector=text&target=" + url.QueryEscape(one.URL), "/probe?collector=passthrough&target=" + url.QueryEscape(two.URL), "/probe?collector=text&target=" + url.QueryEscape(two.URL)} {
		probeOnce(t, server, probe, nil)
	}
	server.scrapeStaticTargets(context.Background(), 0)
	resources := appendToResource(server.drainOTLP(), defaultResourceIdentity(cfg.OTLP), server.selfMetricSet())
	if len(resources) != 1 || len(resources[0].seqs) < 6 || len(resources[0].Set.Metrics) < 100 {
		t.Fatalf("the export is of %d resources", len(resources))
	}
	out, shared := otlpPoints(resources[0].Set, len(resources[0].seqs), "1", nil, nil)
	if shared || len(out) < 50 {
		t.Errorf("the export of %d metrics is taken to have two writers of a stream: %v", len(out), shared)
	}
}
