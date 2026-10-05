//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"strings"
	"testing"
)

// said is what the schema and the exporter say of a file: whether the schema
// takes it, and what the exporter refuses it with, which is nothing where it
// loads it.
type said struct {
	schema bool
	loader string
}

// The four things the two can say of one file: both take it, both refuse
// it, the exporter alone refuses it, having checked what a schema cannot,
// and the schema alone refuses it, the exporter being more lenient than the
// canonical spelling.
var (
	bothTake      = said{schema: true}
	schemaRefuses = said{}
)

func bothRefuse(message string) said    { return said{loader: message} }
func loaderRefuses(message string) said { return said{schema: true, loader: message} }

func (s said) String() string {
	switch {
	case s.schema && s.loader == "":
		return "taken by both"
	case s.schema:
		return "refused by the exporter: " + s.loader
	case s.loader == "":
		return "refused by the schema"
	}
	return "refused by both: " + s.loader
}

// blanksKey is a key that takes text, with what the schema and the exporter
// say of it written as nothing but blanks and written "": the document, the
// place of the key in it, at, and what setting writes there with the value
// for its %s.
type blanksKey struct {
	key, of               string
	document, at, setting string
	blanks, empty         said
}

// name is the key with what tells its row apart.
func (k blanksKey) name() string {
	if k.of == "" {
		return k.key
	}
	return k.key + ", " + k.of
}

// blanksKeys are the keys of a rule, of its labels and of a collector that
// say what is read, each written as nothing but blanks and written "".
func blanksKeys() []blanksKey {
	const noMetricName = "is not a valid Prometheus metric name"
	collector, request, metric, label := "    # collector\n", "      # request\n", "        # metric\n", "            # label\n"
	transform := "      type: jq\n"
	static := strings.Replace(jqCollector, "expression: .l\n", "value: x\n", 1)
	csv := strings.NewReplacer("type: jq", "type: csv", "expression: .v", "expression: v", "expression: .l\n", "value: x\n").Replace(jqCollector)
	passthrough := "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: prometheus\n      # transform\n"
	xpath := "collectors:\n  - name: demo\n    request:\n      type: http\n    # collector\n    transform:\n      type: xpath\n    metrics:\n      - name: v\n        expression: //x:v\n"
	timed := strings.Replace(jqCollector, metric, "        time_format: rfc3339\n"+metric, 1)
	return []blanksKey{
		{key: "collectors[].metrics[].labels[].expression", document: jqCollector, at: "            expression: .l\n", setting: "            expression: %s\n", blanks: bothRefuse(`expression "  " is nothing but blanks`), empty: bothRefuse("needs a value")},
		{key: "collectors[].metrics[].labels[].expression", of: "beside a value", document: static, at: label, setting: "            expression: %s\n", blanks: bothRefuse(`expression "  " is nothing but blanks`), empty: bothTake},
		{key: "collectors[].metrics[].labels[].expression", of: "beside a value, of a csv rule", document: csv, at: label, setting: "            expression: %s\n", blanks: bothRefuse(`expression "  " is nothing but blanks`), empty: bothTake},
		{key: "collectors[].metrics[].labels[].value", document: jqCollector, at: "            expression: .l\n", setting: "            value: %s\n", blanks: bothTake, empty: bothRefuse("needs a value")},
		{key: "collectors[].metrics[].labels[].value", of: "beside an expression", document: jqCollector, at: label, setting: "            value: %s\n", blanks: bothRefuse("sets both value and expression"), empty: bothTake},
		{key: "collectors[].metrics[].labels[].name", document: jqCollector, at: "            name: l\n", setting: "            name: %s\n", blanks: bothRefuse("has a label without a name"), empty: bothRefuse("has a label without a name")},
		{key: "collectors[].metrics[].labels[].value_map{}", document: jqCollector, at: label, setting: "            value_map: {%s: other}\n", blanks: bothRefuse("has surrounding blanks or is empty"), empty: bothRefuse("has surrounding blanks or is empty")},
		{key: "collectors[].metrics[].labels[].value_map.*", document: jqCollector, at: label, setting: "            value_map: {a: %s}\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].metrics[].name", document: jqCollector, at: "        name: v\n", setting: "        name: %s\n", blanks: bothRefuse("has a metric without a name"), empty: bothRefuse("has a metric without a name")},
		{key: "collectors[].metrics[].name", of: "of a prometheus rule", document: prometheusCollector, at: metric, setting: "        name: %s\n", blanks: bothRefuse(noMetricName), empty: bothTake},
		{key: "collectors[].metrics[].expression", document: jqCollector, at: "        expression: .v\n", setting: "        expression: %s\n", blanks: loaderRefuses("has no expression"), empty: loaderRefuses("has no expression")},
		{key: "collectors[].metrics[].expression", of: "of a prometheus rule", document: strings.Replace(prometheusCollector, "- expression: '^up$'\n", "- name: up\n", 1), at: metric, setting: "        expression: %s\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].metrics[].expression", of: "of a python rule", document: pythonCollector, at: metric, setting: "        expression: %s\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].metrics[].items", document: jqCollector, at: metric, setting: "        items: %s\n", blanks: loaderRefuses(`items "  "`), empty: bothTake},
		{key: "collectors[].metrics[].time_format", document: jqCollector, at: metric, setting: "        time_format: %s\n", blanks: loaderRefuses(`time_format "  " is neither`), empty: bothTake},
		{key: "collectors[].metrics[].time_zone", of: "beside a time_format", document: timed, at: metric, setting: "        time_zone: %s\n", blanks: loaderRefuses(`time_zone "  " is not a time zone`), empty: bothTake},
		{key: "collectors[].metrics[].description", document: jqCollector, at: metric, setting: "        description: %s\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].metrics[].type", document: jqCollector, at: metric, setting: "        type: %s\n", blanks: bothRefuse("has invalid type"), empty: bothTake},
		{key: "collectors[].metrics[].error_mode", document: jqCollector, at: metric, setting: "        error_mode: %s\n", blanks: bothRefuse("error_mode has invalid value"), empty: bothTake},
		{key: "collectors[].metrics[].value_map{}", document: jqCollector, at: metric, setting: "        value_map: {%s: 1}\n", blanks: bothRefuse("has surrounding blanks or is empty"), empty: bothRefuse("has surrounding blanks or is empty")},
		{key: "collectors[].transform.include[]", document: passthrough, at: "      # transform\n", setting: "      include: [%s]\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.exclude[]", document: passthrough, at: "      # transform\n", setting: "      exclude: [%s]\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.rename{}", document: passthrough, at: "      # transform\n", setting: "      rename: {%s: up2}\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.rename.*", document: passthrough, at: "      # transform\n", setting: "      rename: {up: %s}\n", blanks: loaderRefuses(noMetricName), empty: loaderRefuses(noMetricName)},
		{key: "collectors[].transform.labels{}", document: jqCollector, at: transform, setting: transform + "      labels: {%s: x}\n", blanks: loaderRefuses("transform.labels has invalid label name"), empty: loaderRefuses("transform.labels has invalid label name")},
		{key: "collectors[].transform.labels.*", document: jqCollector, at: transform, setting: transform + "      labels: {site: %s}\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.remove_labels[]", document: jqCollector, at: transform, setting: transform + "      remove_labels: [%s]\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.rename_labels{}", document: jqCollector, at: transform, setting: transform + "      rename_labels: {%s: site}\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.rename_labels.*", document: jqCollector, at: transform, setting: transform + "      rename_labels: {l: %s}\n", blanks: loaderRefuses("to invalid label name"), empty: loaderRefuses("to invalid label name")},
		{key: "collectors[].transform.pre_script", document: jqCollector, at: transform, setting: transform + "      pre_script: %s\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.script", of: "of a python transform", document: pythonCollector, at: "      script: metric('up', 1)\n", setting: "      script: %s\n", blanks: loaderRefuses("Python transform requires a script"), empty: loaderRefuses("Python transform requires a script")},
		{key: "collectors[].transform.script", of: "of a jq transform", document: jqCollector, at: transform, setting: transform + "      script: %s\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].transform.type", document: jqCollector, at: transform, setting: "      type: %s\n", blanks: bothRefuse("has no transform.type"), empty: bothRefuse("has no transform.type")},
		{key: "collectors[].decoder.type", document: jqCollector, at: collector, setting: "    decoder:\n      type: %s\n", blanks: schemaRefuses, empty: bothTake},
		{key: "collectors[].request.path", document: jqCollector, at: request, setting: "      path: %s\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].request.method", document: jqCollector, at: request, setting: "      method: %s\n", blanks: bothRefuse("has unsupported method"), empty: bothTake},
		{key: "collectors[].metrics_prefix", document: jqCollector, at: collector, setting: "    metrics_prefix: %s\n", blanks: bothRefuse("has invalid metrics_prefix"), empty: bothTake},
		{key: "collectors[].name_escaping", document: jqCollector, at: collector, setting: "    name_escaping: %s\n", blanks: bothRefuse("name_escaping must be"), empty: bothTake},
		{key: "collectors[].response.charset", document: jqCollector, at: collector, setting: "    response:\n      charset: %s\n", blanks: loaderRefuses("unsupported charset"), empty: bothTake},
		{key: "collectors[].response.namespaces{}", document: xpath, at: collector, setting: "    response:\n      namespaces: {x: \"urn:x\", %s: \"urn:y\"}\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].response.namespaces.*", document: xpath, at: collector, setting: "    response:\n      namespaces: {x: %s}\n", blanks: bothTake, empty: bothTake},
		{key: "collectors[].name", document: jqCollector, at: "    name: demo\n", setting: "    name: %s\n", blanks: bothRefuse("has invalid name"), empty: bothRefuse("has invalid name")},
	}
}

// A key written as nothing but blanks is not the key written "": "" is the
// key left out, to the exporter and to the schemas, and blanks are text,
// which each key takes or refuses as it does any other text. The keys of a
// rule, of its labels and of a collector that say what is read are each put
// through the committed schema and the exporter both ways, and the two must
// say what the table says. A label's expression of blanks is refused by
// both, whatever is beside it: the exporter took one beside a value, as no
// expression, and then read the label with it. The rest is as it was. Blanks
// are refused where no text of blanks is what the key takes: a rule's
// expression, name and items, a time_format and a time_zone, a key of a
// value_map, and every key held to a set of values or a pattern. They are
// taken where the text is used as it is written: a label's value, a
// constant of blanks; a prometheus rule's expression and the patterns of
// transform.include and transform.exclude, regular expressions over metric
// names, which may hold blanks; the names transform.rename, remove_labels
// and rename_labels look for, which may be such names; request.path; and a
// namespace of response.namespaces. A pre_script of blanks is no script,
// and a decoder.type of blanks the default, which the schema, describing
// the canonical spelling, does not take. What the exporter alone refuses is
// what a schema cannot tell: that an expression compiles, that a layout is
// one, that a name is a metric's or a label's.
func TestSchemaAndExporterOnKeysWrittenAsBlanks(t *testing.T) {
	schema := loadSchema(t)
	for _, key := range blanksKeys() {
		if strings.Count(key.document, key.at) != 1 {
			t.Fatalf("%s: the document has %q %d times", key.name(), key.at, strings.Count(key.document, key.at))
		}
		for _, written := range []struct {
			what, value string
			want        said
		}{{"as blanks", `"  "`, key.blanks}, {`""`, `""`, key.empty}} {
			document := strings.Replace(key.document, key.at, fmt.Sprintf(key.setting, written.value), 1)
			problems, err := verdicts(t, schema, document)
			if (len(problems) == 0) != written.want.schema || (err == nil) != (written.want.loader == "") || err != nil && !strings.Contains(err.Error(), written.want.loader) {
				t.Errorf("%s, written %s: want it %s; the schema says %v, the exporter %v\n%s", key.name(), written.what, written.want, problems, err, document)
			}
		}
		t.Logf("| %s | %s | %s |", key.name(), key.blanks, key.empty)
	}
}
