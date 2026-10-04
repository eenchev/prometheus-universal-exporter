package decode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A decoder's failure names the line, the column, the series or the point
// it was at, which in a body that changes from scrape to scrape is another
// on each: the log recognises it as one failure all the same. Of each
// decoder, two bodies that fail the same way in different places read
// differently, each naming its place, and are recognised by one text; a
// body that fails another way is recognised by another.
func TestDecodeFailuresThatDifferInTheirPlaceAreRecognisedAsOne(t *testing.T) {
	failure := func(kind, body string) error {
		t.Helper()
		c := &model.Collector{Name: "rows", Decoder: model.DecoderConfig{Type: kind}, Transform: model.TransformConfig{Type: "jq"}}
		_, err := Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: http.Header{}}, c)
		if err == nil {
			t.Fatalf("%s decoded %q", kind, body)
		}
		return err
	}
	deep := strings.Repeat("[", jsonMaxDepth+1)
	for name, tc := range map[string]struct {
		kind         string
		a, b, other  string
		wantA, wantB string
	}{
		"json": {"json", `{"a": x}`, "{\"a\": 1,\n \"b\":  x}", `{"a": y}`,
			"invalid character 'x' looking for beginning of value, at line 1, column 7", "invalid character 'x' looking for beginning of value, at line 2, column 8"},
		"json nested too deep": {"json", deep, "\n " + deep, `{"a": x}`,
			"arrays and objects nested more than 10000 deep, at line 1, column 10001", "arrays and objects nested more than 10000 deep, at line 2, column 10002"},
		"a csv value past the header": {"csv", "a,b\n1,2,3\n", "a,b\n1,2\n1,2,,4\n", "a,a\n1,2\n",
			"CSV line 2 has a value in column 3, which the header does not name", "CSV line 3 has a value in column 4, which the header does not name"},
		"a csv quote": {"csv", "a,b\n\"x\"y,2\n", "a,b\n1,2\n11,\"x\"y\n", "a,a\n1,2\n",
			`CSV decode: parse error on line 2, column 3: extraneous or missing " in quoted-field`, `CSV decode: parse error on line 3, column 6: extraneous or missing " in quoted-field`},
		"xml": {"xml", "<a><b></a>", "<a>\n\n<b></a>", "<a><b></c>",
			"XML decode: XML syntax error on line 1: element <b> closed by </a>", "XML decode: XML syntax error on line 3: element <b> closed by </a>"},
		"a prometheus line": {"prometheus", "up 1\nup{ 2\n", "up 1\ndown 0\nup{ 2\n", "up 1\nup one\n",
			"text format parsing error in line 2: ", "text format parsing error in line 3: "},
		"a prometheus series": {"prometheus", "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\nh_bucket{le=\"+Inf\"} 1\n", "up 1\n# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\nh_bucket{le=\"+Inf\"} 1\n", "# TYPE s summary\ns{quantile=\"0.5\"} 1\ns{quantile=\"0.5\"} 1\n",
			"the histogram h, which starts in line 2, has two buckets with the upper bound +Inf", "the histogram h, which starts in line 3, has two buckets with the upper bound +Inf"},
		"a carbon line": {"graphite", "a.b 1 1000\nc.d one 1000\n", "a.b 1 1000\na.c 1 1000\nc.d one 1000\n", "a.b 1 1000\nc.d two 1000\n",
			`carbon line 2: the value "one" is not a number`, `carbon line 3: the value "one" is not a number`},
		"a graphite series": {"graphite", `[{"target":"","datapoints":[]}]`, `[{"target":"a.b","datapoints":[]},{"target":"","datapoints":[]}]`, `[{"target":"a","datapoints":[[1]]}]`,
			`graphite render JSON: series 0: the series "" has no path`, `graphite render JSON: series 1: the series "" has no path`},
		"a graphite point": {"graphite", `[{"target":"a","datapoints":[[1]]}]`, `[{"target":"a","datapoints":[[1,2],[1]]}]`, `[{"target":"b","datapoints":[[1]]}]`,
			`graphite render JSON: series "a" point 0 has 1 elements`, `graphite render JSON: series "a" point 1 has 1 elements`},
		"a graphite point that is no number": {"graphite", `[{"target":"a","datapoints":[["x",2]]}]`, `[{"target":"a","datapoints":[[1,2],["x",2]]}]`, `[{"target":"a","datapoints":[["y",2]]}]`,
			`graphite render JSON: series "a" point 0 is [x 2]`, `graphite render JSON: series "a" point 1 is [x 2]`},
	} {
		a, b, other := failure(tc.kind, tc.a), failure(tc.kind, tc.b), failure(tc.kind, tc.other)
		if !strings.Contains(a.Error(), tc.wantA) || !strings.Contains(b.Error(), tc.wantB) {
			t.Errorf("%s: the failures are\n%v\n%v\nwant them to say\n%s\n%s", name, a, b, tc.wantA, tc.wantB)
		}
		if a.Error() == b.Error() || model.SameFailureText(a) != model.SameFailureText(b) {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant one text of two", name, a, b, model.SameFailureText(a), model.SameFailureText(b))
		}
		if model.SameFailureText(a) == model.SameFailureText(other) {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised as one, by %s", name, a, other, model.SameFailureText(a))
		}
	}
}

// What a decoder left out and carried on without is reported with the line
// of the first, as an error the log recognises wherever the line is: a
// carbon line skipped, and a sample line that is no part of its family.
func TestWhatADecoderLeftOutIsRecognisedWhereverItsLineIs(t *testing.T) {
	skipped := func(body string) error {
		t.Helper()
		c := &model.Collector{Name: "carbon", Decoder: model.DecoderConfig{Type: "graphite"}, Response: model.ResponseConfig{Graphite: model.GraphiteConfig{InvalidLines: "skip"}}}
		d, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c)
		if err != nil || d.Graphite == nil || d.Graphite.FirstSkipped == nil {
			t.Fatalf("%q: %v, %+v", body, err, d)
		}
		return d.Graphite.FirstSkipped
	}
	leftOut := func(body string) error {
		t.Helper()
		_, report, err := parseExpositionReporting([]byte(body), promOptions{})
		if err != nil || report == nil || report.FirstLeftOut == nil {
			t.Fatalf("%q: %v, %+v", body, err, report)
		}
		return report.FirstLeftOut
	}
	for name, errs := range map[string][3]error{
		"carbon": {skipped("a.b 1 1000\nc.d one 1000\n"), skipped("c.d one 1000\n"), skipped("c.d two 1000\n")},
		"prometheus": {
			leftOut("# TYPE h histogram\nh{quantile=\"0.5\"} 2\nh_sum 9\nh_count 3\n"),
			leftOut("up 1\n# TYPE h histogram\nh_sum 9\nh{quantile=\"0.5\"} 2\nh_count 3\n"),
			leftOut("# TYPE g histogram\ng{quantile=\"0.5\"} 2\ng_sum 9\ng_count 3\n"),
		},
	} {
		a, b, other := errs[0], errs[1], errs[2]
		if a.Error() == b.Error() || model.SameFailureText(a) != model.SameFailureText(b) || model.SameFailureText(a) == model.SameFailureText(other) {
			t.Errorf("%s: %v and %v are recognised by\n%s\n%s\nand %v by\n%s\nwant the first two as one and the third as another", name, a, b, model.SameFailureText(a), model.SameFailureText(b), other, model.SameFailureText(other))
		}
	}
}
