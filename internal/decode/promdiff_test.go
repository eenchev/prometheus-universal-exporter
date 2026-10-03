package decode

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The parser of Prometheus and OpenMetrics text was made cheaper
// (promparse.go) without a change to what it accepts, refuses or returns.
// These tests compare it with the parser it was (promoracle_test.go): on the
// same body, read the same way, the two return the same series in the same
// order, or the same error, word for word.

// sameFloat reports whether two floats are the same bit for bit, so that a
// NaN is itself and -0 is not 0.
func sameFloat(a, b float64) bool { return math.Float64bits(a) == math.Float64bits(b) }

// sameMetrics says where two parses differ, or nothing when they do not.
func sameMetrics(old, got []model.Metric) string {
	if len(old) != len(got) || (old == nil) != (got == nil) {
		return fmt.Sprintf("%d series (nil: %v), were %d (nil: %v)", len(got), got == nil, len(old), old == nil)
	}
	for i := range old {
		a, b := old[i], got[i]
		where := fmt.Sprintf("series %d, %s%v", i, a.Name, a.Labels)
		switch {
		case a.Name != b.Name || a.Help != b.Help || a.Type != b.Type:
			return fmt.Sprintf("%s: is %s %q (%q), was %s %q (%q)", where, b.Type, b.Name, b.Help, a.Type, a.Name, a.Help)
		case !reflect.DeepEqual(a.Labels, b.Labels):
			// A series without labels has an empty map, not none.
			return fmt.Sprintf("%s: labels %#v, were %#v", where, b.Labels, a.Labels)
		case !sameFloat(a.Value, b.Value):
			return fmt.Sprintf("%s: value %v, was %v", where, b.Value, a.Value)
		case (a.Timestamp == nil) != (b.Timestamp == nil) || a.Timestamp != nil && *a.Timestamp != *b.Timestamp:
			return fmt.Sprintf("%s: timestamp %v, was %v", where, describeTime(b.Timestamp), describeTime(a.Timestamp))
		case (a.Histogram == nil) != (b.Histogram == nil) || (a.Summary == nil) != (b.Summary == nil):
			return fmt.Sprintf("%s: histogram %v, summary %v, were %v and %v", where, b.Histogram, b.Summary, a.Histogram, a.Summary)
		}
		if h, g := a.Histogram, b.Histogram; h != nil {
			if len(h.Buckets) != len(g.Buckets) || !sameFloat(h.Sum, g.Sum) || h.Count != g.Count || h.NoSum != g.NoSum || h.NoCount != g.NoCount {
				return fmt.Sprintf("%s: histogram %+v, was %+v", where, *g, *h)
			}
			for j := range h.Buckets {
				if !sameFloat(h.Buckets[j].UpperBound, g.Buckets[j].UpperBound) || h.Buckets[j].CumulativeCount != g.Buckets[j].CumulativeCount {
					return fmt.Sprintf("%s: bucket %d is %+v, was %+v", where, j, g.Buckets[j], h.Buckets[j])
				}
			}
		}
		if s, g := a.Summary, b.Summary; s != nil {
			if len(s.Quantiles) != len(g.Quantiles) || !sameFloat(s.Sum, g.Sum) || s.Count != g.Count || s.NoSum != g.NoSum || s.NoCount != g.NoCount {
				return fmt.Sprintf("%s: summary %+v, was %+v", where, *g, *s)
			}
			for j := range s.Quantiles {
				if !sameFloat(s.Quantiles[j].Quantile, g.Quantiles[j].Quantile) || !sameFloat(s.Quantiles[j].Value, g.Quantiles[j].Value) {
					return fmt.Sprintf("%s: quantile %d is %+v, was %+v", where, j, g.Quantiles[j], s.Quantiles[j])
				}
			}
		}
	}
	return ""
}

func describeTime(at *int64) string {
	if at == nil {
		return "none"
	}
	return strconv.FormatInt(*at, 10)
}

// promReading is a way to read an exposition: as OpenMetrics or not, keeping
// every family or those whose names have an a in them, and with a limit on
// the series kept or without one.
type promReading struct {
	openMetrics bool
	filtered    bool
	limit       int
}

func (r promReading) String() string {
	return fmt.Sprintf("openMetrics=%v filtered=%v limit=%d", r.openMetrics, r.filtered, r.limit)
}

// options are the reading as the parser's options. asked collects the
// names the filter is asked about, in order: the parser asks once for each
// family, at its first sample.
func (r promReading) options(asked *[]string) promOptions {
	options := promOptions{openMetrics: r.openMetrics, limit: r.limit}
	if r.filtered {
		options.keep = func(name string) bool {
			*asked = append(*asked, name)
			return strings.Contains(name, "a")
		}
	}
	return options
}

// compareExposition parses body with the parser and with the oracle, read
// one way, and reports any difference. It says whether the body was
// accepted.
func compareExposition(t *testing.T, body []byte, reading promReading) bool {
	t.Helper()
	var askedOld, asked []string
	old, oldErr := oracleParseExposition(bytes.Clone(body), reading.options(&askedOld))
	// The parser is given a copy it could spoil, to show that it does not.
	given := bytes.Clone(body)
	got, err := parseExposition(given, reading.options(&asked))
	switch {
	case !bytes.Equal(given, body):
		t.Errorf("%q (%s): the parser changed the body it read", clip(body), reading)
	case (err == nil) != (oldErr == nil) || err != nil && err.Error() != oldErr.Error():
		t.Errorf("%q (%s): err=%v, was %v", clip(body), reading, err, oldErr)
	case errors.Is(err, model.ErrLimitExceeded) != errors.Is(oldErr, model.ErrLimitExceeded):
		t.Errorf("%q (%s): err=%#v, was %#v", clip(body), reading, err, oldErr)
	case !reflect.DeepEqual(asked, askedOld):
		t.Errorf("%q (%s): the filter was asked about %q, was about %q", clip(body), reading, asked, askedOld)
	default:
		if diff := sameMetrics(old, got); diff != "" {
			t.Errorf("%q (%s): %s", clip(body), reading, diff)
		}
	}
	return err == nil
}

// promReadings are the ways every body of the table is read.
var promReadings = []promReading{{}, {openMetrics: true}, {filtered: true}, {openMetrics: true, filtered: true}, {limit: 1}, {limit: 3}, {openMetrics: true, limit: 2}, {filtered: true, limit: 2}}

// Expositions that are valid, odd but accepted, and invalid, each read
// every way: the parser and the one it was agree on all of them.
func TestPromParserAgreesWithTheOneItWasOnEdgeCases(t *testing.T) {
	many := func(n int, format string) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, format, i)
		}
		return b.String()
	}
	cases := []string{
		// Nothing, blank lines and lines of other white space.
		"", "\n", "\n\n\n", " ", " \t ", "\r\n", "\r", "\r\r\n", "\f", "\v\n", " \n", "   \n", "a 1\n\f\nb 2\n", "\x00\n", "\x85\n", "\xa0\n", "\xc2\x85\n", "　", "a 1\n  x 2\n",
		// Samples: values, timestamps, blanks, line ends.
		"a 1", "a 1\n", "a 1\r\n", "a 1\r", "a 1\r\r\n", "a\t1\t\n", "  a   1   ", "a 1 2", "a 1 -2", "a 1 2 ", "a 1 2 3", "a 1 2 ", "a 1 2   ", "a 1 2\f", "a 1 1.5", "a 1 1e3", "a 1 9223372036854775807", "a 1 9223372036854775808", "a 1 +5", "a 1 0x10", "a 1 1_0",
		"a NaN", "a nan", "a +Inf", "a -Inf", "a Inf", "a inf", "a infinity", "a 1e3", "a 1E-3", "a .5", "a 5.", "a -0", "a +1", "a 0x1p3", "a 1_000", "a 0x10", "a 1e400", "a 1e-400", "a", "a ", "a x", "a 1x", "a 1,5", "a \"1\"", "a P", "a 1p", "a _",
		"a 1\na 2\n", "a{x=\"1\"} 1\na{x=\"1\"} 2\n", "a 1\nb 2\na{x=\"1\"} 3\n", "b 2\na 1\nb{x=\"1\"} 3\na{y=\"2\"} 4\n",
		// Names.
		"a:b 1", ":a 1", "a:b:c{x=\"1\"} 1", "_ 1", "__a__ 1", "0a 1", "a-b 1", "a.b 1", "é 1", "aé 1", "a\xff 1", "= 1", "{} 1", "{}", "{", "}", "{a} 1", "{a,} 1", "{a=\"1\"} 1", "{a,b} 1", "{a,b=\"1\"} 1", "{b=\"1\",a} 1", "{b=\"1\",a,} 1", "{b=\"1\",a,c=\"2\"} 1", "{b=\"c\",} 1",
		`{"my.metric"} 1`, `{"my.metric",x="1"} 1`, `{x="1","my.metric"} 1`, `"my.metric" 1`, `"my.metric"{x="1"} 1`, `{"a\"b\\c\nd"} 1`, `{"a\tb"} 1`, `{""} 1`, `{"",x="1"} 1`, `{"a","b"} 1`, `{"a"b} 1`, `a"b" 1`, `{"my.metric" x="1"} 1`, `{"é.ü",ä="ö"} 1`, "{\"a\xffb\"} 1", `{"a} 1`, `{"a\"} 1`, `{"a\x"} 1`, `"a 1`, `{"a b"} 1`, `{"a_bucket"} 1`,
		// Labels: spacing, escapes, what is not UTF-8, names written twice.
		`a{} 1`, `a {} 1`, `a{ } 1`, `a{x="1"} 1`, `a{x="1",} 1`, `a{x="1",y="2"} 1`, `a{ x = "1" , y = "2" , } 1`, "a{\tx\t=\t\"1\"\t}\t1", `a{x="1"}1`, `a{x="1"}`, `a{x="1"} `, `a{x="1",,y="2"} 1`, `a{,x="1"} 1`, `a{,} 1`, `a{x} 1`, `a{x=} 1`, `a{x=1} 1`, `a{x="1} 1`, `a{x="1"`, `a{x="1",`, `a{x="1" y="2"} 1`, `a{x="1"y="2"} 1`, `a{x="1";y="2"} 1`, `a{x:y="1"} 1`, `a{0x="1"} 1`, `a{="1"} 1`, `a{x=="1"} 1`, `a{x='1'} 1`,
		`a{x="a\\b\"c\nd"} 1`, `a{x="\\"} 1`, `a{x="\"} 1`, `a{x="\t"} 1`, `a{x="\x41"} 1`, `a{x="\`, `a{x="a\`, `a{x="\\\\\\"} 1`, `a{x="{}"} 1`, `a{x="a,b=\"c\""} 1`, `a{x="é",y="日本"} 1`, "a{x=\"\xff\"} 1", "a{x=\"\xff\",y=\"a\xffb\"} 1", "a{x\xff=\"1\"} 1", "a{x=\"1\"} 1 # not an exemplar here", `a{x=""} 1`, `a{x=" "} 1`,
		`a{x="1",x="2"} 1`, `a{x="1",y="2",x="1"} 1`, `a{__name__="b"} 1`, `a{__name__="b",x="1"} 1`, `a{"x"="1"} 1`, `a{"x.y"="1","z z"="2"} 1`, `a{"__name__"="b"} 1`, `a{"x"="1",x="2"} 1`, `a{""="1"} 1`, "a{\"x\xff\"=\"1\"} 1", `a{"x\"y"="1","x\\y"="2","x\ny"="3"} 1`, `a{le="1"} 1`, `a{quantile="0.5",le="x"} 1`,
		"a{" + many(15, `l%d="v",`) + "} 1", "a{" + many(16, `l%d="v",`) + "} 1", "a{" + many(17, `l%d="v",`) + "} 1", "a{" + many(40, `l%d="v",`) + "} 1", "a{" + many(16, `l%d="v",`) + `l3="w"} 1`, "a{" + many(17, `l%d="v",`) + `l16="w"} 1`, "a{" + many(40, `l%d="v",`) + `l0="w"} 1`, "a{" + many(40, `l%d="v",`) + `l39="w"} 1`, "a{" + many(30, `l%d="v",`) + `l30="w"} 1` + "\na{x=\"1\"} 2\n",
		// HELP and TYPE.
		"# HELP a Some help.\n# TYPE a gauge\na 1\n", "# TYPE a gauge\n# HELP a Some help.\na 1\n", "# HELP a\n# TYPE a\na 1\n", "# HELP\n# TYPE\n", "# HELP a  two  spaces \n# TYPE a counter \na 1\n", "#HELP a x\n#TYPE a gauge\na 1\n", "#\tHELP\ta\tx\n#\tTYPE\ta\tgauge\na 1\n",
		"# HELP a x\n# HELP a y\n", "# TYPE a gauge\n# TYPE a gauge\n", "a 1\n# TYPE a gauge\n", "a 1\n# HELP a late\na 2\n", "# TYPE a gauge\n", "# HELP a only help\n", "# TYPE a GAUGE\na 1\n", "# TYPE a Counter \na 1\n", "# TYPE a bogus\n", "# TYPE a unknown\na 1\n", "# TYPE a info\na 1\n",
		`# HELP a back\\slash new\nline quote\" bare" end` + "\n" + `a 1`, `# HELP a bad \x escape` + "\na 1", `# HELP a lone \`, "# HELP a\xff x\n", "# HELP a \xff\na 1\n", "# HELP é x\n", "# HELP a:b x\na:b 1\n", "# HELP 0a x\n", "# HELP a{ x\n", `# HELP "my.metric" Quoted.` + "\n" + `# TYPE "my.metric" gauge` + "\n" + `{"my.metric"} 1`, `# HELP "a\"b" x` + "\n" + `{"a\"b"} 1`, `# HELP "" x`, `# HELP "a x`, `# TYPE "a"gauge`,
		"# a comment\na 1\n# another\n", "#\n", "# \n", "#a 1\n", "# HELPa x\n", "# UNIT a seconds\na 1\n", "# EOF\n", "# EOF", "a 1\n# EOF\n", "a 1\n# EOF\nb 2\n", "a 1\n# EOF\n\n \n", "a 1\n# EOF \n", "a 1\n# EOF x\nb 2\n", "a 1\n#EOF\nb 2\n", "# EOF\n# EOF\n", "# EOF\n# x\n", "a 1\n# EOF\n \n",
		// Families declared in one order and written in another.
		"# TYPE a gauge\n# TYPE b gauge\nb 1\na 2\n", "# TYPE a gauge\n# TYPE b gauge\nb 1\na 2\nb{x=\"1\"} 3\na{x=\"1\"} 4\n", "c 1\n# TYPE a gauge\n# TYPE b gauge\nb 1\nc{x=\"1\"} 5\na 2\n", "# TYPE h histogram\n# TYPE g gauge\ng 1\nh_bucket{le=\"+Inf\"} 2\ng{x=\"1\"} 3\nh_bucket{x=\"1\",le=\"+Inf\"} 4\nh_count 2\n",
		// Summaries.
		"# TYPE s summary\ns{quantile=\"0.5\"} 1\ns{quantile=\"0.9\"} 2\ns_sum 3\ns_count 4\n", "# TYPE s summary\ns_sum 3\ns_count 4\n", "# TYPE s summary\ns_count 4\n", "# TYPE s summary\ns{quantile=\"0.5\"} 1\n", "# TYPE s summary\ns 1\n", "# TYPE s summary\ns{x=\"1\"} 1\ns_count{x=\"1\"} 2\n",
		"# TYPE s summary\ns{quantile=\"x\"} 1\n", "# TYPE s summary\ns{quantile=\"NaN\"} 1\ns{quantile=\"+Inf\"} 1\n", "# TYPE s summary\ns{quantile=\"0.5\"} 1\ns{quantile=\"0.50\"} 2\n", "# TYPE s summary\ns_sum 1\ns_sum 2\n", "# TYPE s summary\ns_count 1\ns_count 2\n", "# TYPE s summary\ns_count 1.5\n", "# TYPE s summary\ns_count -1\n", "# TYPE s summary\ns_count NaN\n", "# TYPE s summary\ns_count 1e30\n", "# TYPE s summary\ns_count 1e3\n",
		"# TYPE s summary\ns{le=\"1\",quantile=\"0.5\"} 1\ns_count{le=\"1\"} 2\n", "# TYPE s summary\ns{b=\"2\",a=\"1\",quantile=\"0.5\"} 1 10\ns_sum{a=\"1\",b=\"2\"} 3 20\ns_count{b=\"2\",a=\"1\"} 4 30\ns{a=\"1\",quantile=\"0.9\",b=\"2\"} 2\n", "# TYPE s summary\ns_bucket{quantile=\"0.5\"} 1\ns_bucket 2\n", "s_sum 1\ns_count 2\n# TYPE s summary\ns_sum 3\n",
		// Histograms: complete, incomplete, and what cannot be one series.
		"# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_count 2\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\n", "# TYPE h histogram\nh_sum 1\n", "# TYPE h histogram\nh_count 1\n", "# TYPE h histogram\nh 1\n", "# TYPE h histogram\nh{le=\"1\"} 1\nh_count 1\n",
		"# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"1.0\"} 1\nh_count 1\n", "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_count 3\n", "# TYPE h histogram\nh_bucket{le=\"x\"} 1\n", "# TYPE h histogram\nh_bucket{le=\"\"} 1\n", "# TYPE h histogram\nh_bucket{le=\"1_0\"} 1\n", "# TYPE h histogram\nh_bucket{le=\"0x1p3\"} 1\n", "# TYPE h histogram\nh_bucket{le=\"NaN\"} 1\nh_count 1\n", "# TYPE h histogram\nh_bucket{le=\"-Inf\"} 0\nh_bucket{le=\"inf\"} 1\n",
		"# TYPE h histogram\nh_bucket{le=\"1\"} 1.5\n", "# TYPE h histogram\nh_bucket{le=\"1\"} -1\n", "# TYPE h histogram\nh_bucket{le=\"1\"} NaN\n", "# TYPE h histogram\nh_bucket{le=\"1\"} +Inf\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1e30\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 7.0\nh_bucket{le=\"+Inf\"} 1e3\nh_count 1000\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 18446744073709551615\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 18446744073709549568\nh_count 18446744073709549568\n",
		"# TYPE h histogram\nh_sum 1\nh_sum 2\nh_count 1\n", "# TYPE h histogram\nh_count 1\nh_count 1\n", "# TYPE h histogram\nh_sum{x=\"1\"} 1\nh_sum{x=\"2\"} 2\nh_count{x=\"2\"} 1\nh_count{x=\"1\"} 1\n", "# TYPE h histogram\nh_sum NaN\nh_count 0\n", "# TYPE h histogram\nh_bucket{quantile=\"0.5\",le=\"+Inf\"} 1\nh_count{quantile=\"0.5\"} 1\n",
		"# TYPE h histogram\nh_bucket{a=\"1\",b=\"2\",le=\"1\"} 1 100\nh_bucket{le=\"+Inf\",b=\"2\",a=\"1\"} 2 200\nh_sum{b=\"2\",a=\"1\"} 3\nh_count{a=\"1\",b=\"2\"} 2 300\nh_bucket{a=\"1\",le=\"+Inf\"} 5\nh_bucket{a=\"1\",b=\"2\",c=\"3\",le=\"+Inf\"} 6\n",
		"# TYPE h histogram\nh_bucket{a=\"x\xffb\xffy\",le=\"+Inf\"} 1\nh_bucket{a=\"x\",b=\"y\",le=\"+Inf\"} 2\n", "# TYPE h histogram\nh_bucket{a=\"1\",le=\"+Inf\"} 1\n# a comment\nh_bucket{a=\"2\",le=\"+Inf\"} 2\nh_bucket{a=\"1\",le=\"1\"} 0\n", "# TYPE h histogram\nh_bucket{le=\"+Inf\",le=\"1\"} 1\n", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\n# TYPE h histogram\n",
		"# TYPE h histogram\n# TYPE h_count gauge\nh_count 1\nh_bucket{le=\"+Inf\"} 1\n", "h_bucket{le=\"1\"} 1\nh_count 1\n# TYPE h histogram\n", "# TYPE h_bucket histogram\nh_bucket_bucket{le=\"+Inf\"} 1\nh_bucket 3\n", "# TYPE _count histogram\n_count 1\n_count_count 1\n", "# TYPE h histogram\n_bucket{le=\"1\"} 1\n_sum 1\n_count 1\n", "# TYPE a_sum summary\na_sum_sum 1\na_sum_count 1\na_sum 2\n",
		"# TYPE h histogram\n" + many(30, "h_bucket{handler=\"/a\",le=\"%d\"} 1\n") + "h_bucket{handler=\"/a\",le=\"+Inf\"} 1\n" + many(3, "h_bucket{handler=\"/b\",le=\"%d\"} 2\n") + "h_count{handler=\"/b\"} 2\n" + many(40, "h_bucket{handler=\"/c\",le=\"%d\"} 3\n") + "h_count{handler=\"/c\"} 3\n",
		// OpenMetrics: types, _total, _created, exemplars, timestamps.
		"# TYPE c counter\nc_total 1\nc_created 123\n# EOF\n", "# TYPE c counter\nc 1\n# EOF\n", "# TYPE c_total counter\nc_total 1\nc_created 2\n# EOF\n", "# TYPE c counter\n# TYPE c_total gauge\nc_total 1\n", "# TYPE c counter\nc_total{x=\"1\"} 1 # {trace_id=\"abc\"} 0.5 1.5\nc_total{x=\"2\"} 2 12.5 # {a=\"b\"} 1\n",
		"a 1 # {x=\"y\"} 2\n", "a 1 #\n", "a 1# 2\n", "a 1 2#3\n", "a 1 2 #3\n", "a 1 2 # 3 # 4\n", "a{x=\"#\"} 1 # x\n", "a # 1\n", "a 1 1.5\n", "a 1 -1.5\n", "a 1 1e3\n", "a 1 NaN\n", "a 1 +Inf\n", "a 1 1e400\n", "a 1 9.3e15\n", "a 1 1_0\n", "a 1 0x10\n", "a 1 1.0005\n", "a 1 x\n",
		"# TYPE i info\ni_info{v=\"1\"} 1\n", "# TYPE i_info info\ni_info{v=\"1\"} 1\n", "# TYPE i info\ni{v=\"1\"} 1\n", "# TYPE st stateset\nst{st=\"a\"} 1\nst{st=\"b\"} 0\n", "# TYPE u unknown\nu 1\n", "# TYPE u untyped\nu 1\n",
		"# HELP g Help.\n# TYPE g gaugehistogram\ng_bucket{le=\"1\"} 1\ng_bucket{le=\"+Inf\"} 2\ng_gcount 2\ng_gsum 3\n", "# TYPE g gaugehistogram\n# HELP g_bucket Own.\ng_bucket{le=\"1\"} 1\n", "g_gsum 1\n# TYPE g gaugehistogram\ng_gsum 2\ng_bucket{le=\"1\"} 1\n", "# TYPE g gaugehistogram\n# TYPE g_gcount counter\n",
		"# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\nh_created 5\nh_count 1\n", "# TYPE h histogram\nh_created{le=\"x\"} 5\n", "# TYPE h histogram\nh_created{x=\"1\"} -5.5\nh_bucket{le=\"+Inf\"} 1\n", "# TYPE s summary\ns_created 5\ns_count 1\n", "# TYPE s summary\ns_created 5\n", "# TYPE h histogram\n# TYPE h_created gauge\nh_created 1\n", "h_created 1\n# TYPE h histogram\nh_created 2\n",
		"# TYPE a_total counter\n# TYPE a counter\na_total 1\n", "# TYPE a counter\n# TYPE a_created gauge\na_created 1\na_total 2\n",
		// Many series, for the limit and for the slices that hold them.
		many(300, "m{i=\"%d\"} 1\n"), "# TYPE h histogram\n" + many(100, "h_bucket{i=\"%[1]d\",le=\"1\"} 1\nh_bucket{i=\"%[1]d\",le=\"+Inf\"} 2\nh_sum{i=\"%[1]d\"} 3\nh_count{i=\"%[1]d\"} 2\n"), many(50, "fam%[1]d{x=\"1\"} 1\nfam%[1]d{x=\"2\"} 2\n"), many(50, "m%[1]d 1\n") + many(50, "m%[1]d{x=\"1\"} 2\n"),
	}
	accepted := map[promReading]int{}
	for _, body := range cases {
		for _, reading := range promReadings {
			if compareExposition(t, []byte(body), reading) {
				accepted[reading]++
			}
		}
		// A body's lines may end in \r\n, and its last line in nothing.
		compareExposition(t, []byte(strings.ReplaceAll(body, "\n", "\r\n")), promReading{})
		compareExposition(t, []byte(strings.TrimSuffix(body, "\n")), promReading{openMetrics: true})
	}
	for _, reading := range promReadings {
		if n := accepted[reading]; n < 50 || n > len(cases)-50 {
			t.Errorf("%s: %d of %d cases are accepted: the table should hold many of both kinds", reading, n, len(cases))
		}
	}
	t.Logf("%d cases, each read %d ways; %d of them are accepted as the text format without a filter or a limit", len(cases), len(promReadings)+2, accepted[promReading{}])
}

// promGenerator writes random expositions.
type promGenerator struct {
	random *rand.Rand
	out    []byte
	// families are the names the exposition has used, so that later lines
	// come back to them: a second TYPE, an interleaved series, a suffix.
	families []string
}

func (g *promGenerator) pick(choices ...string) string {
	return choices[g.random.IntN(len(choices))]
}

func (g *promGenerator) write(parts ...string) {
	for _, part := range parts {
		g.out = append(g.out, part...)
	}
}

// blank is the space between two parts of a line: mostly one space, or
// none where none is needed.
func (g *promGenerator) blank(needed bool) string {
	switch g.random.IntN(12) {
	case 0:
		return "\t"
	case 1:
		return "  "
	case 2:
		return " \t "
	}
	if needed || g.random.IntN(10) == 0 {
		return " "
	}
	return ""
}

func (g *promGenerator) end() {
	g.write(g.blank(false), g.pick("\n", "\n", "\n", "\n", "\n", "\r\n"))
}

func (g *promGenerator) name() string {
	if len(g.families) > 0 && g.random.IntN(12) == 0 {
		return g.families[g.random.IntN(len(g.families))]
	}
	name := g.pick("a", "b", "requests", "http_request_duration_seconds", "node:cpu", "up", "x_total", "x", "temp_celsius", "h", "s", "my.metric", "métrique", "with space", "q\"uote", "z_info")
	// A number, so that most families of an exposition are different
	// ones, and a suffix that a family's own series may have.
	name += g.pick("", "1", "2", "3", "4", "5", "6", "7")
	if g.random.IntN(6) == 0 {
		name += g.pick("_total", "_sum", "_count", "_bucket", "_created", "_info", "_gsum", "_gcount", "_a")
	}
	g.families = append(g.families, name)
	return name
}

// written is a name as a line writes it: bare when it can be, and quoted,
// with its escapes, when it cannot or at random.
func (g *promGenerator) written(name string) (text string, quoted bool) {
	bare := true
	for _, c := range []byte(name) {
		bare = bare && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == ':')
	}
	if bare && g.random.IntN(8) != 0 {
		return name, false
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(name) + `"`, true
}

// rarely is true once in n times: what makes a line an error is written
// rarely, so that most of an exposition is read before one is found.
func (g *promGenerator) rarely(n int) bool { return g.random.IntN(n) == 0 }

func (g *promGenerator) value() string {
	if g.rarely(150) {
		return g.pick("1e400", "0x1p3", "1_0", "x", "", "1.5e", "0x10", "1,5", "\"1\"")
	}
	return g.pick("0", "1", "2", "3", "10", "100", "1.5", "-1", "0.25", "1e3", "1e-3", "2.0", "7.0", "NaN", "+Inf", "-Inf", "Inf", "18446744073709551616", "-0", ".5", "5.", "12345.678", "1E3", "+1")
}

// count is a value that is a count, in one of the ways a whole number is
// written, and rarely one that is none.
func (g *promGenerator) count(n int) string {
	if g.rarely(150) {
		return g.pick("1.5", "-1", "NaN", "+Inf", "1e30", "x", "18446744073709551616")
	}
	return fmt.Sprintf(g.pick("%d", "%d", "%d", "%d.0", "%de0"), n)
}

func (g *promGenerator) labelValue() string {
	switch g.random.IntN(12) {
	case 0:
		return g.pick(`a\\b`, `say \"hi\"`, `line\nbreak`, `\\`, `\"`, `tail\\`)
	case 1:
		return g.pick("é", "日本", "\xff", "a\xffb", "\xc3", "x\xffy\xffz")
	case 2:
		return g.pick("", " ", "{}", "a,b", "#", " # ", "a b", "=", "}", "{")
	case 3:
		if g.rarely(8) {
			return g.pick(`\x`, `\t`, `\`, "a\"b", `a="b"`)
		}
	}
	return g.pick("0", "1", "2", "a", "b", "eu", "us", "GET", "POST", "/api", "200", "500") + g.pick("", "", "", "1", "2")
}

// labelSet is a series' labels as a line writes them, name="value" each: a
// few labels of few names, so that the sets of two series are often one
// set, and rarely many labels, a reserved name, le or quantile, or a name
// twice.
func (g *promGenerator) labelSet() []string {
	names := []string{"a", "b", "c", "method", "code", "handler", "é.ü", "x y"}
	count := g.random.IntN(4)
	if g.rarely(40) {
		count = 14 + g.random.IntN(8)
	}
	entries := []string{}
	used := map[string]bool{}
	for i := 0; i < count; i++ {
		name := names[g.random.IntN(len(names))]
		if count > 10 {
			name = fmt.Sprintf("l%d", g.random.IntN(24))
		}
		if g.rarely(100) {
			name = g.pick("le", "quantile", "__name__", "__a", "a:b", "0a", "")
		}
		// A name written twice is an error, wanted rarely.
		if used[name] && !g.rarely(60) {
			continue
		}
		used[name] = true
		text, _ := g.written(name)
		if strings.Contains(text, ":") && !g.rarely(4) {
			text = `"` + text + `"`
		}
		entries = append(entries, text+g.blank(false)+"="+g.blank(false)+`"`+g.labelValue()+`"`)
	}
	return entries
}

// labels writes a label set in a random order, with the metric's name
// among them when it is written inside the braces.
func (g *promGenerator) labels(entries []string) {
	entries = append([]string{}, entries...)
	g.random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	g.write("{", g.blank(false))
	for i, entry := range entries {
		if i > 0 {
			g.write(g.blank(false), ",", g.blank(false))
		}
		g.write(entry)
	}
	if len(entries) > 0 && g.rarely(6) {
		g.write(",")
	}
	g.write(g.blank(false), "}")
}

// sample writes a sample line: a name, the labels of its series and, when
// it has one, its le or quantile, a value, and now and then a timestamp
// and an exemplar.
func (g *promGenerator) sample(name string, entries []string, special, value string) {
	if special != "" {
		entries = append(append([]string{}, entries...), special)
	}
	g.write(g.blank(false))
	text, quoted := g.written(name)
	switch {
	case quoted && !g.rarely(12):
		// A quoted name is written inside the braces.
		g.labels(append([]string{text}, entries...))
	case len(entries) > 0 || g.rarely(4):
		g.write(text, g.blank(false))
		g.labels(entries)
	default:
		g.write(text)
	}
	g.write(g.blank(true), value)
	if g.rarely(5) {
		// A time that both formats read, mostly: OpenMetrics alone reads
		// one with a fraction.
		at := g.pick("1700000000000", "1700000000", "-5", "0", "123")
		if g.rarely(12) {
			at = g.pick("1700000000.123", "1e9", "1.5", "x", "NaN", "9223372036854775807", "9223372036854775808", "+Inf")
		}
		g.write(g.blank(true), at)
		if g.rarely(30) {
			g.write(g.blank(true), g.pick("junk", "1", " ", "\f"))
		}
	}
	if g.rarely(60) {
		// An exemplar, which OpenMetrics alone has.
		g.write(g.pick(` # {trace_id="abc"} 0.5`, ` # {trace_id="abc"} 0.5`, ` # {trace_id="abc"} 0.5 1700000000.5`, "\t# {} 1", " # {a=\"b\"} 1", " #", " # x", `# {a="b"} 1`))
	}
	g.end()
}

func (g *promGenerator) comment(name string) {
	text, _ := g.written(name)
	switch g.random.IntN(14) {
	case 0, 1, 2, 3:
		g.write("#", g.blank(true), "TYPE", g.blank(true), text, g.blank(true), g.pick("counter", "gauge", "untyped", "histogram", "summary", "histogram", "summary", "unknown", "info", "stateset", "gaugehistogram", "Gauge", "COUNTER", "bogus", ""))
	case 4, 5, 6:
		g.write("#", g.blank(true), "HELP", g.blank(true), text, g.blank(true), g.pick("Some help.", `Help with \\ and \n and \" and ".`, "", `Bad \x escape.`, `Ends in \`, "Héłp \xff."))
	case 7:
		g.write("# ", g.pick("a comment", "HELP", "TYPE", "UNIT "+name+" seconds", "TYPE "+text, "HELP "+text, "HELPER x y", ""))
	case 8:
		g.write(g.pick("# EOF", "#EOF", "# EOF x", "#  EOF  "))
	case 9:
		g.write(g.pick("", " ", "\t", "\f", " ", " \v "))
	default:
		g.write("# HELP ", text, " Help of ", name, ".\n# TYPE ", text, " ", g.pick("counter", "gauge", "histogram", "summary"))
	}
	g.end()
}

// family writes a family as an exposition does: its HELP and TYPE, mostly,
// and its series. A histogram's or a summary's series has its samples one
// after the other, each with the series' labels in an order of its own;
// now and then it has no _sum, no _count or no +Inf bucket, a sample twice,
// or a count that is not its +Inf bucket's.
func (g *promGenerator) family() {
	name := g.name()
	text, _ := g.written(name)
	kind := g.pick("counter", "gauge", "untyped", "histogram", "histogram", "summary", "summary", "none", "none")
	if g.rarely(25) {
		kind = g.pick("unknown", "info", "stateset", "gaugehistogram")
	}
	if kind != "none" {
		if !g.rarely(3) {
			g.write("# HELP ", text, " Help of ", name, ".")
			g.end()
		}
		g.write("# TYPE ", text, " ", kind)
		g.end()
	}
	series := 1 + g.random.IntN(4)
	switch kind {
	case "histogram", "gaugehistogram":
		for i := 0; i < series; i++ {
			entries := g.labelSet()
			total, below := g.random.IntN(50), 0
			for _, bound := range []string{"0.1", "0.5", "1", "5.0", "1e1"} {
				if g.rarely(2) {
					continue
				}
				below += g.random.IntN(total - below + 1)
				if g.rarely(30) {
					bound = g.pick("0.1", "1.0", "x", "NaN", "-Inf", "")
				}
				g.sample(name+"_bucket", entries, `le="`+bound+`"`, g.count(below))
			}
			if !g.rarely(6) {
				g.sample(name+"_bucket", entries, `le="`+g.pick("+Inf", "+Inf", "+Inf", "Inf", "inf", "+inf")+`"`, g.count(total))
			}
			if !g.rarely(6) {
				g.sample(name+g.pick("_sum", "_sum", "_gsum"), entries, "", g.value())
			}
			if g.rarely(15) {
				total++
			}
			for !g.rarely(6) {
				g.sample(name+g.pick("_count", "_count", "_count", "_gcount"), entries, "", g.count(total))
				if !g.rarely(15) {
					break
				}
			}
			if g.rarely(8) {
				g.sample(name+g.pick("", "_created", "_total", "_bucket"), entries, "", g.value())
			}
		}
	case "summary":
		for i := 0; i < series; i++ {
			entries := g.labelSet()
			for _, quantile := range []string{"0.5", "0.9", "0.99"} {
				if g.rarely(3) {
					continue
				}
				if g.rarely(30) {
					quantile = g.pick("0.5", "0.50", "x", "NaN", "")
				}
				g.sample(name, entries, `quantile="`+quantile+`"`, g.value())
			}
			if !g.rarely(6) {
				g.sample(name+"_sum", entries, "", g.value())
			}
			for !g.rarely(6) {
				g.sample(name+"_count", entries, "", g.count(g.random.IntN(50)))
				if !g.rarely(15) {
					break
				}
			}
			if g.rarely(8) {
				g.sample(name+g.pick("", "_created", "_bucket"), entries, "", g.value())
			}
		}
	default:
		for i := 0; i < series; i++ {
			g.sample(name+g.pick("", "", "", "", "", "", "_total", "_created", "_sum", "_bucket", "_info"), g.labelSet(), "", g.value())
		}
	}
}

// exposition writes one exposition of a few families, with comments and
// stray lines between them.
func (g *promGenerator) exposition() []byte {
	g.out, g.families = g.out[:0], g.families[:0]
	for i, n := 0, g.random.IntN(6); i < n; i++ {
		switch g.random.IntN(12) {
		case 0:
			g.comment(g.name())
		case 1:
			g.sample(g.name(), g.labelSet(), g.pick("", "", "", `le="1"`, `quantile="0.5"`), g.value())
		default:
			g.family()
		}
	}
	if g.random.IntN(3) == 0 {
		g.write("# EOF\n")
	}
	out := bytes.Clone(g.out)
	if g.random.IntN(6) == 0 {
		// The last line need not end.
		out = bytes.TrimRight(out, "\r\n")
	}
	return out
}

// corrupt returns body with one to three small changes: a byte removed,
// replaced or added, the body cut short, or a line written twice.
func (g *promGenerator) corrupt(body []byte) []byte {
	const alphabet = "{}\",=\\ \t\n\r#_:0159.+-eEnpP\xfféaAzle"
	out := bytes.Clone(body)
	for n := 1 + g.random.IntN(3); n > 0; n-- {
		if len(out) == 0 {
			out = append(out, alphabet[g.random.IntN(len(alphabet))])
			continue
		}
		at := g.random.IntN(len(out))
		switch g.random.IntN(5) {
		case 0:
			out = append(out[:at], out[at+1:]...)
		case 1:
			out[at] = alphabet[g.random.IntN(len(alphabet))]
		case 2:
			out = append(out[:at], append([]byte{alphabet[g.random.IntN(len(alphabet))]}, out[at:]...)...)
		case 3:
			out = out[:at]
		default:
			start := bytes.LastIndexByte(out[:at], '\n') + 1
			end := at + bytes.IndexByte(append(out[at:len(out):len(out)], '\n'), '\n')
			line := append(bytes.Clone(out[start:end]), '\n')
			out = append(out[:start], append(line, out[start:]...)...)
		}
	}
	return out
}

// 12,000 random expositions and a corruption of each, 24,000 bodies, each
// read two ways, as the text format or OpenMetrics, with a filter or
// without, with a limit or without: 48,000 parses in which the parser and
// the one it was return the same series or the same error. The expositions
// have escapes, quoted UTF-8 names, timestamps, exemplars, \r\n, a last line
// without an end, histograms and summaries complete and incomplete, series
// written twice, families written apart, comments and # EOF.
func TestPromParserAgreesWithTheOneItWasOnRandomExpositions(t *testing.T) {
	g := &promGenerator{random: rand.New(rand.NewPCG(2026, 1003))}
	const expositions = 12000
	accepted, refused := 0, 0
	compare := func(body []byte) {
		readings := []promReading{
			{openMetrics: g.random.IntN(2) == 0},
			{openMetrics: g.random.IntN(2) == 0, filtered: g.random.IntN(2) == 0, limit: g.random.IntN(3) * g.random.IntN(6)},
		}
		for _, reading := range readings {
			if compareExposition(t, body, reading) {
				accepted++
			} else {
				refused++
			}
		}
	}
	for i := 0; i < expositions && !t.Failed(); i++ {
		body := g.exposition()
		compare(body)
		compare(g.corrupt(body))
	}
	if accepted < expositions || refused < expositions {
		t.Fatalf("%d parses accepted and %d refused: the generator should write many of both", accepted, refused)
	}
	t.Logf("%d parses accepted, %d refused", accepted, refused)
}

// Whatever the body and however it is read, the parser and the one it was
// agree on it; the bodies here are the start of a search for one they do
// not agree on
// (go test -fuzz FuzzPromParserAgreesWithTheOneItWas ./internal/decode/).
func FuzzPromParserAgreesWithTheOneItWas(f *testing.F) {
	for _, body := range []string{
		"# HELP a Help.\n# TYPE a counter\na_total{x=\"1\",y=\"a\\\\b\"} 1 1700000000000\n",
		"# TYPE h histogram\nh_bucket{a=\"1\",le=\"1\"} 1\nh_bucket{le=\"+Inf\",a=\"1\"} 2\nh_sum{a=\"1\"} 3\nh_count{a=\"1\"} 2\n",
		"# TYPE s summary\ns{quantile=\"0.5\"} 1\ns_sum 2\ns_count 3\r\n{\"my.metric\",\"l.1\"=\"v\"} 4 # {t=\"1\"} 1\n# EOF",
		"# TYPE b gauge\n# TYPE a gauge\na 1\nb 2\na{x=\"1\"} 3\n",
	} {
		f.Add([]byte(body), uint8(0))
		f.Add([]byte(body), uint8(1))
		f.Add([]byte(body), uint8(14))
	}
	f.Fuzz(func(t *testing.T, body []byte, reading uint8) {
		compareExposition(t, body, promReading{openMetrics: reading&1 != 0, filtered: reading&2 != 0, limit: int(reading >> 2 & 3)})
	})
}
