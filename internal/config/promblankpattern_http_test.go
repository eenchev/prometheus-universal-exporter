//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A prometheus rule's expression of nothing but blanks loaded: it is a
// regular expression, which matched only the metric names that hold those
// blanks, so the rule passed nothing on
// (transform.TestAPrometheusRulesExpressionOfBlanksIsRefused). The load
// refuses it now, with a name beside it and without, required or not,
// naming the collector and the metric, or the rule's place when it has no
// name, and giving the advice an entry of transform.include gets. Written "" the expression is the key left out,
// and the rule matches the metric of its name, as it did.
func TestAPrometheusRulesExpressionOfBlanksIsRefusedAtLoad(t *testing.T) {
	rule := func(keys string) string {
		return "collectors:\n  - name: node\n    request:\n      type: http\n    transform:\n      type: prometheus\n    metrics:\n      - " + keys
	}
	const advice = `is nothing but blanks; a prometheus rule's expression is a regular expression matched against a metric's name as the target gives it, anywhere in it, so this one matches only the names that hold these blanks: write the pattern that was meant, or, for one that does mean a blank, '[ ]' or '\x20' in single quotes, or leave expression out for the rule to pass on the metric its name names`
	for keys, want := range map[string]string{
		"expression: \"  \"\n":                                         `collector "node" metrics rule 1 expression "  " ` + advice,
		"name: up\n        expression: \" \"\n":                        `collector "node" metric "up" expression " " ` + advice,
		"name: up\n        expression: \"\\t\"\n":                      `collector "node" metric "up" expression "\t" ` + advice,
		"name: up\n        expression: \"\\u00A0\"\n":                  `collector "node" metric "up" expression "\u00a0" ` + advice,
		"name: up\n        required: false\n        expression: ' '\n": `collector "node" metric "up" expression " " ` + advice,
		"name: up\n        expression: \"\\x20\"\n":                    `collector "node" metric "up" expression " " ` + advice,
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", rule(keys))); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%q: %v\nwant it refused with %s", keys, err, want)
		}
	}
	for _, keys := range []string{
		"name: up\n", "name: up\n        expression: \"\"\n", "expression: '^up$'\n", "name: free\n        expression: '[ ]'\n", "name: free\n        expression: '\\x20'\n",
		"name: free\n        expression: 'disk free'\n", "name: free\n        expression: ' free'\n", "name: free\n        expression: \"\\u200B\"\n",
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", rule(keys))); err != nil {
			t.Errorf("%q: %v", keys, err)
		}
	}
	// A prometheus rule's name of nothing but blanks is no metric's name,
	// as it was not: the rule would rename what it matches to it. It is
	// no name to tell the rule by either, which its place does.
	if _, err := Load(testutil.WriteFile(t, "config.yaml", rule("name: \"  \"\n        expression: '^up$'\n"))); err == nil || !strings.HasSuffix(err.Error(), `collector "node" metrics rule 1: "  " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`) {
		t.Errorf("a prometheus rule's name of blanks: %v", err)
	}
	// Under every other transform that reads an expression, one of nothing
	// but blanks is no expression, as it was; a python rule's is not read.
	for _, transform := range []string{"jq", "yq", "xpath", "css", "regex", "csv"} {
		document := strings.NewReplacer("type: prometheus", "type: "+transform).Replace(rule("name: cpu\n        expression: \"  \"\n"))
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.HasSuffix(err.Error(), `collector "node" metric "cpu" has no expression`) {
			t.Errorf("%s: %v", transform, err)
		}
	}
	if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule("name: cpu\n        expression: \"  \"\n"))); err != nil {
		t.Errorf("a python rule's expression of blanks, which is not read: %v", err)
	}
}
