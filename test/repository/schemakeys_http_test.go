//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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

// A rule of a prometheus transform needs no name: the series it passes
// through have their own. A python rule makes no series and names one of
// the script's, which is all it says.
const (
	prometheusCollector = "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: prometheus\n    metrics:\n      - expression: '^up$'\n        # metric\n"
	pythonCollector     = "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: python\n      script: metric('up', 1)\n      # transform\n    metrics:\n      - name: up\n        # metric\n"
)

// derivedCollector is jqCollector with a derived entry that adds up its
// rule's series twice, and a comment where a row writes a key of the entry.
const derivedCollector = jqCollector + "    derived:\n      - hidden: false\n        name: d\n        sum: [v, v]\n        # derived\n"

// derivedBy is derivedCollector adding up the rule's series by its label.
var derivedBy = strings.Replace(derivedCollector, "        sum: [v, v]\n", "        sum: [v]\n        by: [l]\n", 1)

// paginatedCollector is jqCollector fetching its pages by page number.
var paginatedCollector = strings.Replace(jqCollector, "      # request\n", "      pagination:\n        type: page\n        param: page\n        items: .items\n", 1)

// escapingNames is a configuration of one collector with its name_escaping
// set.
func escapingNames(configuration, escaping string) string {
	return strings.Replace(configuration, " name: demo\n", " name: demo\n    name_escaping: "+escaping+"\n", 1)
}

// passthroughCollector is a configuration of one prometheus collector
// without rules, which include and exclude pick the series of, and
// pythonLabel a python rule's label, which names a label of the script's
// series.
const (
	passthroughCollector = "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: prometheus\n      # transform\n"
	pythonLabel          = pythonCollector + "        labels:\n          - name: note\n            expression: note\n            truncate: true\n"
)

// hostRateLimit is an entry of rate_limits but for its name, with a
// comment where a row writes it.
const hostRateLimit = "rate_limits:\n  - rate_limit:\n      requests_per_second: 1\n    hosts: [api.example]\n    # entry\n"

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
		// A derived entry's name is required, and its operation reads
		// families named by text that is not blanks: without it the entry
		// computes nothing, and a sum by nothing but blanks keeps no label.
		// Its name is held to a classic name by the collector's condition
		// on name_escaping, which the schema as it was had none of.
		{key: "collectors[].derived[].name", document: derivedCollector, at: "        name: d\n", setting: "        name: %s\n", valid: "d", invalid: "bad-name", invalidWas: taken},
		{key: "collectors[].derived[].type", document: derivedCollector, at: "        # derived\n", setting: "        type: %s\n", valid: "counter", invalid: "histogram", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].derived[].sum[]", document: derivedCollector, at: "        sum: [v, v]\n", setting: "        sum: [v, %s]\n", valid: "v", invalid: `"  "`},
		{key: "collectors[].derived[].difference[]", document: strings.Replace(derivedCollector, "sum: [v, v]", "difference: [v, v]", 1), at: "        difference: [v, v]\n", setting: "        difference: [v, %s]\n", valid: "v", invalid: `"  "`},
		{key: "collectors[].derived[].ratio[]", document: strings.Replace(derivedCollector, "sum: [v, v]", "ratio: [v, v]", 1), at: "        ratio: [v, v]\n", setting: "        ratio: [v, %s]\n", valid: "v", invalid: `"  "`},
		{key: "collectors[].derived[].by[]", document: derivedBy, at: "        by: [l]\n", setting: "        by: [%s]\n", valid: "l", invalid: `"  "`},
		// A rule's name is required of every transform but prometheus: the
		// schema took a rule without one, and refused "" where the exporter
		// takes it. A python rule names a series of the script, and one
		// without a name, which both took, named none.
		{key: "collectors[].metrics[].name", document: jqCollector, at: "        name: v\n", setting: "        name: %s\n", valid: "v", invalid: "bad-name", absentWas: taken},
		{key: "collectors[].metrics[].name", of: "of a prometheus rule", document: prometheusCollector, at: metric, setting: "        name: %s\n", valid: "up", invalid: "bad-name", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].name", of: "of a python rule", document: strings.Replace(pythonCollector, "      - name: up\n", "      - expression: up\n        name: up\n", 1), at: "        name: up\n", setting: "        name: %s\n", valid: "up", invalid: "bad-name", absentWas: taken},
		// Under name_escaping underscores and values a scrape exports a
		// name that is not classic escaped, so a rule's name and a label's
		// may be any name there but one of blanks alone: both take one
		// with a dot, which both refused under every name_escaping.
		{key: "collectors[].metrics[].name", of: "under name_escaping underscores", document: escapingNames(jqCollector, "underscores"), at: "        name: v\n", setting: "        name: %s\n", valid: "http.server.duration", validWas: refused, invalid: `"  "`, absentWas: taken},
		{key: "collectors[].metrics[].name", of: "under name_escaping values", document: escapingNames(jqCollector, "values"), at: "        name: v\n", setting: "        name: %s\n", valid: `"1st load-average"`, validWas: refused, invalid: `"  "`, absentWas: taken},
		{key: "collectors[].metrics[].name", of: "of a prometheus rule under name_escaping values", document: escapingNames(prometheusCollector, "values"), at: metric, setting: "        name: %s\n", valid: "http.server.duration", validWas: refused, invalid: `" "`, absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].labels[].name", of: "under name_escaping underscores", document: escapingNames(jqCollector, "underscores"), at: "            name: l\n", setting: "            name: %s\n", valid: "service.name", validWas: refused, invalid: `"  "`},
		{key: "collectors[].metrics[].labels[].name", of: "under name_escaping values", document: escapingNames(jqCollector, "values"), at: "            name: l\n", setting: "            name: %s\n", valid: "k8s.pod/name", validWas: refused, invalid: `" "`},
		// What a python rule does not take, the script saying it of its
		// series itself: both refuse each written, which both took, and
		// take each left out and, where it is text, written "". required
		// is a boolean, which has no empty form.
		{key: "collectors[].metrics[].type", of: "of a python rule", document: pythonCollector, at: metric, setting: "        type: %s\n", invalid: "counter", invalidWas: taken, absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].description", of: "of a python rule", document: pythonCollector, at: metric, setting: "        description: %s\n", invalid: "Whether it is up.", invalidWas: taken, absent: true, empty: true},
		{key: "collectors[].metrics[].error_mode", of: "of a python rule", document: pythonCollector, at: metric, setting: "        error_mode: %s\n", invalid: "fail", invalidWas: taken, absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].required", of: "of a python rule", document: pythonCollector, at: metric, setting: "        required: %s\n", invalid: "true", invalidWas: taken, absent: true},
		{key: "collectors[].metrics[].required", of: "of a python rule, false", document: pythonCollector, at: metric, setting: "        required: %s\n", invalid: "false", invalidWas: taken, absent: true},
		// A prometheus rule's expression is a regular expression over
		// metric names: both refuse one of nothing but blanks, which both
		// took, and take it left out and written "", the rule then
		// matching the metric of its name.
		{key: "collectors[].metrics[].expression", of: "of a prometheus rule", document: strings.Replace(prometheusCollector, "- expression: '^up$'\n", "- name: up\n", 1), at: metric, setting: "        expression: %s\n", valid: `"^up$"`, invalid: `"  "`, invalidWas: taken, absent: true, empty: true},
		// A prometheus rule says which metrics it is about by its
		// expression or, without one, by its name: both refuse a rule with
		// neither, the one key it has left out or written "", which the
		// exporter took, and the schema but for a name written "".
		{key: "collectors[].metrics[].name", of: "of a prometheus rule without an expression", document: strings.Replace(prometheusCollector, "- expression: '^up$'\n", "- required: true\n        name: up\n", 1), at: "        name: up\n", setting: "        name: %s\n", valid: "up", invalid: "bad-name", absentWas: taken},
		{key: "collectors[].metrics[].expression", of: "of a prometheus rule without a name", document: strings.Replace(prometheusCollector, "- expression: '^up$'\n", "- required: true\n        expression: '^up$'\n", 1), at: "        expression: '^up$'\n", setting: "        expression: %s\n", valid: `"^up$"`, invalid: `"  "`, invalidWas: taken, absentWas: taken, emptyWas: taken},
		// The keys request.pagination requires: written "", each is as
		// missing as left out, to both, and items of blanks alone, which
		// names no array, both refuse.
		{key: "collectors[].request.pagination.type", document: paginatedCollector, at: "        type: page\n", setting: "        type: %s\n", valid: "offset", invalid: "pages"},
		{key: "collectors[].request.pagination.items", document: paginatedCollector, at: "        items: .items\n", setting: "        items: %s\n", valid: ".data.results", invalid: `"  "`, booleanAlone: `request.pagination.items "true" is not a path`, numberAlone: `request.pagination.items "1" is not a path`},
		// Optional, with allowed values or a pattern: "" is the default.
		{key: "collectors[].metrics_prefix", document: jqCollector, at: collector, setting: "    metrics_prefix: %s\n", valid: "grafana", invalid: "grafana_", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].name_escaping", document: jqCollector, at: collector, setting: "    name_escaping: %s\n", valid: "underscores", invalid: "escape", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].lint.ignore[]", document: jqCollector, at: collector, setting: "    lint:\n      ignore: [%s]\n", valid: "unit-suffix", invalid: "unit_suffix", absent: true},
		{key: "collectors[].decoder.type", document: jqCollector, at: collector, setting: "    decoder:\n      type: %s\n", valid: "json", invalid: "jsonl", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.method", document: jqCollector, at: request, setting: "      method: %s\n", valid: "POST", invalid: "FETCH", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].error_handling.on_fetch_error", document: jqCollector, at: collector, setting: "    error_handling:\n      on_fetch_error: %s\n", valid: "log", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].error_handling.on_decode_error", document: jqCollector, at: collector, setting: "    error_handling:\n      on_decode_error: %s\n", valid: "ignore", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].error_handling.on_transform_error", document: jqCollector, at: collector, setting: "    error_handling:\n      on_transform_error: %s\n", valid: "fail", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].error_mode", document: jqCollector, at: metric, setting: "        error_mode: %s\n", valid: "fail", invalid: "panic", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].type", document: jqCollector, at: metric, setting: "        type: %s\n", valid: "counter", invalid: "timer", absent: true, empty: true, emptyWas: refused},
		{key: "otlp.compression", document: switchedOnOTLP, at: "  # otlp\n", setting: "  compression: %s\n", valid: "none", invalid: "zstd", absent: true, empty: true, emptyWas: refused},
		// Switched off, the block is unchecked by both: the schema held
		// its compression to the values all the same.
		{key: "otlp.compression", of: "switched off", document: strings.Replace(switchedOnOTLP, "enabled: true", "enabled: false", 1), at: "  # otlp\n", setting: "  compression: %s\n", valid: "zstd", validWas: refused, absent: true, empty: true, emptyWas: refused},
		// Keys of another request type: left out of an http collector, and
		// so written "".
		{key: "collectors[].request.rpc", of: "of an http collector", document: jqCollector, at: request, setting: "      rpc: %s\n", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.descriptors", of: "of an http collector", document: jqCollector, at: request, setting: "      descriptors: %s\n", absent: true, empty: true, emptyWas: refused},
		// A label has a value or an expression, and the one written "" is
		// the one left out: alone it is no label, beside the other it is
		// nothing, and one written beside the other is one too many. An
		// expression of nothing but blanks is neither left out nor one:
		// both refuse it, alone, which the schema took, and beside a
		// value, which the exporter took of a csv rule.
		{key: "collectors[].metrics[].labels[].value", document: jqCollector, at: "            expression: .l\n", setting: "            value: %s\n", valid: "x"},
		{key: "collectors[].metrics[].labels[].expression", document: jqCollector, at: "            expression: .l\n", setting: "            expression: %s\n", valid: ".l", invalid: `"  "`, invalidWas: taken},
		{key: "collectors[].metrics[].labels[].value", of: "beside an expression", document: jqCollector, at: label, setting: "            value: %s\n", invalid: "x", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].metrics[].labels[].expression", of: "beside a value", document: strings.Replace(jqCollector, "expression: .l\n", "value: x\n", 1), at: label, setting: "            expression: %s\n", invalid: ".l", absent: true, empty: true, emptyWas: refused},
		// A python rule's label names a label of the script's series and
		// sets no constant: both refuse a value there, which both took.
		{key: "collectors[].metrics[].labels[].value", of: "of a python rule's label", document: pythonLabel, at: "            expression: note\n", setting: "            value: %s\n", invalid: "x", invalidWas: taken},
		// And it cuts that label, which is all it does: both refuse one
		// without truncate: true, which both took.
		{key: "collectors[].metrics[].labels[].truncate", of: "of a python rule's label", document: pythonLabel, at: "            truncate: true\n", setting: "            truncate: %s\n", valid: "true", invalid: "false", invalidWas: taken, absentWas: taken},
		{key: "collectors[].metrics[].labels[].expression", of: "of blanks beside a value, in a csv rule", document: strings.NewReplacer("type: jq", "type: csv", "expression: .v", "expression: v", "expression: .l\n", "value: x\n").Replace(jqCollector), at: label, setting: "            expression: %s\n", invalid: `"\t "`, absent: true, empty: true, emptyWas: refused},
		// Free text of at most one character: "" was the default already.
		{key: "collectors[].response.csv.delimiter", document: csvCollector("';'"), at: "    response:\n      csv:\n        delimiter: ';'\n", setting: "    response:\n      csv:\n        delimiter: %s\n", valid: "';'", invalid: "';;'", absent: true, empty: true},
		// Entries of lists and keys of mappings: an empty one is refused by
		// both, which the schema did not do for a target it may or may not
		// reach.
		{key: "collector_files[]", document: "# files\n" + jqCollector, at: "# files\n", setting: "collector_files: [%s]\n", absent: true},
		// An entry of rate_limits needs a name, which labels its
		// self-metrics, and each of its hosts something to match.
		{key: "rate_limits[].name", document: jqCollector + hostRateLimit, at: "    # entry\n", setting: "    name: %s\n", valid: "api", invalid: "bad-name"},
		{key: "rate_limits[].hosts[]", document: jqCollector + hostRateLimit + "    name: api\n", at: "    hosts: [api.example]\n", setting: "    hosts: [%s]\n", valid: "api.example"},
		{key: "collectors[].request.accept_status[]", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "503", invalid: "600", absent: true},
		{key: "collectors[].request.accept_status[]", of: "written with a point", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "503.0", invalid: "503.5", absent: true},
		{key: "collectors[].request.accept_status[]", of: "with an exponent", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "5.03e2", invalid: "6e2", absent: true},
		{key: "collectors[].request.accept_status[]", of: "in hex", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "0x1F7", invalid: "0x258", absent: true},
		{key: "collectors[].request.accept_status[]", of: "in octal", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "0o767", invalid: "0o1130", absent: true},
		{key: "collectors[].request.accept_status[]", of: "with an underscore", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "5_03", invalid: "6_00", absent: true},
		{key: "collectors[].request.accept_status[]", of: "with a sign and leading zeros", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "+0503", invalid: "-503", absent: true},
		{key: "collectors[].request.accept_status[]", of: "quoted, with a sign", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "'503'", invalid: "'+503'", absent: true},
		{key: "collectors[].request.accept_status[]", of: "quoted, with leading zeros", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "'4XX'", invalid: "'0503'", absent: true},
		{key: "collectors[].request.accept_status[]", of: "quoted, with blanks", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "'503'", invalid: "' 503 '", absent: true},
		{key: "collectors[].request.accept_status[]", of: "quoted, a class with a blank", document: jqCollector, at: request, setting: "      accept_status: [%s]\n", valid: "'5xx'", invalid: "'5xx '", absent: true},
		{key: "collectors[].request.retry.statuses[]", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "503", invalid: "600", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "written with a point", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "503.0", invalid: "503.5", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "with an exponent", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "5.03e2", invalid: "6e2", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "in hex", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "0x1F7", invalid: "0x258", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "in octal", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "0o767", invalid: "0o1130", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "with an underscore", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "5_03", invalid: "6_00", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "with a sign and leading zeros", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "+0503", invalid: "-503", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "quoted, with a sign", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "'503'", invalid: "'+503'", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "quoted, with leading zeros", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "'4XX'", invalid: "'0503'", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "quoted, with blanks", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "'503'", invalid: "' 503 '", absent: true},
		{key: "collectors[].request.retry.statuses[]", of: "quoted, a class with a blank", document: jqCollector, at: request, setting: "      retry:\n        statuses: [%s]\n", valid: "'5xx'", invalid: "'5xx '", absent: true},
		{key: "collectors[].request.allowed_targets[]", document: jqCollector, at: request, setting: "      allowed_targets: [%s]\n", valid: "api.example.com", invalid: "bücher.example", absent: true, emptyWas: taken},
		{key: "collectors[].request.denied_targets[]", document: jqCollector, at: request, setting: "      denied_targets: [%s]\n", valid: "api.example.com", invalid: "bücher.example", absent: true, emptyWas: taken},
		// A collector's proxy: both refuse a scheme no proxy is reached
		// by, and take "", the environment's proxy, as the key left out.
		{key: "collectors[].request.proxy_url", document: jqCollector, at: request, setting: "      proxy_url: %s\n", valid: "http://proxy.example:3128", invalid: "ftp://proxy.example", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.proxy_url", of: "with its scheme in capitals", document: jqCollector, at: request, setting: "      proxy_url: %s\n", valid: "HTTP://proxy.example:3128", invalid: "FTP://proxy.example", absent: true, empty: true, emptyWas: refused},
		// A collector's Unix socket: both refuse a relative path, and take
		// "" as the key left out.
		{key: "collectors[].request.unix_socket", document: jqCollector, at: request, setting: "      unix_socket: %s\n", valid: "/var/run/docker.sock", invalid: "var/run/docker.sock", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.unix_socket", of: "of nothing but blanks", document: jqCollector, at: request, setting: "      unix_socket: %s\n", valid: "/run/php/php-fpm.sock", invalid: `"  "`, absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.redirect_trusted_hosts[]", document: jqCollector, at: request, setting: "      follow_redirects: true\n      redirect_trusted_hosts: [%s]\n", valid: "cdn.example.com", invalid: "bücher.example", absent: true},
		// An entry of include or exclude is a regular expression: both
		// refuse the empty one, which matches every name, and one of
		// nothing but blanks, each of which both took.
		{key: "collectors[].transform.include[]", document: passthroughCollector, at: "      # transform\n", setting: "      include: [%s]\n", valid: `"^node_"`, invalid: `"  "`, absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].transform.exclude[]", document: passthroughCollector, at: "      # transform\n", setting: "      exclude: [%s]\n", valid: `"^go_"`, invalid: `"\t"`, absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].transform.libraries[]", document: pythonCollector, at: "      # transform\n", setting: "      libraries: [%s]\n", valid: "lxml", invalid: "requests", absent: true},
		{key: "collectors[].transform.required_libs[]", document: pythonCollector, at: "      # transform\n", setting: "      required_libs: [%s]\n", valid: "PyYAML", invalid: "requests", absent: true},
		{key: "collectors[].metrics[].value_map{}", document: jqCollector, at: metric, setting: "        value_map: {%s: 1}\n", valid: "up", invalid: `" up"`, absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].metrics[].labels[].value_map{}", document: jqCollector, at: label, setting: "            value_map: {%s: other}\n", valid: `"1"`, invalid: `"1 "`, absent: true, emptyWas: taken, invalidWas: taken},
		// Names of headers and of query parameters, and entries of
		// forward_headers: both refuse one that is empty, a header name
		// that is no HTTP token, a query name with a control character,
		// and a header a probe never forwards, each of which the schemas
		// took. The headers' names are the target file's too.
		{key: "collectors[].request.headers{}", document: jqCollector, at: request, setting: "      headers: {%s: v}\n", valid: "X-Tenant", invalid: `"X Bad"`, absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].request.headers{}", of: "Accept-Encoding", document: jqCollector, at: request, setting: "      headers: {%s: v}\n", valid: "X-Tenant", invalid: "accept-encoding", absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].request.query{}", document: jqCollector, at: request, setting: "      query: {%s: v}\n", valid: `"a b"`, invalid: `"region\n"`, absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].request.forward_headers[]", document: jqCollector, at: request, setting: "      forward_headers: [%s]\n", valid: `" X-Tenant "`, invalid: "X:Tenant", absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].request.forward_headers[]", of: "never forwarded", document: jqCollector, at: request, setting: "      forward_headers: [%s]\n", valid: "X-Proxy-Tenant", invalid: "proxy-connection", absent: true, emptyWas: taken, invalidWas: taken},
		{key: "targets[].request.headers{}", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      headers: {%s: v}\n", valid: "X-Tenant", invalid: `"X:Bad"`, absent: true, emptyWas: taken, invalidWas: taken},
		// The username of a Basic credential: both refuse one with a colon,
		// which ends the username in the credential, and which both took.
		// Left out and written "", it is the empty username beside the
		// password.
		{key: "collectors[].request.basic_auth.username", document: strings.Replace(jqCollector, request, "      basic_auth:\n        password: p\n        # auth\n", 1), at: "        # auth\n", setting: "        username: %s\n", valid: "metrics", invalid: `"metrics:reader"`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.basic_auth.username", file: inTargetFile, document: strings.Replace(oneTarget, target, "    request:\n      basic_auth:\n        password: p\n        # auth\n", 1), at: "        # auth\n", setting: "        username: %s\n", valid: "metrics", invalid: `":"`, absent: true, empty: true, invalidWas: taken},
		// Sizes: "" is no size, to both, and a number of bytes is one
		// however YAML writes the whole number. An output limit has a least
		// besides, which both hold a number to, so its 1 is refused by both
		// (TestSchemaAndExporterAgreeOnTheLeastOutputLimit).
		{key: "collectors[].limits.max_output_bytes", document: jqCollector, at: collector, setting: "    limits:\n      max_output_bytes: %s\n", valid: "1MiB", invalid: "lots", absent: true, size: true},
		{key: "collectors[].limits.max_response_bytes", document: jqCollector, at: collector, setting: "    limits:\n      max_response_bytes: %s\n", valid: "1MiB", invalid: "lots", absent: true, size: true},
		{key: "collectors[].limits.max_script_memory", document: jqCollector, at: collector, setting: "    limits:\n      max_script_memory: %s\n", valid: "64MiB", invalid: "lots", absent: true, size: true},
		// A size under the key's least is refused by both, written as text
		// too, which the schemas took before they read its digits
		// (fetch.SchemaLeastSize).
		{key: "collectors[].limits.max_script_memory", of: "under its least", document: jqCollector, at: collector, setting: "    limits:\n      max_script_memory: %s\n", valid: "32MiB", invalid: "16MiB", absent: true, invalidWas: taken},
		{key: "collectors[].limits.max_output_bytes", of: "under its least", document: jqCollector, at: collector, setting: "    limits:\n      max_output_bytes: %s\n", valid: "38B", invalid: "37B", absent: true, invalidWas: taken},
		{key: "collectors[].limits.max_response_bytes", of: "under its least", document: jqCollector, at: collector, setting: "    limits:\n      max_response_bytes: %s\n", valid: "1KiB", invalid: "1kB", absent: true, invalidWas: taken},
		{key: "collectors[].request.max_response_bytes", of: "under its least", document: jqCollector, at: request, setting: "      max_response_bytes: %s\n", valid: "1KiB", invalid: "1023B", absent: true, invalidWas: taken},
		{key: "otlp.batch_max_bytes", of: "under its least", document: switchedOnOTLP, at: "  # otlp\n", setting: "  batch_max_bytes: %s\n", valid: "64KiB", invalid: "63KiB", absent: true, invalidWas: taken},
		{key: "collectors[].request.max_response_bytes", document: jqCollector, at: request, setting: "      max_response_bytes: %s\n", valid: "1MiB", invalid: "lots", absent: true, size: true},
		{key: "otlp.batch_max_bytes", document: switchedOnOTLP, at: "  # otlp\n", setting: "  batch_max_bytes: %s\n", valid: "4MiB", invalid: "lots", absent: true, size: true},
		{key: "otlp.batch_max_bytes", of: "switched off", document: strings.Replace(switchedOnOTLP, "enabled: true", "enabled: false", 1), at: "  # otlp\n", setting: "  batch_max_bytes: %s\n", valid: "4MiB", invalid: "lots", absent: true, size: true},
		// Durations: "" is no duration, to both, and the number 0 is one.
		{key: "collectors[].cache.ttl", document: jqCollector, at: collector, setting: "    cache:\n      ttl: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].cache.stale_if_error", document: jqCollector, at: collector, setting: "    cache:\n      stale_if_error: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		// failure_backoff's waits are positive: a zero is the exporter's to
		// refuse, initial for itself and max for being shorter than the
		// 30s initial defaults to.
		{key: "collectors[].failure_backoff.initial", document: jqCollector, at: collector, setting: "    failure_backoff:\n      initial: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true, zero: "failure_backoff.initial must be a positive duration"},
		{key: "collectors[].failure_backoff.max", document: jqCollector, at: collector, setting: "    failure_backoff:\n      max: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true, zero: "must not be shorter than failure_backoff.initial 30s"},
		{key: "collectors[].failure_backoff.min_budget", document: jqCollector, at: collector, setting: "    failure_backoff:\n      min_budget: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true, zero: "failure_backoff.min_budget must be a positive duration"},
		{key: "collectors[].limits.script_timeout", document: jqCollector, at: collector, setting: "    limits:\n      script_timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		// A duration of one part under the key's least is refused by both,
		// which the schemas took before they read its digits
		// (fetch.SchemaLeastDuration).
		{key: "collectors[].limits.script_timeout", of: "under its least", document: jqCollector, at: collector, setting: "    limits:\n      script_timeout: %s\n", valid: "1ms", invalid: "500us", absent: true, invalidWas: taken},
		{key: "collectors[].request.retry.backoff", document: jqCollector, at: request, setting: "      retry:\n        backoff: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.timeout", document: switchedOnOTLP, at: "  # otlp\n", setting: "  timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.interval", document: switchedOnOTLP, at: "  # otlp\n", setting: "  interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.interval", of: "under its least", document: switchedOnOTLP, at: "  # otlp\n", setting: "  interval: %s\n", valid: "1s", invalid: "999ms", absent: true, invalidWas: taken},
		{key: "otlp.timeout", of: "switched off", document: strings.Replace(switchedOnOTLP, "enabled: true", "enabled: false", 1), at: "  # otlp\n", setting: "  timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.interval", of: "switched off", document: strings.Replace(switchedOnOTLP, "enabled: true", "enabled: false", 1), at: "  # otlp\n", setting: "  interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.logs.interval", document: switchedOnOTLP, at: "  # otlp\n", setting: "  logs:\n    enabled: true\n    interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "otlp.logs.interval", of: "under its least", document: switchedOnOTLP, at: "  # otlp\n", setting: "  logs:\n    enabled: true\n    interval: %s\n", valid: "1s", invalid: "999ms", absent: true},
		{key: "otlp.logs.interval", of: "switched off", document: switchedOnOTLP, at: "  # otlp\n", setting: "  logs:\n    enabled: false\n    interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},

		// The target file. A target's name left out is made of its
		// collector and its place; its collector is required.
		{key: "targets[].name", file: inTargetFile, document: oneTarget, at: "    name: a\n", setting: "    name: %s\n", valid: "a", invalid: "bad-name", absent: true, empty: true, emptyWas: refused},
		{key: "targets[].collector", file: inTargetFile, document: oneTarget, at: "    collector: demo\n", setting: "    collector: %s\n", valid: "demo", emptyWas: taken, booleanAlone: `references unknown collector "true"`, numberAlone: `references unknown collector "1"`},
		{key: "targets[].request.method", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      method: %s\n", valid: "POST", invalid: "FETCH", absent: true, empty: true, emptyWas: refused},
		{key: "targets[].request.accept_status[]", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "503", invalid: "600", absent: true},
		{key: "targets[].request.accept_status[]", of: "written with a point", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "503.0", invalid: "503.5", absent: true},
		{key: "targets[].request.accept_status[]", of: "with an exponent", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "5.03e2", invalid: "6e2", absent: true},
		{key: "targets[].request.accept_status[]", of: "in hex", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "0x1F7", invalid: "0x258", absent: true},
		{key: "targets[].request.accept_status[]", of: "in octal", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "0o767", invalid: "0o1130", absent: true},
		{key: "targets[].request.accept_status[]", of: "with an underscore", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "5_03", invalid: "6_00", absent: true},
		{key: "targets[].request.accept_status[]", of: "with a sign and leading zeros", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "+0503", invalid: "-503", absent: true},
		{key: "targets[].request.accept_status[]", of: "quoted, with a sign", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "'503'", invalid: "'+503'", absent: true},
		{key: "targets[].request.accept_status[]", of: "quoted, with leading zeros", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "'4XX'", invalid: "'0503'", absent: true},
		{key: "targets[].request.accept_status[]", of: "quoted, with blanks", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "'503'", invalid: "' 503 '", absent: true},
		{key: "targets[].request.accept_status[]", of: "quoted, a class with a blank", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      accept_status: [%s]\n", valid: "'5xx'", invalid: "'5xx '", absent: true},
		{key: "targets[].request.retry.statuses[]", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "503", invalid: "600", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "written with a point", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "503.0", invalid: "503.5", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "with an exponent", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "5.03e2", invalid: "6e2", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "in hex", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "0x1F7", invalid: "0x258", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "in octal", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "0o767", invalid: "0o1130", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "with an underscore", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "5_03", invalid: "6_00", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "with a sign and leading zeros", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "+0503", invalid: "-503", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "quoted, with a sign", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "'503'", invalid: "'+503'", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "quoted, with leading zeros", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "'4XX'", invalid: "'0503'", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "quoted, with blanks", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "'503'", invalid: "' 503 '", absent: true},
		{key: "targets[].request.retry.statuses[]", of: "quoted, a class with a blank", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        statuses: [%s]\n", valid: "'5xx'", invalid: "'5xx '", absent: true},
		{key: "targets[].params{}", file: inTargetFile, document: oneTarget, at: target, setting: "    params: {%s: one}\n", valid: "param_id", invalid: "tenant", absent: true},
		{key: "targets[].labels{}", file: inTargetFile, document: oneTarget, at: target, setting: "    labels: {%s: one}\n", valid: "team", invalid: "job", absent: true, emptyWas: taken, numberAlone: `has invalid label name "1"`},
		{key: "interval", file: inTargetFile, document: oneTarget, at: "interval: 1m\n", setting: "interval: %s\n", valid: "30s", invalid: "soon", duration: true, zero: "interval is required"},
		{key: "targets[].interval", file: inTargetFile, document: oneTarget, at: target, setting: "    interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "interval", of: "under its least", file: inTargetFile, document: oneTarget, at: "interval: 1m\n", setting: "interval: %s\n", valid: "1s", invalid: "500ms", invalidWas: taken},
		{key: "targets[].interval", of: "under its least", file: inTargetFile, document: oneTarget, at: target, setting: "    interval: %s\n", valid: "1s", invalid: "0.5s", absent: true, invalidWas: taken},
		{key: "targets[].request.timeout", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      timeout: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "targets[].request.retry.backoff", file: inTargetFile, document: oneTarget, at: target, setting: "    request:\n      retry:\n        backoff: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
	}
	// request.login, a collector's and a static target's: its type is
	// required; a token_header or token_cookie stands for the token, one
	// of which is required; the rest are optional, an enum's or a
	// pattern's "" the key left out.
	login := func(prefix, document, at, indent string) []schemaKey {
		logged := func(lines ...string) string {
			var block strings.Builder
			for _, line := range lines {
				block.WriteString(indent)
				block.WriteString(line)
				block.WriteByte('\n')
			}
			return strings.Replace(document, at, block.String(), 1)
		}
		json := logged("login:", "  type: json", "  url: /login", "  token: .t", "  field_files: {password: /p}", "  # login")
		oauth2 := logged("login:", "  type: oauth2", "  url: https://idp.example/token", "  client_id: x", "  client_secret_file: /s", "  # login")
		in := func(text string) string { return indent + "  " + text }
		mark := in("# login\n")
		return []schemaKey{
			{key: prefix + ".login.type", document: json, at: in("type: json\n"), setting: in("type: %s\n"), valid: "json", invalid: "form"},
			{key: prefix + ".login.url", document: json, at: in("url: /login\n"), setting: in("url: %s\n"), valid: "/api/login", invalid: "login"},
			{key: prefix + ".login.token_header", document: json, at: in("token: .t\n"), setting: in("token_header: %s\n"), valid: "X-Subject-Token", invalid: `"X Token"`},
			{key: prefix + ".login.token_cookie", document: json, at: in("token: .t\n"), setting: in("token_cookie: %s\n"), valid: "JSESSIONID", invalid: `"S ID"`},
			{key: prefix + ".login.header", document: json, at: mark, setting: in("header: %s\n"), valid: "X-Auth-Token", invalid: `"X Token"`, absent: true, empty: true, emptyWas: refused},
			{key: prefix + ".login.ttl", document: json, at: mark, setting: in("ttl: %s\n"), valid: "15m", invalid: "soon", absent: true, duration: true},
			{key: prefix + ".login.auth_style", document: oauth2, at: mark, setting: in("auth_style: %s\n"), valid: "params", invalid: "auto", absent: true, empty: true, emptyWas: refused},
			{key: prefix + ".login.scopes[]", document: oauth2, at: mark, setting: in("scopes: [%s]\n"), valid: "metrics", invalid: `"a b"`, absent: true},
			{key: prefix + ".login.endpoint_params{}", document: oauth2, at: mark, setting: in("endpoint_params: {%s: x}\n"), valid: "audience", invalid: "grant_type", absent: true},
		}
	}
	keys = append(keys, login("collectors[].request", jqCollector, request, "      ")...)
	keys = append(keys, defaultsSchemaKeys(keys, collector, request)...)
	for _, row := range login("targets[].request", strings.Replace(oneTarget, target, "    request:\n      # target request\n", 1), "      # target request\n", "      ") {
		row.file = inTargetFile
		keys = append(keys, row)
	}
	for i := range keys {
		if keys[i].file == inTargetFile {
			keys[i].configuration = targetsConfiguration
		}
	}
	return keys
}

// defaultsSchemaKeys are the rows of the keys of defaults, which both the
// configuration and a collector file have: each the row of the collector's
// key it sets, written in a defaults block at the top of the file over the
// collector of the row, which takes it. The schemas and the exporter hold
// a key of defaults to what they hold the collector's to.
func defaultsSchemaKeys(rows []schemaKey, collector, request string) []schemaKey {
	var keys []schemaKey
	for _, row := range rows {
		rest, ok := strings.CutPrefix(row.key, "collectors[].")
		if !ok || row.document != jqCollector || row.at != collector && row.at != request {
			continue
		}
		block, _, _ := strings.Cut(rest, ".")
		if row.at == request {
			block, _, _ = strings.Cut(strings.TrimPrefix(rest, "request."), ".")
			block = "request." + strings.TrimRight(block, "[]")
		}
		if !slices.Contains(model.DefaultsKeys, block) {
			continue
		}
		// The collector's setting, indented under defaults.
		setting := "defaults:\n"
		indent := "    "
		if row.at == request {
			setting += "  request:\n"
			indent = "      "
		}
		for _, line := range strings.SplitAfter(row.setting, "\n") {
			if line != "" {
				setting += strings.Replace(line, indent, indent[2:], 1)
			}
		}
		row.key, row.document, row.at, row.setting = "defaults."+rest, "# defaults\n"+jqCollector, "# defaults\n", setting
		keys = append(keys, row)
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
// written "", as it is left out; each duration key takes the number 0,
// which the exporter reads as the duration, and no other number; and each
// size key takes a whole number of bytes however YAML writes the number.
func TestSchemaAndExporterAgreeOnKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, httpSchemaKeys())
}

// request.unix_socket is held to the length a socket's path may have on the
// system the exporter runs on by the exporter alone: 107 bytes on Linux and
// 103 on macOS and the BSDs, which no one maxLength of the schema could say
// for both. A path of the limit's length is taken by both, and one a byte
// longer is past the schema and refused by the exporter, naming its length
// and the limit; it loaded, and every request failed when it dialed.
func TestTheExporterAloneHoldsAUnixSocketToTheSystemsLength(t *testing.T) {
	limit, known := map[string]int{"linux": 107, "darwin": 103, "freebsd": 103, "netbsd": 103, "openbsd": 103, "dragonfly": 103}[runtime.GOOS]
	if !known {
		t.Skipf("no known limit on %s", runtime.GOOS)
	}
	schema := loadSchema(t)
	socket := func(n int) string {
		return strings.Replace(jqCollector, "      # request\n", "      unix_socket: /"+strings.Repeat("s", n-1)+"\n", 1)
	}
	agree(t, schema, fmt.Sprintf("unix_socket of %d bytes", limit), socket(limit), true)
	loadersAlone(t, schema, fmt.Sprintf("unix_socket of %d bytes", limit+1), socket(limit+1),
		fmt.Sprintf("request.unix_socket is %d bytes long, and a Unix socket's path may be at most %d bytes on %s", limit+1, limit, runtime.GOOS))
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
		`otlp with compression: "" and no enabled`:         {"otlp: {compression: \"\"}\n" + testutil.MinimalConfig, false},
		`otlp switched off with compression: ""`:           {"otlp: {enabled: false, compression: \"\"}\n" + testutil.MinimalConfig, true},
		`a label with an expression and value: ""`:         {strings.Replace(jqCollector, "            # label\n", "            value: \"\"\n", 1), true},
		`a label with both written ""`:                     {strings.Replace(strings.Replace(jqCollector, "expression: .l\n", "expression: \"\"\n", 1), "            # label\n", "            value: \"\"\n", 1), false},
		`a label with a value and an expression`:           {strings.Replace(jqCollector, "            # label\n", "            value: x\n", 1), false},
		`a label with a value of one blank`:                {strings.Replace(jqCollector, "expression: .l\n", "value: \" \"\n", 1), true},
		`a label whose value YAML reads as a number`:       {strings.Replace(jqCollector, "expression: .l\n", "value: 0\n", 1), true},
		`a prometheus rule with name: "" and a type`:       {strings.Replace(prometheusCollector, "        # metric\n", "        name: \"\"\n        type: \"\"\n", 1), true},
		`a jq rule with every optional key written ""`:     {strings.Replace(jqCollector, "        # metric\n", "        type: \"\"\n        error_mode: \"\"\n        description: \"\"\n        items: \"\"\n        time_format: \"\"\n        time_zone: \"\"\n", 1), true},
		`a python rule with every optional key written ""`: {strings.Replace(pythonCollector, "        # metric\n", "        type: \"\"\n        error_mode: \"\"\n        description: \"\"\n        expression: \"\"\n        items: \"\"\n        time_format: \"\"\n        time_zone: \"\"\n", 1), true},
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
