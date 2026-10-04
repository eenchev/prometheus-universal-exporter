package transform

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The expressions the engine would read only the start of in the three ways
// found after the first — a NUL byte, a second predicate after parentheses,
// and something after an expression as deep as the parser reads — are
// refused when the rule is checked at load, as a rule's expression and as a
// label's, over XML and over HTML and with namespaces, with an error that
// names the collector, the metric and the label and says what to change.
func TestXPathExpressionsReadOnlyInPartAreRefusedWhenARuleIsChecked(t *testing.T) {
	deep := strings.Repeat("(", 199) + "//a" + strings.Repeat(")", 199) + " 'x'"
	for expression, want := range map[string]string{
		"//td\x00 | //zz 'junk' )": `XPath "//td\x00 | //zz 'junk' )": it has a NUL character at byte 5, which the XPath engine takes for the end of the expression; take it out`,
		"(//a)[@x][1]":             `XPath "(//a)[@x][1]": what stands from byte 10 on, "[1]", would be ignored: the XPath engine reads only one predicate after an expression in parentheses, a function call or a literal; for a second one, put the expression and its first predicate in parentheses of their own, as "((//a)[@x])[1]"`,
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
	for _, expression := range []string{"((//a)[@x])[1]", "//td | //zz", strings.Repeat("(", 198) + "//a" + strings.Repeat(")", 198)} {
		c := xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression, Labels: []model.LabelRule{{Name: "l", Expression: expression}}})
		if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
			t.Errorf("%.30q: %v", expression, err)
		}
	}
}
