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
// alike a value on it, which both took and nothing used, and take alike a
// label with an expression, a value written "", which is the key left out,
// and a constant under transform.labels. Under every other transform a
// rule's label takes a value, as it did.
func TestSchemaAndExporterAgreeOnAPythonRulesLabel(t *testing.T) {
	schema := loadSchema(t)
	label := func(keys string) string {
		return pythonCollector + "        labels:\n          - name: note\n" + keys
	}
	for keys, accepted := range map[string]bool{
		"            expression: note\n            truncate: true\n":                          true,
		"            expression: note\n":                                                      true,
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
	agree(t, schema, "a constant under transform.labels of a python collector", strings.Replace(label("            expression: note\n"), "      # transform\n", "      labels: {site: x}\n", 1), true)
	agree(t, schema, "a prometheus rule's label with a value", prometheusCollector+"        labels:\n          - name: note\n            value: x\n", true)
	agree(t, schema, "a jq rule's label with a value", strings.Replace(jqCollector, "expression: .l\n", "value: x\n", 1), true)
	// The transform's type in another case is the exporter's to take and
	// no canonical spelling: the schema refuses the type, and both refuse
	// the label.
	agree(t, schema, "a Python rule's label with a value", strings.Replace(label("            value: x\n"), "type: python", "type: Python", 1), false)
}
