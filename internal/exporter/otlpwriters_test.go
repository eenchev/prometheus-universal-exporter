package exporter

// One writer for each stream of an OTLP export (otlpMetricsOf): what two
// probes, a probe and a static target, or a probe and the exporter itself
// write under one resource that is one point, or one name, to a receiver.
//
// The tests here keep the conversion as it was (otlpMetricsAsItWas), which
// made a point of every series and a metric of every kind a name had, as
// an oracle: an export is held to it, byte for byte, wherever it had no two
// points of one stream and no name of two kinds
// (TestOTLPExportsAreWhatTheyWereWhereNoTwoSubjectsHadOneKey), and
// elsewhere to what it made without the points written earlier
// (TestNoOTLPExportHasTwoPointsOfOneStream).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// otlpStream is a point of an export's body as a receiver tells it from
// every other: its resource, its metric's kind and name, and its
// attributes; with its value or count, the nanosecond it is of and, for a
// cumulative point, the nanosecond it started at.
type otlpStream struct {
	resource, kind, name, attributes string
	value, at, since                 string
}

// String is the point as a test names it: the resource, the kind, the
// series, its value, and the millisecond it started at when it has one.
func (p otlpStream) String() string {
	line := p.resource + p.kind + " " + p.name + p.attributes + " " + p.value
	if p.since != "" {
		line += " since " + strings.TrimSuffix(p.since, "000000")
	}
	return line
}

// otlpStreams are the points of an export's body, in its order.
func otlpStreams(t *testing.T, body []byte) []otlpStream {
	t.Helper()
	var payload otlpPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("%v:\n%s", err, body)
	}
	attributes := func(open, between, shut string, attributes []otlpAttribute) string {
		parts := make([]string, 0, len(attributes))
		for _, a := range attributes {
			parts = append(parts, a.Key+"="+strconv.Quote(a.Value.StringValue))
		}
		return open + strings.Join(parts, between) + shut
	}
	number := func(v *otlpDouble) string { return strconv.FormatFloat(float64(*v), 'g', -1, 64) }
	var out []otlpStream
	for _, resource := range payload.ResourceMetrics {
		of := attributes("[", " ", "] ", resource.Resource.Attributes)
		for _, scope := range resource.ScopeMetrics {
			for _, m := range scope.Metrics {
				point := func(kind string, labels []otlpAttribute, value, at, since string) {
					out = append(out, otlpStream{resource: of, kind: kind, name: m.Name, attributes: attributes("{", ",", "}", labels), value: value, at: at, since: since})
				}
				switch {
				case m.Gauge != nil:
					for _, p := range m.Gauge.DataPoints {
						point("gauge", p.Attributes, number(p.AsDouble), p.TimeUnixNano, p.StartTimeUnixNano)
					}
				case m.Sum != nil:
					for _, p := range m.Sum.DataPoints {
						point("sum", p.Attributes, number(p.AsDouble), p.TimeUnixNano, p.StartTimeUnixNano)
					}
				case m.Histogram != nil:
					for _, p := range m.Histogram.DataPoints {
						point("histogram", p.Attributes, p.Count, p.TimeUnixNano, p.StartTimeUnixNano)
					}
				case m.Summary != nil:
					for _, p := range m.Summary.DataPoints {
						point("summary", p.Attributes, p.Count, p.TimeUnixNano, p.StartTimeUnixNano)
					}
				}
			}
		}
	}
	return out
}

// streamLines are the points of an export's body, a line each (String).
func streamLines(t *testing.T, body []byte) []string {
	t.Helper()
	var lines []string
	for _, p := range otlpStreams(t, body) {
		lines = append(lines, p.String())
	}
	return lines
}

// oneWriterEach fails unless every stream of the export's body is there
// once: no resource is two of its resources, no name two metrics of a
// resource, and no two points of a metric have the same attributes. It
// fails as well for a gauge's point with a start time and a cumulative
// point without one, when starts says that the export gives them.
func oneWriterEach(t *testing.T, what string, body []byte, starts bool) {
	t.Helper()
	resources, names, points := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var resource, metric string
	for i, p := range otlpStreams(t, body) {
		if i == 0 || p.resource != resource {
			if resources[p.resource] {
				t.Errorf("%s: the export has the resource %s twice", what, p.resource)
			}
			resources[p.resource], resource, metric = true, p.resource, ""
		}
		// The points of a metric follow one another, so a name that
		// comes back is a second metric of it.
		if of := p.kind + " " + p.name; of != metric {
			if names[p.resource+p.name] {
				t.Errorf("%s: the export has two metrics %s under %s", what, p.name, p.resource)
			}
			names[p.resource+p.name], metric = true, of
		}
		if points[p.resource+p.name+p.attributes] {
			t.Errorf("%s: the export has two points of %s%s%s", what, p.resource, p.name, p.attributes)
		}
		points[p.resource+p.name+p.attributes] = true
		if starts && (p.kind == "gauge") != (p.since == "") {
			t.Errorf("%s: the %s point %s%s%s started at %q", what, p.kind, p.resource, p.name, p.attributes, p.since)
		}
	}
}

// nameClashLines are the lines among logs that say a name was exported as
// one kind of two.
func nameClashLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		if line["msg"] == "OTLP metric name written as two kinds under one resource; the data points of the kind written earlier are left out of the export" {
			lines = append(lines, line)
		}
	}
	return lines
}

// loggedTo makes the server log, as JSON and from the level on, to the
// buffer it returns.
func (k *otlpKeyServer) loggedTo(level slog.Level) *bytes.Buffer {
	var logs bytes.Buffer
	k.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: level}))
	return &logs
}

// The series the tests queue: a histogram whose +Inf bucket counts inf
// observations and whose _count is count, which is one OTLP has when the
// two are the same, and a summary of one quantile, which is one OTLP has
// when the quantile is within 0 to 1.
func histogramOf(name string, labels map[string]string, inf, count uint64) model.Metric {
	return model.Metric{Name: name, Type: model.HistogramMetricType, Labels: labels, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 3}, {UpperBound: math.Inf(1), CumulativeCount: inf}}, Sum: 1.5, Count: count}}
}

func summaryOf(name string, labels map[string]string, quantile float64, count uint64) model.Metric {
	return model.Metric{Name: name, Type: model.SummaryMetricType, Labels: labels, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: quantile, Value: 7}}, Sum: 2.5, Count: count}}
}

func valueOf(name string, typ model.MetricType, labels map[string]string, value float64) model.Metric {
	return model.Metric{Name: name, Type: typ, Labels: labels, Value: value}
}

// scrapedAt is the series as a scrape at the millisecond at finds them: a
// set a probe may answer with (MetricSet.Validate), which the test fails
// for when it is none.
func scrapedAt(t *testing.T, at int64, series ...model.Metric) model.MetricSet {
	t.Helper()
	set := model.MetricSet{Metrics: slices.Clone(series)}
	for i := range set.Metrics {
		set.Metrics[i].Timestamp = &at
	}
	if err := set.Validate(model.Limits{}); err != nil {
		t.Fatalf("the series are no set of one scrape: %v", err)
	}
	return set
}

// What two writers of one resource make of one stream, case by case: the
// sets are queued in turn, a second apart, as the scrapes of two probes of
// a collector, of two collectors or of a probe and a static target are, and
// exported at once. was is the export as it was made while every series
// queued was a point of it and a name a metric for every kind it had; now
// is the export: of the points of a metric with the same attributes the
// one written last, and of the kinds of a name the one written last, which
// is logged. Where the two are the same the export is what it was.
func TestTwoWritersOfOneStreamAreExportedAsTheLater(t *testing.T) {
	const svc = `[service.name="svc"] `
	gauge, untyped, counter := model.GaugeMetricType, model.UntypedMetricType, model.CounterMetricType
	le := func(bound string) map[string]string { return map[string]string{"le": bound} }
	a := func(value string) map[string]string { return map[string]string{"a": value} }
	for _, c := range []struct {
		name     string
		writes   [][]model.Metric
		was, now []string
		logged   int
	}{
		{
			name: "a histogram exported as gauges, then a gauge named as its buckets with the le of one",
			writes: [][]model.Metric{
				{histogramOf("h", nil, 4, 5)},
				{valueOf("h_bucket", gauge, le("1"), 9)},
			},
			was: []string{`gauge h_bucket{le="1"} 3`, `gauge h_bucket{le="+Inf"} 4`, `gauge h_bucket{le="1"} 9`, `gauge h_sum{} 1.5`, `gauge h_count{} 5`},
			now: []string{`gauge h_bucket{le="+Inf"} 4`, `gauge h_bucket{le="1"} 9`, `gauge h_sum{} 1.5`, `gauge h_count{} 5`},
		},
		{
			name: "the gauge, then the histogram exported as gauges",
			writes: [][]model.Metric{
				{valueOf("h_bucket", gauge, le("1"), 9)},
				{histogramOf("h", nil, 4, 5)},
			},
			was: []string{`gauge h_bucket{le="1"} 3`, `gauge h_bucket{le="+Inf"} 4`, `gauge h_bucket{le="1"} 9`, `gauge h_sum{} 1.5`, `gauge h_count{} 5`},
			now: []string{`gauge h_bucket{le="1"} 3`, `gauge h_bucket{le="+Inf"} 4`, `gauge h_sum{} 1.5`, `gauge h_count{} 5`},
		},
		{
			name: "a histogram exported as gauges, then a counter named as its sum",
			writes: [][]model.Metric{
				{histogramOf("h", nil, 4, 5)},
				{valueOf("h_sum", counter, nil, 8)},
			},
			was:    []string{`gauge h_bucket{le="1"} 3`, `gauge h_bucket{le="+Inf"} 4`, `gauge h_sum{} 1.5`, `gauge h_count{} 5`, `sum h_sum{} 8 since 2000`},
			now:    []string{`gauge h_bucket{le="1"} 3`, `gauge h_bucket{le="+Inf"} 4`, `gauge h_count{} 5`, `sum h_sum{} 8 since 2000`},
			logged: 1,
		},
		{
			name: "a summary exported as gauges, then a gauge of its name with the quantile",
			writes: [][]model.Metric{
				{summaryOf("s", nil, 1.5, 4)},
				{valueOf("s", gauge, map[string]string{"quantile": "1.5"}, 9), valueOf("s_count", untyped, nil, 6)},
			},
			was: []string{`gauge s{quantile="1.5"} 9`, `gauge s{quantile="1.5"} 7`, `gauge s_sum{} 2.5`, `gauge s_count{} 4`, `gauge s_count{} 6`},
			now: []string{`gauge s{quantile="1.5"} 9`, `gauge s_sum{} 2.5`, `gauge s_count{} 6`},
		},
		{
			name:   "a gauge, then a counter of its name and labels",
			writes: [][]model.Metric{{valueOf("m", gauge, nil, 1)}, {valueOf("m", counter, nil, 2)}},
			was:    []string{`sum m{} 2 since 2000`, `gauge m{} 1`},
			now:    []string{`sum m{} 2 since 2000`},
			logged: 1,
		},
		{
			name:   "the counter, then the gauge",
			writes: [][]model.Metric{{valueOf("m", counter, nil, 2)}, {valueOf("m", gauge, nil, 1)}},
			was:    []string{`sum m{} 2 since 1000`, `gauge m{} 1`},
			now:    []string{`gauge m{} 1`},
			logged: 1,
		},
		{
			name:   "a gauge, then a counter of its name and other labels",
			writes: [][]model.Metric{{valueOf("m", gauge, a("1"), 1), valueOf("m", gauge, a("3"), 3)}, {valueOf("m", counter, a("2"), 2)}},
			was:    []string{`sum m{a="2"} 2 since 2000`, `gauge m{a="1"} 1`, `gauge m{a="3"} 3`},
			now:    []string{`sum m{a="2"} 2 since 2000`},
			logged: 1,
		},
		{
			name:   "a gauge, a counter and then a histogram of one name",
			writes: [][]model.Metric{{valueOf("m", gauge, nil, 1)}, {valueOf("m", counter, a("2"), 2)}, {histogramOf("m", nil, 4, 4)}},
			was:    []string{`sum m{a="2"} 2 since 2000`, `gauge m{} 1`, `histogram m{} 4 since 3000`},
			now:    []string{`histogram m{} 4 since 3000`},
			logged: 1,
		},
		{
			name:   "a histogram, then a gauge of its name",
			writes: [][]model.Metric{{histogramOf("h", nil, 4, 4)}, {valueOf("h", gauge, nil, 1)}},
			was:    []string{`gauge h{} 1`, `histogram h{} 4 since 1000`},
			now:    []string{`gauge h{} 1`},
			logged: 1,
		},
		{
			name:   "a gauge, then an untyped series of its name and labels",
			writes: [][]model.Metric{{valueOf("m", gauge, nil, 1)}, {valueOf("m", untyped, nil, 2)}},
			was:    []string{`gauge m{} 1`, `gauge m{} 2`},
			now:    []string{`gauge m{} 2`},
		},
		{
			name:   "the untyped series, then the gauge",
			writes: [][]model.Metric{{valueOf("m", untyped, nil, 2)}, {valueOf("m", gauge, nil, 1)}},
			was:    []string{`gauge m{} 1`, `gauge m{} 2`},
			now:    []string{`gauge m{} 1`},
		},
		{
			name:   "a counter that is NaN, exported as a gauge, then a gauge of its name and labels",
			writes: [][]model.Metric{{valueOf("c", counter, nil, math.NaN())}, {valueOf("c", gauge, nil, 1)}},
			was:    []string{`gauge c{} NaN`, `gauge c{} 1`},
			now:    []string{`gauge c{} 1`},
		},
		{
			name:   "a counter that is NaN, a gauge of its name and labels, and a counter of the family: gauges all, for the point left out too",
			writes: [][]model.Metric{{valueOf("c", counter, a("1"), math.NaN())}, {valueOf("c", gauge, a("1"), 1)}, {valueOf("c", counter, a("2"), 5)}},
			was:    []string{`gauge c{a="1"} NaN`, `gauge c{a="2"} 5`, `gauge c{a="1"} 1`},
			now:    []string{`gauge c{a="2"} 5`, `gauge c{a="1"} 1`},
		},
		// What was one writer's already, and is as it was.
		{
			name:   "a gauge, then an untyped series of its name and other labels",
			writes: [][]model.Metric{{valueOf("m", gauge, a("2"), 1)}, {valueOf("m", untyped, a("1"), 2)}},
			was:    []string{`gauge m{a="2"} 1`, `gauge m{a="1"} 2`},
			now:    []string{`gauge m{a="2"} 1`, `gauge m{a="1"} 2`},
		},
		{
			name:   "a gauge, then a gauge of its name and labels: one series, where the later waited in place of the earlier",
			writes: [][]model.Metric{{valueOf("m", gauge, nil, 1)}, {valueOf("m", gauge, nil, 2)}},
			was:    []string{`gauge m{} 2`},
			now:    []string{`gauge m{} 2`},
		},
		{
			name:   "a histogram, then a gauge named as its count: the histogram is exported under its own name alone",
			writes: [][]model.Metric{{histogramOf("h", nil, 4, 4)}, {valueOf("h_count", gauge, nil, 9), valueOf("h_bucket", gauge, le("1"), 8)}},
			was:    []string{`histogram h{} 4 since 1000`, `gauge h_bucket{le="1"} 8`, `gauge h_count{} 9`},
			now:    []string{`histogram h{} 4 since 1000`, `gauge h_bucket{le="1"} 8`, `gauge h_count{} 9`},
		},
		{
			name:   "a summary, then a gauge named as its sum",
			writes: [][]model.Metric{{summaryOf("s", nil, 0.5, 4)}, {valueOf("s_sum", gauge, nil, 9)}},
			was:    []string{`summary s{} 4 since 1000`, `gauge s_sum{} 9`},
			now:    []string{`summary s{} 4 since 1000`, `gauge s_sum{} 9`},
		},
		{
			name:   "a counter x_total, then a counter x: no name is changed on the way",
			writes: [][]model.Metric{{valueOf("x_total", counter, nil, 1)}, {valueOf("x", counter, nil, 2)}},
			was:    []string{`sum x{} 2 since 2000`, `sum x_total{} 1 since 1000`},
			now:    []string{`sum x{} 2 since 2000`, `sum x_total{} 1 since 1000`},
		},
		{
			name:   "a counter, then a counter of its family that is NaN: the family of the export is gauges, each series a point",
			writes: [][]model.Metric{{valueOf("c", counter, a("1"), 5)}, {valueOf("c", counter, a("2"), math.NaN())}},
			was:    []string{`gauge c{a="1"} 5`, `gauge c{a="2"} NaN`},
			now:    []string{`gauge c{a="1"} 5`, `gauge c{a="2"} NaN`},
		},
		{
			name:   "a histogram, then a series of its family exported as gauges: so is the first",
			writes: [][]model.Metric{{histogramOf("h", a("1"), 4, 4)}, {histogramOf("h", a("2"), 4, 5)}},
			was:    []string{`gauge h_bucket{a="1",le="1"} 3`, `gauge h_bucket{a="1",le="+Inf"} 4`, `gauge h_bucket{a="2",le="1"} 3`, `gauge h_bucket{a="2",le="+Inf"} 4`, `gauge h_sum{a="1"} 1.5`, `gauge h_sum{a="2"} 1.5`, `gauge h_count{a="1"} 4`, `gauge h_count{a="2"} 5`},
			now:    []string{`gauge h_bucket{a="1",le="1"} 3`, `gauge h_bucket{a="1",le="+Inf"} 4`, `gauge h_bucket{a="2",le="1"} 3`, `gauge h_bucket{a="2",le="+Inf"} 4`, `gauge h_sum{a="1"} 1.5`, `gauge h_sum{a="2"} 1.5`, `gauge h_count{a="1"} 4`, `gauge h_count{a="2"} 5`},
		},
	} {
		server, old := newOTLPKeyServer(t, "svc", nil), oldOTLPKeys()
		logs := server.loggedTo(slog.LevelInfo)
		identity := defaultResourceIdentity(server.otlp)
		for i, series := range c.writes {
			set := scrapedAt(t, int64(1000*(i+1)), series...)
			server.queueOTLPResource(set, identity, scrapeTime{})
			old.queue(identity, set)
		}
		sent, was := server.push(t, server.drainOTLP()), old.export(t, old.drain())
		under := func(lines []string) []string {
			out := make([]string, 0, len(lines))
			for _, line := range lines {
				out = append(out, svc+line)
			}
			return out
		}
		wantLines(t, c.name+": the export", streamLines(t, sent), under(c.now)...)
		wantLines(t, c.name+": the export as it was", streamLines(t, was), under(c.was)...)
		oneWriterEach(t, c.name, sent, true)
		if slices.Equal(c.was, c.now) && !bytes.Equal(sent, was) {
			t.Errorf("%s: the export is sent as\n%s\nand was\n%s", c.name, sent, was)
		}
		if logged := nameClashLines(t, logs); len(logged) != c.logged {
			t.Errorf("%s: %d names are logged as exported as one kind of two, want %d:\n%s", c.name, len(logged), c.logged, logs)
		}
		// A point left out for a later one of its stream is replaced, as
		// one waiting is, and not a point given up on.
		server.Server.otlp.mu.Lock()
		dropped := server.Server.otlp.dropped
		server.Server.otlp.mu.Unlock()
		if dropped != 0 {
			t.Errorf("%s: %d points are counted as dropped", c.name, dropped)
		}
	}
}

// A name is one writer's under its resource, not under another's: a gauge
// of a static target with a resource of its own and a counter of that name
// under the exporter's are both exported, as they were, and so are a gauge
// and an untyped series of one name and labels under the two; nothing is
// logged.
func TestWritersOfTwoResourcesAreExportedApart(t *testing.T) {
	server, old := newOTLPKeyServer(t, "svc", nil), oldOTLPKeys()
	logs := server.loggedTo(slog.LevelInfo)
	own := defaultResourceIdentity(server.otlp)
	target := targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ServiceName: "legacy"}}, server.otlp)
	first := scrapedAt(t, 1000, valueOf("m", model.GaugeMetricType, nil, 1), valueOf("v", model.GaugeMetricType, nil, 3))
	second := scrapedAt(t, 2000, valueOf("m", model.CounterMetricType, nil, 2), valueOf("v", model.UntypedMetricType, nil, 4))
	server.queueOTLPResource(first, target, scrapeTime{})
	server.queueOTLPResource(second, own, scrapeTime{})
	old.queue(target, first)
	old.queue(own, second)
	sent, was := server.push(t, server.drainOTLP()), old.export(t, old.drain())
	wantLines(t, "the export", streamLines(t, sent),
		`[service.name="legacy"] gauge m{} 1`,
		`[service.name="legacy"] gauge v{} 3`,
		`[service.name="svc"] sum m{} 2 since 2000`,
		`[service.name="svc"] gauge v{} 4`)
	if !bytes.Equal(sent, was) {
		t.Errorf("the export is sent as\n%s\nand was\n%s", sent, was)
	}
	if logged := nameClashLines(t, logs); len(logged) != 0 {
		t.Errorf("names of two resources are logged as exported as one kind of two:\n%s", logs)
	}
}

// With otlp.probe_attributes the points of two probes differ in their
// collector and target attributes, and a metric is still its name's under
// the one resource: a gauge of one collector and a counter of that name of
// another are exported as the kind written last, and two gauges as two
// points of one metric.
func TestANameOfTwoKindsIsOneWritersWithProbeAttributesToo(t *testing.T) {
	server := newOTLPKeyServer(t, "svc", nil)
	server.otlp.ProbeAttributes = true
	server.Server = NewServer(config.NewManager(&model.Config{OTLP: server.otlp}, "", slog.New(slog.DiscardHandler)), "python3", slog.New(slog.DiscardHandler))
	logs := server.loggedTo(slog.LevelInfo)
	server.queueProbeOTLP(scrapedAt(t, 1000, valueOf("m", model.GaugeMetricType, nil, 1), valueOf("v", model.GaugeMetricType, nil, 3)), &model.Collector{Name: "one"}, "http://t", time.Time{})
	server.queueProbeOTLP(scrapedAt(t, 2000, valueOf("m", model.CounterMetricType, nil, 2), valueOf("v", model.GaugeMetricType, nil, 4)), &model.Collector{Name: "two"}, "http://t", time.Time{})
	sent := server.push(t, server.drainOTLP())
	wantLines(t, "the export", streamLines(t, sent),
		`[service.name="svc"] sum m{collector="two",target="http://t"} 2 since 2000`,
		`[service.name="svc"] gauge v{collector="one",target="http://t"} 3`,
		`[service.name="svc"] gauge v{collector="two",target="http://t"} 4`)
	oneWriterEach(t, "the export", sent, true)
	if logged := nameClashLines(t, logs); len(logged) != 1 {
		t.Errorf("%d names are logged as exported as one kind of two, want m:\n%s", len(logged), logs)
	}
}

// One scrape's set has every stream once, whatever made it: a set with a
// series twice, a name of two types, a gauge named as a sample of its
// histogram or of its summary, or a histogram or a summary with a label
// that is its samples' own, is refused before anything of it is queued
// (MetricSet.Validate), and a histogram's bounds and a summary's quantiles
// are each there once (Histogram.Settle, Summary.Settle). So two points of
// one stream, and two kinds of a name, come of two sets under one resource
// alone.
func TestOneScrapesSetHasNoStreamTwice(t *testing.T) {
	gauge, counter := model.GaugeMetricType, model.CounterMetricType
	for name, series := range map[string][]model.Metric{
		"a series twice": {valueOf("m", gauge, nil, 1), valueOf("m", gauge, nil, 2)},
		"a series with a label and with it empty":  {valueOf("m", gauge, map[string]string{"a": ""}, 1), valueOf("m", gauge, nil, 2)},
		"a name of two types":                      {valueOf("m", gauge, map[string]string{"a": "1"}, 1), valueOf("m", counter, map[string]string{"a": "2"}, 2)},
		"a gauge named as its histogram's buckets": {histogramOf("h", nil, 4, 5), valueOf("h_bucket", gauge, map[string]string{"le": "1"}, 9)},
		"a counter named as its histogram's sum":   {histogramOf("h", nil, 4, 4), valueOf("h_sum", counter, nil, 9)},
		"a gauge named as its summary's count":     {summaryOf("s", nil, 1.5, 4), valueOf("s_count", gauge, nil, 9)},
		"a gauge named as its summary":             {summaryOf("s", nil, 1.5, 4), valueOf("s", gauge, map[string]string{"quantile": "1.5"}, 9)},
		"a histogram with the label le":            {histogramOf("h", map[string]string{"le": "1"}, 4, 5), histogramOf("h", nil, 4, 5)},
		"a summary with the label quantile":        {summaryOf("s", map[string]string{"quantile": "1.5"}, 1.5, 4), summaryOf("s", nil, 1.5, 4)},
	} {
		set := model.MetricSet{Metrics: series}
		if err := set.Validate(model.Limits{}); err == nil {
			t.Errorf("%s: the set is one a scrape may answer with", name)
		}
	}
	twice := histogramOf("h", nil, 4, 4)
	twice.Histogram.Buckets = append(twice.Histogram.Buckets, model.Bucket{UpperBound: 1, CumulativeCount: 3})
	if err := twice.Histogram.Settle(); err == nil {
		t.Error("a histogram with a bound twice is one a scrape may answer with")
	}
	again := summaryOf("s", nil, 0.5, 4)
	again.Summary.Quantiles = append(again.Summary.Quantiles, model.Quantile{Quantile: 0.5, Value: 1})
	if err := again.Summary.Settle(); err == nil {
		t.Error("a summary with a quantile twice is one a scrape may answer with")
	}
}

// A series a probe answers with that is named and labelled as one of the
// exporter's own — a collector that scrapes the exporter itself — is the
// exporter's own stream under its resource: the export has the exporter's
// point, written at the export, and not the probe's beside it. A probe's
// gauge of the name of one of the exporter's counters is left out for the
// counter, and logged.
func TestAProbesSeriesOfTheExportersOwnStreamIsExportedAsTheExporters(t *testing.T) {
	server := newOTLPKeyServer(t, "svc", nil)
	logs := server.loggedTo(slog.LevelInfo)
	identity := defaultResourceIdentity(server.otlp)
	server.queueOTLPResource(scrapedAt(t, 1000,
		valueOf("http_exporter_otlp_exports_total", model.CounterMetricType, map[string]string{"result": "success"}, 77),
		valueOf("http_exporter_otlp_exports_total", model.CounterMetricType, map[string]string{"result": "other"}, 78),
		valueOf("http_exporter_otlp_export_duration_seconds", model.UntypedMetricType, nil, 79),
		valueOf("http_exporter_otlp_export_retries_total", model.GaugeMetricType, map[string]string{"a": "1"}, 80),
	), identity, scrapeTime{})
	server.exportOTLP(context.Background(), time.Minute)
	sent := server.sent(t)
	var points []string
	for _, p := range otlpStreams(t, sent) {
		if strings.HasPrefix(p.name, "http_exporter_otlp_export") {
			points = append(points, p.kind+" "+p.name+p.attributes+" "+p.value)
		}
	}
	wantLines(t, "the export's points of the names the probe wrote", points,
		`sum http_exporter_otlp_exports_total{result="other"} 78`,
		`sum http_exporter_otlp_exports_total{result="success"} 0`,
		`sum http_exporter_otlp_exports_total{result="failure"} 0`,
		`sum http_exporter_otlp_export_retries_total{} 0`,
		`gauge http_exporter_otlp_export_duration_seconds{} 0`)
	oneWriterEach(t, "the export", sent, true)
	logged := nameClashLines(t, logs)
	if len(logged) != 1 || logged[0]["metric"] != "http_exporter_otlp_export_retries_total" || logged[0]["kind"] != "sum" || logged[0]["left_out_kind"] != "gauge" {
		t.Errorf("the names logged as exported as one kind of two, want the retries as the exporter's sum:\n%s", logs)
	}
}

// A point that an export could not deliver waits again beside the point of
// its stream another writer queued meanwhile — a gauge, and then an untyped
// series of its name and labels, which are two series to the queue — and
// the next export has the stream once, as the later wrote it; so it has a
// name whose other kind was queued meanwhile. The point that waited again
// is the earlier however many exports failed.
func TestAPointThatWaitsAgainIsExportedUnlessItsStreamWasWrittenSince(t *testing.T) {
	server := newOTLPKeyServer(t, "svc", nil)
	identity := defaultResourceIdentity(server.otlp)
	server.queueOTLPResource(scrapedAt(t, 1000, valueOf("m", model.GaugeMetricType, nil, 1), valueOf("n", model.CounterMetricType, nil, 3), valueOf("o", model.GaugeMetricType, nil, 5)), identity, scrapeTime{})
	for range 2 {
		server.mu.Lock()
		server.unavailable = true
		server.mu.Unlock()
		server.exportOTLP(context.Background(), time.Minute)
		server.mu.Lock()
		server.unavailable = false
		server.mu.Unlock()
	}
	server.queueOTLPResource(scrapedAt(t, 2000, valueOf("m", model.UntypedMetricType, nil, 2), valueOf("n", model.GaugeMetricType, nil, 4)), identity, scrapeTime{})
	if waiting := server.otlpPoints; waiting != 5 {
		t.Fatalf("%d points wait for the export, want the three that waited again and the two queued since", waiting)
	}
	sent := server.push(t, server.drainOTLP())
	wantLines(t, "the export", streamLines(t, sent),
		`[service.name="svc"] gauge m{} 2`,
		`[service.name="svc"] gauge n{} 4`,
		`[service.name="svc"] gauge o{} 5`)
	oneWriterEach(t, "the export", sent, true)
}

// The start times follow the streams. A counter whose name another writer
// exports as a gauge is left out of the exports the gauge was written last
// for, and is still one series to the start times, which have seen the
// count it had in each: when it is exported again it has started where it
// was first seen, and where its count fell, in an export that left it out
// too. Two writers of one counter are one series: the count of the one
// after the higher count of the other is a reset, and the higher count
// after it is none.
func TestStartTimesFollowTheStreamsOfAnExport(t *testing.T) {
	server := newOTLPKeyServer(t, "svc", nil)
	identity := defaultResourceIdentity(server.otlp)
	export := func(at int64, series ...[]model.Metric) []string {
		for i, of := range series {
			server.queueOTLPResource(scrapedAt(t, at+int64(100*i), of...), identity, scrapeTime{})
		}
		sent := server.push(t, server.drainOTLP())
		oneWriterEach(t, "the export", sent, true)
		return streamLines(t, sent)
	}
	const svc = `[service.name="svc"] `
	counter := func(value float64) []model.Metric {
		return []model.Metric{valueOf("jobs", model.CounterMetricType, nil, value)}
	}
	gauge := []model.Metric{valueOf("jobs", model.GaugeMetricType, map[string]string{"a": "1"}, 1)}
	wantLines(t, "the counter, left out for the gauge written after it", export(1000, counter(10), gauge), svc+`gauge jobs{a="1"} 1`)
	wantLines(t, "the counter alone", export(2000, counter(12)), svc+`sum jobs{} 12 since 1000`)
	wantLines(t, "the counter after a reset, left out", export(3000, counter(5), gauge), svc+`gauge jobs{a="1"} 1`)
	wantLines(t, "the counter, above the count it had before the reset", export(4000, counter(20)), svc+`sum jobs{} 20 since 3000`)
	wantLines(t, "the counter, written after the gauge", export(5000, gauge, counter(21)), svc+`sum jobs{} 21 since 3000`)
	// Two writers of the counter, each with its own count.
	wantLines(t, "another writer's lower count", export(6000, counter(2)), svc+`sum jobs{} 2 since 6000`)
	wantLines(t, "the first writer's count again", export(7000, counter(22)), svc+`sum jobs{} 22 since 6000`)
}

// A name exported as one kind of two is logged once, as a warning that
// names the metric, the kind exported, the kinds and the points left out
// and the resource, and says what to change; while it goes on, whichever
// kind is written last, it is a repeat, at debug level, and logged again
// after five minutes with how often it happened. Another name, and the name
// under another resource, is logged for itself. Points of one stream, of
// which the later is exported, are not logged.
func TestANameOfTwoKindsIsLoggedOnceAndThenAsARepeat(t *testing.T) {
	server := newOTLPKeyServer(t, "svc", nil)
	logs := server.loggedTo(slog.LevelDebug)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	server.failures.now = func() time.Time { return now }
	own := defaultResourceIdentity(server.otlp)
	target := targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"k": "v"}}}, server.otlp)
	gauges := []model.Metric{valueOf("m", model.GaugeMetricType, map[string]string{"a": "1"}, 1), valueOf("m", model.GaugeMetricType, map[string]string{"a": "2"}, 1), valueOf("same", model.GaugeMetricType, nil, 1)}
	counters := []model.Metric{valueOf("m", model.CounterMetricType, nil, 2), valueOf("same", model.UntypedMetricType, nil, 2)}
	export := func(at int64, identity otlpResourceIdentity, first, second []model.Metric) []map[string]any {
		logs.Reset()
		server.queueOTLPResource(scrapedAt(t, at, first...), identity, scrapeTime{})
		server.queueOTLPResource(scrapedAt(t, at+500, second...), identity, scrapeTime{})
		oneWriterEach(t, "the export", server.push(t, server.drainOTLP()), true)
		return nameClashLines(t, logs)
	}
	logged := export(1000, own, gauges, counters)
	if len(logged) != 1 || logged[0]["level"] != "WARN" || logged[0]["metric"] != "m" || logged[0]["kind"] != "sum" || logged[0]["left_out_kind"] != "gauge" || logged[0]["left_out_points"] != float64(2) || logged[0]["service_name"] != "svc" {
		t.Fatalf("the first export with m as a gauge and a counter logged\n%s", logs)
	}
	for _, say := range []string{"export this metric name as different kinds", "rename one of the metrics", "otlp.service_name or otlp.resource_attributes"} {
		if text, _ := logged[0]["error"].(string); !strings.Contains(text, say) {
			t.Errorf("the line says %q, and not %q", text, say)
		}
	}
	// The other way round, a minute later: the same clash, going on.
	now = now.Add(time.Minute)
	logged = export(2000, own, counters, gauges)
	if len(logged) != 1 || logged[0]["level"] != "DEBUG" || logged[0]["repeat"] != true || logged[0]["kind"] != "gauge" || logged[0]["left_out_kind"] != "sum" || logged[0]["left_out_points"] != float64(1) {
		t.Fatalf("the second export logged\n%s", logs)
	}
	// Under another resource the name is another's.
	logged = export(3000, target, gauges, counters)
	if len(logged) != 1 || logged[0]["level"] != "WARN" || logged[0]["repeat"] != nil || logged[0]["repeated"] != nil {
		t.Fatalf("the export of the name under another resource logged\n%s", logs)
	}
	// Past the five minutes, the line again, with what happened meanwhile.
	now = now.Add(5 * time.Minute)
	logged = export(4000, own, gauges, counters)
	if len(logged) != 1 || logged[0]["level"] != "WARN" || logged[0]["repeated"] != float64(2) || logged[0]["failing_since"] != "2026-10-06T12:00:00Z" {
		t.Fatalf("the export after five minutes logged\n%s", logs)
	}
	// Three kinds of a name, and another name of two: a line each, the
	// kinds left out in one.
	logged = export(5000, own, append(slices.Clone(gauges), valueOf("n", model.GaugeMetricType, nil, 1), histogramOf("o", nil, 4, 4)),
		[]model.Metric{summaryOf("m", nil, 0.5, 4), valueOf("n", model.CounterMetricType, nil, 1), valueOf("o", model.GaugeMetricType, nil, 1)})
	if len(logged) != 3 || logged[0]["metric"] != "m" || logged[0]["kind"] != "summary" || logged[1]["metric"] != "n" || logged[1]["level"] != "WARN" || logged[2]["metric"] != "o" || logged[2]["left_out_kind"] != "histogram" {
		t.Fatalf("the export of three names of two kinds logged\n%s", logs)
	}
	logs.Reset()
	server.queueOTLPResource(scrapedAt(t, 6000, valueOf("m", model.CounterMetricType, nil, 2)), own, scrapeTime{})
	server.queueOTLPResource(scrapedAt(t, 6500, valueOf("m", model.GaugeMetricType, nil, 1)), own, scrapeTime{})
	server.queueOTLPResource(scrapedAt(t, 7000, summaryOf("m", nil, 0.5, 4)), own, scrapeTime{})
	oneWriterEach(t, "the export", server.push(t, server.drainOTLP()), true)
	if logged = nameClashLines(t, logs); len(logged) != 1 || logged[0]["kind"] != "summary" || logged[0]["left_out_kind"] != "gauge, sum" || logged[0]["left_out_points"] != float64(2) {
		t.Fatalf("the export of a name of three kinds logged\n%s", logs)
	}
}

// The key a name of two kinds is remembered under in the failure log is
// that resource's and that name's alone, whatever the two hold: a resource
// whose key ends with what another's name begins with is another subject,
// and so is every subject of another kind.
func TestTheKeyOfANameOfTwoKindsIsItsResourcesAndItsNamesAlone(t *testing.T) {
	keys := map[string]string{}
	for _, subject := range [][2]string{{"r", "m"}, {"r\x00m", ""}, {"", "r\x00m"}, {"r", "\x00m"}, {"1\x00r", "m"}, {"r1", "\x00m"}} {
		key := otlpNameClashKey(subject[0], subject[1])
		if other, made := keys[key.bytes]; made {
			t.Errorf("the name %q of the resource %q has the key of %s", subject[1], subject[0], other)
		}
		keys[key.bytes] = strconv.Quote(subject[1]) + " of " + strconv.Quote(subject[0])
		if key.collector != "" || key.static {
			t.Errorf("the key of %q of %q is a collector's or a static target's: %+v", subject[1], subject[0], key)
		}
	}
	for what, other := range map[string]subjectKey{
		"a static target's metric left out of the endpoint": staticClashKey("r", "m"),
		"a static target's": staticTargetKey("r", "m"),
		"what a trip found": failureKey("r", "m", ""),
		"a probe's":         probeFailureKey("r", "m", ""),
	} {
		if _, made := keys[other.bytes]; made {
			t.Errorf("the key of %s is that of a name of two kinds", what)
		}
	}
}

// An export in which no two series may be one stream is made once, as it
// was: each start time is asked for once, and making the points allocates
// what it did. Where a name is of two kinds the points are gone through
// again, and the start time of a point that is exported is asked for
// twice, of one that is left out once.
func TestAnExportWithoutSharedStreamsIsMadeOnce(t *testing.T) {
	asked := map[string]int{}
	start := func(m model.Metric, _ string) string {
		asked[m.Name+" "+m.Labels["id"]]++
		return "1"
	}
	set := otlpBenchSet(50)
	set.Metrics = append(set.Metrics, valueOf("item_size_bytes", model.GaugeMetricType, nil, 1), histogramOf("item_seconds", nil, 4, 4), summaryOf("item_wait_seconds", nil, 0.5, 4), valueOf("item_free_bytes", model.UntypedMetricType, nil, 1))
	if _, clashes := otlpMetricsOf(otlpResourceSet{Set: set}, "1", start); len(clashes) != 0 || len(asked) != 52 {
		t.Fatalf("%d names of two kinds and %d start times asked for, want none and 52", len(clashes), len(asked))
	}
	for series, times := range asked {
		if times != 1 {
			t.Errorf("the start time of %s is asked for %d times, want once", series, times)
		}
	}
	now, _ := alloctest.Allocations(20, func() { otlpMetrics(set, "1", nil) })
	was, _ := alloctest.Allocations(20, func() { otlpMetricsAsItWas(set, "1", nil) })
	if now > was {
		t.Errorf("the points of an export without shared streams take %v allocations, and took %v", now, was)
	}
	// The gauge of the name of the counters is written last.
	clear(asked)
	set.Metrics = append(set.Metrics, valueOf("item_seconds", model.CounterMetricType, map[string]string{"id": "sum"}, 1))
	out, clashes := otlpMetricsOf(otlpResourceSet{Set: set}, "1", start)
	if len(clashes) != 1 || clashes[0].name != "item_seconds" || clashes[0].kept != otlpKindSum || clashes[0].points != 1 || len(out) != 5 {
		t.Fatalf("the names of two kinds are %+v, and the export has %d metrics", clashes, len(out))
	}
	for series, times := range asked {
		if want := map[bool]int{true: 1, false: 2}[series == "item_seconds "]; times != want {
			t.Errorf("the start time of %s is asked for %d times, want %d", series, times, want)
		}
	}
}

// One scrape's set, which has every stream once, is made the points it was
// made, byte for byte, with the start times asked for in the order they
// were: generated sets of gauges, untyped series, counters, histograms and
// summaries, some with values their type does not allow, so that many are
// gone through a second time for the families exported as gauges, and
// nothing is left out of any.
func TestOneScrapesPointsAreMadeAsTheyWere(t *testing.T) {
	sets := alloctest.UnlessRaced(2000, 300)
	run := &writerRun{random: rand.New(rand.NewPCG(7, 36))} //nolint:gosec // series for a test
	asGauges, several := 0, 0
	for n := range sets {
		var set model.MetricSet
		for range 1 + run.random.IntN(4) {
			set.Metrics = append(set.Metrics, run.scrape().Metrics...)
			if set.Validate(model.Limits{}) != nil {
				break
			}
		}
		for set.Validate(model.Limits{}) != nil {
			set.Metrics = set.Metrics[:len(set.Metrics)-1]
		}
		var asked, askedBefore []string
		start := func(to *[]string) func(m model.Metric, at string) string {
			return func(m model.Metric, at string) string {
				*to = append(*to, sameSubject(m.Labels, m.Name, string(m.Type))+at)
				return "5"
			}
		}
		now, err := json.Marshal(otlpMetrics(set, "9", start(&asked)))
		if err != nil {
			t.Fatal(err)
		}
		was, err := json.Marshal(otlpMetricsAsItWas(set, "9", start(&askedBefore)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(now, was) || !slices.Equal(asked, askedBefore) {
			t.Fatalf("set %d is made\n%s\nasking for the start times of %q, and was made\n%s\nasking for %q", n, now, asked, was, askedBefore)
		}
		if _, shared := otlpPoints(set, 0, "9", nil, nil); shared {
			asGauges++
		}
		if len(set.Metrics) > 3 {
			several++
		}
	}
	if asGauges < sets/10 || several < sets/3 {
		t.Errorf("of %d sets, %d had more than three series and %d were gone through a second time", sets, several, asGauges)
	}
}

// writerRun is one generated run of exports: the resources its writers
// share or have for themselves, and what their scrapes are drawn from.
type writerRun struct {
	random    *rand.Rand
	resources []otlpResourceIdentity
	clock     int64
}

// The names a generated scrape draws from: a family and the names of its
// samples, so that a histogram or a summary exported as gauges writes what
// a gauge of another writer does, and two of the exporter's own.
var writerRunNames = []string{"w", "w", "w_bucket", "w_sum", "w_count", "v_total", "http_exporter_otlp_exports_total", "http_exporter_otlp_export_duration_seconds"}

// scrape is a set one scrape may answer with (MetricSet.Validate), of a
// few series of every type, some with values their type does not allow,
// each as of a millisecond of its own, later than every one before it.
func (r *writerRun) scrape() model.MetricSet {
	types := []model.MetricType{model.GaugeMetricType, model.GaugeMetricType, model.UntypedMetricType, model.CounterMetricType, model.HistogramMetricType, model.SummaryMetricType}
	var set model.MetricSet
	for range 1 + r.random.IntN(5) {
		m := model.Metric{Name: pick(r.random, writerRunNames), Type: types[r.random.IntN(len(types))], Labels: map[string]string{}}
		if r.random.IntN(3) == 0 {
			m.Labels["a"] = pick(r.random, []string{"1", "2"})
		}
		odd := r.random.IntN(3) == 0
		switch m.Type {
		case model.HistogramMetricType:
			m.Histogram = &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 3}, {UpperBound: math.Inf(1), CumulativeCount: 4}}, Sum: 1.5, Count: 4}
			if odd {
				m.Histogram.Count = 5
			}
		case model.SummaryMetricType:
			m.Summary = &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 7}}, Sum: 2.5, Count: 4}
			if odd {
				m.Summary.Quantiles[0].Quantile = 1.5
			}
		case model.CounterMetricType:
			if m.Value = 5; odd {
				m.Value = math.NaN()
			}
			if strings.HasSuffix(m.Name, "_exports_total") && r.random.IntN(2) == 0 {
				m.Labels["result"] = "success"
			}
		default:
			// The labels the samples of a family exported as gauges
			// have.
			switch r.random.IntN(4) {
			case 0:
				m.Labels["le"] = pick(r.random, []string{"1", "+Inf"})
			case 1:
				m.Labels["quantile"] = pick(r.random, []string{"0.5", "1.5"})
			}
		}
		// A series one scrape cannot have beside those it has is left
		// out of it.
		set.Metrics = append(set.Metrics, m)
		if set.Validate(model.Limits{}) != nil {
			set.Metrics = set.Metrics[:len(set.Metrics)-1]
		}
	}
	for i := range set.Metrics {
		r.clock++
		at := r.clock
		set.Metrics[i].Timestamp = &at
	}
	return set
}

// ownPointsAt is the millisecond the model gives the exporter's own
// points, later than any scrape of a run and than now.
const ownPointsAt = int64(4102444800000)

// writtenAt is when a point was written, in milliseconds, and whether it
// is one of the exporter's own, written at the export: those of the
// scrapes of a run are of the first seconds of 1970.
func (p otlpStream) writtenAt(t *testing.T) (at int64, own bool) {
	t.Helper()
	nanoseconds, err := strconv.ParseInt(p.at, 10, 64)
	if err != nil {
		t.Fatalf("the point %s is of %q", p, p.at)
	}
	at = nanoseconds / int64(time.Millisecond)
	return at, at > 1_000_000_000_000
}

// writersLine is a point as the generated runs compare it: its stream, and
// when it was written, which says whose it is.
func (p otlpStream) writersLine(t *testing.T) string {
	t.Helper()
	at, own := p.writtenAt(t)
	if own {
		return p.resource + p.kind + " " + p.name + p.attributes + " the exporter's own"
	}
	return p.resource + p.kind + " " + p.name + p.attributes + " written at " + strconv.FormatInt(at, 10)
}

// ofTheLastWriters are, of the points an export had while every series
// was one, those of one writer for each stream: of each name of a resource
// the points of the kind written last, and of those with the same
// attributes the one written last; with how many names it had as two
// kinds, how many points it leaves out for another of their stream, and
// how many of the points it leaves out were left out for one of the
// exporter's own.
func ofTheLastWriters(t *testing.T, all []otlpStream) (kept []string, kinds, twice, forOwn int) {
	t.Helper()
	later := func(a, b otlpStream) bool {
		at, _ := a.writtenAt(t)
		than, _ := b.writtenAt(t)
		return at > than
	}
	names, streams := map[string]otlpStream{}, map[string]otlpStream{}
	twoKinds := map[string]bool{}
	for _, p := range all {
		name := p.resource + p.name
		last, seen := names[name]
		if seen && last.kind != p.kind {
			twoKinds[name] = true
		}
		if !seen || later(p, last) {
			names[name] = p
		}
	}
	for _, p := range all {
		if p.kind != names[p.resource+p.name].kind {
			continue
		}
		stream := p.resource + p.name + p.attributes
		if last, seen := streams[stream]; !seen || later(p, last) {
			streams[stream] = p
		}
	}
	for _, p := range all {
		last := names[p.resource+p.name]
		if p.kind == last.kind {
			if last = streams[p.resource+p.name+p.attributes]; last == p {
				kept = append(kept, p.writersLine(t))
				continue
			}
			twice++
		}
		if _, own := last.writtenAt(t); own {
			forOwn++
		}
	}
	slices.Sort(kept)
	return kept, len(twoKinds), twice, forOwn
}

// No export has two points of one stream or a name as two metrics, in runs
// of scrapes of gauges, untyped series, counters, histograms and summaries
// of a few names — a family's and those of its samples, and two of the
// exporter's own — with labels that are a sample's too, some with values
// their type does not allow, queued under the exporter's resource and
// under static targets' own, some exports failing so that their points
// wait again while more are queued. Every export, with the exporter's own
// metrics, is the points the conversion made of what waited while every
// series was a point (otlpMetricsAsItWas), without those of a kind of
// their name, and those of a stream, that were not written last: nothing
// else is left out and nothing added, a gauge's point has no start time
// and every other has one.
func TestNoOTLPExportHasTwoPointsOfOneStream(t *testing.T) {
	runs := alloctest.UnlessRaced(300, 40)
	const rounds = 5
	exports, plain, withKinds, withTwice, withOwn, waitedAgain := 0, 0, 0, 0, 0, 0
	for seed := range runs {
		run := &writerRun{random: rand.New(rand.NewPCG(uint64(seed), 36))} //nolint:gosec // exports for a test
		server, waiting := newOTLPKeyServer(t, "svc", nil), ownOTLPKeys()
		run.resources = []otlpResourceIdentity{defaultResourceIdentity(server.otlp), targetResource(&model.StaticTarget{}, server.otlp), targetResource(&model.StaticTarget{OTLP: model.TargetOTLPConfig{ServiceName: "legacy"}}, server.otlp)}
		queue := func() {
			for range 1 + run.random.IntN(4) {
				identity, set := run.resources[run.random.IntN(len(run.resources))], run.scrape()
				server.queueOTLPResource(set, identity, scrapeTime{})
				waiting.queue(identity, set)
			}
		}
		for round := range rounds {
			queue()
			switch run.random.IntN(5) {
			case 0:
				// The export fails, and its points wait again.
				server.mu.Lock()
				server.unavailable = true
				server.mu.Unlock()
				server.exportOTLP(context.Background(), time.Minute)
				server.mu.Lock()
				server.unavailable = false
				server.mu.Unlock()
				waitedAgain++
				continue
			case 1:
				// It fails while a scrape is queued.
				drained, were := server.drainOTLP(), waiting.drain()
				queue()
				server.requeueOTLP(drained)
				waiting.requeue(were)
				waitedAgain++
				continue
			}
			// The export as it was: every series that waited, and the
			// exporter's own under its resource.
			own := server.selfMetricSet()
			for i := range own.Metrics {
				at := ownPointsAt
				own.Metrics[i].Timestamp = &at
			}
			payload, ownResource := otlpPayload{}, run.resources[0].key()
			were := waiting.drain()
			if !slices.ContainsFunc(were, func(r *otlpModelResource) bool { return r.identity.key() == ownResource }) {
				were = append(were, &otlpModelResource{identity: run.resources[0]})
			}
			for _, resource := range were {
				metrics := resource.metrics
				if resource.identity.key() == ownResource {
					metrics = append(slices.Clone(metrics), own.Metrics...)
				}
				payload.ResourceMetrics = append(payload.ResourceMetrics, otlpResourceMetrics{Resource: otlpResource{Attributes: resource.identity.attributes()}, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "prometheus-universal-exporter"}, Metrics: otlpMetricsAsItWas(model.MetricSet{Metrics: metrics}, "0", nil)}}})
			}
			was, err := encodeOTLP(payload, model.OTLPCompressionNone)
			if err != nil {
				t.Fatal(err)
			}
			want, kinds, twice, forOwn := ofTheLastWriters(t, otlpStreams(t, was))
			server.exportOTLP(context.Background(), time.Minute)
			sent := server.sent(t)
			what := "run " + strconv.Itoa(seed) + ", export " + strconv.Itoa(round)
			oneWriterEach(t, what, sent, true)
			var got []string
			for _, p := range otlpStreams(t, sent) {
				got = append(got, p.writersLine(t))
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("%s is sent as\n  %s\nwant, of the points it had while every series was one,\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			exports++
			if kinds == 0 && twice == 0 {
				plain++
			}
			if kinds > 0 {
				withKinds++
			}
			if twice > 0 {
				withTwice++
			}
			if forOwn > 0 {
				withOwn++
			}
		}
	}
	// What the runs are of, scaled with how many there are.
	if exports < 2*runs || plain < runs/4 || withKinds < runs/2 || withTwice < runs/2 || withOwn < runs/4 || waitedAgain < runs/2 {
		t.Errorf("of %d exports of %d runs, %d had every stream once already, %d a name of two kinds, %d two points of a stream, %d a point of a stream of the exporter's own; %d exports failed", exports, runs, plain, withKinds, withTwice, withOwn, waitedAgain)
	}
	t.Logf("%d exports of %d runs: %d with every stream once already, %d with a name of two kinds, %d with two points of a stream, %d with a point of a stream of the exporter's own; %d exports failed", exports, runs, plain, withKinds, withTwice, withOwn, waitedAgain)
}

// otlpMetricsAsItWas is otlpMetrics as it was while every series queued was
// a point of the export and a name was a metric for every kind it had.
func otlpMetricsAsItWas(set model.MetricSet, now string, start func(m model.Metric, at string) string) []otlpMetric {
	var out []otlpMetric
	index := map[string]int{}
	// metric is the metric of a name and a kind that a point is added to. A
	// name first seen, or reused with another kind, is a new metric, so
	// points of different kinds are never mixed in one.
	metric := func(name, help string, kind int) *otlpMetric {
		i, seen := index[name]
		if !seen || otlpKind(out[i]) != kind {
			index[name] = len(out)
			i = len(out)
			out = append(out, newOTLPMetric(name, help, kind))
		}
		return &out[i]
	}
	untyped := untypedFamilies(set)
	var e expositionWriter
	for _, m := range set.Metrics {
		at := now
		if m.Timestamp != nil {
			at = strconv.FormatInt(*m.Timestamp*int64(time.Millisecond), 10)
		}
		attributes := otlpAttributesForLabels(m.Labels)
		gauge := func(name string, attributes []otlpAttribute, value float64) {
			v := otlpDouble(value)
			g := metric(name, m.Help, otlpKindGauge).Gauge
			g.DataPoints = append(g.DataPoints, otlpNumberDataPoint{Attributes: attributes, TimeUnixNano: at, AsDouble: &v})
		}
		kind := otlpKindOf(m)
		if kind != otlpKindGauge && untyped[m.Name] {
			// The lines the text format writes for the series, each a gauge.
			switch kind {
			case otlpKindHistogram:
				for _, b := range e.ascendingBuckets(m.Histogram.Buckets) {
					if !math.IsInf(b.UpperBound, 1) {
						gauge(m.Name+"_bucket", otlpAttributesWith(m.Labels, "le", strconv.FormatFloat(b.UpperBound, 'g', -1, 64)), float64(b.CumulativeCount))
					}
				}
				if inf, has := m.Histogram.InfBucket(); has {
					gauge(m.Name+"_bucket", otlpAttributesWith(m.Labels, "le", "+Inf"), float64(inf))
				}
				if !m.Histogram.NoSum {
					gauge(m.Name+"_sum", attributes, m.Histogram.Sum)
				}
				if !m.Histogram.NoCount {
					gauge(m.Name+"_count", attributes, float64(m.Histogram.Count))
				}
			case otlpKindSummary:
				for _, q := range e.ascendingQuantiles(m.Summary.Quantiles) {
					gauge(m.Name, otlpAttributesWith(m.Labels, "quantile", strconv.FormatFloat(q.Quantile, 'g', -1, 64)), q.Value)
				}
				if !m.Summary.NoSum {
					gauge(m.Name+"_sum", attributes, m.Summary.Sum)
				}
				if !m.Summary.NoCount {
					gauge(m.Name+"_count", attributes, float64(m.Summary.Count))
				}
			default:
				gauge(m.Name, attributes, m.Value)
			}
			continue
		}
		startAt := ""
		switch {
		case kind == otlpKindGauge:
		case m.Created != 0:
			// One of the exporter's own series, which says when it began to
			// count (selfcreated.go).
			startAt = strconv.FormatInt(m.Created*int64(time.Millisecond), 10)
		case start != nil:
			startAt = start(m, at)
		}
		switch kind {
		case otlpKindHistogram:
			point := otlpHistogramPoint(m, attributes, at)
			point.StartTimeUnixNano = startAt
			h := metric(m.Name, m.Help, kind).Histogram
			h.DataPoints = append(h.DataPoints, point)
		case otlpKindSummary:
			point := otlpSummaryPoint(m, attributes, at)
			point.StartTimeUnixNano = startAt
			s := metric(m.Name, m.Help, kind).Summary
			s.DataPoints = append(s.DataPoints, point)
		case otlpKindSum:
			v := otlpDouble(m.Value)
			s := metric(m.Name, m.Help, kind).Sum
			s.DataPoints = append(s.DataPoints, otlpNumberDataPoint{Attributes: attributes, StartTimeUnixNano: startAt, TimeUnixNano: at, AsDouble: &v})
		default:
			gauge(m.Name, attributes, m.Value)
		}
	}
	return out
}
