//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// jqCollector is a configuration of one http collector with a jq transform,
// one rule and one label, and a comment at each place a row writes a key:
// of the collector, its request, its rule and the rule's label.
const jqCollector = `collectors:
  - coalesce: true
    name: demo
    # collector
    request:
      type: http
      # request
    transform:
      type: jq
    metrics:
      - required: true
        name: v
        expression: .v
        # metric
        labels:
          - truncate: false
            name: l
            expression: .l
            # label
`

// A rule of a prometheus or python transform needs no name: the series a
// prometheus rule passes through have their own, and a script names its.
const (
	prometheusCollector = "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: prometheus\n    metrics:\n      - expression: '^up$'\n        # metric\n"
	pythonCollector     = "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: python\n      script: metric('up', 1)\n      # transform\n    metrics:\n      - description: Whether it is up.\n        # metric\n"
)

// switchedOnOTLP is a configuration with its otlp block switched on.
const switchedOnOTLP = "otlp:\n  enabled: true\n  endpoint: http://collector.invalid:4318/v1/metrics\n  # otlp\n" + testutil.MinimalConfig

// oneTarget is a target file of one target of the collector demo, and
// targetsConfiguration the configuration that collector is in: its path has
// a placeholder with a default, so a target may fill it or leave it.
const (
	oneTarget            = "interval: 1m\ntargets:\n  - export_via_otlp: false\n    name: a\n    collector: demo\n    target: http://a.example\n    # target\n"
	targetsConfiguration = "collectors:\n  - name: demo\n    request:\n      type: http\n      path: /status/{{param_id:all}}\n    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
)

// httpSchemaKeys are the keys of an http collector, of the otlp block and of
// the target file that a schema holds to allowed values, a pattern or a
// length: what both say of each left out, written "", written well and
// written badly, and what the schema said before "" was the key left out to
// it.
func httpSchemaKeys() []schemaKey {
	collector, request, metric, label := "    # collector\n", "      # request\n", "        # metric\n", "            # label\n"
	target := "    # target\n"
	keys := []schemaKey{
		// Required: written "", each is as missing as left out, to both.
		{key: "collectors[].name", document: jqCollector, at: "    name: demo\n", setting: "    name: %s\n", valid: "demo", invalid: "bad-name"},
		{key: "collectors[].transform.type", document: jqCollector, at: "      type: jq\n", setting: "      type: %s\n", valid: "jq", invalid: "jsonpath"},
		{key: "collectors[].request.type", document: jqCollector, at: "      type: http\n", setting: "      type: %s\n", valid: "http", invalid: "gopher"},
		{key: "collectors[].metrics[].labels[].name", document: jqCollector, at: "            name: l\n", setting: "            name: %s\n", valid: "l", invalid: "bad-name"},
		// A rule's name is required of every transform but prometheus and
		// python: the schema took a rule without one, and refused "" where
		// the exporter takes it.
		{key: "collectors[].metrics[].name", document: jqCollector, at: "        name: v\n", setting: "        name: %s\n", valid: "v", invalid: "bad-name", absentWas: taken},
		{key: "collectors[].metrics[].name", of: "of a prometheus rule", document: prometheusCollector, at: metric, setting: "        name: %s\n", valid: "up", invalid: "bad-name", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].name", of: "of a python rule", document: pythonCollector, at: metric, setting: "        name: %s\n", valid: "up", invalid: "bad-name", absent: true, empty: true, emptyWas: refused},
		// Optional, with allowed values or a pattern: "" is the default.
		{key: "collectors[].metrics_prefix", document: jqCollector, at: collector, setting: "    metrics_prefix: %s\n", valid: "grafana", invalid: "grafana_", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].name_escaping", document: jqCollector, at: collector, setting: "    name_escaping: %s\n", valid: "underscores", invalid: "escape", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].decoder.type", document: jqCollector, at: collector, setting: "    decoder:\n      type: %s\n", valid: "json", invalid: "jsonl", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.method", document: jqCollector, at: request, setting: "      method: %s\n", valid: "POST", invalid: "FETCH", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].error_handling.on_fetch_error", document: jqCollector, at: collector, setting: "    error_handling:\n      on_fetch_error: %s\n", valid: "log", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].error_handling.on_decode_error", document: jqCollector, at: collector, setting: "    error_handling:\n      on_decode_error: %s\n", valid: "ignore", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].error_handling.on_transform_error", document: jqCollector, at: collector, setting: "    error_handling:\n      on_transform_error: %s\n", valid: "fail", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].error_mode", document: jqCollector, at: metric, setting: "        error_mode: %s\n", valid: "fail", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].type", document: jqCollector, at: metric, setting: "        type: %s\n", valid: "counter", invalid: "timer", absent: true, empty: true, emptyWas: refused},
		{key: "otlp.compression", document: switchedOnOTLP, at: "  # otlp\n", setting: "  compression: %s\n", valid: "none", invalid: "zstd", absent: true, empty: true, emptyWas: refused},
		// Keys of another request type: left out of an http collector, and
		// so written "".
		{key: "collectors[].request.rpc", of: "of an http collector", document: jqCollector, at: request, setting: "      rpc: %s\n", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.descriptors", of: "of an http collector", document: jqCollector, at: request, setting: "      descriptors: %s\n", absent: true, empty: true, emptyWas: refused},
		// A label has a value or an expression, and the one written "" is
		// the one left out: alone it is no label, beside the other it is
		// nothing, and one written beside the other is one too many.
		{key: "collectors[].metrics[].labels[].value", document: jqCollector, at: "            expression: .l\n", setting: "            value: %s\n", valid: "x"},
		{key: "collectors[].metrics[].labels[].expression", document: jqCollector, at: "            expression: .l\n", setting: "            expression: %s\n", valid: ".l"},
		{key: "collectors[].metrics[].labels[].value", of: "beside an expression", document: jqCollector, at: label, setting: "            value: %s\n", invalid: "x", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].labels[].expression", of: "beside a value", document: strings.Replace(jqCollector, "expression: .l\n", "value: x\n", 1), at: label, setting: "            expression: %s\n", invalid: ".l", absent: true, empty: true, emptyWas: refused},
		// Free text of at most one character: "" was the default already.
		{key: "collectors[].response.csv.delimiter", document: csvCollector("';'"), at: "    response:\n      csv:\n        delimiter: ';'\n", setting: "    response:\n      csv:\n        delimiter: %s\n", valid: "';'", invalid: "';;'", absent: true, empty: true},
		// Entries of lists and keys of mappings: an empty one is refused by
		// both, which the schema did not do for a target it may or may not
		// reach.
		{key: "collector_files[]", document: "# files\n" + jqCollector, at: "# files\n", setting: "collector_files: [%s]\n", absent: true},
		{key: "collectors[].request.accept_status[]", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "503", invalid: "600", absent: true},
		{key: "collectors[].request.allowed_targets[]", document: jqCollector, at: request, setting: "      allowed_targets: [%s]\n", valid: "api.example.com", invalid: "bücher.example", absent: true, emptyWas: taken},
		{key: "collectors[].request.denied_targets[]", document: jqCollector, at: request, setting: "      denied_targets: [%s]\n", valid: "api.example.com", invalid: "bücher.example", absent: true, emptyWas: taken},
		{key: "collectors[].request.redirect_trusted_hosts[]", document: jqCollector, at: request, setting: "      follow_redirects: true\n      redirect_trusted_hosts: [%s]\n", valid: "cdn.example.com", invalid: "bücher.example", absent: true},
		{key: "collectors[].transform.libraries[]", document: pythonCollector, at: "      # transform\n", setting: "      libraries: [%s]\n", valid: "lxml", invalid: "requests", absent: true},
		{key: "collectors[].transform.required_libs[]", document: pythonCollector, at: "      # transform\n", setting: "      required_libs: [%s]\n", valid: "PyYAML", invalid: "requests", absent: true},
		{key: "collectors[].metrics[].value_map{}", document: jqCollector, at: metric, setting: "        value_map: {%s: 1}\n", valid: "up", invalid: `" up"`, absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].metrics[].labels[].value_map{}", document: jqCollector, at: label, setting: "            value_map: {%s: other}\n", valid: `"1"`, invalid: `"1 "`, absent: true, emptyWas: taken, invalidWas: taken},
		// Sizes: "" is no size, to both.
		{key: "collectors[].limits.max_output_bytes", document: jqCollector, at: collector, setting: "    limits:\n      max_output_bytes: %s\n", valid: "1MiB", invalid: "lots", absent: true},
		{key: "collectors[].limits.max_response_bytes", document: jqCollector, at: collector, setting: "    limits:\n      max_response_bytes: %s\n", valid: "1MiB", invalid: "lots", absent: true},
		{key: "collectors[].limits.max_script_memory", document: jqCollector, at: collector, setting: "    limits:\n      max_script_memory: %s\n", valid: "64MiB", invalid: "lots", absent: true},
		{key: "collectors[].request.max_response_bytes", document: jqCollector, at: request, setting: "      max_response_bytes: %s\n", valid: "1MiB", invalid: "lots", absent: true},
		// Durations: "" is no duration, to both, and the number 0 is one.
		{key: "collectors[].cache.ttl", document: jqCollector, at: collector, setting: "    cache:\n      ttl: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].cache.stale_if_error", document: jqCollector, at: collector, setting: "    cache:\n      stale_if_error: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].limits.script_timeout", document: jqCollector, at: collector, setting: "    limits:\n      script_timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].request.retry.backoff", document: jqCollector, at: request, setting: "      retry:\n        backoff: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.timeout", document: switchedOnOTLP, at: "  # otlp\n", setting: "  timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.interval", document: switchedOnOTLP, at: "  # otlp\n", setting: "  interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.timeout", of: "switched off", document: strings.Replace(switchedOnOTLP, "enabled: true", "enabled: false", 1), at: "  # otlp\n", setting: "  timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.interval", of: "switched off", document: strings.Replace(switchedOnOTLP, "enabled: true", "enabled: false", 1), at: "  # otlp\n", setting: "  interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},

		// The target file. A target's name left out is made of its
		// collector and its place; its collector is required.
		{key: "targets[].name", file: inTargetFile, document: oneTarget, at: "    name: a\n", setting: "    name: %s\n", valid: "a", invalid: "bad-name", absent: true, empty: true, emptyWas: refused},
		{key: "targets[].collector", file: inTargetFile, document: oneTarget, at: "    collector: demo\n", setting: "    collector: %s\n", valid: "demo", emptyWas: taken},
		{key: "targets[].request.method", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      method: %s\n", valid: "POST", invalid: "FETCH", absent: true, empty: true, emptyWas: refused},
		{key: "targets[].request.accept_status[]", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "503", invalid: "600", absent: true},
		{key: "targets[].params{}", file: inTargetFile, document: oneTarget, at: target, setting: "    params: {%s: one}\n", valid: "param_id", invalid: "tenant", absent: true},
		{key: "targets[].labels{}", file: inTargetFile, document: oneTarget, at: target, setting: "    labels: {%s: one}\n", valid: "team", invalid: "job", absent: true, emptyWas: taken},
		{key: "interval", file: inTargetFile, document: oneTarget, at: "interval: 1m\n", setting: "interval: %s\n", valid: "30s", invalid: "soon", duration: true, zero: "interval is required"},
		{key: "targets[].interval", file: inTargetFile, document: oneTarget, at: target, setting: "    interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "targets[].request.timeout", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "targets[].request.retry.backoff", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        backoff: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
	}
	for i := range keys {
		if keys[i].file == inTargetFile {
			keys[i].configuration = targetsConfiguration
		}
	}
	return keys
}

// To the exporter an optional key written "" is the key left out: its
// default, when it takes one of a set of values or text of a pattern. The
// schemas refused "" for those keys — decoder.type, name_escaping,
// request.method, the error policies, a rule's type and error_mode,
// otlp.compression, metrics_prefix, a target's name and method — and an
// editor flagged what the exporter takes. Each key of an http collector, of
// the otlp block and of the target file that a schema holds to values, a
// pattern or a length is put through both, left out, written "", written
// well and written badly; a key the exporter requires is refused by both
// written "", as it is left out; and each duration key takes the number 0,
// which the exporter reads as the duration, and no other number.
func TestSchemaAndExporterAgreeOnKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, httpSchemaKeys())
}

// A rule about a key being written goes by the key being written, not by
// its being there: to both, a block that enabled switches on still needs its
// enabled beside a key written "", since the exporter asks of that block
// which keys it has; and a rule of a transform that takes no name takes one
// written "" beside the keys it has.
func TestSchemaAndExporterAgreeOnAnEmptyKeyBesideARule(t *testing.T) {
	schema := loadSchema(t)
	for name, test := range map[string]struct {
		document string
		accepted bool
	}{
		`otlp with compression: "" and no enabled`:     {"otlp: {compression: \"\"}\n" + testutil.MinimalConfig, false},
		`otlp switched off with compression: ""`:       {"otlp: {enabled: false, compression: \"\"}\n" + testutil.MinimalConfig, true},
		`a label with an expression and value: ""`:     {strings.Replace(jqCollector, "            # label\n", "            value: \"\"\n", 1), true},
		`a label with both written ""`:                 {strings.Replace(strings.Replace(jqCollector, "expression: .l\n", "expression: \"\"\n", 1), "            # label\n", "            value: \"\"\n", 1), false},
		`a label with a value and an expression`:       {strings.Replace(jqCollector, "            # label\n", "            value: x\n", 1), false},
		`a label with a value of one blank`:            {strings.Replace(jqCollector, "expression: .l\n", "value: \" \"\n", 1), true},
		`a label whose value YAML reads as a number`:   {strings.Replace(jqCollector, "expression: .l\n", "value: 0\n", 1), true},
		`a prometheus rule with name: "" and a type`:   {strings.Replace(prometheusCollector, "        # metric\n", "        name: \"\"\n        type: \"\"\n", 1), true},
		`a jq rule with every optional key written ""`: {strings.Replace(jqCollector, "        # metric\n", "        type: \"\"\n        error_mode: \"\"\n        description: \"\"\n        items: \"\"\n        time_format: \"\"\n        time_zone: \"\"\n", 1), true},
	} {
		agree(t, schema, name, test.document, test.accepted)
	}
}

// A value_map's keys are the texts looked up, without blanks around them:
// the schema and the exporter refuse alike the empty key and a key with a
// blank before or after it — a space, a tab, a line break, a no-break space
// or any other blank strings.TrimSpace takes off — in a rule's map and in a
// label's, and take alike "*", the key for any other value, a key with a
// blank inside it and one that begins with a character that only looks like
// a blank. A rule's values are numbers to both, .nan and .inf among them,
// and text that spells a number is none; a label's are text, and "" leaves
// the label off unless the label is required, which both refuse. Where a
// value_map may stand — not on a static label, not in a python or a
// prometheus rule, not in a rule without a name — is the exporter's alone
// to say, as it is for time_format.
func TestSchemaAndExporterAgreeOnValueMaps(t *testing.T) {
	schema := loadSchema(t)
	rule := func(valueMap string) string {
		return strings.Replace(jqCollector, "        # metric\n", "        value_map: "+valueMap+"\n", 1)
	}
	label := func(keys string) string {
		return strings.Replace(jqCollector, "            # label\n", keys, 1)
	}
	for written, accepted := range map[string]bool{
		`{up: 1, down: 0}`: true, `{"*": 0}`: true, `{up: 1, "*": 0}`: true, `{"a b": 1}`: true, `{"a  b": 1}`: true, `{}`: true,
		`{up: .nan}`: true, `{up: .inf}`: true, `{up: -.inf}`: true, `{up: 0x10}`: true, `{up: 1_000}`: true, `{up: 1.5e3}`: true,
		`{"\u200Bup": 1}`: true, `{"up\uFEFF": 1}`: true, `{"\u180Eup": 1}`: true,

		`{"": 1}`: false, `{'': 1, up: 2}`: false, `{" up": 1}`: false, `{"up ": 1}`: false, `{" ": 1}`: false, `{"\tup": 1}`: false,
		`{"up\n": 1}`: false, `{"\u00A0up": 1}`: false, `{"up\u0085": 1}`: false, `{"\u3000up": 1}`: false, `{"up\u2028": 1}`: false,
		`{up: 1, "down ": 0}`: false,
		`{up: NaN}`:           false, `{up: Inf}`: false, `{up: +Inf}`: false, `{up: "1"}`: false, `{up: one}`: false, `{up: true}`: false, `{up: [1]}`: false,
		`[up]`: false, `up`: false,
	} {
		agree(t, schema, "value_map: "+written, rule(written), accepted)
	}
	// Every blank strings.TrimSpace takes off is refused before a key and
	// after one, by both.
	blanks := 0
	for r := rune(0); r <= 0xFFFF; r++ {
		if !unicode.IsSpace(r) {
			continue
		}
		blanks++
		for _, key := range []string{string(r) + "up", "up" + string(r)} {
			agree(t, schema, fmt.Sprintf("value_map: a key with U+%04X", r), rule("{"+yamlQuoted(key)+": 1}"), false)
		}
	}
	if blanks < 20 {
		t.Fatalf("%d blanks tried", blanks)
	}
	for keys, accepted := range map[string]bool{
		"            value_map: {\"1\": running, \"2\": stopped}\n":            true,
		"            value_map: {\"*\": other}\n":                              true,
		"            value_map: {\"1\": \"\"}\n":                               true,
		"            value_map: {\"1\": 2, \"3\": true}\n":                     true,
		"            value_map: {}\n":                                          true,
		"            required: false\n            value_map: {\"1\": \"\"}\n":  true,
		"            required: true\n            value_map: {\"1\": one}\n":    true,
		"            value_map: {\"\": running}\n":                             false,
		"            value_map: {\" 1\": running}\n":                           false,
		"            value_map: {\"1\\t\": running}\n":                         false,
		"            value_map: {\"1\": [running]}\n":                          false,
		"            required: true\n            value_map: {\"1\": \"\"}\n":   false,
		"            required: true\n            value_map: {a: b, \"*\": ''}": false,
	} {
		agree(t, schema, "a label's "+strings.TrimSpace(keys), label(keys), accepted)
	}
	static := strings.Replace(jqCollector, "expression: .l\n", "value: x\n", 1)
	loadersAlone(t, schema, "a static label with a value_map", strings.Replace(static, "            # label\n", "            value_map: {x: y}\n", 1), `label "l" sets value_map with a static value`)
	loadersAlone(t, schema, "a python rule with a value_map", strings.Replace(pythonCollector, "        # metric\n", "        value_map: {up: 1}\n", 1), "sets value_map or scale, which the python transform does not use")
	loadersAlone(t, schema, "a prometheus rule with a value_map", strings.Replace(prometheusCollector, "        # metric\n", "        value_map: {up: 1}\n", 1), "sets value_map, which the prometheus transform does not use")
	loadersAlone(t, schema, "a label's value_map in a rule without a name", strings.Replace(prometheusCollector, "        # metric\n", "        labels:\n          - name: l\n            expression: l\n            value_map: {a: b}\n", 1), `sets value_map on a rule without a name`)
}

// yamlQuoted is a text as a double-quoted YAML scalar, every character of
// it written as its escape.
func yamlQuoted(text string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		b.WriteString(`\u`)
		for shift := 12; shift >= 0; shift -= 4 {
			b.WriteByte(hex[r>>shift&0xF])
		}
	}
	b.WriteByte('"')
	return b.String()
}
