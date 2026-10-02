package decode

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A histogram or a summary is kept as the target wrote it: one without a
// _sum or a _count is marked as having none rather than given a sum or a
// count of 0, a histogram's +Inf bucket and _count are each kept as written,
// and what cannot be one series fails the decode naming it (promparse.go,
// model.Histogram.Settle).

// A histogram without a _count has its +Inf bucket's count, which counts
// every observation, and is marked as written without one; without a _sum it
// is marked so too. OpenMetrics allows a histogram of buckets alone.
func TestPromParseHistogramWithoutCountTakesItFromTheInfBucket(t *testing.T) {
	buckets := []model.Bucket{{UpperBound: 1, CumulativeCount: 5}, {UpperBound: math.Inf(1), CumulativeCount: 7}}
	for name, tc := range map[string]struct {
		body string
		om   bool
		want model.Histogram
	}{
		"buckets alone, OpenMetrics": {
			body: "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\n# EOF\n", om: true,
			want: model.Histogram{Buckets: buckets, Count: 7, NoSum: true, NoCount: true},
		},
		"no _count": {
			body: "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3\n",
			want: model.Histogram{Buckets: buckets, Sum: 3, Count: 7, NoCount: true},
		},
		"no _sum": {
			body: "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_count 7\n",
			want: model.Histogram{Buckets: buckets, Count: 7, NoSum: true},
		},
		"both": {
			body: "# TYPE h histogram\nh_sum 3\nh_count 7\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\n",
			want: model.Histogram{Buckets: buckets, Sum: 3, Count: 7},
		},
		"a _count and no +Inf bucket": {
			body: "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_sum 3\nh_count 7\n",
			want: model.Histogram{Buckets: buckets[:1], Sum: 3, Count: 7},
		},
	} {
		t.Run(name, func(t *testing.T) {
			metrics, err := parseExposition([]byte(tc.body), promOptions{openMetrics: tc.om})
			if err != nil || len(metrics) != 1 || metrics[0].Histogram == nil {
				t.Fatalf("%v %#v", err, metrics)
			}
			if got := *metrics[0].Histogram; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("histogram %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A summary has only what the target wrote of it: quantiles alone, a count
// alone, or a sum of 0 that the target did write.
func TestPromParseSummaryKeepsOnlyWhatTheTargetWrote(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want model.Summary
	}{
		"quantiles alone": {
			body: "# TYPE s summary\ns{quantile=\"0.5\"} 2\n",
			want: model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: 2}}, NoSum: true, NoCount: true},
		},
		"a count alone": {
			body: "# TYPE s summary\ns_count 4\n",
			want: model.Summary{Count: 4, NoSum: true},
		},
		"a sum of 0 and a count of 0": {
			body: "# TYPE s summary\ns_sum 0\ns_count 0\n",
			want: model.Summary{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			metrics := parseOK(t, tc.body)
			if len(metrics) != 1 || metrics[0].Summary == nil {
				t.Fatalf("%#v", metrics)
			}
			if got := *metrics[0].Summary; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("summary %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A target that counts an observation in its buckets and in its _count
// without a lock is read between the two now and then, its +Inf bucket one
// behind its _count or ahead of it. The decode refused such a histogram, and
// with it every family of the scrape, which main had answered. It is read as
// it was written: the +Inf bucket keeps its count and the _count its own,
// beside the families around it.
func TestPromParseKeepsAnInfBucketAndACountThatDiffer(t *testing.T) {
	inf := math.Inf(1)
	for name, tc := range map[string]struct {
		body string
		want model.Histogram
	}{
		"the +Inf bucket behind the count": {
			body: "# TYPE up gauge\nup 1\n# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 7\nh_sum 3.5\nh_count 8\n# TYPE other gauge\nother 2\n",
			want: model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 5}, {UpperBound: inf, CumulativeCount: 7}}, Sum: 3.5, Count: 8},
		},
		"the count behind the +Inf bucket, and written first": {
			body: "# TYPE up gauge\nup 1\n# TYPE h histogram\nh_count 6\nh_bucket{le=\"+Inf\"} 7\nh_bucket{le=\"1\"} 5\n# TYPE other gauge\nother 2\n",
			want: model.Histogram{Buckets: []model.Bucket{{UpperBound: inf, CumulativeCount: 7}, {UpperBound: 1, CumulativeCount: 5}}, Count: 6, NoSum: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, openMetrics := range []bool{false, true} {
				metrics, err := parseExposition([]byte(tc.body), promOptions{openMetrics: openMetrics})
				if err != nil || len(metrics) != 3 || metrics[0].Name != "up" || metrics[2].Name != "other" || metrics[1].Histogram == nil {
					t.Fatalf("OpenMetrics %v: %v %#v", openMetrics, err, metrics)
				}
				if got := *metrics[1].Histogram; !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("OpenMetrics %v: histogram %+v, want %+v", openMetrics, got, tc.want)
				}
			}
		})
	}
}

// A histogram with neither a +Inf bucket nor a _count failed the decode
// too, and every family of the scrape with it. It is read with the buckets
// and the _sum it has, marked as written without a _count and holding no
// +Inf bucket, so that it is passed on without either.
func TestPromParseKeepsAHistogramWithNeitherAnInfBucketNorACount(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want model.Histogram
	}{
		"buckets and a sum": {
			body: "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_sum 3\n# TYPE g gauge\ng 1\n",
			want: model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 5}}, Sum: 3, NoCount: true},
		},
		"a sum alone": {
			body: "# TYPE h histogram\nh_sum 3\n# TYPE g gauge\ng 1\n",
			want: model.Histogram{Sum: 3, NoCount: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			metrics := parseOK(t, tc.body)
			if len(metrics) != 2 || metrics[0].Histogram == nil || metrics[1].Name != "g" {
				t.Fatalf("%#v", metrics)
			}
			if got := *metrics[0].Histogram; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("histogram %+v, want %+v", got, tc.want)
			}
			if _, has := metrics[0].Histogram.InfBucket(); has {
				t.Fatalf("the histogram has a +Inf bucket: %+v", metrics[0].Histogram)
			}
		})
	}
}

// What cannot be one series fails the decode, as two samples of a plain
// series fail the scrape, and the error names the series: two buckets with
// one bound however it is written, two values of one quantile, and a second
// _sum or _count.
func TestPromParseRefusesAHistogramOrSummaryThatIsNotOneSeries(t *testing.T) {
	for _, test := range []struct{ body, want string }{
		{
			"# TYPE h histogram\ng 1\nh_bucket{op=\"get\",le=\"1\"} 5\nh_bucket{le=\"1.0\",op=\"get\"} 6\nh_bucket{op=\"get\",le=\"+Inf\"} 7\n",
			`text format parsing error: the histogram h{op="get"}, which starts in line 3, has two buckets with the upper bound 1`,
		},
		{
			"# TYPE h histogram\nh_bucket{le=\"+Inf\"} 7\nh_bucket{le=\"Inf\"} 7\n",
			"the histogram h, which starts in line 2, has two buckets with the upper bound +Inf",
		},
		{
			"# TYPE h histogram\nh_bucket{le=\"NaN\"} 1\nh_bucket{le=\"nan\"} 2\nh_bucket{le=\"+Inf\"} 7\n",
			"the histogram h, which starts in line 2, has two buckets with the upper bound NaN",
		},
		{
			"# TYPE s summary\ns{quantile=\"0.5\"} 2\ns{quantile=\"0.50\"} 3\n",
			"text format parsing error: the summary s, which starts in line 2, has two values for the quantile 0.5",
		},
		{
			"# TYPE s summary\ns_sum{a=\"1\"} 1\ns_sum{a=\"2\"} 1\ns_sum{a=\"1\"} 2\n",
			`text format parsing error in line 4: second s_sum sample for the summary s{a="1"}`,
		},
		{
			"# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\nh_count 1\nh_count 1\n",
			"text format parsing error in line 4: second h_count sample for the histogram h",
		},
	} {
		_, err := parsePrometheusText([]byte(test.body))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: err=%v, want it to contain %q", test.body, err, test.want)
		}
	}
}

// A sample that belongs to a histogram or a summary family by its name and
// is none of the samples such a family has — one named as the histogram
// itself, a bucket without an le label, one named as the summary without a
// quantile label — has a value with no place in the series. It was read and
// left out without a word, so a target's "h 5" under "# TYPE h histogram"
// vanished. It is a malformed line, and fails the decode as one does: the
// error names the line and says which samples the family has. That holds in
// a family the collector's rules do not keep, whose lines are checked as any
// are, and beside valid samples of the same series.
func TestPromParseRefusesASampleThatIsNoPartOfItsHistogramOrSummary(t *testing.T) {
	for _, test := range []struct{ body, want string }{
		{
			"# TYPE h histogram\nh 5\n",
			"text format parsing error in line 2: expected h_bucket with an le label, h_sum or h_count as a sample of the histogram h, got h",
		},
		{
			"g 1\n# TYPE h histogram\nh{a=\"1\"} 5\nother 1\n",
			"text format parsing error in line 3: expected h_bucket with an le label, h_sum or h_count as a sample of the histogram h, got h",
		},
		{
			"# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_count 2\nh 5\n",
			"text format parsing error in line 4: expected h_bucket with an le label, h_sum or h_count as a sample of the histogram h, got h",
		},
		{
			"# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_bucket{op=\"get\"} 2\n",
			"text format parsing error in line 3: expected h_bucket with an le label, h_sum or h_count as a sample of the histogram h, got h_bucket without an le label",
		},
		{
			"# TYPE \"my.h\" histogram\n{\"my.h\"} 5\n",
			"text format parsing error in line 2: expected my.h_bucket with an le label, my.h_sum or my.h_count as a sample of the histogram my.h, got my.h",
		},
		{
			"# TYPE s summary\ns 5\n",
			"text format parsing error in line 2: expected s with a quantile label, s_sum or s_count as a sample of the summary s, got s without a quantile label",
		},
		{
			"# TYPE s summary\ns{quantile=\"0.5\"} 1\ns_count 2\ns{op=\"get\"} 5\n",
			"text format parsing error in line 4: expected s with a quantile label, s_sum or s_count as a sample of the summary s, got s without a quantile label",
		},
	} {
		for _, options := range []promOptions{{}, {openMetrics: true}, {keep: func(name string) bool { return name == "g" }}} {
			_, err := parseExposition([]byte(test.body), options)
			if err == nil || err.Error() != test.want {
				t.Errorf("%q (OpenMetrics %v, filtered %v): err=%v, want %q", test.body, options.openMetrics, options.keep != nil, err, test.want)
			}
		}
	}
}

// What the formats let a family have beside its buckets, quantiles, _sum and
// _count is still read: OpenMetrics' _created, which is read and dropped, a
// summary's sample with a quantile label, and a sample whose name only
// starts as the family's, which is a family of its own.
func TestPromParseStillReadsWhatAHistogramOrSummaryFamilyMayHave(t *testing.T) {
	metrics, err := parseExposition([]byte("# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_count 2\nh_sum 1\nh_created 5\n# TYPE s summary\ns_created 5\ns{quantile=\"0.5\"} 1\n# EOF\n"), promOptions{openMetrics: true})
	if err != nil || len(metrics) != 2 || metrics[0].Histogram == nil || metrics[0].Histogram.Count != 2 || metrics[1].Summary == nil || len(metrics[1].Summary.Quantiles) != 1 {
		t.Fatalf("%v %#v", err, metrics)
	}
	metrics = parseOK(t, "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_total 5\nh_created 6\n")
	if len(metrics) != 3 || metrics[1].Name != "h_total" || metrics[1].Type != model.UntypedMetricType || metrics[1].Value != 5 || metrics[2].Name != "h_created" || metrics[2].Value != 6 {
		t.Fatalf("%#v", metrics)
	}
}

// A series that the collector's rules do not keep is not held, and so is not
// checked as a series either: only its samples are, line by line.
func TestPromParseDoesNotSettleSeriesItDoesNotKeep(t *testing.T) {
	body := "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"1.0\"} 5\n# TYPE g gauge\ng 1\n"
	metrics, err := parseExposition([]byte(body), promOptions{keep: func(name string) bool { return name == "g" }})
	if err != nil || len(metrics) != 1 || metrics[0].Name != "g" {
		t.Fatalf("%v %#v", err, metrics)
	}
}

// A count is a whole number of observations however it is written: 7.0 and
// 1e3 are, and are read as 7 and 1000. A fraction is no count, and fails the
// decode (promparse_test.go) rather than losing its fraction on the way.
func TestPromParseReadsAWholeCountInAnySpelling(t *testing.T) {
	metrics, err := parsePrometheusText([]byte("# TYPE h histogram\nh_bucket{le=\"1\"} 7.0\nh_bucket{le=\"+Inf\"} 1e3\nh_count 1000.0\nh_sum 2.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	h := metrics[0].Histogram
	if h == nil || h.Count != 1000 || len(h.Buckets) != 2 || h.Buckets[0].CumulativeCount != 7 || h.Buckets[1].CumulativeCount != 1000 || h.Sum != 2.5 {
		t.Fatalf("%+v", h)
	}
}
