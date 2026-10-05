//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// oddTarget is what a target may write in the Prometheus text format that
// OpenMetrics does not allow: a negative counter, a histogram with its
// buckets out of order and their counts falling, and a summary with
// quantiles as percentages, beside a histogram that is only out of order.
const oddTarget = `# HELP c_total Requests.
# TYPE c_total counter
c_total -5
# TYPE h histogram
h_bucket{le="1"} 2
h_bucket{le="0.5"} 3
h_bucket{le="+Inf"} 4
h_sum 1.5
h_count 4
# TYPE s summary
s{quantile="99"} 2
s{quantile="50"} 1
s_sum 3
s_count 4
# TYPE ok histogram
ok_bucket{le="+Inf"} 4
ok_bucket{le="1"} 2
ok_bucket{le="0.5"} 1
ok_sum 3
ok_count 4
`

// End to end: a target's values that the text format allows and OpenMetrics
// does not pass through a probe. The decoder does not refuse them; the text
// format answers with them, its buckets and quantiles in order; and the
// OpenMetrics answer, which a strict parser reads, holds the same series
// with the families that are not of their type as unknown, and the one that
// is as a histogram.
func TestOddValuesOfATargetPassThroughAProbeInBothFormats(t *testing.T) {
	target := utf8Target(t, oddTarget)
	server := verboseServer(t, false, passthrough("pass", "", ""))
	server.logger = testutil.QuietLogger(t)
	probe := func(accept string) string {
		t.Helper()
		r := probeOnce(t, server, "/probe?collector=pass&target="+url.QueryEscape(target.URL), http.Header{"Accept": {accept}})
		if r.Code != http.StatusOK {
			t.Fatalf("the probe answered %d: %s", r.Code, r.Body.String())
		}
		return r.Body.String()
	}
	wantText := `# HELP c_total Requests.
# TYPE c_total counter
c_total -5
# TYPE h histogram
h_bucket{le="0.5"} 3
h_bucket{le="1"} 2
h_bucket{le="+Inf"} 4
h_sum 1.5
h_count 4
# TYPE s summary
s{quantile="50"} 1
s{quantile="99"} 2
s_sum 3
s_count 4
# TYPE ok histogram
ok_bucket{le="0.5"} 1
ok_bucket{le="1"} 2
ok_bucket{le="+Inf"} 4
ok_sum 3
ok_count 4
`
	text := probe("text/plain")
	if text != wantText {
		t.Fatalf("the text format:\n%s\nwant:\n%s", text, wantText)
	}
	wantOpenMetrics := `# TYPE c_total unknown
# HELP c_total Requests.
c_total -5
# TYPE h_bucket unknown
h_bucket{le="0.5"} 3
h_bucket{le="1.0"} 2
h_bucket{le="+Inf"} 4
# TYPE h_sum unknown
h_sum 1.5
# TYPE h_count unknown
h_count 4
# TYPE s unknown
s{quantile="50.0"} 1
s{quantile="99.0"} 2
# TYPE s_sum unknown
s_sum 3
# TYPE s_count unknown
s_count 4
# TYPE ok histogram
ok_bucket{le="0.5"} 1
ok_bucket{le="1.0"} 2
ok_bucket{le="+Inf"} 4
ok_sum 3
ok_count 4
# EOF
`
	answer := probe(prometheus3Accept)
	if answer != wantOpenMetrics {
		t.Fatalf("OpenMetrics:\n%s\nwant:\n%s", answer, wantOpenMetrics)
	}
	if err := strictOpenMetricsError(answer); err != nil {
		t.Fatalf("a strict parser refuses the answer: %v", err)
	}
	set := &model.MetricSet{Metrics: []model.Metric{{Name: "c_total", Type: model.CounterMetricType}}}
	if got, want := answerSeries(t, set, answer, true), answerSeries(t, set, text, false); !slices.Equal(got, want) {
		t.Fatalf("OpenMetrics holds\n%s\nand the text format\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// End to end: the same target's values reach the OTLP endpoint, the
// families that are not of their type as gauges of their lines, and the
// histogram that is only out of order as a histogram with its bounds in
// order.
func TestOddValuesOfATargetReachTheOTLPEndpoint(t *testing.T) {
	target := utf8Target(t, oddTarget)
	received := make(chan []byte, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- readOTLPBody(t, r)
	}))
	defer endpoint.Close()
	cfg := &model.Config{Collectors: []model.Collector{passthrough("pass", "", "")}, OTLP: otlpConfig(endpoint.URL + "/v1/metrics")}
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	if r := probeOnce(t, server, "/probe?collector=pass&target="+url.QueryEscape(target.URL), nil); r.Code != http.StatusOK {
		t.Fatalf("the probe answered %d: %s", r.Code, r.Body.String())
	}
	server.exportOTLP(context.Background(), 5*time.Second)

	var payload otlpPayload
	select {
	case body := <-received:
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%v\n%s", err, body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("nothing was exported")
	}
	var probed []otlpMetric
	for _, resource := range payload.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			if err := otlpError(scope.Metrics); err != nil {
				t.Fatalf("the export is not valid OTLP: %v", err)
			}
			for _, m := range scope.Metrics {
				if !strings.HasPrefix(m.Name, "http_exporter_") {
					probed = append(probed, m)
				}
			}
		}
	}
	var histogram *otlpHistogramDataPoint
	for i, m := range probed {
		if m.Name == "ok" && m.Histogram != nil {
			histogram = &m.Histogram.DataPoints[0]
			probed = slices.Delete(probed, i, i+1)
			break
		}
	}
	if histogram == nil || !reflect.DeepEqual(histogram.ExplicitBounds, []otlpDouble{0.5, 1}) || !reflect.DeepEqual(histogram.BucketCounts, []string{"1", "1", "2"}) || *histogram.Sum != 3 {
		t.Fatalf("the histogram that was only out of order arrived as %+v", histogram)
	}
	want := []string{
		`c_total{} -5`,
		`h_bucket{le="+Inf"} 4`, `h_bucket{le="0.5"} 3`, `h_bucket{le="1"} 2`, `h_count{} 4`, `h_sum{} 1.5`,
		`s_count{} 4`, `s_sum{} 3`, `s{quantile="50"} 1`, `s{quantile="99"} 2`,
	}
	if got := otlpGaugeSeries(probed); !slices.Equal(got, want) || slices.ContainsFunc(probed, func(m otlpMetric) bool { return m.Gauge == nil }) {
		t.Fatalf("the families that are not of their type arrived as\n%s\nwant gauges\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
