//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
)

// An entry of transform.include or transform.exclude is a regular
// expression over metric names. The schema and the exporter refuse alike
// the empty entry, which matches every name, and an entry of nothing but
// blanks — a space, a tab, a line break, a no-break space or any other
// blank strings.TrimSpace takes off, alone or several — which matches only
// the names that hold them; both took each. They take alike every other
// entry: a pattern with a blank in it or around it, the two ways the
// exporter's message gives of writing a blank, a character that only looks
// like a blank, an entry YAML reads as a number or a boolean, and an empty
// list. That a pattern compiles, and that the keys belong to a prometheus
// transform without rules, stay the exporter's alone to say.
func TestSchemaAndExporterAgreeOnEntriesOfIncludeAndExclude(t *testing.T) {
	schema := loadSchema(t)
	with := func(setting string) string {
		return strings.Replace(passthroughCollector, "      # transform\n", "      "+setting+"\n", 1)
	}
	for _, key := range []string{"include", "exclude"} {
		for written, accepted := range map[string]bool{
			`["^node_"]`: true, `[up, "go_.*"]`: true, `['.*']`: true, `['[ ]']`: true, `['\x20']`: true, `["a b"]`: true, `[" up"]`: true, `["up "]`: true,
			`["\u200B"]`: true, `["\uFEFF"]`: true, `[1]`: true, `[true]`: true, `[]`: true,

			`[""]`: false, `['']`: false, `[" "]`: false, `["  "]`: false, `["\t"]`: false, `["\x20"]`: false, `[" \t\r\n"]`: false, `["\u00A0"]`: false,
			`[up, ""]`: false, `["", up]`: false, `[up, "  "]`: false, `[[up]]`: false, `up`: false,
		} {
			agree(t, schema, "transform."+key+": "+written, with(key+": "+written), accepted)
		}
		blanks := 0
		for r := rune(0); r <= 0xFFFF; r++ {
			if !unicode.IsSpace(r) {
				continue
			}
			blanks++
			for _, entry := range []string{string(r), string(r) + string(r), " " + string(r)} {
				agree(t, schema, fmt.Sprintf("transform.%s: an entry of U+%04X", key, r), with(key+": ["+yamlQuoted(entry)+"]"), false)
			}
			agree(t, schema, fmt.Sprintf("transform.%s: an entry with U+%04X in it", key, r), with(key+": ["+yamlQuoted("a"+string(r)+"b")+"]"), true)
		}
		if blanks < 20 {
			t.Fatalf("%d blanks tried", blanks)
		}
		loadersAlone(t, schema, "transform."+key+" that does not compile", with(key+": ['(']"), "transform."+key+` "(": error parsing regexp`)
		loadersAlone(t, schema, "transform."+key+" beside rules", with(key+": [up]")+"    metrics:\n      - expression: '^up$'\n", "sets transform."+key+", which picks or renames the metrics a prometheus transform passes through")
	}
	// The other lists and mappings of names in a transform are as they
	// were: an empty name loads, and matches no metric and no label.
	for _, setting := range []string{`rename: {"": up2}`, `rename: {"  ": up2}`, `remove_labels: [""]`, `remove_labels: ["  "]`, `rename_labels: {"": site}`, `labels: {site: ""}`, `labels: {site: " "}`} {
		agree(t, schema, "transform."+setting, with(setting), true)
	}
}

// A python rule's label names a label of the script's series to cut with
// truncate: true, and sets no constant: the schema and the exporter refuse
// alike a value on it, which both took and nothing used, and a label
// without truncate: true, which both took and which cut nothing, and take
// alike a label with an expression and truncate: true, a value written "",
// which is the key left out, and a constant under transform.labels. Under
// every other transform a rule's label takes a value, and needs no
// truncate, as it did.
func TestSchemaAndExporterAgreeOnAPythonRulesLabel(t *testing.T) {
	schema := loadSchema(t)
	label := func(keys string) string {
		return pythonCollector + "        labels:\n          - name: note\n" + keys
	}
	for keys, accepted := range map[string]bool{
		"            expression: note\n            truncate: true\n":                          true,
		"            expression: note\n":                                                      false,
		"            expression: note\n            truncate: false\n":                         false,
		"            expression: note\n            truncate: \"true\"\n":                      false,
		"            expression: note\n            value: \"\"\n            truncate: true\n": true,
		"            value: x\n":                                                              false,
		"            value: x\n            truncate: true\n":                                  false,
		"            value: \" \"\n":                                                          false,
		"            value: 0\n":                                                              false,
		"            value: x\n            expression: \"\"\n":                                false,
		"            value: x\n            expression: note\n":                                false,
		"            value: \"\"\n":                                                           false,
		"            truncate: true\n":                                                        false,
	} {
		agree(t, schema, "a python rule's label with "+strings.Join(strings.Fields(keys), " "), label(keys), accepted)
	}
	agree(t, schema, "a constant under transform.labels of a python collector", strings.Replace(label("            expression: note\n            truncate: true\n"), "      # transform\n", "      labels: {site: x}\n", 1), true)
	agree(t, schema, "a prometheus rule's label with a value", prometheusCollector+"        labels:\n          - name: note\n            value: x\n", true)
	agree(t, schema, "a prometheus rule's label without truncate", prometheusCollector+"        labels:\n          - name: note\n            expression: note\n", true)
	agree(t, schema, "a jq rule's label with truncate: false", strings.Replace(jqCollector, "            # label\n", "", 1), true)
	agree(t, schema, "a jq rule's label with a value", strings.Replace(jqCollector, "expression: .l\n", "value: x\n", 1), true)
	// The transform's type in another case is the exporter's to take and
	// no canonical spelling: the schema refuses the type, and both refuse
	// the label.
	agree(t, schema, "a Python rule's label with a value", strings.Replace(label("            value: x\n"), "type: python", "type: Python", 1), false)
}

// A rule of a python collector makes no series: it names one the script
// makes, and says nothing the script says of its series itself. The schema
// and the exporter refuse alike its type, description, required and
// error_mode, each of which both took and nothing read, and a rule without a
// name, which named no series; they take alike a rule of a name alone, one
// with an expression, which is not read, each text key written "", which is
// the key left out, and a python collector without rules. required is
// written when it is there, false as true. What the exporter refused of a
// python rule before — items, scale, a value_map, a time_format — stays its
// alone to say, as it was, and every other transform takes the four keys.
func TestSchemaAndExporterAgreeOnAPythonRule(t *testing.T) {
	schema := loadSchema(t)
	rule := func(keys string) string {
		return strings.Replace(pythonCollector, "        # metric\n", keys, 1)
	}
	for keys, accepted := range map[string]bool{
		"":                                  true,
		"        expression: .up\n":         true,
		"        expression: \"  \"\n":      true,
		"        type: \"\"\n":              true,
		"        description: \"\"\n":       true,
		"        error_mode: \"\"\n":        true,
		"        labels: []\n":              true,
		"        type: counter\n":           false,
		"        type: gauge\n":             false,
		"        type: untyped\n":           false,
		"        type: histogram\n":         false,
		"        type: timer\n":             false,
		"        description: Up or not.\n": false,
		"        description: \" \"\n":      false,
		"        description: 0\n":          false,
		"        description: false\n":      false,
		"        required: true\n":          false,
		"        required: false\n":         false,
		"        error_mode: fail\n":        false,
		"        error_mode: log\n":         false,
		"        error_mode: ignore\n":      false,
		"        error_mode: panic\n":       false,
		"        type: counter\n        description: Up or not.\n        required: true\n        error_mode: fail\n": false,
		"        type: \"\"\n        description: \"\"\n        error_mode: \"\"\n        required: false\n":         false,
	} {
		agree(t, schema, "a python rule with "+strings.Join(strings.Fields(keys), " "), rule(keys), accepted)
	}
	for name, test := range map[string]struct {
		document string
		accepted bool
	}{
		"a python rule without a name":             {strings.Replace(pythonCollector, "      - name: up\n", "      - expression: up\n", 1), false},
		`a python rule with name: ""`:              {strings.Replace(pythonCollector, "      - name: up\n", "      - name: \"\"\n", 1), false},
		"a python rule of nothing":                 {strings.Replace(pythonCollector, "      - name: up\n", "      - {}\n", 1), false},
		"a python rule of a label alone":           {strings.Replace(pythonLabel, "      - name: up\n", "      - expression: up\n", 1), false},
		"a python collector without rules":         {strings.Replace(pythonCollector, "    metrics:\n      - name: up\n        # metric\n", "", 1), true},
		"a python collector with metrics: []":      {strings.Replace(pythonCollector, "    metrics:\n      - name: up\n        # metric\n", "    metrics: []\n", 1), true},
		"two python rules, the second with a type": {pythonCollector + "      - name: jobs\n        type: counter\n", false},
		"two python rules of names alone":          {pythonCollector + "      - name: jobs\n", true},
	} {
		agree(t, schema, name, test.document, test.accepted)
	}
	for _, transform := range []string{"jq", "yq"} {
		document := strings.NewReplacer("type: jq", "type: "+transform, "        # metric\n", "        type: counter\n        description: Up or not.\n        error_mode: fail\n").Replace(strings.Replace(jqCollector, "      - required: true\n", "      - required: false\n", 1))
		agree(t, schema, "a "+transform+" rule with the four keys", document, true)
	}
	agree(t, schema, "a prometheus rule with the four keys and no name", strings.Replace(prometheusCollector, "        # metric\n", "        type: counter\n        description: Up or not.\n        required: false\n        error_mode: fail\n", 1), true)
	// The transform's type in another case is the exporter's to take and
	// no canonical spelling: the schema refuses the type, and both refuse
	// the rule.
	agree(t, schema, "a Python rule with a type", strings.Replace(rule("        type: counter\n"), "type: python", "type: Python", 1), false)
	for keys, message := range map[string]string{
		"        items: .rows[]\n":       "sets items, which only the jq, yq and css transforms support",
		"        scale: 2\n":             "sets value_map or scale, which the python transform does not use",
		"        time_format: rfc3339\n": "sets time_format, which the python transform does not use",
	} {
		loadersAlone(t, schema, "a python rule with "+strings.TrimSpace(keys), rule(keys), message)
	}
}

// A prometheus rule's expression is a regular expression over metric names,
// as an entry of transform.include is. The schema and the exporter refuse
// alike one of nothing but blanks — a space, a tab, a line break, a
// no-break space or any other blank strings.TrimSpace takes off, alone or
// several — which matched only the names that hold them and which both
// took; they take alike the expression left out and written "", a pattern
// with a blank in it or around it, the two ways the exporter's message
// gives of writing a blank, a character that only looks like a blank, and
// an expression YAML reads as a number. Under the transforms whose
// expression is in another language an expression of blanks stays the
// exporter's alone to refuse, as a rule that has none, and a python rule's,
// which is not read, is taken by both.
func TestSchemaAndExporterAgreeOnAPrometheusRulesExpression(t *testing.T) {
	schema := loadSchema(t)
	named := strings.Replace(prometheusCollector, "- expression: '^up$'\n", "- name: up\n", 1)
	for _, document := range []string{named, strings.Replace(named, "        # metric\n", "        required: false\n        # metric\n", 1)} {
		with := func(written string) string {
			return strings.Replace(document, "        # metric\n", "        expression: "+written+"\n", 1)
		}
		for written, accepted := range map[string]bool{
			`"^up$"`: true, `'.*'`: true, `'[ ]'`: true, `'\x20'`: true, `"a b"`: true, `" up"`: true, `"up "`: true, `"\u200B"`: true, `"\uFEFF"`: true, `1`: true, `""`: true, `''`: true,
			`" "`: false, `"  "`: false, `"\t"`: false, `"\x20"`: false, `" \t\r\n"`: false, `"\u00A0"`: false, `' '`: false,
		} {
			agree(t, schema, "a prometheus rule's expression: "+written, with(written), accepted)
		}
		agree(t, schema, "a prometheus rule without an expression", document, true)
		blanks := 0
		for r := rune(0); r <= 0xFFFF; r++ {
			if !unicode.IsSpace(r) {
				continue
			}
			blanks++
			for _, expression := range []string{string(r), string(r) + string(r), " " + string(r)} {
				agree(t, schema, fmt.Sprintf("a prometheus rule's expression of U+%04X", r), with(yamlQuoted(expression)), false)
			}
			agree(t, schema, fmt.Sprintf("a prometheus rule's expression with U+%04X in it", r), with(yamlQuoted("a"+string(r)+"b")), true)
		}
		if blanks < 20 {
			t.Fatalf("%d blanks tried", blanks)
		}
		loadersAlone(t, schema, "a prometheus rule's expression that does not compile", with("'('"), `expression "(": error parsing regexp`)
	}
	agree(t, schema, "a prometheus rule of an expression of blanks alone", strings.Replace(prometheusCollector, "'^up$'", `"  "`, 1), false)
	loadersAlone(t, schema, "a jq rule's expression of blanks", strings.Replace(jqCollector, "expression: .v\n", "expression: \"  \"\n", 1), `metric "v" has no expression`)
	agree(t, schema, "a python rule's expression of blanks", strings.Replace(pythonCollector, "        # metric\n", "        expression: \"  \"\n", 1), true)
}

// A prometheus rule passes on the metrics whose names its expression
// matches, or, without one, the metric its name names; with neither it
// matches no metric. The schema and the exporter refuse alike a rule that
// has neither key, each left out or written "", which is the key left out,
// whatever else the rule sets and wherever it stands among the collector's
// rules, which both took; they take alike a rule with either key or both,
// an expression YAML reads as a number or a boolean, the pattern of every
// name, a collector without rules and one with metrics: []. Under every
// other transform a rule is held to what it was: one without a name is
// refused by both, one of a name alone is a python rule to both and, under
// jq, refused by the exporter alone for the expression it lacks.
func TestSchemaAndExporterAgreeOnAPrometheusRuleOfNeitherANameNorAnExpression(t *testing.T) {
	schema := loadSchema(t)
	rules := func(written string) string {
		return strings.Replace(prometheusCollector, "      - expression: '^up$'\n        # metric\n", written, 1)
	}
	for written, accepted := range map[string]bool{
		"      - {}\n":                                   false,
		"      - name: \"\"\n":                           false,
		"      - expression: \"\"\n":                     false,
		"      - name: \"\"\n        expression: \"\"\n": false,
		"      - name: ''\n        expression: ''\n":     false,
		"      - name: ~\n":                              false,
		"      - type: counter\n":                        false,
		"      - type: \"\"\n":                           false,
		"      - description: Whether it is up.\n":       false,
		"      - required: true\n":                       false,
		"      - required: false\n":                      false,
		"      - error_mode: ignore\n":                   false,
		"      - scale: 2\n":                             false,
		"      - labels:\n          - name: site\n            value: rack1\n": false,
		"      - name: up\n      - type: counter\n":                           false,
		"      - type: counter\n      - name: up\n":                           false,
		"      - name: up\n      - {}\n      - expression: '^node_'\n":        false,
		"      - name: up\n":                                       true,
		"      - expression: '^node_'\n":                           true,
		"      - name: load\n        expression: '^node_load1$'\n": true,
		"      - name: up\n        expression: \"\"\n":             true,
		"      - name: \"\"\n        expression: '^up$'\n":         true,
		"      - expression: '.*'\n        type: counter\n":        true,
		"      - expression: 0\n":                                  true,
		"      - expression: false\n":                              true,
		"      - expression: '^$'\n":                               true,
		"      - name: up\n      - expression: '^node_'\n        labels:\n          - name: site\n            value: rack1\n": true,
		"      []\n": true,
	} {
		agree(t, schema, "prometheus rules "+strings.Join(strings.Fields(written), " "), rules(written), accepted)
	}
	agree(t, schema, "a prometheus collector without metrics", strings.Replace(rules(""), "    metrics:\n", "", 1), true)
	// The transform's type in another case is the exporter's to take and
	// no canonical spelling: the schema refuses the type, and both refuse
	// the rule.
	agree(t, schema, "a Prometheus rule of neither", strings.Replace(rules("      - {}\n"), "type: prometheus", "type: Prometheus", 1), false)
	// What the exporter refused such a rule for before, it refuses it for
	// still, and where a schema cannot tell that, the schema refuses the
	// rule for having neither key.
	for written, message := range map[string]string{
		"      - items: .rows[]\n": `metrics rule 1 sets items, which only the jq, yq and css transforms support`,
		"      - labels:\n          - name: site\n            expression: site\n            value_map: {a: b}\n": `sets value_map on a rule without a name`,
	} {
		problems, err := verdicts(t, schema, rules(written))
		if len(problems) == 0 || err == nil || !strings.Contains(err.Error(), message) || strings.Contains(err.Error(), "has neither a name nor an expression") {
			t.Errorf("%q: want it refused by the schema, and by the exporter with %q alone; the schema says %v, the exporter %v", written, message, problems, err)
		}
	}
	// The other transforms, as they were.
	agree(t, schema, "a jq rule of nothing", strings.Replace(jqCollector, "        name: v\n        expression: .v\n", "", 1), false)
	loadersAlone(t, schema, "a jq rule of a name alone", strings.Replace(jqCollector, "        expression: .v\n", "", 1), `metric "v" has no expression`)
	agree(t, schema, "a jq rule of an expression alone", strings.Replace(jqCollector, "        name: v\n", "", 1), false)
	agree(t, schema, "a python rule of a name alone", pythonCollector, true)
	agree(t, schema, "a python rule of an expression alone", strings.Replace(pythonCollector, "      - name: up\n", "      - expression: up\n", 1), false)
}

// Two rules of a collector that are the same rule — alike in name,
// expression, items, labels, value_map and time_format — make every series
// twice, and the exporter refuses them when the configuration loads. A
// schema cannot say it: it has no way to compare one item of a list with
// another by some of their keys, with "" as the key left out and the labels
// in any order, and uniqueItems, which compares whole items, would take two
// rules that differ in a description or a scale and are the same rule, and
// two that write `expression: ""` and nothing. So this is the exporter's
// alone to refuse, as the description of metrics says, and the schema takes
// each such pair. Both take alike the pairs that are two rules: of one name
// and another label, of other expressions, a prometheus rule of a name
// beside one of that name's pattern, and a python rule twice.
func TestTheExporterAloneRefusesTheSameRuleTwice(t *testing.T) {
	schema := loadSchema(t)
	prometheus := func(rules string) string {
		return strings.Replace(prometheusCollector, "      - expression: '^up$'\n        # metric\n", rules, 1)
	}
	jqRule := "      - required: true\n        name: v\n        expression: .v\n        labels:\n          - truncate: false\n            name: l\n            expression: .l\n"
	for name, document := range map[string]string{
		"a prometheus name twice":              prometheus("      - name: up\n      - name: up\n"),
		"a prometheus expression twice":        prometheus("      - expression: '^up$'\n      - name: load\n      - expression: '^up$'\n"),
		`a name, and with expression: ""`:      prometheus("      - name: up\n      - name: up\n        expression: \"\"\n"),
		"a prometheus rule and another scale":  prometheus("      - name: up\n      - name: up\n        scale: 2\n        description: Up.\n"),
		"a jq rule pasted twice":               jqCollector + jqRule,
		"a jq rule and what it says besides":   jqCollector + strings.Replace(jqRule, "required: true", "required: false\n        error_mode: ignore", 1),
		"a jq rule and its label cut":          jqCollector + strings.Replace(jqRule, "truncate: false", "truncate: true", 1),
		"a jq rule with its labels otherwise":  strings.Replace(jqCollector, "            # label\n", "          - name: site\n            value: a\n", 1) + strings.Replace(jqRule, "          - truncate", "          - name: site\n            value: a\n          - truncate", 1),
		"a jq rule and the same time_format":   strings.Replace(jqCollector, "        # metric\n", "        time_format: rfc3339\n", 1) + strings.Replace(jqRule, "        labels:", "        time_format: rfc3339\n        time_zone: UTC\n        labels:", 1),
		"a jq rule and the same value_map":     strings.Replace(jqCollector, "        # metric\n", "        value_map: {up: 1}\n", 1) + strings.Replace(jqRule, "        labels:", "        value_map: {up: 1}\n        labels:", 1),
		"the same rule in a collector's third": jqCollector + "      - name: w\n        expression: .w\n" + jqRule,
	} {
		loadersAlone(t, schema, name, document, "are the same rule")
	}
	for name, document := range map[string]string{
		"a jq rule with another label":          jqCollector + strings.Replace(jqRule, "expression: .l", "expression: .m", 1),
		"a jq rule with a label more":           jqCollector + jqRule + "          - name: site\n            value: a\n",
		"a jq rule with another expression":     jqCollector + strings.Replace(jqRule, "expression: .v", "expression: .w", 1),
		"a jq rule with another value_map":      strings.Replace(jqCollector, "        # metric\n", "        value_map: {up: 1}\n", 1) + strings.Replace(jqRule, "        labels:", "        value_map: {down: 0}\n        labels:", 1),
		"a jq rule with another time_format":    strings.Replace(jqCollector, "        # metric\n", "        time_format: rfc3339\n", 1) + strings.Replace(jqRule, "        labels:", "        time_format: rfc1123\n        labels:", 1),
		"a prometheus name and its pattern":     prometheus("      - name: up\n      - name: up\n        expression: '^up$'\n"),
		"a prometheus name with a label":        prometheus("      - name: up\n      - name: up\n        labels:\n          - name: site\n            value: a\n"),
		"a python rule twice":                   pythonCollector + "      - name: up\n",
		"a python rule twice with a label each": pythonLabel + "      - name: up\n        labels:\n          - name: note\n            expression: note\n            truncate: true\n",
	} {
		agree(t, schema, name, document, true)
	}
}
