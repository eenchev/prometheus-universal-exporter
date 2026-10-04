package transform

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// failureOf is the failure of a rule under error_mode fail on a body.
func failureOf(t *testing.T, kind, transform string, rule model.MetricRule, body string) error {
	t.Helper()
	rule.Type, rule.ErrorMode = model.GaugeMetricType, model.ErrorModeFail
	c := model.Collector{Name: "rows", Decoder: model.DecoderConfig{Type: kind}, Transform: model.TransformConfig{Type: transform}, Metrics: []model.MetricRule{rule}}
	r := &fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	_, err = Transform(LeaveRuleLoggingToCaller(t.Context()), d, r, &c, "python3")
	if err == nil {
		t.Fatalf("%s %q: no rule failed on %s", transform, rule.Expression, body)
	}
	return err
}

// Every failure of a transform that names where in the response it happened
// — a row, a node, an item, the place of a value among a label's — or what
// it counted there is recognised by the log as the same failure when only
// that differs: of each, two responses that fail in different places read
// differently, each naming its place, and are recognised by one text.
func TestTransformFailuresThatDifferInTheirPlaceAreRecognisedAsOne(t *testing.T) {
	required := true
	label := func(name, expression string) []model.LabelRule {
		return []model.LabelRule{{Name: name, Expression: expression}}
	}
	needed := func(name, expression string) []model.LabelRule {
		return []model.LabelRule{{Name: name, Expression: expression, Required: required}}
	}
	html := func(rows string) string { return "<html><body><table>" + rows + "</table></body></html>" }
	for name, tc := range map[string]struct {
		kind, transform string
		rule            model.MetricRule
		a, b            string
		wantA, wantB    string
	}{
		"a csv cell empty": {"csv", "csv", model.MetricRule{Name: "m", Expression: "used"},
			"host,used\nh1,1\nh2,\n", "host,used\nh1,\nh2,1\n",
			`CSV column "used" is empty in row 2`, `CSV column "used" is empty in row 1`},
		"an xpath node without a value": {"xml", "xpath", model.MetricRule{Name: "m", Expression: "//v"},
			"<r><v>1</v><v></v></r>", "<r><v></v><v>1</v></r>",
			`metric "m" value is missing for node 1: XPath "//v" selected a node without a value`, `metric "m" value is missing for node 0: XPath "//v" selected a node without a value`},
		"an xpath node that is no number": {"xml", "xpath", model.MetricRule{Name: "m", Expression: "//v"},
			"<r><v>1</v><v>x</v></r>", "<r><v>x</v><v>1</v></r>",
			`metric "m" node 1: value "x" is not a number; map text to numbers with value_map`, `metric "m" node 0: value "x" is not a number; map text to numbers with value_map`},
		"an xpath node without a required label": {"xml", "xpath", model.MetricRule{Name: "m", Expression: "//v", Labels: needed("l", "@l")},
			`<r><v l="a">1</v><v>2</v></r>`, `<r><v>2</v><v l="a">1</v></r>`,
			`metric "m" label "l" is missing for node 1`, `metric "m" label "l" is missing for node 0`},
		"an xpath label that adds up text": {"xml", "xpath", model.MetricRule{Name: "m", Expression: "//v", Labels: label("l", "sum(../n)")},
			`<r><h><v>1</v><n>2</n></h><h><v>2</v><n>x</n></h></r>`, `<r><h><v>1</v><n>x</n></h><h><v>2</v><n>2</n></h></r>`,
			`metric "m" node 1 label "l": XPath "sum(../n)" cannot be computed: it adds up text that is not a number, first "x" (1 of 1 nodes)`, `metric "m" node 0 label "l": XPath "sum(../n)" cannot be computed: it adds up text that is not a number, first "x" (1 of 1 nodes)`},
		"a css item without a value": {"html", "css", model.MetricRule{Name: "m", Items: "tr", Expression: "td.v"},
			html(`<tr><td class="v">1</td></tr><tr><td class="v"></td></tr>`), html(`<tr><td class="v"></td></tr><tr><td class="v">1</td></tr>`),
			`metric "m" value is missing for item 1: CSS selector "td.v" matched an element without a value`, `metric "m" value is missing for item 0: CSS selector "td.v" matched an element without a value`},
		"a css item that is no number": {"html", "css", model.MetricRule{Name: "m", Items: "tr", Expression: "td.v"},
			html(`<tr><td class="v">1</td></tr><tr><td class="v">x</td></tr>`), html(`<tr><td class="v">x</td></tr><tr><td class="v">1</td></tr>`),
			`metric "m" item 1: value "x" is not a number; map text to numbers with value_map`, `metric "m" item 0: value "x" is not a number; map text to numbers with value_map`},
		"a css item with several values": {"html", "css", model.MetricRule{Name: "m", Items: "tr", Expression: "td.v"},
			html(`<tr><td class="v">1</td></tr><tr><td class="v">1</td><td class="v">2</td></tr>`), html(`<tr><td class="v">1</td><td class="v">2</td><td class="v">3</td></tr>`),
			`metric "m" item 1: CSS selector "td.v" matched 2 elements; within an item it must match at most one`, `metric "m" item 0: CSS selector "td.v" matched 3 elements; within an item it must match at most one`},
		"a css item without a required label": {"html", "css", model.MetricRule{Name: "m", Items: "tr", Expression: "td.v", Labels: needed("l", "td.l")},
			html(`<tr><td class="v">1</td><td class="l">a</td></tr><tr><td class="v">1</td></tr>`), html(`<tr><td class="v">1</td></tr>`),
			`metric "m" label "l" is missing for item 1`, `metric "m" label "l" is missing for item 0`},
		"css elements without items": {"html", "css", model.MetricRule{Name: "m", Expression: "td"},
			html(`<tr><td>1</td><td>2</td></tr>`), html(`<tr><td>1</td><td>2</td><td>3</td></tr>`),
			`metric "m" CSS selector "td" matched 2 elements, but without items a css metric is one value`, `metric "m" CSS selector "td" matched 3 elements, but without items a css metric is one value`},
		"a jq item without a value": {"json", "jq", model.MetricRule{Name: "m", Items: ".hosts[]", Expression: ".used"},
			`{"hosts":[{"used":1},{"used":null}]}`, `{"hosts":[{"used":null},{"used":1}]}`,
			`metric "m" value is missing for item 1`, `metric "m" value is missing for item 0`},
		"a jq item that is no number": {"json", "jq", model.MetricRule{Name: "m", Items: ".hosts[]", Expression: ".used"},
			`{"hosts":[{"used":1},{"used":"x"}]}`, `{"hosts":[{"used":"x"},{"used":1}]}`,
			`metric "m" item 1: value "x" is not a number; map text to numbers with value_map`, `metric "m" item 0: value "x" is not a number; map text to numbers with value_map`},
		"a jq item whose expression fails": {"json", "jq", model.MetricRule{Name: "m", Items: ".hosts[]", Expression: ".used.now"},
			`{"hosts":[{"used":{"now":1}},{"used":"x"}]}`, `{"hosts":[{"used":"x"}]}`,
			`metric "m" item 1 expression: `, `metric "m" item 0 expression: `},
		"a jq item with several values": {"json", "jq", model.MetricRule{Name: "m", Items: ".hosts[]", Expression: ".used[]"},
			`{"hosts":[{"used":[1]},{"used":[1,2]}]}`, `{"hosts":[{"used":[1,2,3]}]}`,
			`metric "m" item 1 expression: expression ".used[]" produced 2 values for one item; it must produce at most one`, `metric "m" item 0 expression: expression ".used[]" produced 3 values for one item; it must produce at most one`},
		"a jq item whose label fails": {"json", "jq", model.MetricRule{Name: "m", Items: ".hosts[]", Expression: ".used", Labels: label("l", ".name.first")},
			`{"hosts":[{"used":1,"name":{"first":"a"}},{"used":1,"name":"x"}]}`, `{"hosts":[{"used":1,"name":"x"}]}`,
			`metric "m" item 1 label "l": `, `metric "m" item 0 label "l": `},
		"a jq item whose label is a list": {"json", "jq", model.MetricRule{Name: "m", Items: ".hosts[]", Expression: ".used", Labels: label("l", ".tags")},
			`{"hosts":[{"used":1,"tags":"a"},{"used":1,"tags":["a","b"]}]}`, `{"hosts":[{"used":1,"tags":["a","b","c"]}]}`,
			`metric "m" item 1 label "l" is an array of 2 values, not a single value`, `metric "m" item 0 label "l" is an array of 3 values, not a single value`},
		"a jq item without a required label": {"json", "jq", model.MetricRule{Name: "m", Items: ".hosts[]", Expression: ".used", Labels: needed("l", ".name")},
			`{"hosts":[{"used":1,"name":"a"},{"used":1}]}`, `{"hosts":[{"used":1}]}`,
			`metric "m" label "l" is missing for item 1`, `metric "m" label "l" is missing for item 0`},
		"a jq label's value that is a list": {"json", "jq", model.MetricRule{Name: "m", Expression: ".hosts[].used", Labels: label("l", ".hosts[].tags")},
			`{"hosts":[{"used":1,"tags":"a"},{"used":1,"tags":["a","b"]}]}`, `{"hosts":[{"used":1,"tags":["a","b","c"]},{"used":1,"tags":"a"}]}`,
			`label "l" value 1 is an array of 2 values, not a single value`, `label "l" value 0 is an array of 3 values, not a single value`},
		// The rule fails of its second value's label, read only once its
		// first value had failed the scrape: the label is then read to its
		// end to say what fails the rule as a whole.
		"a jq label's value that is a list, past a value that is no number": {"json", "jq", model.MetricRule{Name: "m", Expression: ".hosts[].used", Labels: label("l", ".hosts[].tags")},
			`{"hosts":[{"used":"x","tags":"a"},{"used":1,"tags":["a","b"]}]}`, `{"hosts":[{"used":"x","tags":"a"},{"used":1,"tags":"a"},{"used":1,"tags":["a","b","c"]}]}`,
			`label "l" value 1 is an array of 2 values, not a single value`, `label "l" value 2 is an array of 3 values, not a single value`},
		"jq label values that do not pair": {"json", "jq", model.MetricRule{Name: "m", Expression: ".hosts[].used", Labels: label("l", ".names[]")},
			`{"hosts":[{"used":1},{"used":2},{"used":3}],"names":["a","b"]}`, `{"hosts":[{"used":1},{"used":2},{"used":3},{"used":4}],"names":["a","b","c"]}`,
			`label "l" gave 2 values for 3 series, so they cannot be paired`, `label "l" gave 3 values for 4 series, so they cannot be paired`},
	} {
		a, b := failureOf(t, tc.kind, tc.transform, tc.rule, tc.a), failureOf(t, tc.kind, tc.transform, tc.rule, tc.b)
		if !strings.Contains(a.Error(), tc.wantA) || !strings.Contains(b.Error(), tc.wantB) {
			t.Errorf("%s: the failures are\n%v\n%v\nwant them to say\n%s\n%s", name, a, b, tc.wantA, tc.wantB)
		}
		if a.Error() == b.Error() || model.SameFailureText(a) != model.SameFailureText(b) {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant one text of two", name, a, b, model.SameFailureText(a), model.SameFailureText(b))
		}
	}

	// Another value in the same place is another failure.
	rule := model.MetricRule{Name: "m", Expression: "//v"}
	if a, b := failureOf(t, "xml", "xpath", rule, "<r><v>x</v></r>"), failureOf(t, "xml", "xpath", rule, "<r><v>y</v></r>"); model.SameFailureText(a) == model.SameFailureText(b) {
		t.Errorf("the failures\n%v\n%v\nare recognised as one, by %s", a, b, model.SameFailureText(a))
	}

	// What a script left at another place of its metrics, and a row a
	// pre-script left that is none.
	for name, pair := range map[string][2]error{
		"a script's entry that is no metric": {second(pythonMetricFrom(3, "up")), second(pythonMetricFrom(7, "up"))},
		"a pre-script's series that is none": {
			second(prometheusFromPython(map[string]any{"metrics": []any{map[string]any{"name": "up", "type": "gauge", "value": 1.0}, "up"}})),
			second(prometheusFromPython(map[string]any{"metrics": []any{"up"}})),
		},
		"a pre-script's series without a name": {
			second(prometheusFromPython(map[string]any{"metrics": []any{map[string]any{"name": "up", "type": "gauge", "value": 1.0}, map[string]any{"type": "gauge", "value": 1.0}}})),
			second(prometheusFromPython(map[string]any{"metrics": []any{map[string]any{"type": "gauge", "value": 1.0}}})),
		},
		"a pre-script's row that is none": {
			transformCSVRows(t, []any{map[string]any{"value": "1"}, "web01"}, "data = rows\n", model.MetricRule{Name: "m", Expression: "value"}).err,
			transformCSVRows(t, []any{"web01"}, "data = rows\n", model.MetricRule{Name: "m", Expression: "value"}).err,
		},
		"a decoded row that is none": {
			transformCSVRows(t, []any{map[string]any{"value": "1"}, "web01"}, "", model.MetricRule{Name: "m", Expression: "value"}).err,
			transformCSVRows(t, []any{"web01"}, "", model.MetricRule{Name: "m", Expression: "value"}).err,
		},
	} {
		if a, b := pair[0], pair[1]; a == nil || b == nil || a.Error() == b.Error() || model.SameFailureText(a) != model.SameFailureText(b) {
			t.Errorf("%s: the failures\n%v\n%v\nwant two texts recognised by one", name, a, b)
		}
	}
}

// second is the error of a call that returns a value and an error.
func second[T any](_ T, err error) error { return err }
