//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// histogramRuleConfiguration is a configuration of one jq collector whose
// rule counts the durations of a target's requests, in milliseconds, in a
// histogram of seconds per method, beside a gauge of how many there are.
const histogramRuleConfiguration = `collectors:
  - name: app
    request:
      type: http
    decoder:
      type: json
    transform:
      type: jq
    metrics:
      - name: request_duration_seconds
        description: Durations of the requests the target lists.
        type: histogram
        items: .requests[]
        expression: .duration_ms
        scale: 0.001
        labels:
          - name: method
            expression: .method
        histogram:
          buckets: [0.01, 0.1, 1]
      - name: requests
        expression: .requests | length
`

// histogramRuleTarget lists five requests: three GETs, of 5ms, 100ms and
// 2.5s, and two POSTs, of 50ms and 1s, each on or between the bounds.
const histogramRuleTarget = `{"requests": [
  {"method": "GET", "duration_ms": 5},
  {"method": "POST", "duration_ms": 50},
  {"method": "GET", "duration_ms": 100},
  {"method": "POST", "duration_ms": 1000},
  {"method": "GET", "duration_ms": 2500}
]}`

// End to end: a probe answers a histogram rule's histograms as the text
// format writes a histogram — a _bucket line of each bound, cumulative, and
// of +Inf, then _sum and _count, under one # TYPE line of the family — and
// OpenMetrics writes them as its histograms, and the gauge beside them as it
// was.
func TestAProbeAnswersAHistogramRulesHistograms(t *testing.T) {
	target := utf8Target(t, histogramRuleTarget)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", histogramRuleConfiguration))
	if err != nil {
		t.Fatalf("the configuration does not load: %v", err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	probe := "/probe?collector=app&target=" + url.QueryEscape(target.URL)

	const text = "# HELP request_duration_seconds Durations of the requests the target lists.\n" +
		"# TYPE request_duration_seconds histogram\n" +
		"request_duration_seconds_bucket{le=\"0.01\",method=\"GET\"} 1\n" +
		"request_duration_seconds_bucket{le=\"0.1\",method=\"GET\"} 2\n" +
		"request_duration_seconds_bucket{le=\"1\",method=\"GET\"} 2\n" +
		"request_duration_seconds_bucket{le=\"+Inf\",method=\"GET\"} 3\n" +
		"request_duration_seconds_sum{method=\"GET\"} 2.605\n" +
		"request_duration_seconds_count{method=\"GET\"} 3\n" +
		"request_duration_seconds_bucket{le=\"0.01\",method=\"POST\"} 0\n" +
		"request_duration_seconds_bucket{le=\"0.1\",method=\"POST\"} 1\n" +
		"request_duration_seconds_bucket{le=\"1\",method=\"POST\"} 2\n" +
		"request_duration_seconds_bucket{le=\"+Inf\",method=\"POST\"} 2\n" +
		"request_duration_seconds_sum{method=\"POST\"} 1.05\n" +
		"request_duration_seconds_count{method=\"POST\"} 2\n" +
		"# TYPE requests gauge\n" +
		"requests 5\n"
	if answer := probeOnce(t, server, probe, nil); answer.Code != http.StatusOK || answer.Body.String() != text {
		t.Errorf("the probe is answered %d:\n%s\nwant\n%s", answer.Code, answer.Body, text)
	}

	const openMetrics = "# TYPE request_duration_seconds histogram\n" +
		"# HELP request_duration_seconds Durations of the requests the target lists.\n" +
		"request_duration_seconds_bucket{le=\"0.01\",method=\"GET\"} 1\n" +
		"request_duration_seconds_bucket{le=\"0.1\",method=\"GET\"} 2\n" +
		"request_duration_seconds_bucket{le=\"1.0\",method=\"GET\"} 2\n" +
		"request_duration_seconds_bucket{le=\"+Inf\",method=\"GET\"} 3\n" +
		"request_duration_seconds_sum{method=\"GET\"} 2.605\n" +
		"request_duration_seconds_count{method=\"GET\"} 3\n" +
		"request_duration_seconds_bucket{le=\"0.01\",method=\"POST\"} 0\n" +
		"request_duration_seconds_bucket{le=\"0.1\",method=\"POST\"} 1\n" +
		"request_duration_seconds_bucket{le=\"1.0\",method=\"POST\"} 2\n" +
		"request_duration_seconds_bucket{le=\"+Inf\",method=\"POST\"} 2\n" +
		"request_duration_seconds_sum{method=\"POST\"} 1.05\n" +
		"request_duration_seconds_count{method=\"POST\"} 2\n" +
		"# TYPE requests gauge\n" +
		"requests 5\n" +
		"# EOF\n"
	if answer := probeOnce(t, server, probe, http.Header{"Accept": {"application/openmetrics-text; version=1.0.0"}}); answer.Code != http.StatusOK || answer.Body.String() != openMetrics {
		t.Errorf("the OpenMetrics probe is answered %d:\n%s\nwant\n%s", answer.Code, answer.Body, openMetrics)
	}
}

// A histogram rule's histograms reach OTLP as OTLP histograms: a data point
// of each set of labels, its bucket counts each bucket's own, not
// cumulative, as OTLP has them, with the rule's bounds and the count and
// sum of its values.
func TestAHistogramRulesHistogramsAreExportedAsOTLPHistograms(t *testing.T) {
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", histogramRuleConfiguration))
	if err != nil {
		t.Fatalf("the configuration does not load: %v", err)
	}
	c := &cfg.Collectors[0]
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(histogramRuleTarget), Headers: http.Header{"Content-Type": {"application/json"}}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := transform.Transform(context.Background(), d, r, c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	out := otlpMetrics(*set, "1", nil)
	if len(out) != 2 || out[0].Name != "request_duration_seconds" || out[0].Histogram == nil {
		t.Fatalf("exported %+v", out)
	}
	type point struct {
		method, count string
		sum           otlpDouble
		counts        []string
		bounds        []otlpDouble
	}
	var got []point
	for _, p := range out[0].Histogram.DataPoints {
		method := ""
		for _, a := range p.Attributes {
			if a.Key == "method" {
				method = a.Value.StringValue
			}
		}
		sum := otlpDouble(-1)
		if p.Sum != nil {
			sum = *p.Sum
		}
		got = append(got, point{method, p.Count, sum, p.BucketCounts, p.ExplicitBounds})
	}
	want := []point{
		{"GET", "3", 2.605, []string{"1", "1", "0", "1"}, []otlpDouble{0.01, 0.1, 1}},
		{"POST", "2", 1.05, []string{"0", "1", "1", "0"}, []otlpDouble{0.01, 0.1, 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("data points\n%+v\nwant\n%+v", got, want)
	}
}
