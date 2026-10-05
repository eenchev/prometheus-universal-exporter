package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// enginePanicPage is a document the engine panics on with each of the
// expressions of enginePanics: its cells are text that is no number, and
// one has the attribute a predicate then asks a number of. It reads the
// same as HTML and as XML.
const enginePanicPage = `<html><body><table><tr><td x="1">1,234</td><td>n/a</td><td x="5">7</td></tr></table></body></html>`

// enginePanics are expressions the engine compiles and then panics on over
// enginePanicPage, each with what it says.
var enginePanics = []struct{ expression, said string }{
	{"sum('abc')", "sum() function argument type must be a node-set or number"},
	{"sum(string(//td))", "sum() function argument type must be a node-set or number"},
	{"sum(translate(//td[2], ',', ''))", "sum() function argument type must be a node-set or number"},
	{"//td[contains(@x, 5)]", "contains() function argument type must be string"},
	{"sum(//td[contains(@x, 5)])", "contains() function argument type must be string"},
	{"sum(//td[contains(@x, 5)]) + 1", "contains() function argument type must be string"},
	{"count(//td[starts-with(1, 'a')])", "starts-with() function argument type must be string"},
	{"count(//td[ends-with(., 1)])", "ends-with() function argument type must be string"},
	{"replace(//td, '(', '')", "replace() function second argument is not a valid regexp pattern, err: error parsing regexp: missing closing ): `(`"},
	{"substring(//td, '1')", "substring() function first argument type must be number"},
	{"substring('abc', 0 div 0)", "runtime error: slice bounds out of range [9223372036854775807:3]"},
	{"//td = true()", "runtime error: invalid memory address or nil pointer dereference"},
	{"true() = //td", "runtime error: invalid memory address or nil pointer dereference"},
	{"(//td)[sum('x')]", "sum() function argument type must be a node-set or number"},
}

// decodedXML is the document an XML body is parsed into.
func decodedXML(t *testing.T, body string) *xmlquery.Node {
	t.Helper()
	root, err := decode.ParseXML([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// enginePanicCollector is an xpath collector of a rule the engine panics
// on, between two rules that work.
func enginePanicCollector(decoder string, rule model.MetricRule) model.Collector {
	return model.Collector{Name: "panics", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{
		{Name: "cells", Type: model.GaugeMetricType, Expression: "count(//td)"},
		rule,
		{Name: "last", Type: model.GaugeMetricType, Expression: "//td[3]"},
	}}
}

// An expression the engine panics on while it evaluates it is the failure
// of its rule, over XML and over HTML: under ignore and log the rules
// around it give their series and the failure is reported once, and under
// fail the transform fails with the rule's failure, which names the metric,
// the expression and what the engine said, without a stack. The panic left
// the transform before, whatever the mode, and took the probe with it.
func TestAnEnginePanicIsTheFailureOfItsRule(t *testing.T) {
	for _, decoder := range []string{"xml", "html"} {
		kind := map[string]string{"xml": "XPath", "html": "HTML XPath"}[decoder]
		for _, test := range enginePanics {
			want := fmt.Sprintf("metric %q %s %q cannot be evaluated: the XPath engine failed on it: %s", "broken", kind, test.expression, test.said)
			for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeLog, model.ErrorModeFail} {
				c := enginePanicCollector(decoder, model.MetricRule{Name: "broken", Type: model.GaugeMetricType, Expression: test.expression, ErrorMode: mode})
				if err := CheckMetricRule(&c, &c.Metrics[1]); err != nil {
					t.Fatalf("%s: the load refuses it: %v", test.expression, err)
				}
				set, failures, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, c, markupContentType[decoder], enginePanicPage)
				if mode == model.ErrorModeFail {
					var failure *MetricFailure
					if !errors.As(err, &failure) || failure.Metric != "broken" || failure.Collector != "panics" || err.Error() != want {
						t.Errorf("%s over %s under fail: the error %v, want the rule's failure %q", test.expression, decoder, err, want)
					}
					if set != nil || len(failures) != 0 {
						t.Errorf("%s over %s under fail: the series %v and the failures %+v beside the error", test.expression, decoder, htmlSeries(set), failures)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s over %s under %s: %v", test.expression, decoder, mode, err)
					continue
				}
				if got := htmlSeries(set); !slices.Equal(got, []string{"cells 3", "last 7"}) {
					t.Errorf("%s over %s under %s: the series %q, want those of the rules around it", test.expression, decoder, mode, got)
				}
				if len(failures) != 1 || failures[0].Metric != "broken" || failures[0].Failures != 1 || failures[0].Missing != 0 || failures[0].First.Error() != want || failures[0].Logged != (mode == model.ErrorModeLog) {
					t.Errorf("%s over %s under %s: the failures %+v, want one of the rule that reads %q", test.expression, decoder, mode, failures, want)
				}
			}
		}
	}
}

// A label the engine panics on fails its rule the same way, and the failure
// names the label and its expression. The rule gives no series then, those
// of the nodes before the one the engine failed at included: the series of
// the first cell, which has no x, was made before the engine came to the
// second. A label the engine panics on at the one series of a computed
// value, and one that is read once for the rule, are told the same.
func TestAnEnginePanicInALabelNamesTheLabel(t *testing.T) {
	const page = `<html><body><table><tr><td>1</td><td x="5">2</td><td>3</td></tr></table></body></html>`
	for _, decoder := range []string{"xml", "html"} {
		kind := map[string]string{"xml": "XPath", "html": "HTML XPath"}[decoder]
		for _, test := range []struct{ expression, label, said string }{
			{"//td", "contains(@x, 5)", "contains() function argument type must be string"},
			{"//td", "sum('abc')", "sum() function argument type must be a node-set or number"},
			{"//td", "sum(string(.)) + sum(../td[contains(@x, 5)])", "contains() function argument type must be string"},
			{"//td", "count(//td[contains(@x, 5)])", "contains() function argument type must be string"},
			{"//td", "//td[contains(@x, 5)]", "contains() function argument type must be string"},
			{"count(//td)", "//td = true()", "runtime error: invalid memory address or nil pointer dereference"},
			{"sum(//td)", "substring(//td, '1')", "substring() function first argument type must be number"},
		} {
			want := fmt.Sprintf("metric %q label %q: %s %q cannot be evaluated: the XPath engine failed on it: %s", "broken", "kind", kind, test.label, test.said)
			rule := model.MetricRule{Name: "broken", Type: model.GaugeMetricType, Expression: test.expression, Labels: []model.LabelRule{
				{Name: "site", Value: "fra1"}, {Name: "row", Expression: "name(..)"}, {Name: "kind", Expression: test.label}, {Name: "after", Expression: "string(.)"},
			}}
			for _, mode := range []string{model.ErrorModeLog, model.ErrorModeFail} {
				rule.ErrorMode = mode
				c := enginePanicCollector(decoder, rule)
				if err := CheckMetricRule(&c, &c.Metrics[1]); err != nil {
					t.Fatalf("%s: the load refuses it: %v", test.label, err)
				}
				set, failures, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, c, markupContentType[decoder], page)
				if mode == model.ErrorModeFail {
					var failure *MetricFailure
					if !errors.As(err, &failure) || failure.Metric != "broken" || err.Error() != want {
						t.Errorf("label %s over %s under fail: the error %v, want %q", test.label, decoder, err, want)
					}
					continue
				}
				if err != nil {
					t.Errorf("label %s over %s under log: %v", test.label, decoder, err)
					continue
				}
				if got := htmlSeries(set); !slices.Equal(got, []string{"cells 3", "last 3"}) {
					t.Errorf("label %s over %s under log: the series %q, want those of the rules around it and none of the rule", test.label, decoder, got)
				}
				if len(failures) != 1 || failures[0].Failures != 1 || failures[0].First.Error() != want {
					t.Errorf("label %s over %s under log: the failures %+v, want one that reads %q", test.label, decoder, failures, want)
				}
			}
		}
	}
}

// The series a rule made before the engine failed are given back to
// limits.max_metrics with the rule's failure: the two series of the rules
// around it fit a limit of two, which the one the rule had made and lost
// would have filled.
func TestTheSeriesOfARuleTheEngineFailedOnAreGivenBack(t *testing.T) {
	const page = `<r><td>1</td><td x="5">2</td><td>3</td></r>`
	c := enginePanicCollector("xml", model.MetricRule{Name: "broken", Type: model.GaugeMetricType, Expression: "//td", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{{Name: "kind", Expression: "contains(@x, 5)"}}})
	c.Limits.MaxMetrics = 2
	set, failures, err := transformWith(context.Background(), t, c, "application/xml", page)
	if err != nil {
		t.Fatal(err)
	}
	if got := htmlSeries(set); !slices.Equal(got, []string{"cells 3", "last 3"}) || len(failures) != 1 {
		t.Fatalf("the series %q and the failures %+v, want the series of the rules around it and one failure", got, failures)
	}
}

// The nodes of a sum() are read before the engine evaluates its expression,
// whether or not the engine would come to the call. Where reading them
// makes the engine panic, that is not the rule's failure by itself: the
// call is left to the engine, which gives the expression the value it has
// without the call — the left of the `or` is true, that of the `and` false
// — as it did before the nodes were read at all. Where the engine does come
// to the call it panics in its turn, and that is the rule's failure. In a
// label the same holds.
func TestAPanicWhileASumsNodesAreReadLeavesTheSumToTheEngine(t *testing.T) {
	const page = `<r><a x="1">1</a><a x="5">7</a></r>`
	for expression, want := range map[string]string{
		"count(//a) > 0 or sum(//a[contains(@x, 5)]) > 0":   "m 1",
		"count(//zz) > 0 and sum(//a[contains(@x, 5)]) > 0": "m 0",
		"count(//zz) > 0 or sum(//a[contains(@x, 5)]) > 0":  `metric "m" XPath "count(//zz) > 0 or sum(//a[contains(@x, 5)]) > 0" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`,
		"sum(//a[contains(@x, 5)])":                         `metric "m" XPath "sum(//a[contains(@x, 5)])" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`,
		"sum(//a[contains(@x, 5)]) + sum(//a)":              `metric "m" XPath "sum(//a[contains(@x, 5)]) + sum(//a)" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`,
	} {
		// The engine alone, as the rule was evaluated before the nodes of
		// a sum were read.
		engine := func() (value string) {
			defer func() {
				if recover() != nil {
					value = "panic"
				}
			}()
			result, _ := xpathValue(xmlNodes, decodedXML(t, page), xpath.MustCompile(expression))
			return "m " + map[any]string{true: "1", false: "0"}[result]
		}()
		if strings.HasPrefix(want, "m ") != (engine != "panic") || engine != "panic" && engine != want {
			t.Fatalf("%s: the engine alone gives %q, and the test wants %q", expression, engine, want)
		}
		c := xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression})
		set, _, err := transformWith(context.Background(), t, c, "application/xml", page)
		got := strings.Join(htmlSeries(set), "; ")
		if err != nil {
			got = err.Error()
		}
		if got != want {
			t.Errorf("%s: %q, want %q", expression, got, want)
		}
		// As a label, on the one series of a computed value.
		c = xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "count(//a)", Labels: []model.LabelRule{{Name: "l", Expression: expression}}})
		set, _, err = transformWith(context.Background(), t, c, "application/xml", page)
		if strings.HasPrefix(want, "m ") {
			wantLabel := fmt.Sprintf("m{l=%q} 2", map[string]string{"m 1": "true", "m 0": "false"}[want])
			if err != nil || !slices.Equal(htmlSeries(set), []string{wantLabel}) {
				t.Errorf("label %s: %q, %v; want %q", expression, htmlSeries(set), err, wantLabel)
			}
		} else if wantLabel := strings.Replace(want, `metric "m" XPath`, `metric "m" label "l": XPath`, 1); err == nil || err.Error() != wantLabel {
			t.Errorf("label %s: %v, want %q", expression, err, wantLabel)
		}
	}
}

// What the engine panicked on is not kept for the next response: probes of
// a target whose answers the engine fails on now and then, many at once,
// each get the series of their own answer — the failure for the one, the
// right numbers for the other — however the compiled copies of the
// expressions pass between them.
func TestGoodAnswersAfterOnesTheEngineFailedOnAreReadRight(t *testing.T) {
	const (
		bad  = `<r><a x="1">1</a><a>2</a><a x="5">7</a><b x="1">3</b></r>`
		good = `<r><a>1</a><a>2</a><a>7</a><b x="1">3</b></r>`
	)
	selecting := "//a[not(contains(@x, 5))]"
	for name, test := range map[string]struct {
		rule model.MetricRule
		want []string
	}{
		"a sum the exporter adds up": {model.MetricRule{Name: "m", Expression: "sum(" + selecting + ")"}, []string{"m 10"}},
		"a sum the engine computes":  {model.MetricRule{Name: "m", Expression: "sum(" + selecting + ") + count(//b)"}, []string{"m 11"}},
		"nodes":                      {model.MetricRule{Name: "m", Expression: selecting + "[2]"}, []string{"m 2"}},
		"a label": {model.MetricRule{Name: "m", Expression: "//b", Labels: []model.LabelRule{{Name: "sum", Expression: "sum(" + selecting + ")"}, {Name: "first", Expression: "(" + selecting + ")[last()]"}}},
			[]string{`m{first="7",sum="10"} 3`}},
		"a label read at each node": {model.MetricRule{Name: "m", Expression: "//a", Labels: []model.LabelRule{{Name: "others", Expression: "count(../a[not(contains(@x, 5))]) - 1"}}},
			[]string{`m{others="2"} 1`, `m{others="2"} 2`, `m{others="2"} 7`}},
	} {
		t.Run(name, func(t *testing.T) {
			test.rule.Type, test.rule.ErrorMode = model.GaugeMetricType, model.ErrorModeFail
			c := xpathRuleCollector("xml", test.rule)
			var probes sync.WaitGroup
			for probe := range 8 {
				probes.Go(func() {
					for answer := range 30 {
						body, failing := good, false
						if (probe+answer)%2 == 0 {
							body, failing = bad, true
						}
						set, _, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, c, "application/xml", body)
						switch {
						case failing && (err == nil || !strings.Contains(err.Error(), "the XPath engine failed on it: contains() function argument type must be string")):
							t.Errorf("the answer the engine fails on gave %q, %v", htmlSeries(set), err)
						case !failing && (err != nil || !slices.Equal(htmlSeries(set), test.want)):
							t.Errorf("the good answer gave %q, %v; want %q", htmlSeries(set), err, test.want)
						}
					}
				})
			}
			probes.Wait()
		})
	}
}

// A runtime error says it is one in the rule's failure, a failed type
// assertion included, whose own words do not: such a failure is the
// engine's or the exporter's own code, and no matter of the expression's
// arguments.
func TestARuntimeErrorIsToldInTheRulesFailure(t *testing.T) {
	recovered := func(fail func()) (failed any) {
		defer func() { failed = recover() }()
		fail()
		return nil
	}
	rule := model.MetricRule{Name: "m", Expression: "//a", Labels: []model.LabelRule{{Name: "l", Expression: "name()"}}}
	var number any = 1.5
	for want, failed := range map[string]any{
		`metric "m" XPath "//a" cannot be evaluated: the XPath engine failed on it: runtime error: interface conversion: interface {} is float64, not string`: recovered(func() { _ = number.(string) }),
		`metric "m" XPath "//a" cannot be evaluated: the XPath engine failed on it: runtime error: index out of range [3] with length 0`:                      recovered(func() { _ = []int{}[len(rule.Name)+2] }),
		`metric "m" XPath "//a" cannot be evaluated: the XPath engine failed on it: matches() got error`:                                                      "matches() got error",
		`metric "m" XPath "//a" cannot be evaluated: the XPath engine failed on it: unknown item: 35`:                                                         errors.New("unknown item: 35"),
	} {
		if got := xpathEngineFailure("XPath", rule, planXPathLabels(rule, nil, false), failed).Error(); got != want {
			t.Errorf("the failure %q, want %q", got, want)
		}
	}
}

// transformXPathUnguarded is transformXPathNodes as it was before a panic of
// the engine was recovered: each rule evaluated without the deferred call.
func transformXPathUnguarded[N comparable](ctx context.Context, root N, nodes xpathNodes[N], rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		program, err := expr.CompileXPath(rule.Expression, nil)
		if err != nil {
			return nil, err
		}
		expression, plan := program.Get(), planXPathLabels(rule, nil, nodes.html)
		err = xpathRule(ctx, out, root, nodes, rule, c, plan, expression, program.Sums())
		releaseXPathLabels(plan)
		program.Put(expression)
		if err != nil {
			return nil, err
		}
	}
	return noSeriesIsNil(out), nil
}

// The recovery costs a rule the engine does not panic on no allocation: a
// transform makes as many as it made without it, and the same series,
// whatever its rules read — nodes, labels by a walk and by the engine,
// computed values and sums.
func TestRecoveringAnEnginePanicCostsNoAllocation(t *testing.T) {
	if raceDetector {
		t.Skip("allocations cannot be counted under the race detector")
	}
	var page strings.Builder
	page.WriteString(`<status site="fra1">`)
	for i := range 200 {
		fmt.Fprintf(&page, `<row id="%d"><v>%d</v><name>n%d</name></row>`, i, i, i)
	}
	page.WriteString(`</status>`)
	root := decodedXML(t, page.String())
	for name, rules := range map[string][]model.MetricRule{
		"no labels":         {{Name: "m", Expression: "//row/v"}},
		"labels by a walk":  {{Name: "m", Expression: "//row/v", Labels: []model.LabelRule{{Name: "id", Expression: "../@id"}, {Name: "n", Expression: "../name"}}}},
		"an engine's label": {{Name: "m", Expression: "//row/v", Labels: []model.LabelRule{{Name: "id", Expression: "concat(../@id, '-', ../name)"}}}},
		"computed values":   {{Name: "m", Expression: "count(//row)"}, {Name: "s", Expression: "sum(//row/v)"}, {Name: "p", Expression: "sum(//row/v) div count(//row)"}},
		"a sum in a label":  {{Name: "m", Expression: "//row/v", Labels: []model.LabelRule{{Name: "s", Expression: "sum(../v)"}}}},
	} {
		c := &model.Collector{Name: "cost", Metrics: rules}
		ctx := LeaveRuleLoggingToCaller(context.Background())
		var series, unguardedSeries []string
		guarded, _ := alloctest.Allocations(20, func() {
			set, err := transformXPathNodes(ctx, root, xmlNodes, rules, c, nil)
			if err != nil {
				t.Fatal(err)
			}
			series = append(series[:0], set.Metrics[0].Name, set.Metrics[len(set.Metrics)-1].Name)
		})
		unguarded := alloctest.AllocsAtMost(20, guarded, func() {
			set, err := transformXPathUnguarded(ctx, root, xmlNodes, rules, c)
			if err != nil {
				t.Fatal(err)
			}
			unguardedSeries = append(unguardedSeries[:0], set.Metrics[0].Name, set.Metrics[len(set.Metrics)-1].Name)
		})
		if guarded != unguarded || !slices.Equal(series, unguardedSeries) {
			t.Errorf("%s: %v allocations for the series %q, and %v for %q without the recovery", name, guarded, series, unguarded, unguardedSeries)
		}
		// And the series are the same ones.
		with, err := transformXPathNodes(ctx, root, xmlNodes, rules, c, nil)
		without, unguardedErr := transformXPathUnguarded(ctx, root, xmlNodes, rules, c)
		if err != nil || unguardedErr != nil || !slices.Equal(htmlSeries(with), htmlSeries(without)) || len(with.Metrics) == 0 {
			t.Errorf("%s: %d series, %v, and %d, %v, without the recovery, which differ", name, len(with.Metrics), err, len(without.Metrics), unguardedErr)
		}
	}
}

// The failures docs/CONFIGURATION.md quotes read as it quotes them, and the
// ways out it gives are ones: contains() with a string for its second
// argument has a value, a count compared has one where //a compared with
// true() has none, and number() of text that is no number is the rule's
// missing value.
func TestTheEngineFailuresTheDocumentationQuotes(t *testing.T) {
	const page = `<jobs><a x="5">1</a><v>n/a</v><job kind="batch"><up>1</up></job></jobs>`
	for want, rule := range map[string]model.MetricRule{
		`metric "marked" XPath "//a[contains(@x, 5)]" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`:                          {Name: "marked", Expression: "//a[contains(@x, 5)]"},
		`metric "v" XPath "sum(string(//v))" cannot be evaluated: the XPath engine failed on it: sum() function argument type must be a node-set or number`:                          {Name: "v", Expression: "sum(string(//v))"},
		`metric "job_up" label "kind": XPath "substring(../@kind, '1')" cannot be evaluated: the XPath engine failed on it: substring() function first argument type must be number`: {Name: "job_up", Expression: "//job/up", Labels: []model.LabelRule{{Name: "kind", Expression: "substring(../@kind, '1')"}}},
	} {
		rule.Type, rule.ErrorMode = model.GaugeMetricType, model.ErrorModeFail
		_, _, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, xpathRuleCollector("xml", rule), "application/xml", page)
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", rule.Expression, err, want)
		}
	}
	for expression, want := range map[string]string{"//a[contains(@x, '5')]": "m 1", "count(//a[contains(@none, '5')])": "m 0", "count(//a) > 0": "m 1", "count(//none) > 0": "m 0", "sum(translate(//a, ',', ''))": "m 1", "number(//v)": ""} {
		optional := false
		set, _, err := transformWith(context.Background(), t, xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression, ErrorMode: model.ErrorModeFail, Required: &optional}), "application/xml", page)
		if got := strings.Join(htmlSeries(set), "; "); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", expression, got, err, want)
		}
	}
}

// The engine's failure is the rule's failure of the scrape, alone: where
// nodes of the rule had failed before the engine did — a cell without a
// value, and one that is no number — the rule is reported once, with no
// missing value and with what the engine said, under log and under ignore,
// and not with the first of those nodes as on a day the engine did not fail.
// That text was the same on both days, so the log held the failure back as a
// repeat over a rule that had stopped giving any series. The failures of the
// rule before it and of the one after it are counted as they were. Under
// fail the first node that fails ends the scrape, on both days, before the
// engine comes to the one it fails on.
func TestAnEngineFailureTakesThePlaceOfTheNodesThatFailedBeforeIt(t *testing.T) {
	const (
		quiet    = `<html><body><table><tr><td h="a">1</td><td h="b"></td><td h="c">n/a</td><td h="d">3</td></tr></table></body></html>`
		stumbles = `<html><body><table><tr><td h="a">1</td><td h="b"></td><td h="c">n/a</td><td h="d" x="5">3</td><td h="e"></td></tr></table></body></html>`
	)
	for _, decoder := range []string{"xml", "html"} {
		kind := map[string]string{"xml": "XPath", "html": "HTML XPath"}[decoder]
		blank := fmt.Sprintf("metric %q value is missing for node 1: %s %q selected a node without a value", "load", kind, "//td")
		engine := fmt.Sprintf("metric %q label %q: %s %q cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string", "load", "marked", kind, "contains(@x, 5)")
		for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail} {
			c := model.Collector{Name: "table", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{
				{Name: "before", Type: model.GaugeMetricType, Expression: "//td", ErrorMode: model.ErrorModeLog},
				{Name: "load", Type: model.GaugeMetricType, Expression: "//td", ErrorMode: mode, Labels: []model.LabelRule{{Name: "host", Expression: "@h"}, {Name: "marked", Expression: "contains(@x, 5)"}}},
				{Name: "after", Type: model.GaugeMetricType, Expression: "//td", ErrorMode: model.ErrorModeIgnore},
			}}
			for _, day := range []struct {
				page           string
				load           []string
				count, missing uint64
				first          string
				around, missed uint64
				aroundSeries   int
				failsTheScrape string
			}{
				{page: quiet, load: []string{`load{host="a",marked="false"} 1`, `load{host="d",marked="false"} 3`}, count: 2, missing: 1, first: blank, around: 2, missed: 1, aroundSeries: 2, failsTheScrape: blank},
				{page: stumbles, count: 1, missing: 0, first: engine, around: 3, missed: 2, aroundSeries: 2, failsTheScrape: blank},
			} {
				set, failures, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, c, markupContentType[decoder], day.page)
				if mode == model.ErrorModeFail {
					var failure *MetricFailure
					if !errors.As(err, &failure) || failure.Metric != "load" || err.Error() != day.failsTheScrape {
						t.Errorf("%s under fail: the error %v, want the failure of the first node, %q", decoder, err, day.failsTheScrape)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s under %s: %v", decoder, mode, err)
				}
				var load []string
				for _, series := range htmlSeries(set) {
					if strings.HasPrefix(series, "load") {
						load = append(load, series)
					}
				}
				if !slices.Equal(load, day.load) || len(set.Metrics) != len(day.load)+2*day.aroundSeries {
					t.Errorf("%s under %s: the series %q, want of the rule %q and %d of each rule around it", decoder, mode, htmlSeries(set), day.load, day.aroundSeries)
				}
				if len(failures) != 3 {
					t.Fatalf("%s under %s: the failures %+v, want those of three rules", decoder, mode, failures)
				}
				if f := failures[1]; f.Metric != "load" || f.Failures != day.count || f.Missing != day.missing || f.First.Error() != day.first || f.Logged != (mode == model.ErrorModeLog) {
					t.Errorf("%s under %s: the rule's failure %+v, want %d, %d of them missing values, that reads %q", decoder, mode, f, day.count, day.missing, day.first)
				}
				for _, f := range []RuleFailure{failures[0], failures[2]} {
					if f.Failures != day.around || f.Missing != day.missed || !strings.Contains(f.First.Error(), "value is missing for node 1") {
						t.Errorf("%s under %s: the failure %+v of a rule around it, want %d, %d of them missing values", decoder, mode, f, day.around, day.missed)
					}
				}
			}
		}
		// The two days are two failures to the log, which tells them by
		// this text.
		if model.SameFailureText(errors.New(blank)) == model.SameFailureText(errors.New(engine)) {
			t.Errorf("%s: the two failures read the same", decoder)
		}
	}
}

// What a runtime error of the engine says after "runtime error:" moves with
// the response: substring() with a length past the end of the text gives the
// bounds it sliced at, which are other numbers for a text of another length.
// The failure reads in full, numbers and all, and is recognised by its text
// up to there, so the two are one failure to the log; this holds for a rule
// and for a label, under log and under fail, and for a failed type
// assertion. What the engine raised itself is recognised by all it says.
func TestAnEnginesRuntimeErrorIsOneFailureWhateverItsNumbers(t *testing.T) {
	for name, test := range map[string]struct {
		rule       model.MetricRule
		said, same string
	}{
		"a rule": {
			model.MetricRule{Name: "m", Expression: "substring(//a, 2, 10)"},
			`metric "m" XPath "substring(//a, 2, 10)" cannot be evaluated: the XPath engine failed on it: runtime error: slice bounds out of range [:%d] with length %d`,
			`metric "m" XPath "substring(//a, 2, 10)" cannot be evaluated: the XPath engine failed on it: runtime error:`,
		},
		"a label": {
			model.MetricRule{Name: "m", Expression: "count(//a)", Labels: []model.LabelRule{{Name: "l", Expression: "substring(//a, 2, 10)"}}},
			`metric "m" label "l": XPath "substring(//a, 2, 10)" cannot be evaluated: the XPath engine failed on it: runtime error: slice bounds out of range [:%d] with length %d`,
			`metric "m" label "l": XPath "substring(//a, 2, 10)" cannot be evaluated: the XPath engine failed on it: runtime error:`,
		},
	} {
		for _, mode := range []string{model.ErrorModeLog, model.ErrorModeFail} {
			test.rule.Type, test.rule.ErrorMode = model.GaugeMetricType, mode
			var recognised []string
			for _, text := range []string{"ab", "abcde"} {
				_, failures, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, xpathRuleCollector("xml", test.rule), "application/xml", "<a>"+text+"</a>")
				if mode == model.ErrorModeLog {
					if err != nil || len(failures) != 1 {
						t.Fatalf("%s under log over %q: %v and the failures %+v", name, text, err, failures)
					}
					err = failures[0].First
				}
				if want := fmt.Sprintf(test.said, len(text)+1, len(text)); err == nil || err.Error() != want {
					t.Errorf("%s under %s over %q: the failure %v, want %q", name, mode, text, err, want)
				}
				recognised = append(recognised, model.SameFailureText(err))
			}
			if recognised[0] != test.same || recognised[1] != test.same {
				t.Errorf("%s under %s: the failures are recognised by %q, want both by %q", name, mode, recognised, test.same)
			}
		}
	}
	rule := model.MetricRule{Name: "m", Expression: "//a"}
	var number any = 1.5
	asserted := func() (failed any) {
		defer func() { failed = recover() }()
		_ = number.(string)
		return nil
	}()
	for same, failed := range map[string]any{
		`metric "m" XPath "//a" cannot be evaluated: the XPath engine failed on it: runtime error:`:                                           asserted,
		`metric "m" XPath "//a" cannot be evaluated: the XPath engine failed on it: contains() function argument type must be string`:         "contains() function argument type must be string",
		`metric "m" XPath "//a" cannot be evaluated: the XPath engine failed on it: runtime error: this text was raised and is no such error`: errors.New("runtime error: this text was raised and is no such error"),
	} {
		failure := xpathEngineFailure("XPath", rule, nil, failed)
		if got := model.SameFailureText(failure); got != same || !strings.HasPrefix(failure.Error(), same) {
			t.Errorf("the failure %q is recognised by %q, want by %q", failure, got, same)
		}
	}
}

// engineLogs transforms a document with a rule's failures logged to a
// logger of the given level, and gives back the records it wrote.
func engineLogs(t *testing.T, level slog.Level, ctx context.Context, rule model.MetricRule, page string) []map[string]any {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: level}))
	if _, _, err := transformWith(WithRuleLogger(ctx, logger), t, xpathRuleCollector("xml", rule), "application/xml", page); err != nil && rule.ErrorMode != model.ErrorModeFail {
		t.Fatal(err)
	}
	var records []map[string]any
	for line := range strings.Lines(logs.String()) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// A runtime error may be a fault of the exporter's own code, and under
// ignore nothing is logged of the rule's failure: its stack is logged, at
// debug level and nowhere else, one line a failure, with the collector, the
// metric and the failure, whatever the rule's error_mode and whether or not
// the caller logs the rules' failures itself. The failure's text has no
// stack, at info level there is none in the log, and a failure the engine
// raised itself, which is the expression's, has none at any level.
func TestTheStackOfARuntimeErrorIsLoggedAtDebugLevelOnly(t *testing.T) {
	const page = `<r><a x="5">ab</a></r>`
	stacks := func(records []map[string]any) (lines []map[string]any) {
		for _, record := range records {
			text, _ := json.Marshal(record)
			if !strings.Contains(string(text), "goroutine ") {
				continue
			}
			if failure, _ := record["error"].(string); strings.Contains(failure, "goroutine ") || strings.Contains(failure, ".go:") {
				t.Errorf("the failure's text has a stack: %s", text)
			}
			lines = append(lines, record)
		}
		return lines
	}
	for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeLog, model.ErrorModeFail} {
		for name, ctx := range map[string]context.Context{"the transform logs": context.Background(), "the caller logs": LeaveRuleLoggingToCaller(context.Background())} {
			runtimeError := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "substring(//a, 2, 10)", ErrorMode: mode}
			lines := stacks(engineLogs(t, slog.LevelDebug, ctx, runtimeError, page))
			if len(lines) != 1 {
				t.Fatalf("under %s, %s: %d lines with a stack at debug level, want one", mode, name, len(lines))
			}
			line := lines[0]
			stack, _ := line["stack"].(string)
			if line["level"] != "DEBUG" || line["msg"] != "the XPath engine failed with a runtime error" || line["collector"] != "attributes" || line["metric"] != "m" ||
				line["error"] != `metric "m" XPath "substring(//a, 2, 10)" cannot be evaluated: the XPath engine failed on it: runtime error: slice bounds out of range [:3] with length 2` ||
				!strings.Contains(stack, "antchfx/xpath") || !strings.Contains(stack, "evaluateXPathRule") {
				t.Errorf("under %s, %s: the line %v, want the failure and the stack of the panic at debug level", mode, name, line)
			}
			if lines := stacks(engineLogs(t, slog.LevelInfo, ctx, runtimeError, page)); len(lines) != 0 {
				t.Errorf("under %s, %s: a stack at info level: %v", mode, name, lines)
			}
			raised := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//a[contains(@x, 5)]", ErrorMode: mode}
			if lines := stacks(engineLogs(t, slog.LevelDebug, ctx, raised, page)); len(lines) != 0 {
				t.Errorf("under %s, %s: a stack for a failure the engine raised itself: %v", mode, name, lines)
			}
		}
	}
}

// xpathEngineFailureBefore is xpathEngineFailure as it was before a runtime
// error was recognised by the start of its text.
func xpathEngineFailureBefore(kind string, rule model.MetricRule, plan []xpathLabel, failed any) error {
	said := fmt.Sprint(failed)
	if _, ours := failed.(runtime.Error); ours && !strings.HasPrefix(said, "runtime error: ") {
		said = "runtime error: " + said
	}
	for i := range plan {
		if plan[i].evaluating {
			return fmt.Errorf("metric %q label %q: %s %q cannot be evaluated: the XPath engine failed on it: %s", rule.Name, plan[i].name, kind, rule.Labels[i].Expression, said)
		}
	}
	return fmt.Errorf("metric %q %s %q cannot be evaluated: the XPath engine failed on it: %s", rule.Name, kind, rule.Expression, said)
}

// The failure of a rule the engine panicked on reads as it read, to the
// letter, whatever the engine panicked with — a string, an error, a runtime
// error that says it is one and one that does not, a value of another kind —
// for a rule's expression and for each of its labels, over XML and HTML; and
// but for a runtime error it is recognised by the same text as before, all
// of it.
func TestAnEngineFailureReadsAsItDid(t *testing.T) {
	recovered := func(fail func()) (failed any) {
		defer func() { failed = recover() }()
		fail()
		return nil
	}
	rule := model.MetricRule{Name: "m", Expression: "//a", Labels: []model.LabelRule{{Name: "site", Value: "fra1"}, {Name: "l", Expression: "name()"}, {Name: "k", Expression: "contains(@x, 5)"}}}
	var number any = 1.5
	var none *model.MetricRule
	for _, failed := range []any{
		"contains() function argument type must be string", "", "runtime error: said by the engine", errors.New("unknown item: 35"), fmt.Errorf("wrapped: %w", errors.New("inner")), 35, nil,
		recovered(func() { _ = number.(string) }), recovered(func() { _ = []int{}[len(rule.Name)+2] }), recovered(func() { _ = none.Name }), recovered(func() { _ = "ab"[:len(rule.Name)+2] }),
	} {
		for _, kind := range []string{"XPath", "HTML XPath"} {
			for evaluating := -1; evaluating < len(rule.Labels); evaluating++ {
				plan := planXPathLabels(rule, nil, kind == "HTML XPath")
				if evaluating >= 0 {
					plan[evaluating].evaluating = true
				}
				before, now := xpathEngineFailureBefore(kind, rule, plan, failed), xpathEngineFailure(kind, rule, plan, failed)
				if before.Error() != now.Error() {
					t.Errorf("%v: the failure reads %q, and read %q", failed, now, before)
				}
				if _, runtimeError := failed.(runtime.Error); !runtimeError && model.SameFailureText(now) != model.SameFailureText(before) {
					t.Errorf("%v: the failure is recognised by %q, and was by %q", failed, model.SameFailureText(now), model.SameFailureText(before))
				}
			}
		}
	}
}
