//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An XPath expression the engine would read only the start of is refused
// when the configuration is loaded, for a rule and for a label, over an XML
// and over an HTML decoder and with response.namespaces set, with an error
// that names the collector, the metric, the label and the place: such a
// rule loaded, and was evaluated as the part before the place.
func TestAnXPathExpressionReadOnlyInPartIsRefusedAtLoad(t *testing.T) {
	for _, test := range []struct {
		name string
		rule model.MetricRule
		want string
	}{
		{"a parenthesis too many", model.MetricRule{Name: "m", Expression: "sum(//a))"},
			`collector "checked" metric "m" XPath "sum(//a))": the ")" at byte 9 closes nothing`},
		{"a bracket too many", model.MetricRule{Name: "m", Expression: "sum(//a)] + 1"},
			`collector "checked" metric "m" XPath "sum(//a)] + 1": the "]" at byte 9 closes nothing`},
		{"a string after a path", model.MetricRule{Name: "m", Expression: "//a 'x'"},
			`collector "checked" metric "m" XPath "//a 'x'": what stands from byte 5 on, "'x'", is no part of the expression before it, which is all the XPath engine would read; join the two with an operator, or take it out`},
		{"a label", model.MetricRule{Name: "m", Expression: "//a", Labels: []model.LabelRule{{Name: "l", Expression: "string(../@id))"}}},
			`collector "checked" metric "m" label "l" XPath "string(../@id))": the ")" at byte 15 closes nothing`},
		{"a label with two expressions", model.MetricRule{Name: "m", Expression: "//a", Labels: []model.LabelRule{{Name: "l", Expression: "../b ../c"}}},
			`collector "checked" metric "m" label "l" XPath "../b ../c": what stands from byte 6 on, "../c", is no part of the expression before it`},
	} {
		for _, decoder := range []string{"xml", "html", ""} {
			for _, namespaces := range []map[string]string{nil, {"p": "urn:p"}} {
				if decoder == "html" && namespaces != nil {
					continue
				}
				test.rule.Type = model.GaugeMetricType
				c := ruleCollector("xpath", test.rule)
				c.Decoder.Type, c.Response.Namespaces = decoder, namespaces
				if err := validateOne(c); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Errorf("%s, decoder %q, namespaces %v: %v, want an error with %q", test.name, decoder, namespaces, err, test.want)
				}
			}
		}
	}
	// One the engine reads to its end loads, brackets in its strings and all.
	whole := ruleCollector("xpath", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "sum(//a[. != ')'][@k = \"]\"])", Labels: []model.LabelRule{{Name: "l", Expression: "concat('(', ../@id, ']')"}}})
	if err := validateOne(whole); err != nil {
		t.Errorf("an expression read to its end: %v", err)
	}
}
