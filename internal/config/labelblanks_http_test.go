//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// labelCollector is a configuration of one http collector of a transform,
// with one rule of that transform and the label as it is written.
func labelCollector(transform, label string) string {
	rule := map[string]string{
		"jq": "expression: .v", "yq": "expression: .v", "xpath": "expression: //v", "css": "items: tr\n        expression: td.v",
		"regex": `expression: 'v=(\d+) site=(\w+)'`, "csv": "expression: cpu", "prometheus": "expression: '^up$'", "python": "expression: cpu",
	}[transform]
	script := ""
	if transform == "python" {
		script = "      script: metric('cpu', 'gauge', 1)\n"
	}
	return "collectors:\n  - name: racks\n    request:\n      type: http\n    transform:\n      type: " + transform + "\n" + script +
		"    metrics:\n      - name: cpu\n        " + rule + "\n        labels:\n          - name: site\n" + label
}

// A label is a constant, value, or is read from the response, expression,
// and the loader refuses one that sets both. An expression of nothing but
// blanks beside a value passed for no expression there, and was one to
// everything after: the label was not static, so the value was never
// exported, and this csv collector loaded and read the column whose name is
// the two blanks. From the response "cpu,  \n1,rack\n", whose second column
// has that name, it exported
//
//	cpu{site="rack"} 1
//
// from "cpu,  \n1,\n", where the column is empty, cpu 1 without the label,
// and from "cpu\n1\n" nothing, the rule failing for a column that is not in
// the response; never cpu{site="x"} 1. The load refuses it now, naming the
// collector, the metric and the label, and saying what to write.
func TestACSVLabelOfAValueAndAnExpressionOfBlanksIsRefused(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector("csv", "            value: x\n            expression: \"  \"\n")))
	const want = `collector "racks" metric "cpu" label "site" expression "  " is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant`
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("Load: %v, want it refused with %s", err, want)
	}
	// Without the expression, or with it written "", which is the key left
	// out, the label is the constant.
	for _, label := range []string{"            value: x\n", "            value: x\n            expression: \"\"\n"} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector("csv", label)))
		if err != nil {
			t.Fatalf("%q: %v", label, err)
		}
		if site := cfg.Collectors[0].Metrics[0].Labels[0]; !site.Static() || site.Value != "x" {
			t.Fatalf("%q: the label loads as %+v", label, site)
		}
	}
}

// A label's expression of nothing but blanks is refused under every
// transform, in the same words, whether a value is beside it, or none, or
// one written "": blanks are neither the key left out nor an expression.
// Before, the label beside a value was refused under jq and yq for a missing
// query, under xpath for an expression that gives no node-set, under css for
// a selector that is not there and under regex for a capture group the
// regex does not have, each naming the transform's language and not the
// mistake; it loaded under csv, which read the column of that name, under
// prometheus, which read the source label of that name and left the
// constant off, and under python, whose script sets the labels. Alone, it
// was refused as a label that needs a value or an expression.
func TestALabelExpressionOfBlanksIsRefusedUnderEveryTransform(t *testing.T) {
	for _, transform := range []string{"jq", "yq", "xpath", "css", "regex", "csv", "prometheus", "python"} {
		for written, blanks := range map[string]string{`"  "`: "  ", `" "`: " ", `"\t"`: "\t", `" \n "`: " \n ", `"\u00A0"`: "\u00A0", `"\u3000 "`: "\u3000 ", `'  '`: "  "} {
			for _, label := range []string{
				"            value: x\n            expression: " + written + "\n",
				"            expression: " + written + "\n",
				"            expression: " + written + "\n            value: \"\"\n",
				"            value: \" \"\n            expression: " + written + "\n",
				"            expression: " + written + "\n            required: true\n",
			} {
				_, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector(transform, label)))
				want := fmt.Sprintf(`collector "racks" metric "cpu" label "site" expression %q is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant`, blanks)
				if err == nil || !strings.HasSuffix(err.Error(), want) {
					t.Errorf("%s, %q: %v, want it refused with %s", transform, label, err, want)
				}
			}
		}
	}
}

// What is not an expression of blanks is taken as it was. A value of
// blanks is a constant of blanks, exported as written, which Prometheus
// keeps: only an empty value is no label to it. An expression with blanks
// around it is the expression as written, and a character that only looks
// like a blank is one too, for the transform's language to take or refuse.
// A label with neither key, or with both written "", still needs one of
// them, and one with a value and an expression still has one too many.
func TestALabelThatIsNotAnExpressionOfBlanksLoadsAsItDid(t *testing.T) {
	for label, want := range map[string]struct{ value, expression string }{
		"            value: \"  \"\n":                               {"  ", ""},
		"            value: \"\\t\"\n":                              {"\t", ""},
		"            value: \" x \"\n":                              {" x ", ""},
		"            value: \"  \"\n            expression: \"\"\n": {"  ", ""},
		"            expression: \" .site \"\n":                     {"", " .site "},
		"            expression: .site\n            value: \"\"\n":  {"", ".site"},
	} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector("jq", label)))
		if err != nil {
			t.Errorf("%q: %v", label, err)
			continue
		}
		if site := cfg.Collectors[0].Metrics[0].Labels[0]; site.Value != want.value || site.Expression != want.expression || site.Static() != (want.expression == "") {
			t.Errorf("%q: the label loads as %+v", label, site)
		}
	}
	for label, want := range map[string]string{
		"            truncate: false\n":                               `label "site" needs a value, for a static label, or an expression, to read it from the response`,
		"            value: \"\"\n            expression: \"\"\n":     `label "site" needs a value, for a static label, or an expression, to read it from the response`,
		"            value: x\n            expression: .site\n":       `label "site" sets both value and expression`,
		"            value: \"  \"\n            expression: .site\n":  `label "site" sets both value and expression`,
		"            expression: \"\\u200B\"\n":                       `label "site" expression "\u200b"`,
		"            value: x\n            expression: \"\\uFEFF\"\n": `label "site" sets both value and expression`,
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector("jq", label))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want it refused with %s", label, err, want)
		}
	}
}
