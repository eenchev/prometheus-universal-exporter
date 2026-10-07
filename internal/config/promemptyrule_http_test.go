//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// prometheusRules is a configuration of one prometheus collector, node,
// with the rules as they are written, each a line of a list.
func prometheusRules(rules string) string {
	return "collectors:\n  - name: node\n    request:\n      type: http\n    transform:\n      type: prometheus\n    metrics:\n" + rules
}

// A prometheus rule passes on the metrics whose names its expression
// matches, or, without one, the metric its name names. A rule with neither
// loaded: it matched no metric
// (transform.TestAPrometheusRuleOfNeitherANameNorAnExpressionMatchesNoMetric),
// so a required one was reported as missing on every scrape, as if the
// target had left a metric out, and one with required: false did nothing
// without a word. The load refuses it now, whatever else the rule sets,
// each key left out or written "", which is the key left out. The rule has
// no name to be named by, so the error names the collector and the rule's
// place among the collector's, counted from 1, and says what a prometheus
// rule needs. A rule with either key loads as it did.
func TestAPrometheusRuleOfNeitherANameNorAnExpressionIsRefusedAtLoad(t *testing.T) {
	for rules, place := range map[string]int{
		"      - {}\n":                               0,
		"      - name: \"\"\n":                       0,
		"      - expression: \"\"\n":                 0,
		"      - name: ''\n        expression: ''\n": 0,
		"      - type: counter\n":                    0,
		"      - description: Whether it is up.\n":   0,
		"      - required: true\n":                   0,
		"      - required: false\n":                  0,
		"      - error_mode: ignore\n":               0,
		"      - scale: 2\n":                         0,
		"      - labels:\n          - name: site\n            value: rack1\n":                        0,
		"      - labels:\n          - name: site\n            expression: instance\n":                0,
		"      - type: gauge\n        description: Up.\n        required: false\n        scale: 2\n": 0,
		"      - name: up\n      - type: counter\n":                                                  1,
		"      - expression: '^node_'\n      - name: up\n      - labels: [{name: site, value: x}]\n": 2,
		"      - name: up\n      - {}\n      - expression: '^node_'\n":                               1,
	} {
		want := ruleOfNeither("node", place)
		if _, err := Load(testutil.WriteFile(t, "config.yaml", prometheusRules(rules))); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%q: %v\nwant it refused with %s", rules, err, want)
		}
	}
	// Each such rule of a collector is named, by its place, in order and
	// beside what the other rules are refused for.
	_, err := Load(testutil.WriteFile(t, "config.yaml", prometheusRules("      - {}\n      - name: up\n      - name: bad-name\n      - required: false\n")))
	var problems model.Problems
	if !errors.As(err, &problems) || len(problems) != 3 {
		t.Fatalf("two rules of neither and one with a name that is none: %v", err)
	}
	for i, want := range []string{ruleOfNeither("node", 0), `collector "node" metric "bad-name": "bad-name" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped`, ruleOfNeither("node", 3)} {
		if !strings.HasSuffix(problems[i].Error(), want) {
			t.Errorf("problem %d is %v\nwant %s", i+1, problems[i], want)
		}
	}
	// In a collector file it is named the same way.
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "collectors.yaml", prometheusRules("      - name: up\n      - type: counter\n"))
	if _, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n")); err == nil || !strings.HasSuffix(err.Error(), ruleOfNeither("node", 1)) {
		t.Errorf("in a collector file: %v", err)
	}
	// A rule of neither that the load refused for something else is refused
	// for that, in the words it was but for the rule's place, which names
	// it where `metric ""` did, and is not told the rest until it loads
	// that far.
	for rules, want := range map[string]string{
		"      - type: timer\n":                     `collector "node" metrics rule 1 has invalid type "timer"`,
		"      - error_mode: panic\n":               `collector "node" metrics rule 1 error_mode has invalid value "panic"`,
		"      - name: \"  \"\n":                    `collector "node" metrics rule 1: "  " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`,
		"      - expression: \"  \"\n":              `leave expression out for the rule to pass on the metric its name names`,
		"      - items: .rows[]\n":                  `collector "node" metrics rule 1 sets items, which only the jq, yq and css transforms support`,
		"      - labels:\n          - name: site\n": `collector "node" metrics rule 1 label "site" needs a value, for a static label, or an expression, to read it from the response`,
		"      - labels:\n          - name: site\n            expression: site\n            value_map: {a: b}\n": `collector "node" metrics rule 1 label "site" sets value_map on a rule without a name; name the rule, so its series are known`,
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", prometheusRules(rules)))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "has neither a name nor an expression") {
			t.Errorf("%q: %v\nwant it refused with %s alone", rules, err, want)
		}
	}
	// With one of the two the rule says which metrics it is about, and
	// loads; so does a collector that passes everything on, having no rule.
	for _, rules := range []string{
		"      - name: up\n", "      - expression: '^node_'\n", "      - name: load\n        expression: '^node_load1$'\n", "      - expression: '.*'\n        type: counter\n",
		"      - name: up\n        expression: \"\"\n", "      - name: \"\"\n        expression: '^up$'\n", "      - expression: 0\n", "      - name: up\n      - name: up\n        labels: [{name: site, value: x}]\n",
		"      - expression: '^$'\n", "      []\n",
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", prometheusRules(rules))); err != nil {
			t.Errorf("%q: %v", rules, err)
		}
	}
	if _, err := Load(testutil.WriteFile(t, "config.yaml", strings.TrimSuffix(prometheusRules(""), "    metrics:\n"))); err != nil {
		t.Errorf("a prometheus collector without metrics: %v", err)
	}
}

// After that check no transform loads a rule that says nothing of what it
// reads. Under each of the eight, a rule of nothing, of a name alone, of an
// expression alone and of both is loaded, alone and as the second rule of
// its collector. jq, yq, xpath, css, regex and csv need both keys, and say
// which is missing. A python rule names a series of the script and needs
// its name, its expression being read by nothing. A prometheus rule needs
// one of the two. A rule without a name has none to be told of by, so each
// of them says which rule of the collector it is, by its place.
func TestNoTransformLoadsARuleThatSaysNothingOfWhatItReads(t *testing.T) {
	const noName, loads = `collector "node" metrics rule %d has no name`, ""
	const pythonNoName = `collector "node" metrics rule %d ` + pythonRuleName
	expressions := map[string]string{"jq": ".v", "yq": ".v", "xpath": "//v", "css": "td.v", "regex": `'v=(\d+)'`, "csv": "v", "prometheus": "'^v$'", "python": ".v"}
	type verdicts struct{ nothing, name, expression, both string }
	needsBoth := verdicts{noName, `collector "node" metric "v" has no expression`, noName, loads}
	table := map[string]verdicts{
		"jq": needsBoth, "yq": needsBoth, "xpath": needsBoth, "css": needsBoth, "regex": needsBoth, "csv": needsBoth,
		"prometheus": {nothing: "collector \"node\" metrics rule %d " + prometheusRuleOfNeither},
		"python":     {nothing: pythonNoName, expression: pythonNoName},
	}
	if len(table) != len(expressions) || len(table) != 8 {
		t.Fatalf("%d transforms in the table", len(table))
	}
	for transform, want := range table {
		document := func(first, rule string) string {
			script := ""
			if transform == "python" {
				script = "      script: metric('v', 'gauge', 1)\n"
			}
			return "collectors:\n  - name: node\n    request:\n      type: http\n    transform:\n      type: " + transform + "\n" + script + "    metrics:\n" + first + "      - " + rule
		}
		expression := "expression: " + expressions[transform] + "\n"
		sound := "      - name: w\n        " + expression
		for what, test := range map[string]struct{ rule, want string }{
			"nothing":             {"{}\n", want.nothing},
			"a description alone": {"description: A value.\n", want.nothing},
			"a name alone":        {"name: v\n", want.name},
			"an expression alone": {expression, want.expression},
			"both":                {"name: v\n        " + expression, want.both},
		} {
			for place, first := range []string{"", sound} {
				// A message about a rule without a name names its place.
				message := strings.Replace(test.want, "%d", []string{"1", "2"}[place], 1)
				_, err := Load(testutil.WriteFile(t, "config.yaml", document(first, test.rule)))
				switch {
				case message == loads && err != nil:
					t.Errorf("a %s rule of %s, rule %d: %v", transform, what, place+1, err)
				case message != loads && (err == nil || !slices.Contains(strings.Split(err.Error(), "\n"), message)):
					t.Errorf("a %s rule of %s, rule %d: %v\nwant it refused with %s", transform, what, place+1, err, message)
				}
			}
		}
	}
}
