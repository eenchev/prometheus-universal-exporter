//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// prometheusProtobufAccept is the Accept header of a Prometheus 3 that
// scrapes native histograms: protobuf first.
const prometheusProtobufAccept = "application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.6,application/openmetrics-text;version=1.0.0;q=0.5,text/plain;version=1.0.0;q=0.4,text/plain;version=0.0.4;q=0.3,*/*;q=0.2"

// histogramServer is a verbose server with one collector, app, whose trips
// took the durations given, and cfg's settings besides.
func histogramServer(t *testing.T, change func(*model.Config), durations ...time.Duration) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("app", "text")}, Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}}}
	if change != nil {
		change(cfg)
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	_, _, stats := server.collectorStats()
	for _, d := range durations {
		server.observeTargetScrape(stats["app"], d)
	}
	return server
}

func selfMetricsAccepting(t *testing.T, server *Server, accept string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	server.metricsHandler(w, r)
	return w
}

// protoFields reads one protobuf message into its fields, by number, each
// field's values in order: a varint or fixed64 as a uint64, a bytes field as
// []byte.
func protoFields(t *testing.T, b []byte) map[protowire.Number][]any {
	t.Helper()
	out := map[protowire.Number][]any{}
	for len(b) > 0 {
		number, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			t.Fatalf("tag: %v", protowire.ParseError(n))
		}
		b = b[n:]
		var value any
		switch typ {
		case protowire.VarintType:
			value, n = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			value, n = protowire.ConsumeFixed64(b)
		case protowire.BytesType:
			value, n = protowire.ConsumeBytes(b)
		default:
			t.Fatalf("field %d has wire type %d", number, typ)
		}
		if n < 0 {
			t.Fatalf("field %d: %v", number, protowire.ParseError(n))
		}
		b = b[n:]
		out[number] = append(out[number], value)
	}
	return out
}

func protoDouble(v any) float64 { return math.Float64frombits(v.(uint64)) }
func protoSint(v any) int64     { return protowire.DecodeZigZag(v.(uint64)) }

// protoFamilies reads a delimited protobuf answer into its families by name.
func protoFamilies(t *testing.T, body []byte) map[string]map[protowire.Number][]any {
	t.Helper()
	out := map[string]map[protowire.Number][]any{}
	for len(body) > 0 {
		message, n := protowire.ConsumeBytes(body)
		if n < 0 {
			t.Fatalf("length: %v", protowire.ParseError(n))
		}
		body = body[n:]
		family := protoFields(t, message)
		out[string(family[1][0].([]byte))] = family
	}
	return out
}

// A Prometheus that asks for protobuf first gets the self-metrics in it: the
// families of the text answer, with their types, help, labels and values,
// and the scrape-time histogram with its classic buckets and native ones:
// schema 3, the zero threshold 2^-128, the zero bucket, and spans and deltas
// that give each duration's bucket at schema 3.
func TestSelfMetricsAnswerProtobufWithNativeHistograms(t *testing.T) {
	server := histogramServer(t, nil, 0, 3*time.Millisecond, 5*time.Millisecond, 70*time.Millisecond, 2*time.Second)
	w := selfMetricsAccepting(t, server, prometheusProtobufAccept)
	if got := w.Header().Get("Content-Type"); got != protobufContentType {
		t.Fatalf("Content-Type %q", got)
	}
	if w.Header().Get("Vary") != "Accept" {
		t.Fatalf("Vary %q", w.Header().Get("Vary"))
	}
	families := protoFamilies(t, w.Body.Bytes())
	text := selfMetricsAccepting(t, server, "").Body.String()
	for name := range families {
		if !strings.Contains(text, "# TYPE "+name+" ") {
			t.Errorf("family %s is not in the text answer", name)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			if name, _, _ = strings.Cut(name, " "); families[name] == nil {
				t.Errorf("family %s of the text answer is not in the protobuf one", name)
			}
		}
	}

	scrapes := families["http_exporter_scrapes_total"]
	if scrapes == nil || scrapes[3][0].(uint64) != protoCounter || len(scrapes[2]) != 1 {
		t.Fatalf("http_exporter_scrapes_total: %v", scrapes)
	}

	family := families["http_exporter_collector_scrape_duration_seconds"]
	if family[3][0].(uint64) != protoHistogram || len(family[4]) != 1 {
		t.Fatalf("family: %v", family)
	}
	metric := protoFields(t, family[4][0].([]byte))
	label := protoFields(t, metric[1][0].([]byte))
	if string(label[1][0].([]byte)) != "collector" || string(label[2][0].([]byte)) != "app" {
		t.Fatalf("label %v", label)
	}
	h := protoFields(t, metric[7][0].([]byte))
	if h[1][0].(uint64) != 5 || math.Abs(protoDouble(h[2][0])-2.078) > 1e-9 {
		t.Fatalf("count %v sum %v", h[1][0], protoDouble(h[2][0]))
	}
	// The classic buckets, without +Inf: 0, 3 ms and 5 ms are in le=0.005.
	var bounds []float64
	var cumulative []uint64
	for _, raw := range h[3] {
		bucket := protoFields(t, raw.([]byte))
		cumulative = append(cumulative, bucket[1][0].(uint64))
		bounds = append(bounds, protoDouble(bucket[2][0]))
	}
	if !slices.Equal(bounds, scrapeDurationBuckets) || cumulative[0] != 3 || cumulative[4] != 4 || cumulative[len(cumulative)-1] != 5 {
		t.Fatalf("classic buckets %v %v", bounds, cumulative)
	}
	if schema := protoSint(h[5][0]); schema != 3 {
		t.Fatalf("schema %d", schema)
	}
	if threshold := protoDouble(h[6][0]); threshold != math.Ldexp(1, -128) {
		t.Fatalf("zero threshold %v", threshold)
	}
	if h[7][0].(uint64) != 1 {
		t.Fatalf("zero count %v", h[7][0])
	}
	// Prometheus's buckets at schema 3 of 3 ms, 5 ms, 70 ms and 2 s:
	// -67, -61, -30 and 8, each a span of its own.
	var spans [][2]int64
	for _, raw := range h[12] {
		span := protoFields(t, raw.([]byte))
		spans = append(spans, [2]int64{protoSint(span[1][0]), int64(span[2][0].(uint64))})
	}
	if want := [][2]int64{{-67, 1}, {5, 1}, {30, 1}, {37, 1}}; !slices.Equal(spans, want) {
		t.Fatalf("spans %v, want %v", spans, want)
	}
	var deltas []int64
	for _, raw := range h[13] {
		deltas = append(deltas, protoSint(raw))
	}
	if !slices.Equal(deltas, []int64{1, 0, 0, 0}) {
		t.Fatalf("deltas %v", deltas)
	}
}

// Spans join neighbouring buckets and deltas count from the bucket before,
// across spans: counts 2, 3 in buckets 0 and 1 of OpenTelemetry (1 and 2 of
// Prometheus), then 1 in 4 (5) are the spans {1, 2}, {2, 1} and the deltas
// 2, 1, -2.
func TestNativeBucketsAreSpansAndDeltas(t *testing.T) {
	b := appendNativeBuckets(nil, &model.ExponentialHistogram{Scale: 2, Offset: 0, Counts: []uint64{2, 3, 0, 0, 1}})
	h := protoFields(t, b)
	var spans [][2]int64
	for _, raw := range h[12] {
		span := protoFields(t, raw.([]byte))
		spans = append(spans, [2]int64{protoSint(span[1][0]), int64(span[2][0].(uint64))})
	}
	var deltas []int64
	for _, raw := range h[13] {
		deltas = append(deltas, protoSint(raw))
	}
	if protoSint(h[5][0]) != 2 || !slices.Equal(spans, [][2]int64{{1, 2}, {2, 1}}) || !slices.Equal(deltas, []int64{2, 1, -2}) {
		t.Fatalf("schema %d spans %v deltas %v", protoSint(h[5][0]), spans, deltas)
	}
}

// Protobuf is answered only to a scraper that prefers it to every text
// format it accepts, and only while web.self_metrics.native_histograms is
// not false; everyone else gets the text formats as before, without a
// trace of the native buckets.
func TestSelfMetricsNegotiateProtobuf(t *testing.T) {
	server := histogramServer(t, nil, 2*time.Second)
	for accept, want := range map[string]string{
		"":                       expositionContentType,
		prometheusProtobufAccept: protobufContentType,
		"application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited":                                          protobufContentType,
		"application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.5,text/plain":                         expositionContentType,
		"application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.5,text/plain;q=0.5":                   expositionContentType,
		"application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=text":                                               expositionContentType,
		"application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0":                                      expositionContentType,
		"application/openmetrics-text;version=1.0.0":                                                                                          "application/openmetrics-text; version=1.0.0; charset=utf-8",
		"application/openmetrics-text;q=0.9,application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.8": "application/openmetrics-text; version=1.0.0; charset=utf-8",
	} {
		w := selfMetricsAccepting(t, server, accept)
		if got := w.Header().Get("Content-Type"); got != want {
			t.Errorf("Accept %q: Content-Type %q, want %q", accept, got, want)
		}
		if want != protobufContentType && strings.Contains(w.Body.String(), "schema") {
			t.Errorf("Accept %q: %s", accept, w.Body.String())
		}
	}
	off := histogramServer(t, func(cfg *model.Config) { no := false; cfg.Web.SelfMetrics.NativeHistograms = &no }, 2*time.Second)
	if got := selfMetricsAccepting(t, off, prometheusProtobufAccept).Header().Get("Content-Type"); got != "application/openmetrics-text; version=1.0.0; charset=utf-8" {
		t.Fatalf("native_histograms false: %q", got)
	}
}

// With otlp.exponential_histograms the scrape-time histograms are exported
// as OTLP exponential histograms: cumulative, at the scale the recorder
// settled at for their durations, with the zero bucket, the positive
// buckets from their offset, the sum, the count, the least and the greatest
// duration. Without it they are explicit-bucket histograms, as before.
func TestOTLPExportsOwnDurationsAsExponentialHistograms(t *testing.T) {
	for _, exponential := range []bool{true, false} {
		t.Run(strconv.FormatBool(exponential), func(t *testing.T) {
			var raw []byte
			endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				raw, _ = io.ReadAll(r.Body)
			}))
			defer endpoint.Close()
			server := histogramServer(t, func(cfg *model.Config) {
				cfg.OTLP = otlpConfig(endpoint.URL + "/v1/metrics")
				cfg.OTLP.Compression = "none"
				cfg.OTLP.ExponentialHistograms = exponential
			}, 0, 3*time.Millisecond, 5*time.Millisecond, 70*time.Millisecond, 2*time.Second)
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
				if string(metric["name"]) == `"http_exporter_collector_scrape_duration_seconds"` {
					found = metric
				}
			}
			if found == nil {
				t.Fatalf("no histogram in %s", raw)
			}
			if !exponential {
				if found["histogram"] == nil || found["exponentialHistogram"] != nil {
					t.Fatalf("%v", found)
				}
				return
			}
			if found["histogram"] != nil {
				t.Fatalf("both kinds: %v", found)
			}
			var h struct {
				AggregationTemporality int
				DataPoints             []struct {
					Count, ZeroCount string
					Sum, Min, Max    float64
					Scale            int32
					Positive         struct {
						Offset       int32
						BucketCounts []string
					}
					StartTimeUnixNano string
				}
			}
			if err := json.Unmarshal(found["exponentialHistogram"], &h); err != nil {
				t.Fatal(err)
			}
			p := h.DataPoints[0]
			// 3 ms to 2 s fit 160 buckets at scale 4: OpenTelemetry's
			// buckets -135, -123, -62 and 15.
			counts := make([]string, 15+135+1)
			for i := range counts {
				counts[i] = "0"
			}
			for _, index := range []int{-135, -123, -62, 15} {
				counts[index+135] = "1"
			}
			if h.AggregationTemporality != 2 || p.Count != "5" || p.ZeroCount != "1" || p.Scale != 4 || p.Positive.Offset != -135 ||
				!slices.Equal(p.Positive.BucketCounts, counts) || math.Abs(p.Sum-2.078) > 1e-9 || p.Min != 0 || p.Max != 2 || p.StartTimeUnixNano == "" {
				t.Fatalf("%s", found["exponentialHistogram"])
			}
		})
	}
}

// The creation times are in a protobuf answer as in an OpenMetrics one: only
// with web.self_metrics.created_timestamps, as each counter's, histogram's
// and summary's created_timestamp, the time the series began to count.
func TestProtobufCarriesCreationTimesOnlyWithTheSetting(t *testing.T) {
	for _, created := range []bool{false, true} {
		server := histogramServer(t, func(cfg *model.Config) { cfg.Web.SelfMetrics.CreatedTimestamps = created }, time.Second)
		families := protoFamilies(t, selfMetricsAccepting(t, server, prometheusProtobufAccept).Body.Bytes())
		histogram := protoFields(t, protoFields(t, families["http_exporter_collector_scrape_duration_seconds"][4][0].([]byte))[7][0].([]byte))
		counter := protoFields(t, protoFields(t, families["http_exporter_scrapes_total"][4][0].([]byte))[3][0].([]byte))
		for what, fields := range map[string][]any{"histogram": histogram[15], "counter": counter[3]} {
			if (len(fields) == 1) != created {
				t.Fatalf("created_timestamps %v: the %s has %d creation times", created, what, len(fields))
			}
			if created {
				if seconds := protoFields(t, fields[0].([]byte))[1][0].(uint64); seconds != uint64(exporterStart().Unix()) && seconds < uint64(time.Now().Unix())-60 {
					t.Fatalf("the %s began at %d", what, seconds)
				}
			}
		}
	}
}
