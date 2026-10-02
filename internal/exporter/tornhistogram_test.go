//go:build !select_request_types || request_type_http

package exporter

import (
	"math"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A histogram's +Inf bucket and its _count are one number written twice. A
// target that counts an observation in the one and then in the other without
// a lock is read between the two now and then, and writes two numbers that
// differ; and a malformed one writes neither. The decoder refused both kinds,
// and with one such series every family of the scrape: a 502 where main had
// answered 200. Such a histogram is passed on as it was written
// (model.Histogram), in every format the exporter writes.

// tornTarget is a target read between two of its updates: the +Inf bucket of
// its histogram is one behind its _count, beside two healthy families.
const tornTarget = `# TYPE up_thing gauge
up_thing 1
# TYPE h histogram
h_bucket{le="1"} 5
h_bucket{le="+Inf"} 7
h_sum 3.5
h_count 8
# TYPE other_total counter
other_total 3
`

// bareTarget is a target whose histogram has neither a +Inf bucket nor a
// _count, beside a healthy family.
const bareTarget = `# TYPE up_thing gauge
up_thing 1
# TYPE h histogram
h_bucket{le="0.5"} 1
h_bucket{le="1"} 5
h_sum 3.5
`

// probeBothFormats probes a pass-through collector for a target that answers
// with body, and returns the text format's answer and OpenMetrics'.
func probeBothFormats(t *testing.T, body string) (text, openMetrics string) {
	t.Helper()
	target := utf8Target(t, body)
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
	return probe("text/plain"), probe(prometheus3Accept)
}

// End to end: the probe of a target read between two updates answers 200,
// and the text format holds every line as the target wrote it, the +Inf
// bucket with its count and the _count with its own. OpenMetrics, whose
// histogram has one number for the two, holds the same series as unknown
// families under their sample names, which a strict parser reads, and the
// healthy families keep their types. The text answer read by the exporter's
// own reader gives the series the target's body gives.
func TestAHistogramReadBetweenTwoUpdatesPassesThroughAProbe(t *testing.T) {
	text, openMetrics := probeBothFormats(t, tornTarget)
	if text != tornTarget {
		t.Fatalf("the text format:\n%s\nwant what the target wrote:\n%s", text, tornTarget)
	}
	want := `# TYPE up_thing gauge
up_thing 1
# TYPE h_bucket unknown
h_bucket{le="1.0"} 5
h_bucket{le="+Inf"} 7
# TYPE h_sum unknown
h_sum 3.5
# TYPE h_count unknown
h_count 8
# TYPE other counter
other_total 3
# EOF
`
	if openMetrics != want {
		t.Fatalf("OpenMetrics:\n%s\nwant:\n%s", openMetrics, want)
	}
	if err := strictOpenMetricsError(openMetrics); err != nil {
		t.Fatalf("a strict parser refuses the answer: %v", err)
	}
	read, written := decodedAnswer(t, text, false), decodedAnswer(t, tornTarget, false)
	if !reflect.DeepEqual(read, written) || len(read) != 3 {
		t.Fatalf("the text answer reads back as\n%+v\nand the target's body as\n%+v", read, written)
	}
	if h := read[1].Histogram; h == nil || h.Count != 8 || len(h.Buckets) != 2 || h.Buckets[1].CumulativeCount != 7 {
		t.Fatalf("the histogram reads back as %+v", h)
	}
	// The OpenMetrics answer is one the exporter reads too: the same samples,
	// the histogram's as series of no type.
	var names []string
	for _, m := range decodedAnswer(t, openMetrics, true) {
		names = append(names, string(m.Type)+" "+m.Name)
	}
	if want := []string{"gauge up_thing", "untyped h_bucket", "untyped h_bucket", "untyped h_sum", "untyped h_count", "counter other_total"}; !slices.Equal(names, want) {
		t.Fatalf("the exporter's own reader found %q in the OpenMetrics answer, want %q", names, want)
	}
}

// End to end: a histogram with neither a +Inf bucket nor a _count is passed
// on with the lines it has, and is given neither; in OpenMetrics, where a
// histogram must have a +Inf bucket, as unknown families.
func TestAHistogramWithNeitherAnInfBucketNorACountPassesThroughAProbe(t *testing.T) {
	text, openMetrics := probeBothFormats(t, bareTarget)
	if text != bareTarget {
		t.Fatalf("the text format:\n%s\nwant what the target wrote:\n%s", text, bareTarget)
	}
	want := `# TYPE up_thing gauge
up_thing 1
# TYPE h_bucket unknown
h_bucket{le="0.5"} 1
h_bucket{le="1.0"} 5
# TYPE h_sum unknown
h_sum 3.5
# EOF
`
	if openMetrics != want {
		t.Fatalf("OpenMetrics:\n%s\nwant:\n%s", openMetrics, want)
	}
	if err := strictOpenMetricsError(openMetrics); err != nil {
		t.Fatalf("a strict parser refuses the answer: %v", err)
	}
	if read, written := decodedAnswer(t, text, false), decodedAnswer(t, bareTarget, false); !reflect.DeepEqual(read, written) || len(read) != 2 {
		t.Fatalf("the text answer reads back as\n%+v\nand the target's body as\n%+v", read, written)
	}
}

// Every histogram the decoder can have read, the ones whose +Inf bucket and
// _count differ and the ones with neither among them, is written in the text
// format so that the exporter's own reader makes the same answer of it: what
// one exporter passes on, the next one scraping it passes on unchanged.
func TestTheTextAnswerOfAnyHistogramReadsBackToItself(t *testing.T) {
	read, torn, bare := 0, 0, 0
	for _, c := range oddCases(0) {
		m := c.set.Metrics[0]
		// The reader reads a count as a float, and so not the ones near
		// 2^64, which are left out here.
		if !c.single || m.Histogram == nil || hasLabel(m, "le") || strings.Contains(c.name, " huge ") {
			continue
		}
		text := string(appendMetricSet(nil, c.set))
		again := string(appendMetricSet(nil, &model.MetricSet{Metrics: decodedAnswer(t, text, false)}))
		if again != text {
			t.Fatalf("%s: the answer\n%s\nreads back as\n%s", c.name, text, again)
		}
		read++
		switch inf, has := m.Histogram.InfBucket(); {
		case !has:
			bare++
		case !m.Histogram.NoCount && inf != m.Histogram.Count:
			torn++
			if !strings.Contains(text, "h_bucket{") || !strings.Contains(text, "h_count") {
				t.Fatalf("%s: the answer lacks the +Inf bucket or the count:\n%s", c.name, text)
			}
		}
	}
	if read < 500 || torn < 40 || bare < 50 {
		t.Fatalf("%d histograms read back, %d of them with a +Inf bucket and a count that differ and %d with neither: the cases do not cover them", read, torn, bare)
	}
}

// Over OTLP such a histogram is gauges under its sample names, as every
// family is whose values its type does not allow: a histogram point has one
// count, and would have to drop one of the two numbers, or invent one.
func TestAHistogramWhoseInfBucketAndCountDifferIsGaugesOverOTLP(t *testing.T) {
	buckets := []model.Bucket{{UpperBound: 1, CumulativeCount: 5}, {UpperBound: math.Inf(1), CumulativeCount: 7}}
	for name, tc := range map[string]struct {
		h    model.Histogram
		want []string
	}{
		"a +Inf bucket behind the count": {
			h:    model.Histogram{Buckets: buckets, Sum: 3.5, Count: 8},
			want: []string{`h_bucket{le="+Inf"} 7`, `h_bucket{le="1"} 5`, `h_count{} 8`, `h_sum{} 3.5`},
		},
		"neither": {
			h:    model.Histogram{Buckets: buckets[:1], Sum: 3.5, NoCount: true},
			want: []string{`h_bucket{le="1"} 5`, `h_sum{} 3.5`},
		},
	} {
		out := roundTripOTLP(t, model.Metric{Name: "h", Type: model.HistogramMetricType, Histogram: &tc.h})
		if err := otlpError(out); err != nil {
			t.Fatalf("%s: the export is not valid OTLP: %v", name, err)
		}
		if got := otlpGaugeSeries(out); !slices.Equal(got, tc.want) || slices.ContainsFunc(out, func(m otlpMetric) bool { return m.Gauge == nil }) {
			t.Errorf("%s: exported as\n%s\nwant gauges\n%s", name, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
	}
	// One whose two numbers agree, or that has one of them, stays a
	// histogram, with that count.
	for name, h := range map[string]model.Histogram{
		"both, the same":   {Buckets: buckets, Sum: 3.5, Count: 7},
		"the bucket alone": {Buckets: buckets, Count: 7, NoSum: true, NoCount: true},
		"the count alone":  {Buckets: buckets[:1], Sum: 3.5, Count: 7},
	} {
		out := roundTripOTLP(t, model.Metric{Name: "h", Type: model.HistogramMetricType, Histogram: &h})
		if len(out) != 1 || out[0].Histogram == nil || out[0].Histogram.DataPoints[0].Count != "7" || !slices.Equal(out[0].Histogram.DataPoints[0].BucketCounts, []string{"5", "2"}) {
			t.Errorf("%s: exported as %+v", name, out)
		}
	}
}
