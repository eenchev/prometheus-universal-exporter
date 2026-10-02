package exporter

import (
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A histogram or summary read from a target without a _sum or a _count goes
// out without one, in the text format, in OpenMetrics and over OTLP, rather
// than with a sum or a count of 0 the target never wrote (exposition.go,
// model.Histogram).

// incompleteSeries are a histogram of buckets alone, one with a sum and no
// count, a summary of quantiles alone and one of a count alone, as the
// prometheus decoder reads them.
func incompleteSeries() *model.MetricSet {
	buckets := []model.Bucket{{UpperBound: 1, CumulativeCount: 5}, {UpperBound: math.Inf(1), CumulativeCount: 7}}
	return &model.MetricSet{Metrics: []model.Metric{
		{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Buckets: buckets, Count: 7, NoSum: true, NoCount: true}},
		{Name: "i", Type: model.HistogramMetricType, Histogram: &model.Histogram{Buckets: buckets, Sum: 3, Count: 7, NoCount: true}},
		{Name: "s", Type: model.SummaryMetricType, Summary: &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 2}}, NoSum: true, NoCount: true}},
		{Name: "t", Type: model.SummaryMetricType, Summary: &model.Summary{Count: 4, NoSum: true}},
	}}
}

// The text format writes the lines the series have, and the +Inf bucket with
// the histogram's count.
func TestExpositionTextLeavesOutASumOrCountTheSeriesDoesNotHave(t *testing.T) {
	want := "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\n" +
		"# TYPE i histogram\ni_bucket{le=\"1\"} 5\ni_bucket{le=\"+Inf\"} 7\ni_sum 3\n" +
		"# TYPE s summary\ns{quantile=\"0.5\"} 2\n" +
		"# TYPE t summary\nt_count 4\n"
	if got := string(appendMetricSet(nil, incompleteSeries())); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// OpenMetrics writes the same lines. Its histogram has a _sum and a _count
// or neither, so the histogram with a sum and no count, which is no
// OpenMetrics histogram, is written as unknown families of its sample names,
// as a strict parser needs it.
func TestOpenMetricsLeavesOutASumOrCountTheSeriesDoesNotHave(t *testing.T) {
	want := "# TYPE h histogram\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\n" +
		"# TYPE i_bucket unknown\ni_bucket{le=\"1.0\"} 5\ni_bucket{le=\"+Inf\"} 7\n" +
		"# TYPE i_sum unknown\ni_sum 3\n" +
		"# TYPE s summary\ns{quantile=\"0.5\"} 2\n" +
		"# TYPE t summary\nt_count 4\n# EOF\n"
	got := string(appendOpenMetrics(nil, incompleteSeries()))
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	checkOpenMetricsClaims(t, got)
	if text := string(appendMetricSet(nil, incompleteSeries())); !slices.Equal(exposedSeries(strings.ReplaceAll(got, `le="1.0"`, `le="1"`)), exposedSeries(text)) {
		t.Fatalf("OpenMetrics has other series than the text format:\n%s\n%s", got, text)
	}
}

// A histogram or summary written as unknown families, because a name it
// would claim is another family's, has a _sum or _count family only for the
// series that have the line, and a summary without quantiles none of its own
// name.
func TestOpenMetricsUnknownFamiliesHoldOnlyTheLinesTheSeriesHave(t *testing.T) {
	set := incompleteSeries()
	set.Metrics = append(set.Metrics,
		model.Metric{Name: "h", Type: model.HistogramMetricType, Labels: map[string]string{"op": "get"}, Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: math.Inf(1), CumulativeCount: 2}}, Sum: 9, Count: 2}},
		// These three make h, i and t give way.
		model.Metric{Name: "h_created", Type: model.GaugeMetricType, Value: 1},
		model.Metric{Name: "i_count", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"own": "yes"}},
		model.Metric{Name: "t_created", Type: model.GaugeMetricType, Value: 1},
	)
	want := "# TYPE h_bucket unknown\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_bucket{le=\"+Inf\",op=\"get\"} 2\n" +
		"# TYPE h_sum unknown\nh_sum{op=\"get\"} 9\n" +
		"# TYPE h_count unknown\nh_count{op=\"get\"} 2\n" +
		"# TYPE i_bucket unknown\ni_bucket{le=\"1.0\"} 5\ni_bucket{le=\"+Inf\"} 7\n" +
		"# TYPE i_sum unknown\ni_sum 3\n" +
		"# TYPE s summary\ns{quantile=\"0.5\"} 2\n" +
		"# TYPE t_count unknown\nt_count 4\n" +
		"# TYPE h_created gauge\nh_created 1\n" +
		"# TYPE i_count gauge\ni_count{own=\"yes\"} 1\n" +
		"# TYPE t_created gauge\nt_created 1\n# EOF\n"
	got := string(appendOpenMetrics(nil, set))
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	checkOpenMetricsClaims(t, got)
}

// End to end: a target's histogram of buckets alone and its summary of
// quantiles alone pass through a probe unchanged in either format, and one
// with a bucket written twice fails the probe naming the series.
func TestIncompleteHistogramsAndSummariesPassThroughAProbe(t *testing.T) {
	bodies := map[string]string{
		"/alone":     "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\n# TYPE s summary\ns{quantile=\"0.5\"} 2\n",
		"/duplicate": "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"1.0\"} 6\nh_bucket{le=\"+Inf\"} 7\nh_sum 3\nh_count 7\n",
	}
	target := utf8Target(t, "")
	target.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(bodies[r.URL.Path]))
	})
	server := verboseServer(t, false, passthrough("pass", "", ""))
	server.logger = testutil.QuietLogger(t)
	probe := func(path, accept string) (int, string) {
		r := probeOnce(t, server, "/probe?collector=pass&target="+url.QueryEscape(target.URL+path), http.Header{"Accept": {accept}})
		return r.Code, r.Body.String()
	}
	if code, body := probe("/alone", "text/plain"); code != http.StatusOK || body != bodies["/alone"] {
		t.Fatalf("text: %d\n%s", code, body)
	}
	wantOM := "# TYPE h histogram\nh_bucket{le=\"1.0\"} 5\nh_bucket{le=\"+Inf\"} 7\n# TYPE s summary\ns{quantile=\"0.5\"} 2\n# EOF\n"
	if code, body := probe("/alone", "application/openmetrics-text"); code != http.StatusOK || body != wantOM {
		t.Fatalf("OpenMetrics: %d\n%s", code, body)
	}
	want := "the histogram h, which starts in line 2, has two buckets with the upper bound 1"
	if code, body := probe("/duplicate", "text/plain"); code != http.StatusBadGateway || !strings.Contains(body, want) {
		t.Errorf("/duplicate: %d %s, want 502 with %q", code, body, want)
	}
}

// OTLP's histogram may leave its sum out, and does for a histogram read
// without one; its summary cannot, and one read without a sum or a count is
// sent with 0 for it, which is what a field left unset is in OTLP.
func TestOTLPLeavesOutTheSumAHistogramDoesNotHave(t *testing.T) {
	out := roundTripOTLP(t, incompleteSeries().Metrics...)
	if len(out) != 4 || out[0].Histogram == nil || out[1].Histogram == nil || out[2].Summary == nil || out[3].Summary == nil {
		t.Fatalf("%+v", out)
	}
	alone := out[0].Histogram.DataPoints[0]
	if alone.Sum != nil || alone.Count != "7" || len(alone.BucketCounts) != 2 || alone.BucketCounts[0] != "5" || alone.BucketCounts[1] != "2" {
		t.Fatalf("a histogram of buckets alone: %+v", alone)
	}
	if withSum := out[1].Histogram.DataPoints[0]; withSum.Sum == nil || *withSum.Sum != 3 || withSum.Count != "7" {
		t.Fatalf("a histogram with a sum and no count: %+v", withSum)
	}
	if quantiles := out[2].Summary.DataPoints[0]; quantiles.Count != "0" || quantiles.Sum != 0 || len(quantiles.QuantileValues) != 1 {
		t.Fatalf("a summary of quantiles alone: %+v", quantiles)
	}
	if count := out[3].Summary.DataPoints[0]; count.Count != "4" || count.Sum != 0 {
		t.Fatalf("a summary of a count alone: %+v", count)
	}
}
