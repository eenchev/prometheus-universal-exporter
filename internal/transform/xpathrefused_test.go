package transform

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The expressions the engine would read only the start of in the three ways
// found after the first — a NUL byte, a predicate after parentheses within a
// path, and something after an expression as deep as the parser reads — are
// refused when the rule is checked at load, as a rule's expression and as a
// label's, over XML and over HTML and with namespaces, with an error that
// names the collector, the metric and the label and says what to change.
func TestXPathExpressionsReadOnlyInPartAreRefusedWhenARuleIsChecked(t *testing.T) {
	deep := strings.Repeat("(", 199) + "//a" + strings.Repeat(")", 199) + " 'x'"
	for expression, want := range map[string]string{
		"//td\x00 | //zz 'junk' )": `XPath "//td\x00 | //zz 'junk' )": it has a NUL character at byte 5, which the XPath engine takes for the end of the expression; take it out`,
		"//a/(b)[1]":               `XPath "//a/(b)[1]": what stands from byte 8 on, "[1]", would be ignored: the XPath engine reads no predicate after parentheses within a path; write the predicate inside them, after the step it is for, as in "//a/(b[1])"`,
		deep:                       `'x'": it is nested too deeply: parentheses, predicates and function arguments may stand 198 deep within one another; write it less deep`,
	} {
		for _, decoder := range []string{"xml", "html"} {
			for _, namespaces := range []map[string]string{nil, {"p": "urn:p"}} {
				if decoder == "html" && namespaces != nil {
					continue
				}
				c := xpathRuleCollector(decoder, model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression})
				c.Response.Namespaces = namespaces
				if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.HasPrefix(err.Error(), `collector "attributes" metric "m" XPath "`) || !strings.HasSuffix(err.Error(), want) {
					t.Errorf("%.30q over %s: %v, want the rule's refusal ending %q", expression, decoder, err, want)
				}
				c = xpathRuleCollector(decoder, model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//a", Labels: []model.LabelRule{{Name: "l", Expression: expression}}})
				c.Response.Namespaces = namespaces
				if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.HasPrefix(err.Error(), `collector "attributes" metric "m" label "l" XPath "`) || !strings.HasSuffix(err.Error(), want) {
					t.Errorf("label %.30q over %s: %v, want the label's refusal ending %q", expression, decoder, err, want)
				}
			}
		}
	}
	// The forms the refusals name load.
	for _, expression := range []string{"//a/(b[1])", "//td | //zz", strings.Repeat("(", 198) + "//a" + strings.Repeat(")", 198)} {
		c := xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression, Labels: []model.LabelRule{{Name: "l", Expression: expression}}})
		if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
			t.Errorf("%.30q: %v", expression, err)
		}
	}
}

// A second predicate after an expression in parentheses, a function call or
// a literal is read and evaluated by the engine (antchfx/xpath from v1.3.9),
// and was refused at load while the engine ignored it: a rule and a label
// with chained predicates now load, over XML and over HTML and with
// namespaces, and give what the predicates select — the second cell of
// those with an x, not the first, nothing where the last predicate holds for
// no node — and a label's chained predicates do the same.
func TestChainedPredicatesAfterParenthesesLoadAndSelect(t *testing.T) {
	const page = `<html><body><table><tr><td x="a">1</td><td>2</td><td x="b">3</td><td x="c">4</td></tr></table></body></html>`
	for _, decoder := range []string{"xml", "html"} {
		for _, namespaces := range []map[string]string{nil, {"p": "urn:p"}} {
			if decoder == "html" && namespaces != nil {
				continue
			}
			for expression, want := range map[string][]string{
				"(//td)[@x][2]":                      {`second{last="c",x="b"} 3`},
				"(//td)[@x] [2] [. = 3]":             {`second{last="c",x="b"} 3`},
				"(//td)[position() > 1][@x][last()]": {`second{last="c",x="c"} 4`},
				"sum((//td)[@x][position() > 1])":    {`second{last="c"} 7`},
				"(//td)[@x][2][. = 4]":               nil,
			} {
				c := xpathRuleCollector(decoder, model.MetricRule{Name: "second", Type: model.GaugeMetricType, Expression: expression, ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{
					{Name: "x", Expression: "string(@x)"},
					{Name: "last", Expression: "string((//td)[@x][3][. = 4]/@x)"},
				}})
				c.Response.Namespaces = namespaces
				if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
					t.Errorf("%s over %s: the load refuses it: %v", expression, decoder, err)
					continue
				}
				set, _, err := transformWith(context.Background(), t, c, markupContentType[decoder], page)
				if got := htmlSeries(set); err != nil || !slices.Equal(got, want) {
					t.Errorf("%s over %s with namespaces %v: the series %q and the error %v, want %q", expression, decoder, namespaces, got, err, want)
				}
			}
		}
	}
}
