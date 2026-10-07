//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"strings"
	"testing"
)

// A rule's name and the names of its labels are held by the exporter to
// what a scrape of the collector holds a name to: under name_escaping fail,
// written, left out or written "", to the classic names, and under
// underscores and values to any name that is not blanks alone. The schema
// says the same, by a rule of the collector that reads its name_escaping:
// both refuse a name with a dot, a dash, a blank in it, a leading digit or
// a letter outside ASCII under fail, which the schema refused under every
// name_escaping and the exporter did too, and both take each under the
// other two, under a jq, a prometheus and a python transform. A classic
// name is taken by both under all of them, a name of nothing but blanks by
// neither, and a rule's name written "" is the key left out, to both, where
// a rule needs none.
//
// A name is text under each of them: one written as a number or as a
// boolean, 1 or true, is refused by both, the exporter saying how YAML
// reads it and to quote it, and quoted it is the name it spells, which both
// take where a name may begin with a digit. Under underscores and values
// both took it unquoted too, as the name "1", and under fail the exporter
// took true where the schema refused it.
//
// What a schema cannot tell stays the exporter's alone, as the keys'
// descriptions say: a name Prometheus reserves, beginning with two
// underscores, and one that underscores exports as such a name. So does
// what a pass-through's transform writes, the keys of transform.labels and
// what rename and rename_labels rename to, which the schemas hold to
// nothing.
func TestSchemaAndExporterAgreeOnANameUnderEachNameEscaping(t *testing.T) {
	schema := loadSchema(t)
	collectors := map[string]string{
		"jq":         "collectors:\n  - name: demo\n%s    request:\n      type: http\n    transform:\n      type: jq\n    metrics:\n      - expression: .v\n        name: %s\n        labels:\n          - expression: .l\n            name: %s\n",
		"prometheus": "collectors:\n  - name: demo\n%s    request:\n      type: http\n    transform:\n      type: prometheus\n    metrics:\n      - expression: '^up$'\n        name: %s\n        labels:\n          - expression: l\n            name: %s\n",
		"python":     "collectors:\n  - name: demo\n%s    request:\n      type: http\n    transform:\n      type: python\n      script: metric('up', 1)\n    metrics:\n      - name: %s\n        labels:\n          - expression: l\n            truncate: true\n            name: %s\n",
	}
	classic := []string{"up", "a_b", "_", "job:up:sum", "UP1"}
	classicLabels := []string{"site", "a_b", "_", "L1"}
	others := []string{"http.server.duration", "bad-name", `"node load"`, "1up", "é", `" up"`, `"up "`, "a/b", `'a"b'`, `"1"`}
	blanks := []string{`" "`, `"  "`, `"\t"`}
	tried := 0
	for transform, collector := range collectors {
		for _, escaping := range []string{"", "    name_escaping: \"\"\n", "    name_escaping: fail\n", "    name_escaping: underscores\n", "    name_escaping: values\n"} {
			escapes := strings.Contains(escaping, "underscores") || strings.Contains(escaping, "values")
			written := func(name, label string) string { return fmt.Sprintf(collector, escaping, name, label) }
			what := func(of, name string) string {
				return fmt.Sprintf("a %s rule's %s %s, %s", transform, of, name, strings.TrimSpace(escaping))
			}
			for _, name := range classic {
				tried++
				agree(t, schema, what("name", name), written(name, "site"), true)
			}
			for _, label := range classicLabels {
				agree(t, schema, what("label", label), written("up", label), true)
			}
			for _, name := range others {
				tried++
				// A colon is a character of a classic metric name and of
				// no classic label name.
				agree(t, schema, what("name", name), written(name, "site"), escapes)
				agree(t, schema, what("label", name), written("up", name), escapes)
			}
			agree(t, schema, what("label", "job:up"), written("up", "job:up"), escapes)
			for _, name := range blanks {
				agree(t, schema, what("name", name), written(name, "site"), false)
				agree(t, schema, what("label", name), written("up", name), false)
			}
			agree(t, schema, what("label", `""`), written("up", `""`), false)
			for name, kind := range map[string]string{"1": "a number", "1.5e3": "a number", "0x10": "a number", "true": "a boolean", "False": "a boolean"} {
				tried++
				before, _, _ := strings.Cut(written(name, "site"), " name: "+name+"\n")
				refusedByBoth(t, schema, what("name", name), written(name, "site"), fmt.Sprintf("line %d: name is written %s, which YAML reads as %s, not as text; to use that text there, quote it: %q", strings.Count(before, "\n")+1, name, kind, name))
				refusedByBoth(t, schema, what("label", name), written("up", name), fmt.Sprintf("name is written %s, which YAML reads as %s, not as text; to use that text there, quote it: %q", name, kind, name))
			}
			for _, name := range []string{`"true"`, `'False'`} {
				agree(t, schema, what("name", name), written(name, "site"), true)
				agree(t, schema, what("label", name), written("up", name), true)
			}
			agree(t, schema, what("name", `""`), written(`""`, "site"), transform == "prometheus")

			// Reserved names, and the names underscores makes of some.
			loadersAlone(t, schema, what("name", "__up"), written("__up", "site"), `"__up" starts with "__", which Prometheus reserves`)
			loadersAlone(t, schema, what("label", "__site"), written("up", "__site"), `label name "__site" starts with __, which Prometheus reserves for its own labels`)
			if strings.Contains(escaping, "underscores") {
				loadersAlone(t, schema, what("name", "1_up"), written("1_up", "site"), `"1_up" is exported as "__up" under name_escaping underscores, which starts with "__", which Prometheus reserves`)
				loadersAlone(t, schema, what("label", "..site"), written("up", "..site"), `label name "..site" is exported as "__site" under name_escaping underscores, which starts with __, which Prometheus reserves for its own labels`)
			}
			if strings.Contains(escaping, "values") {
				agree(t, schema, what("name", "1_up"), written("1_up", "site"), true)
				agree(t, schema, what("label", "..site"), written("up", "..site"), true)
			}
		}
	}
	if tried < 200 {
		t.Fatalf("%d names were tried", tried)
	}
	// The names a transform's own settings write.
	for _, setting := range []string{"labels: {deployment.env: prod}", "rename: {up: http.server.up}", "rename_labels: {job: service.name}"} {
		document := strings.Replace(passthroughCollector, "      # transform\n", "      "+setting+"\n", 1)
		loadersAlone(t, schema, "transform."+setting, document, "set the collector's name_escaping to underscores or values to export it escaped")
		for _, escaping := range []string{"underscores", "values"} {
			agree(t, schema, "transform."+setting+" under name_escaping "+escaping, escapingNames(document, escaping), true)
		}
	}
}
