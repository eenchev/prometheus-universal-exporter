//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// pythonRule is a configuration of one python collector whose script makes
// the series cpu, with one rule of the keys as they are written.
func pythonRule(keys string) string {
	return "collectors:\n  - name: racks\n    request:\n      type: http\n    transform:\n      type: python\n      script: metric('cpu', 'gauge', 1)\n    metrics:\n      - " + keys
}

// Under a python transform the script makes the series and gives each its
// type and its help text, and fails the scrape itself when it must; a rule
// of the collector makes none. A rule's type, description, required and
// error_mode loaded all the same and did nothing: a rule saying counter left
// the script's series the gauge the script made it, its description was no
// series' help, and {name: up, error_mode: fail} did not fail a scrape whose
// script made no up (transform.TestAPythonRuleChangesNoSeriesButByTruncate
// shows each). The load refuses each now, naming the collector and the
// metric and saying where the script says it instead. A key written "" is
// the key left out, as everywhere, and required is written when it is
// there, true or false: the loader holds it apart from the key left out.
func TestAPythonRuleDoesNotTakeWhatItsScriptSays(t *testing.T) {
	for keys, message := range map[string]string{
		"name: cpu\n        type: counter\n":                        pythonRuleType,
		"name: cpu\n        type: gauge\n":                          pythonRuleType,
		"name: cpu\n        type: untyped\n":                        pythonRuleType,
		"name: cpu\n        description: CPU in use.\n":             pythonRuleDescription,
		"name: cpu\n        description: \" \"\n":                   pythonRuleDescription,
		"name: cpu\n        description: 0\n":                       pythonRuleDescription,
		"name: cpu\n        required: true\n":                       pythonRuleRequired,
		"name: cpu\n        required: false\n":                      pythonRuleRequired,
		"name: cpu\n        error_mode: fail\n":                     pythonRuleErrorMode,
		"name: cpu\n        error_mode: log\n":                      pythonRuleErrorMode,
		"name: cpu\n        error_mode: ignore\n":                   pythonRuleErrorMode,
		"name: cpu\n        error_mode: \" LOG \"\n":                pythonRuleErrorMode,
		"name: cpu\n        expression: x\n        type: counter\n": pythonRuleType,
	} {
		want := `collector "racks" metric "cpu" ` + message
		if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule(keys))); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%q: %v\nwant it refused with %s", keys, err, want)
		}
	}
	// All four are reported at once, in the order the documentation lists
	// the keys, with the labels that cut nothing after them.
	_, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule("name: cpu\n        error_mode: fail\n        required: true\n        description: CPU in use.\n        type: counter\n        labels:\n          - name: site\n            expression: site\n")))
	var problems model.Problems
	if !errors.As(err, &problems) || len(problems) != 5 {
		t.Fatalf("a rule with all four and a label that cuts nothing: %v", err)
	}
	for i, message := range []string{pythonRuleType, pythonRuleDescription, pythonRuleRequired, pythonRuleErrorMode, `label "site" ` + pythonRuleLabel} {
		if want := `collector "racks" metric "cpu" ` + message; !strings.HasSuffix(problems[i].Error(), want) {
			t.Errorf("problem %d is %v\nwant %s", i+1, problems[i], want)
		}
	}
	// A key written "" is the key left out, and the rule loads with the
	// defaults it had: a gauge under log, which nothing reads.
	for _, keys := range []string{
		"name: cpu\n", "name: cpu\n        type: \"\"\n        description: \"\"\n        error_mode: \"\"\n", "name: cpu\n        expression: not read\n",
		"name: cpu\n        labels:\n          - name: site\n            expression: site\n            truncate: true\n",
	} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule(keys)))
		if err != nil {
			t.Errorf("%q: %v", keys, err)
			continue
		}
		if rule := cfg.Collectors[0].Metrics[0]; rule.Name != "cpu" || rule.Type != model.GaugeMetricType || rule.ErrorMode != model.ErrorModeLog || rule.Required != nil || rule.Description != "" {
			t.Errorf("%q: the rule loads as %+v", keys, rule)
		}
	}
	// What was refused of a python rule is refused in the words it was,
	// alone and with each of the four keys beside it.
	for keys, message := range map[string]string{
		"type: histogram\n":      `collector "racks" metric "cpu" has type histogram, which only a prometheus transform can give, passing through a histogram that has its buckets or quantiles; a python rule reads one value, so use gauge, counter or untyped`,
		"type: timer\n":          `collector "racks" metric "cpu" has invalid type "timer"`,
		"error_mode: panic\n":    `collector "racks" metric "cpu" error_mode has invalid value "panic"; want fail, log or ignore`,
		"items: .rows[]\n":       `collector "racks" metric "cpu" sets items, which only the jq, yq and css transforms support`,
		"scale: 2\n":             `collector "racks" metric "cpu" sets value_map or scale, which the python transform does not use: its script sets each value with metric(...)`,
		"value_map: {up: 1}\n":   `collector "racks" metric "cpu" sets value_map or scale, which the python transform does not use: its script sets each value with metric(...)`,
		"time_format: rfc3339\n": `collector "racks" metric "cpu" sets time_format, which the python transform does not use: its script sets each value with metric(...)`,
		"labels:\n          - name: site\n            expression: site\n            required: true\n":    `collector "racks" metric "cpu" label "site" cannot be required: a python transform's labels come from its script, not from label expressions`,
		"labels:\n          - name: site\n            expression: site\n            value_map: {a: b}\n": `collector "racks" metric "cpu" label "site" sets value_map, which the python transform does not use: its script sets each label`,
		"labels:\n          - name: site\n            value: x\n":                                        `collector "racks" metric "cpu" label "site" sets value, which a python rule's label does not take: the script sets the labels of its series itself, with metric(..., labels={...}), and a rule's label only names one of them to cut with truncate: true; for a constant on every series of the collector, set transform.labels`,
		"labels:\n          - name: site\n":                                                              `collector "racks" metric "cpu" label "site" needs a value, for a static label, or an expression, to read it from the response`,
		"labels:\n          - name: site\n            expression: \" \"\n            truncate: true\n":   `collector "racks" metric "cpu" label "site" expression " " is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant`,
	} {
		for _, beside := range []string{"", "description: CPU in use.\n        required: false\n        ", "type: counter\n        error_mode: fail\n        "} {
			if strings.HasPrefix(keys, "type:") && strings.Contains(beside, "type:") || strings.HasPrefix(keys, "error_mode:") && strings.Contains(beside, "error_mode:") {
				continue
			}
			written := "name: cpu\n        " + beside + keys
			if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule(written))); err == nil || !strings.HasSuffix(err.Error(), ".yaml: "+message) && err.Error() != message {
				t.Errorf("%q: %v\nwant it refused with %s", written, err, message)
			}
		}
	}
	// time_zone without time_format is refused under every transform, and
	// with it time_format is, so a python rule takes neither.
	if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule("name: cpu\n        time_zone: UTC\n"))); err == nil || !strings.Contains(err.Error(), "time_zone") {
		t.Errorf("a python rule with a time_zone: %v", err)
	}
	// Every other transform takes the four keys, as it did.
	for _, transform := range []string{"jq", "yq", "xpath", "css", "regex", "csv", "prometheus"} {
		document := strings.Replace(labelCollector(transform, "            value: x\n"), "      - name: cpu\n", "      - name: cpu\n        type: counter\n        description: CPU in use.\n        required: false\n        error_mode: fail\n", 1)
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err != nil {
			t.Errorf("%s: %v", transform, err)
			continue
		}
		if rule := cfg.Collectors[0].Metrics[0]; rule.Type != model.CounterMetricType || rule.Description != "CPU in use." || rule.Required == nil || *rule.Required || rule.ErrorMode != model.ErrorModeFail {
			t.Errorf("%s: the rule loads as %+v", transform, rule)
		}
	}
}

// A python rule makes no series: it names one of the script's. Its name is
// the metric the script makes, which a debug probe's report lists when the
// script made none of it, and each of its labels names a label of that
// series to cut with truncate: true. A rule without a name named nothing —
// its labels cut nothing, whatever truncate said — and a label without
// truncate: true did nothing at all, each with nothing said: an author who
// wrote an expression there for a label of the series got no such label.
// The load refuses both now, the rule without a name told of by its place
// among the collector's rules. A rule with a name and no label loads, as it
// did, and so does one with an expression, which is not read.
func TestAPythonRuleNamesASeriesAndItsLabelsCutOne(t *testing.T) {
	cut := "labels:\n          - name: site\n            expression: site\n            truncate: true\n"
	for _, keys := range []string{cut, "name: \"\"\n        " + cut, "expression: cpu\n", "{}\n"} {
		want := `collector "racks" metrics rule 1 ` + pythonRuleName
		if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule(keys))); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%q: %v\nwant it refused with %s", keys, err, want)
		}
	}
	// A name of nothing but blanks is no metric's name, as it was not.
	if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule("name: \"  \"\n"))); err == nil || !strings.HasSuffix(err.Error(), `collector "racks" metrics rule 1: "  " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`) {
		t.Errorf("a name of blanks: %v", err)
	}
	for _, label := range []string{"            expression: site\n", "            expression: site\n            truncate: false\n", "            expression: site\n            value: \"\"\n"} {
		want := `collector "racks" metric "cpu" label "site" ` + pythonRuleLabel
		if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule("name: cpu\n        labels:\n          - name: site\n"+label))); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%q: %v\nwant it refused with %s", label, err, want)
		}
	}
	// Each label that cuts nothing is named, and the ones that cut are not.
	_, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule("name: cpu\n        labels:\n          - {name: site, expression: site}\n          - {name: note, expression: note, truncate: true}\n          - {name: zone, expression: zone}\n")))
	var problems model.Problems
	if !errors.As(err, &problems) || len(problems) != 2 {
		t.Fatalf("two labels that cut nothing beside one that cuts: %v", err)
	}
	for i, name := range []string{"site", "zone"} {
		if want := fmt.Sprintf(`collector "racks" metric "cpu" label %q %s`, name, pythonRuleLabel); !strings.HasSuffix(problems[i].Error(), want) {
			t.Errorf("problem %d is %v\nwant %s", i+1, problems[i], want)
		}
	}
	for _, keys := range []string{"name: cpu\n", "name: cpu\n        expression: .cpu[\n", "name: cpu\n        " + cut, "name: cpu\n        labels: []\n"} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", pythonRule(keys))); err != nil {
			t.Errorf("%q: %v", keys, err)
		}
	}
	// A python collector needs no rule at all, and a prometheus rule, which
	// passes series through under their own names, still needs no name.
	for _, document := range []string{
		strings.Replace(pythonRule(""), "    metrics:\n      - ", "", 1), strings.Replace(pythonRule(""), "    metrics:\n      - ", "    metrics: []\n", 1),
		"collectors:\n  - name: racks\n    request:\n      type: http\n    transform:\n      type: prometheus\n    metrics:\n      - expression: '^up$'\n        labels:\n          - name: site\n            expression: site\n",
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err != nil {
			t.Errorf("%v\n%s", err, document)
		}
	}
}
