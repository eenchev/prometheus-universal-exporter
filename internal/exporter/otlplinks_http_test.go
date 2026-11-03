//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/protobuf/encoding/protowire"
)

// Metrics, logs and traces linked (otlptraceslink.go): a log record carries
// the trace and the span it was logged in only for a trace that was
// exported, the scrape-time histogram's buckets carry exemplars of exported
// traces in OpenMetrics, protobuf and OTLP, and with otlp.traces.propagate
// a trip's requests carry its traceparent.

// linkedServer is a traces server whose log goes through the log export's
// tee too, with otlp.logs on, and its stderr.
func linkedServer(t *testing.T, receiver *logReceiver, threshold time.Duration, change func(*model.Config)) (*Server, *lockedBuffer) {
	t.Helper()
	server := tracesServer(t, receiver, threshold, func(c *model.Config) {
		c.OTLP.Logs.Enabled = true
		if change != nil {
			change(c)
		}
	}, nil)
	stderr := &lockedBuffer{}
	logs := NewOTLPLogs(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server.SetOTLPLogs(logs)
	server.logger = slog.New(logs.Handler())
	return server, stderr
}

// recordOf is the record of the line msg logged about the target address.
func recordOf(t *testing.T, records []otlpLogRecord, msg, address string) otlpLogRecord {
	t.Helper()
	for _, r := range records {
		if text(r.Body) == msg && strings.Contains(text(attributesOf(r)["target"]), address) {
			return r
		}
	}
	t.Fatalf("no record %q of %s among %+v", msg, address, records)
	return otlpLogRecord{}
}

// A probe's "probe failed" line is linked to its trace when the trace is
// exported, as slow: its record carries the trace id, the id of the span of
// the stage that failed, which was running when the line was logged, and
// the sampled flag. A fast probe's trace is dropped, and its record carries
// none of them; with otlp.traces off no record does. stderr's lines are as
// they were: no trace or span id, linked or not.
func TestALogRecordIsLinkedToItsProbesTraceOnlyWhenExported(t *testing.T) {
	slow := tracedTarget(t, 60*time.Millisecond, http.StatusInternalServerError)
	fast := tracedTarget(t, 0, http.StatusInternalServerError)
	receiver := newLogReceiver(t)
	server, stderr := linkedServer(t, receiver, 40*time.Millisecond, nil)
	for _, target := range []string{slow.URL, fast.URL} {
		if answer := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(target), nil); answer.Code != http.StatusBadGateway {
			t.Fatalf("the probe of %s is answered %d", target, answer.Code)
		}
	}
	exportTracesNow(server)
	exportLogsNow(server)
	spans := receiver.spans(t)
	var root, stage *receivedSpan
	for i := range spans {
		switch spans[i].Name {
		case "probe text":
			root = &spans[i]
		case "http_status":
			stage = &spans[i]
		}
	}
	if root == nil || stage == nil || stage.Status == nil {
		t.Fatalf("the slow probe's spans are %+v", spans)
	}
	records := receiver.logRecords(t)
	linked := recordOf(t, records, "probe failed", strings.TrimPrefix(slow.URL, "http://"))
	if linked.TraceID != root.TraceID || linked.SpanID != stage.SpanID || linked.Flags != 1 {
		t.Errorf("the slow probe's record has trace %q, span %q and flags %d; want %q, the failed http_status stage's span %q, and 1", linked.TraceID, linked.SpanID, linked.Flags, root.TraceID, stage.SpanID)
	}
	dropped := recordOf(t, records, "probe failed", strings.TrimPrefix(fast.URL, "http://"))
	if dropped.TraceID != "" || dropped.SpanID != "" || dropped.Flags != 0 {
		t.Errorf("the fast probe's trace was dropped, yet its record has trace %q, span %q and flags %d", dropped.TraceID, dropped.SpanID, dropped.Flags)
	}
	receiver.mu.Lock()
	for i, path := range receiver.paths {
		if path == "/v1/logs" && bytes.Contains(receiver.bodies[i], []byte(`"traceId":""`)) {
			t.Errorf("a record has an empty traceId:\n%s", receiver.bodies[i])
		}
	}
	receiver.mu.Unlock()
	for _, unwanted := range []string{"trace", "span"} {
		if strings.Contains(stderr.String(), unwanted) {
			t.Errorf("stderr's lines carry a %s id:\n%s", unwanted, stderr.String())
		}
	}

	off := newLogReceiver(t)
	server, _ = linkedServer(t, off, 40*time.Millisecond, func(c *model.Config) { c.OTLP.Traces.Enabled = false })
	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(slow.URL), nil)
	exportLogsNow(server)
	if r := recordOf(t, off.logRecords(t), "probe failed", strings.TrimPrefix(slow.URL, "http://")); r.TraceID != "" || r.SpanID != "" || r.Flags != 0 {
		t.Errorf("with otlp.traces off, a record has trace %q, span %q and flags %d", r.TraceID, r.SpanID, r.Flags)
	}
}

// A record is linked when the log export drains it, by the time it was
// logged: to the innermost stage running then, else to the root. One whose
// trace is not yet decided when it is drained leaves without a link, and
// gets none later; one of a trace dropped gets none either.
func TestALogRecordIsLinkedWhenItsTraceIsDecidedBeforeItLeaves(t *testing.T) {
	logs := NewOTLPLogs(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	logs.configure(model.OTLPConfig{Enabled: true, Endpoint: "http://collector.invalid/v1/metrics", Logs: model.OTLPLogsConfig{Enabled: true, Endpoint: "http://collector.invalid/v1/logs"}})
	logger := slog.New(logs.Handler())
	link := newTraceLink(traceParent{})
	ctx := withTraceLink(context.Background(), link)
	decoded := func(raw []byte) otlpLogRecord {
		var r otlpLogRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	logger.InfoContext(ctx, "before the decision")
	undecided := logs.drain()
	if r := decoded(undecided[0]); r.TraceID != "" || r.SpanID != "" || r.Flags != 0 {
		t.Errorf("a record drained before its trace was decided carries %+v", r)
	}

	begin := time.Now()
	logger.InfoContext(ctx, "within the stage")
	mid := time.Now()
	time.Sleep(time.Millisecond)
	logger.InfoContext(ctx, "after the stage")
	stages := []spanStage{{name: "http", outcome: "ok", start: begin, end: mid}}
	made := buildSpans(spanRoot{name: "probe text", traceID: link.traceID, spanID: link.rootID, start: begin, end: time.Now()}, stages, nil)
	link.settle(true, stages, made)
	trace, root := link.ids()
	got := logs.drain()
	if len(got) != 2 {
		t.Fatalf("%d records drained", len(got))
	}
	if r := decoded(got[0]); r.TraceID != trace || r.SpanID != made[1].SpanID || r.Flags != 1 {
		t.Errorf("the line logged within the stage carries trace %q, span %q, flags %d; want %q, the stage's %q, 1", r.TraceID, r.SpanID, r.Flags, trace, made[1].SpanID)
	}
	if r := decoded(got[1]); r.TraceID != trace || r.SpanID != root || r.Flags != 1 {
		t.Errorf("the line logged after the stage carries trace %q, span %q, flags %d; want %q, the root's %q, 1", r.TraceID, r.SpanID, r.Flags, trace, root)
	}

	dropped := newTraceLink(traceParent{})
	logger.InfoContext(withTraceLink(context.Background(), dropped), "dropped")
	dropped.settle(false, nil, nil)
	if r := decoded(logs.drain()[0]); r.TraceID != "" || r.SpanID != "" || r.Flags != 0 {
		t.Errorf("a record of a dropped trace carries %+v", r)
	}
}

// exemplarServer is a verbose server with one collector, app, and the
// histogram's statistics.
func exemplarServer(t *testing.T) (*Server, *serverStats) {
	t.Helper()
	server := histogramServer(t, nil)
	_, _, stats := server.collectorStats()
	return server, stats["app"]
}

// The scrape-time histogram's bucket carries the latest exemplar of a trip
// whose trace was exported, and none of one dropped or not yet decided; one
// observed later wins whichever is exported first. OpenMetrics writes it
// after the bucket's count as "# {trace_id=…,span_id=…} value timestamp",
// the +Inf bucket's too; the Prometheus text format writes none.
func TestTheScrapeTimeHistogramCarriesExemplarsOfExportedTraces(t *testing.T) {
	server, stats := exemplarServer(t)
	earlier, later, dropped, undecided, slowest := newTraceLink(traceParent{}), newTraceLink(traceParent{}), newTraceLink(traceParent{}), newTraceLink(traceParent{}), newTraceLink(traceParent{})
	server.observeTargetScrape(stats, 60*time.Millisecond, earlier)
	server.observeTargetScrape(stats, 70*time.Millisecond, later)
	server.observeTargetScrape(stats, 80*time.Millisecond, dropped)
	server.observeTargetScrape(stats, 3*time.Millisecond, undecided)
	server.observeTargetScrape(stats, 90*time.Second, slowest)
	later.settle(true, nil, nil)
	earlier.settle(true, nil, nil)
	dropped.settle(false, nil, nil)
	slowest.settle(true, nil, nil)
	stats.mu.Lock()
	kept, inf := *stats.durations.exemplars[4], *stats.durations.exemplars[len(scrapeDurationBuckets)]
	stats.mu.Unlock()
	trace, span := later.ids()
	if kept.TraceID != trace || kept.SpanID != span || kept.Value != 0.07 {
		t.Fatalf("the 0.1 bucket's exemplar is %+v, want the later trip's", kept)
	}
	stamp := func(at time.Time) string { return strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', -1, 64) }
	om := selfMetricsAccepting(t, server, "application/openmetrics-text;version=1.0.0").Body.String()
	slowTrace, slowSpan := slowest.ids()
	for _, want := range []string{
		`http_exporter_collector_scrape_duration_seconds_bucket{collector="app",le="0.1"} 4 # {trace_id="` + trace + `",span_id="` + span + `"} 0.07 ` + stamp(kept.Time) + "\n",
		`http_exporter_collector_scrape_duration_seconds_bucket{collector="app",le="+Inf"} 5 # {trace_id="` + slowTrace + `",span_id="` + slowSpan + `"} 90 ` + stamp(inf.Time) + "\n",
		`http_exporter_collector_scrape_duration_seconds_bucket{collector="app",le="0.005"} 1` + "\n",
		`http_exporter_collector_scrape_duration_seconds_bucket{collector="app",le="0.05"} 1` + "\n",
	} {
		requireContains(t, om, want)
	}
	if n := strings.Count(om, " # {"); n != 2 {
		t.Errorf("OpenMetrics has %d exemplars, want 2:\n%s", n, om)
	}
	if text := selfMetricsAccepting(t, server, "").Body.String(); strings.Contains(text, "trace_id") {
		t.Errorf("the text format carries an exemplar:\n%s", text)
	}
	undecided.settle(true, nil, nil)
	if om := selfMetricsAccepting(t, server, "application/openmetrics-text;version=1.0.0").Body.String(); strings.Count(om, " # {") != 3 {
		t.Errorf("a trace exported after its trip did not make its bucket's exemplar:\n%s", om)
	}
}

// protoExemplar reads an Exemplar message: its labels, value and time.
func protoExemplar(t *testing.T, b []byte) (map[string]string, float64, time.Time) {
	t.Helper()
	fields := protoFields(t, b)
	labels := map[string]string{}
	for _, pair := range fields[1] {
		p := protoFields(t, pair.([]byte))
		labels[string(p[1][0].([]byte))] = string(p[2][0].([]byte))
	}
	ts := protoFields(t, fields[3][0].([]byte))
	var nanos uint64
	if len(ts[2]) > 0 {
		nanos = ts[2][0].(uint64)
	}
	return labels, protoDouble(fields[2][0]), time.Unix(int64(ts[1][0].(uint64)), int64(nanos)) //nolint:gosec // G115: a test's times
}

// The protobuf answer carries the exemplar on its classic bucket, field 3
// of the Bucket, and among the native histogram's exemplars, field 16 of
// the Histogram, with its labels, value and timestamp.
func TestTheProtobufAnswerCarriesTheHistogramsExemplars(t *testing.T) {
	server, stats := exemplarServer(t)
	link := newTraceLink(traceParent{})
	server.observeTargetScrape(stats, 70*time.Millisecond, link)
	link.settle(true, nil, nil)
	stats.mu.Lock()
	want := *stats.durations.exemplars[4]
	stats.mu.Unlock()
	trace, span := link.ids()
	families := protoFamilies(t, selfMetricsAccepting(t, server, prometheusProtobufAccept).Body.Bytes())
	metric := protoFields(t, families["http_exporter_collector_scrape_duration_seconds"][4][0].([]byte))
	histogram := protoFields(t, metric[7][0].([]byte))
	check := func(where string, b []byte) {
		labels, value, at := protoExemplar(t, b)
		if labels["trace_id"] != trace || labels["span_id"] != span || len(labels) != 2 || value != 0.07 || !at.Equal(want.Time) {
			t.Errorf("%s: the exemplar has %v, %v at %v; want trace %s span %s, 0.07 at %v", where, labels, value, at, trace, span, want.Time)
		}
	}
	found := 0
	for k, raw := range histogram[3] {
		bucket := protoFields(t, raw.([]byte))
		if len(bucket[3]) == 0 {
			continue
		}
		found++
		if bound := protoDouble(bucket[2][0]); bound != 0.1 {
			t.Errorf("bucket %d, le %v, has an exemplar", k, bound)
		}
		check("the classic bucket", bucket[3][0].([]byte))
	}
	if found != 1 || len(histogram[16]) != 1 {
		t.Fatalf("%d classic buckets and %d native exemplars have an exemplar, want 1 and 1", found, len(histogram[16]))
	}
	check("the native exemplars", histogram[16][0].([]byte))
}

// The +Inf bucket's exemplar, of an observation past the last bound, is
// written in protobuf on an explicit +Inf classic bucket, of the count, as
// client_golang writes it, since the classic buckets are where a Prometheus
// that keeps the classic histogram reads exemplars. Without that exemplar no
// +Inf bucket is written, sample_count being it.
func TestTheProtobufAnswerWritesTheInfBucketWithItsExemplar(t *testing.T) {
	server, stats := exemplarServer(t)
	classic := func() []map[protowire.Number][]any {
		families := protoFamilies(t, selfMetricsAccepting(t, server, prometheusProtobufAccept).Body.Bytes())
		metric := protoFields(t, families["http_exporter_collector_scrape_duration_seconds"][4][0].([]byte))
		histogram := protoFields(t, metric[7][0].([]byte))
		var out []map[protowire.Number][]any
		for _, raw := range histogram[3] {
			out = append(out, protoFields(t, raw.([]byte)))
		}
		return out
	}
	dropped := newTraceLink(traceParent{})
	server.observeTargetScrape(stats, 90*time.Second, dropped)
	dropped.settle(false, nil, nil)
	without := classic()
	if last := without[len(without)-1]; math.IsInf(protoDouble(last[2][0]), 1) {
		t.Fatalf("a +Inf bucket is written without an exemplar: %v", last)
	}
	exported := newTraceLink(traceParent{})
	server.observeTargetScrape(stats, 95*time.Second, exported)
	exported.settle(true, nil, nil)
	buckets := classic()
	if len(buckets) != len(without)+1 {
		t.Fatalf("%d classic buckets with the +Inf exemplar, %d without; want one more", len(buckets), len(without))
	}
	last := buckets[len(buckets)-1]
	if !math.IsInf(protoDouble(last[2][0]), 1) || last[1][0].(uint64) != 2 || len(last[3]) != 1 {
		t.Fatalf("the last classic bucket is %v; want le +Inf, a count of 2 and an exemplar", last)
	}
	trace, span := exported.ids()
	if labels, value, _ := protoExemplar(t, last[3][0].([]byte)); labels["trace_id"] != trace || labels["span_id"] != span || value != 95 {
		t.Errorf("the +Inf bucket's exemplar has %v, %v; want trace %s span %s, 95", labels, value, trace, span)
	}
	for _, b := range buckets[:len(buckets)-1] {
		if len(b[3]) != 0 {
			t.Errorf("bucket le %v has an exemplar", protoDouble(b[2][0]))
		}
	}
}

// The OTLP export's histogram and exponential histogram points carry the
// exemplars, with the trace and span ids in hex, the value and the time in
// nanoseconds.
func TestTheOTLPHistogramPointsCarryTheExemplars(t *testing.T) {
	server, stats := exemplarServer(t)
	link := newTraceLink(traceParent{})
	server.observeTargetScrape(stats, 70*time.Millisecond, link)
	server.observeTargetScrape(stats, 2*time.Millisecond, newTraceLink(traceParent{}))
	link.settle(true, nil, nil)
	trace, span := link.ids()
	var histogram model.Metric
	for _, m := range server.verboseCollectorMetrics(map[string]*serverStats{"app": stats}) {
		if m.Name == "http_exporter_collector_scrape_duration_seconds" {
			histogram = m
		}
	}
	stats.mu.Lock()
	at := strconv.FormatInt(stats.durations.exemplars[4].Time.UnixNano(), 10)
	stats.mu.Unlock()
	want := fmt.Sprintf(`"exemplars":[{"timeUnixNano":%q,"asDouble":0.07,"spanId":%q,"traceId":%q}]`, at, span, trace)
	for name, point := range map[string]any{"histogram": otlpHistogramPoint(histogram, nil, "1"), "exponential histogram": otlpExponentialHistogramPoint(histogram, nil, "1")} {
		raw, err := json.Marshal(point)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("the %s point is\n%s\nwant %s", name, raw, want)
		}
	}
	if raw, _ := json.Marshal(otlpHistogramPoint(model.Metric{Histogram: &model.Histogram{Count: 1, Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}}}}, nil, "1")); strings.Contains(string(raw), "exemplars") {
		t.Errorf("a target's histogram has exemplars: %s", raw)
	}
}

// A slow probe's trip is its bucket's exemplar, with the exported trace's id
// and its root span's; a fast probe's trip, whose trace was dropped, makes
// none.
func TestAnExportedProbesTripIsItsBucketsExemplar(t *testing.T) {
	receiver := newLogReceiver(t)
	slow := tracedTarget(t, 60*time.Millisecond, http.StatusOK)
	fast := tracedTarget(t, 0, http.StatusOK)
	server := tracesServer(t, receiver, 40*time.Millisecond, func(c *model.Config) { c.Web.SelfMetrics.Verbose = true }, nil)
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
	exemplar := regexp.MustCompile(`(?m)^http_exporter_collector_scrape_duration_seconds_bucket\{collector="text",le="([^"]+)"\} \d+ # \{trace_id="([0-9a-f]{32})",span_id="([0-9a-f]{16})"\} [0-9.e-]+ [0-9.]+$`)
	found := exemplar.FindAllStringSubmatch(om, -1)
	if len(found) != 1 || found[0][2] != root.TraceID || found[0][3] != root.SpanID {
		t.Fatalf("the exemplars are %q, want one of trace %s span %s:\n%s", found, root.TraceID, root.SpanID, om)
	}
	if le, _ := strconv.ParseFloat(found[0][1], 64); le < 0.05 {
		t.Errorf("the exemplar is on the bucket le=%s, under the slow trip's time", found[0][1])
	}
}

// headerTarget answers value=3 and keeps the trace headers of each request.
type headerTarget struct {
	*httptest.Server
	mu      sync.Mutex
	parents []string
	states  []string
}

func newHeaderTarget(t *testing.T, wait time.Duration) *headerTarget {
	t.Helper()
	h := &headerTarget{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.parents = append(h.parents, strings.Join(r.Header.Values("Traceparent"), "|"))
		h.states = append(h.states, strings.Join(r.Header.Values("Tracestate"), "|"))
		h.mu.Unlock()
		time.Sleep(wait)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=3\n"))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *headerTarget) last() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.parents[len(h.parents)-1], h.states[len(h.states)-1]
}

// With otlp.traces.propagate, a probe's request carries the traceparent of
// its trace, under the id its CLIENT span has, which the target's span
// names as its parent: sampled 00 while nothing has kept the trace yet, 01
// when sample_ratio has, its threshold in the tracestate, and when the
// caller's sampled traceparent has under parent_based, the caller's trace
// id and tracestate as it came; a caller's trace not sampled is sent 00,
// without the caller's threshold, which no probability kept it by. Off,
// nothing is sent.
func TestAProbesRequestCarriesItsTraceparent(t *testing.T) {
	target := newHeaderTarget(t, 50*time.Millisecond)
	probe := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	traceparent := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-(0[01])$`)
	for _, test := range []struct {
		name   string
		change func(*model.Config)
		caller http.Header
		flags  string
		state  string
	}{
		{name: "not yet sampled", flags: "00"},
		{name: "kept by the ratio", change: func(c *model.Config) { c.OTLP.Traces.SampleRatio = 1 }, flags: "01", state: "ot=th:0"},
		{name: "kept by the caller", change: func(c *model.Config) { c.OTLP.Traces.ParentBased = true },
			caller: http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}, "Tracestate": {"ot=th:8,vendor=x"}}, flags: "01", state: "ot=th:8,vendor=x"},
		{name: "a caller's not sampled", change: func(c *model.Config) { c.OTLP.Traces.ParentBased = true },
			caller: http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00"}, "Tracestate": {"ot=th:8,vendor=x"}}, flags: "00", state: "vendor=x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			receiver := newLogReceiver(t)
			server := tracesServer(t, receiver, 10*time.Millisecond, func(c *model.Config) {
				c.OTLP.Traces.Propagate = true
				if test.change != nil {
					test.change(c)
				}
			}, nil)
			probeOnce(t, server, probe, test.caller)
			exportTracesNow(server)
			sent, state := target.last()
			m := traceparent.FindStringSubmatch(sent)
			if m == nil {
				t.Fatalf("the request's traceparent is %q", sent)
			}
			var get *receivedSpan
			spans := receiver.spans(t)
			for i := range spans {
				if spans[i].Name == "GET" {
					get = &spans[i]
				}
			}
			if get == nil {
				t.Fatalf("no request span among %+v", spans)
			}
			if m[1] != get.TraceID || m[2] != get.SpanID || m[3] != test.flags || state != test.state {
				t.Errorf("sent traceparent %q and tracestate %q; want trace %s, the request span's id %s, flags %s and tracestate %q", sent, state, get.TraceID, get.SpanID, test.flags, test.state)
			}
			if test.caller != nil && m[1] != "0af7651916cd43dd8448eb211c80319c" {
				t.Errorf("the trace is %s, not the caller's", m[1])
			}
		})
	}
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, 10*time.Millisecond, nil, nil)
	probeOnce(t, server, probe, nil)
	if sent, state := target.last(); sent != "" || state != "" {
		t.Errorf("without propagate the request carries %q and %q", sent, state)
	}
}

// With otlp.traces.propagate and the trace export off, a probe's caller's
// traceparent and tracestate are passed on as they came, and nothing is sent
// without one, or with one that cannot be read. A traceparent the collector
// sets in request.headers is sent as it is, with no tracestate of the
// exporter's, whether the export is on or off.
func TestPropagateWithTheTraceExportOffPassesTheCallersOn(t *testing.T) {
	target := newHeaderTarget(t, 0)
	probe := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, 10*time.Millisecond, func(c *model.Config) {
		c.OTLP.Traces.Enabled, c.OTLP.Traces.Propagate = false, true
	}, nil)
	caller := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	probeOnce(t, server, probe, http.Header{"Traceparent": {caller}, "Tracestate": {"vendor=x,other=y"}})
	if sent, state := target.last(); sent != caller || state != "vendor=x,other=y" {
		t.Errorf("the caller's trace context arrives as %q and %q", sent, state)
	}
	for _, header := range []http.Header{nil, {"Traceparent": {"not-a-traceparent"}}} {
		probeOnce(t, server, probe, header)
		if sent, state := target.last(); sent != "" || state != "" {
			t.Errorf("with %v the request carries %q and %q", header, sent, state)
		}
	}
	configured := "00-11111111111111111111111111111111-2222222222222222-01"
	for _, on := range []bool{false, true} {
		server := tracesServer(t, receiver, 10*time.Millisecond, func(c *model.Config) {
			c.OTLP.Traces.Enabled, c.OTLP.Traces.Propagate, c.OTLP.Traces.SampleRatio = on, true, 1
			c.Collectors[0].Request.Headers = map[string]string{"traceparent": configured}
		}, nil)
		probeOnce(t, server, probe, http.Header{"Traceparent": {caller}})
		if sent, state := target.last(); sent != configured || state != "" {
			t.Errorf("trace export %v: the configured traceparent arrives as %q, with tracestate %q", on, sent, state)
		}
	}
}

// The debug page's run sends the traceparent of the run's trace with
// otlp.traces.propagate, and its request lines show it; its log records
// show the trace and span they would be linked to, the run's own being
// never exported.
func TestTheDebugPageShowsTheTraceparentSentAndTheLinks(t *testing.T) {
	target := newHeaderTarget(t, 0)
	failing := tracedTarget(t, 0, http.StatusInternalServerError)
	receiver := newLogReceiver(t)
	server, _ := linkedServer(t, receiver, time.Hour, func(c *model.Config) { c.OTLP.Traces.Propagate = true })
	server.SetDebug(DebugFull)
	answer := postRun(t, server, map[string]any{"configured": "text", "target": target.URL}, http.StatusOK)
	sent, _ := target.last()
	if len(answer.Requests) != 1 || !strings.Contains(strings.Join(answer.Requests[0].Headers, "\n"), "Traceparent: "+sent) || sent == "" {
		t.Errorf("the target got %q; the run's request shows %+v", sent, answer.Requests)
	}
	answer = postRun(t, server, map[string]any{"configured": "text", "target": failing.URL}, http.StatusOK)
	if answer.OTLP == nil || answer.OTLP.Logs == nil || answer.OTLP.Traces == nil || len(answer.OTLP.Logs.Records) == 0 {
		t.Fatalf("the OTLP tab: %+v", answer.OTLP)
	}
	record := answer.OTLP.Logs.Records[0]
	if !hexTraceID.MatchString(record.TraceID) || !hexSpanID.MatchString(record.SpanID) || !strings.Contains(record.Link, "would link") {
		t.Errorf("the run's record shows trace %q, span %q: %s", record.TraceID, record.SpanID, record.Link)
	}
	if strings.Contains(answer.OTLP.Logs.Body, "traceId") {
		t.Errorf("the run's log export body links a record:\n%s", answer.OTLP.Logs.Body)
	}
	if !strings.Contains(answer.OTLP.Traces.Body, `"traceId": "`+record.TraceID+`"`) || !strings.Contains(answer.OTLP.Traces.Body, `"spanId": "`+record.SpanID+`"`) {
		t.Errorf("the record's trace %s and span %s are not the tab's spans':\n%s", record.TraceID, record.SpanID, answer.OTLP.Traces.Body)
	}
}

// A static target's scrape is linked as a probe is: its "static target
// scrape failed" record carries the trace and the span of the stage that
// failed, and with otlp.traces.propagate its request carries its
// traceparent, under the request span's id.
func TestAStaticTargetsScrapeIsLinkedAsAProbeIs(t *testing.T) {
	var sent atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Store(r.Header.Get("Traceparent"))
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(target.Close)
	receiver := newLogReceiver(t)
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "plain", Collector: "text", Target: target.URL}}}
	server := tracesServer(t, receiver, 10*time.Millisecond, func(c *model.Config) {
		c.OTLP.Logs.Enabled, c.OTLP.Traces.Propagate = true, true
	}, file)
	logs := NewOTLPLogs(slog.NewJSONHandler(&lockedBuffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server.SetOTLPLogs(logs)
	server.logger = slog.New(logs.Handler())
	server.scrapeStaticTargets(context.Background(), 0)
	exportTracesNow(server)
	exportLogsNow(server)
	byName := map[string]receivedSpan{}
	for _, span := range receiver.spans(t) {
		byName[span.Name] = span
	}
	stage, get := byName["http_status"], byName["GET"]
	if stage.SpanID == "" || get.SpanID == "" {
		t.Fatalf("the scrape's spans are %+v", byName)
	}
	var record *otlpLogRecord
	for _, r := range receiver.logRecords(t) {
		if text(r.Body) == "static target scrape failed" {
			record = &r
		}
	}
	if record == nil || record.TraceID != stage.TraceID || record.SpanID != stage.SpanID || record.Flags != 1 {
		t.Errorf("the scrape's record is %+v, want trace %s and the failed stage's span %s", record, stage.TraceID, stage.SpanID)
	}
	if want := "00-" + get.TraceID + "-" + get.SpanID + "-00"; sent.Load() != want {
		t.Errorf("the scrape's request carries %q, want %q", sent.Load(), want)
	}
}

// A trip shared by two probes is sent once, under the traceparent of the
// probe that started it: that probe's request span has the id sent, and
// the probe that joined has a request span of its own id, so that no span
// id is in both traces.
func TestASharedTripIsSentUnderItsStartersTraceparent(t *testing.T) {
	var sent atomic.Value
	held := make(chan struct{})
	arrived := make(chan struct{}, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Store(r.Header.Get("Traceparent"))
		arrived <- struct{}{}
		<-held
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=3\n"))
	}))
	t.Cleanup(target.Close)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) { c.OTLP.Traces.Propagate = true }, nil)
	path := "/probe?collector=text&target=" + url.QueryEscape(target.URL)
	doneA := probeAsync(context.Background(), server, path, nil)
	<-arrived
	doneB := probeAsync(context.Background(), server, path, nil)
	waitForWaiters(t, server, 2)
	close(held)
	waitOutcome(t, doneA, "the probe that started the trip")
	waitOutcome(t, doneB, "the probe that joined")
	exportTracesNow(server)
	traces := tracesOf(receiver.spans(t))
	if len(traces) != 2 {
		t.Fatalf("%d traces were exported", len(traces))
	}
	m := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-00$`).FindStringSubmatch(sent.Load().(string))
	if m == nil {
		t.Fatalf("the trip's traceparent is %q", sent.Load())
	}
	starter, joiner := traces[m[1]], []receivedSpan(nil)
	for id, trace := range traces {
		if id != m[1] {
			joiner = trace
		}
	}
	get, ok := spanNamed(starter, "GET")
	if !ok || get.SpanID != m[2] {
		t.Errorf("the starter's request span is %+v, want the id sent, %s", get.otlpSpan, m[2])
	}
	if other, ok := spanNamed(joiner, "GET"); !ok || other.SpanID == m[2] {
		t.Errorf("the joiner's request span is %+v, with the starter's id", other.otlpSpan)
	}
}
