//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// checkedCollector is a jq collector over JSON with what a test adds to its
// request block, to the collector itself and to its one metric rule.
func checkedCollector(request, collector, metric string) string {
	return "collectors:\n  - name: a\n    request:\n      type: http\n" + request +
		"    decoder: {type: json}\n    transform: {type: jq}\n" + collector +
		"    metrics:\n      - name: m\n        expression: .x\n" + metric
}

func loadChecked(t *testing.T, request, collector, metric string) (*model.Config, error) {
	t.Helper()
	return Load(testutil.WriteFile(t, "config.yaml", checkedCollector(request, collector, metric)))
}

// A negative limit is refused naming the key and the value; it once became
// the default without a word. 0, and a limit left out, are the default.
func TestANegativeLimitIsRefused(t *testing.T) {
	for _, key := range []string{"max_metrics", "max_labels_per_metric", "max_label_value_length", "max_metric_name_length", "max_help_length", "max_cache_entries"} {
		_, err := loadChecked(t, "", "    limits: {"+key+": -3}\n", "")
		if want := fmt.Sprintf(`collector "a" limits.%s is -3, and a limit must not be negative; leave it out, or 0, for the default`, key); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", key, err, want)
		}
	}
	for name, test := range map[string]struct{ limits, want string }{
		"a negative timeout":       {"script_timeout: -1s", `collector "a" limits.script_timeout is -1s, and a timeout must not be negative; leave it out for the default, 100ms`},
		"a negative size":          {"max_output_bytes: -1", "line 7: size -1 is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB"},
		"a negative size, in text": {"max_response_bytes: -1MiB", `line 7: size "-1MiB" is not a number of bytes or a number with a unit`},
	} {
		if _, err := loadChecked(t, "", "    limits: {"+test.limits+"}\n", ""); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	// A size built in code rather than read from a file is checked too.
	c := testutil.Collector("a", "text")
	c.Limits.MaxOutputBytes = -1
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), "limits.max_output_bytes is -1, and a limit must not be negative") {
		t.Errorf("a negative max_output_bytes: %v", err)
	}
	cfg, err := loadChecked(t, "", "    limits: {max_metrics: 0, max_output_bytes: 0, script_timeout: 0s}\n    max_concurrent_probes: 0\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if l := cfg.Collectors[0].Limits; l.MaxMetrics != 10000 || l.MaxOutputBytes != 1<<20 || l.ScriptTimeout == 0 {
		t.Fatalf("0 is the default: %+v", l)
	}
}

// A number with a fraction where a whole number belongs is refused naming the
// key, the value and the line, in the configuration and the static target
// file alike; YAML would drop the fraction, making 0.5 the default and 1.9
// one. A whole number written as 1.0 or 1e3 is one.
func TestAFractionWhereAWholeNumberBelongsIsRefused(t *testing.T) {
	for name, test := range map[string]struct{ request, collector, want string }{
		"a limit":                 {"", "    limits: {max_metrics: 0.5}\n", "line 7: max_metrics is 0.5, which is not a whole number"},
		"max_concurrent_probes":   {"", "    max_concurrent_probes: 1.9\n", "line 7: max_concurrent_probes is 1.9, which is not a whole number"},
		"retry attempts":          {"      retry: {attempts: 2.5}\n", "", "line 5: attempts is 2.5, which is not a whole number"},
		"a size without a unit":   {"      max_response_bytes: 1.5\n", "", `line 5: size "1.5" is not a whole number of bytes; write a whole number, or a number with a unit such as 1.5KiB`},
		"merged in from an x-key": {"", "    limits: {<<: *limits}\nx-limits: &limits {max_metrics: 2.5}\n", "max_metrics is 2.5, which is not a whole number"},
	} {
		document := checkedCollector(test.request, test.collector, "")
		if name == "merged in from an x-key" {
			document = "x-limits: &limits {max_metrics: 2.5}\n" + checkedCollector("", "    limits: {<<: *limits}\n", "")
		}
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	targets := "interval: 1m\nconcurrency: 1.5\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {retry: {attempts: 0.5}}\n"
	_, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targets))
	for _, want := range []string{"line 2: concurrency is 1.5, which is not a whole number", "line 7: attempts is 0.5, which is not a whole number"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("static targets: error %v, want %q", err, want)
		}
	}
	// With another mistake in the file, both are reported at once.
	_, err = loadChecked(t, "", "    limits: {max_metrics: 0.5}\n    requst: {}\n", "")
	if err == nil || !strings.Contains(err.Error(), `unknown key "requst"`) || !strings.Contains(err.Error(), "max_metrics is 0.5") {
		t.Errorf("an unknown key and a fraction: %v", err)
	}
	cfg, err := loadChecked(t, "", "    limits: {max_metrics: 1e3, max_help_length: 100.0}\n    max_concurrent_probes: 4.0\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if x := cfg.Collectors[0]; x.Limits.MaxMetrics != 1000 || x.Limits.MaxHelpLength != 100 || x.MaxConcurrentProbes != 4 {
		t.Fatalf("whole numbers written as floats: %+v, %d", x.Limits, x.MaxConcurrentProbes)
	}
}

// A number with a fraction, or a null key, in an anchor is a mistake only
// where the decoder uses it. A mapping that merges the anchor in and sets the
// key itself, before the merge key or after it, overrides it, and of a list
// of merges the first that sets a key is the one read: the overridden value
// was refused all the same, though the configuration it is written in loads
// with the value the mapping gives. One that is used is refused, at the line
// it is written on.
func TestAnOverriddenValueOfAnAnchorIsNotRefused(t *testing.T) {
	const anchors = "x-half: &half {max_metrics: 0.5}\nx-seven: &seven {max_metrics: 7}\nx-nine: &nine {<<: *half, max_metrics: 9}\nx-retry: &retry {retry: {attempts: 2.5}}\n"
	for name, test := range map[string]struct {
		request, limits string
		maxMetrics      int
	}{
		"its own key after the merge":      {"", "{<<: *half, max_metrics: 5}", 5},
		"its own key before the merge":     {"", "{max_metrics: 5, <<: *half}", 5},
		"the first of a list sets the key": {"", "{<<: [*seven, *half]}", 7},
		"a merged mapping overrides its":   {"", "{<<: *nine}", 9},
		"a whole mapping of its own":       {"      <<: *retry\n      retry: {attempts: 2}\n", "{max_metrics: 3}", 3},
	} {
		document := anchors + checkedCollector(test.request, "    limits: "+test.limits+"\n", "")
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := cfg.Collectors[0].Limits.MaxMetrics; got != test.maxMetrics {
			t.Errorf("%s: max_metrics is %d, want %d", name, got, test.maxMetrics)
		}
	}
	if cfg, err := Load(testutil.WriteFile(t, "config.yaml", anchors+checkedCollector("      <<: *retry\n      retry: {attempts: 2}\n", "", ""))); err != nil || cfg.Collectors[0].Request.Retry.Attempts != 2 {
		t.Errorf("its own retry over the merged one: %v", err)
	}
	for name, test := range map[string]struct{ request, limits, metric, want string }{
		"merged in and not overridden":      {"", "{<<: *half}", "", "line 1: max_metrics is 0.5, which is not a whole number"},
		"the first of a list has it":        {"", "{<<: [*half, *seven]}", "", "line 1: max_metrics is 0.5, which is not a whole number"},
		"a merged mapping's own merge":      {"", "{<<: *third}", "", "line 1: max_metrics is 0.5, which is not a whole number"},
		"a merged mapping with the mistake": {"      <<: *retry\n", "{}", "", "line 4: attempts is 2.5, which is not a whole number"},
		"a null key merged in":              {"", "{}", "        value_map: {<<: *states, down: 0}\n", "line 6: value_map has the key ~, which YAML reads as no key at all"},
	} {
		document := anchors + "x-third: &third {<<: *half}\nx-states: &states {~: 5, up: 1}\n" + checkedCollector(test.request, "    limits: "+test.limits+"\n", test.metric)
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	// The static target file is walked the same way.
	targets := "x-half: &half {attempts: 0.5}\ninterval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {retry: {<<: *half, attempts: 1}}\n"
	file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targets))
	if err != nil {
		t.Fatalf("static targets: %v", err)
	}
	if attempts := file.Targets[0].Request.Retry.Attempts; attempts == nil || *attempts != 1 {
		t.Fatalf("static targets: retry.attempts is %v, want 1", attempts)
	}
}

// A size of 2^63 bytes or more is past what a size can hold and is refused,
// with a unit or without, rather than wrapping round to a negative one.
func TestASizeTooLargeIsRefused(t *testing.T) {
	for _, size := range []string{"8388608TiB", "9223372036854775808", "99999999999999999999", "1e30"} {
		if _, err := loadChecked(t, "", "    limits: {max_output_bytes: "+size+"}\n", ""); err == nil || !strings.Contains(err.Error(), "line 7: size") || !strings.Contains(err.Error(), size) {
			t.Errorf("%s: %v", size, err)
		}
	}
	for _, size := range []string{"8388608TiB", "9223372036854775808"} {
		if _, err := loadChecked(t, "", "    limits: {max_output_bytes: "+size+"}\n", ""); err == nil || !strings.Contains(err.Error(), "is too large; a size is under 8EiB, which is 2^63 bytes") {
			t.Errorf("%s: %v", size, err)
		}
	}
	cfg, err := loadChecked(t, "", "    limits: {max_output_bytes: 8388607TiB}\n", "")
	if err != nil || cfg.Collectors[0].Limits.MaxOutputBytes != 8388607<<40 {
		t.Fatalf("the largest whole number of TiB: %v", err)
	}
}

// A mapping key YAML reads as no value — null, ~, or nothing before the
// colon — would be dropped with its entry, so it is refused naming the
// mapping and the line; quoted, it is the text.
func TestANullMappingKeyIsRefused(t *testing.T) {
	for name, test := range map[string]struct{ request, metric, want string }{
		"null in a value_map":       {"", "        value_map: {null: 0, up: 1}\n", "line 10: value_map has the key null, which YAML reads as no key at all, so the entry would be dropped; to use that text as the key, quote it"},
		"a tilde in a value_map":    {"", "        value_map: {~: 0, up: 1}\n", "line 10: value_map has the key ~, which YAML reads as no key at all"},
		"nothing before the colon":  {"", "        value_map:\n          ? \n          : 0\n          up: 1\n", "line 11: value_map has the key nothing, which YAML reads as no key at all"},
		"in a label's value_map":    {"", "        labels:\n          - name: state\n            expression: .state\n            value_map: {Null: none}\n", "line 13: value_map has the key Null"},
		"a header named by nothing": {"      headers: {~: x}\n", "", "line 5: headers has the key ~"},
	} {
		if _, err := loadChecked(t, test.request, "", test.metric); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	cfg, err := loadChecked(t, "", "", "        value_map: {\"null\": 0, '~': 2, up: 1}\n")
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Collectors[0].Metrics[0].ValueMap; len(m) != 3 || m["~"] != 2 {
		t.Fatalf("quoted keys: %v", m)
	}
}

// A {{param_...}} placeholder is filled in only where the request takes one.
// In any other setting of a collector it would be used as written — sent as
// the token, exported as the label — so it is refused naming the field. Where
// placeholders are filled in, they still load.
func TestAPlaceholderWhereNoneIsFilledIsRefused(t *testing.T) {
	for name, test := range map[string]struct{ request, collector, metric, where string }{
		"bearer_token":           {"      bearer_token: \"{{param_token:abc}}\"\n", "", "", "request.bearer_token"},
		"basic_auth username":    {"      basic_auth: {username: \"{{param_user}}\", password: x}\n", "", "", "request.basic_auth.username"},
		"basic_auth password":    {"      basic_auth: {username: u, password: \"{{param_password}}\"}\n", "", "", "request.basic_auth.password"},
		"tls.server_name":        {"      tls: {server_name: \"{{param_host}}\"}\n", "", "", "request.tls.server_name"},
		"bearer_token_file":      {"      bearer_token_file: \"/run/{{param_tenant}}/token\"\n", "", "", "request.bearer_token_file"},
		"a basic_auth_file path": {"      basic_auth_file: {username: \"/run/{{param_tenant}}/user\", password: /run/password}\n", "", "", "request.basic_auth_file.username"},
		"transform.labels":       {"", "    transform: {type: jq, labels: {tenant: \"{{param_tenant:acme}}\"}}\n", "", "transform.labels.tenant"},
		"a static label value":   {"", "", "        labels:\n          - name: region\n            value: \"{{param_region:eu}}\"\n", `metric "m" label "region" value`},
		"a metric's value_map":   {"", "", "        value_map: {\"{{param_state}}\": 1}\n", `metric "m" value_map`},
		"a label's value_map":    {"", "", "        labels:\n          - name: region\n            expression: .region\n            value_map: {eu: \"{{param_region}}\"}\n", `metric "m" label "region" value_map.eu`},
		"with a space":           {"      bearer_token: \"{{ param_token }}\"\n", "", "", "request.bearer_token"},
	} {
		collector := test.collector
		document := checkedCollector(test.request, "", test.metric)
		if collector != "" {
			document = strings.Replace(document, "    transform: {type: jq}\n", collector, 1)
		}
		_, err := Load(testutil.WriteFile(t, "config.yaml", document))
		want := fmt.Sprintf(`collector "a" %s has a {{param_...}} placeholder, which is not filled in there and would be used as written; a probe's parameters fill placeholders only in the request's path, body, header and query values`, test.where)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", name, err, want)
		}
	}
	// Every mistake of the kind is reported in one pass.
	_, err := loadChecked(t, "      bearer_token: \"{{param_token}}\"\n", "", "        labels:\n          - name: region\n            value: \"{{param_x}}\"\n")
	if err == nil || !strings.Contains(err.Error(), "request.bearer_token") || !strings.Contains(err.Error(), `metric "m" label "region" value`) {
		t.Errorf("two placeholders: %v", err)
	}
	cfg, err := loadChecked(t, "      method: POST\n      path: /api/{{param_tenant:acme}}\n      headers: {X-Tenant: \"{{param_tenant:acme}}\"}\n      query: {region: \"{{param_region:eu}}\"}\n      body: '{\"service\": {{param_service:web|json}}}'\n      bearer_token: \"{{not_a_param}}\"\n", "", "")
	if err != nil {
		t.Fatalf("placeholders where they are filled in: %v", err)
	}
	if got := cfg.Collectors[0].Request.BearerToken; got != "{{not_a_param}}" {
		t.Fatalf("other braces are text: %q", got)
	}
}

// A pattern of a prometheus transform matches metric names, which hold no
// braces, so a placeholder in one is refused as in any setting.
func TestAPlaceholderInAPrometheusPatternIsRefused(t *testing.T) {
	for _, key := range []string{"include", "exclude"} {
		document := "collectors:\n  - name: a\n    request: {type: http}\n    transform: {type: prometheus, " + key + ": [\"^{{param_prefix}}_\"]}\n"
		want := `collector "a" transform.` + key + `[0] has a {{param_...}} placeholder, which is not filled in there`
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", key, err, want)
		}
	}
}

// What a collector writes in a language of its own is not searched for
// placeholders: the text {{param_ in a Python script, an expression or a
// description is that language's, and refusing it refused scripts and
// expressions that ran as written — an f-string's literal braces, a nested
// dictionary, a regex matching the text, a description naming the parameter
// the path takes. They load, as they are written, and the scripts pass the
// interpreter's check.
func TestCodeExpressionsAndDescriptionsMayHoldTheTextOfAPlaceholder(t *testing.T) {
	python := func(key, code string) string {
		transform := "    transform:\n      type: jq\n      pre_script: |\n" + code
		if key == "script" {
			transform = "    transform:\n      type: python\n      script: |\n" + code
		}
		document := "collectors:\n  - name: a\n    request: {type: http}\n    decoder: {type: json}\n" + transform
		if key == "pre_script" {
			document += "    metrics:\n      - name: m\n        expression: .x\n"
		}
		return document
	}
	rules := func(transform, rule string) string {
		return "collectors:\n  - name: a\n    request: {type: http, path: \"/api/{{param_tenant}}\"}\n    transform: {type: " + transform + "}\n    metrics:\n      - name: m\n" + rule
	}
	for name, test := range map[string]struct {
		document string
		// holds is where the text is kept, as written.
		holds func(x *model.Collector) string
	}{
		"an f-string in a script": {python("script", "        param_x = 1\n        s = f\"{{param_x}}\"\n        metric(name=\"v\", value=1)\n"),
			func(x *model.Collector) string { return x.Transform.Script }},
		"a comment and a string in a script": {python("script", "        # builds a link like /probe?path=/api/{{param_tenant}}\n        tmpl = \"{{param_\" + \"x}}\"\n        metric(name=\"v\", value=1)\n"),
			func(x *model.Collector) string { return x.Transform.Script }},
		"nested braces in a pre_script": {python("pre_script", "        param_a = 1\n        data = {\"x\": {{param_a}: 1}} if False else {\"x\": 1}\n"),
			func(x *model.Collector) string { return x.Transform.PreScript }},
		"a description": {rules("jq", "        description: \"Depth of the queue of the tenant given as {{param_tenant}}\"\n        expression: .x\n"),
			func(x *model.Collector) string { return x.Metrics[0].Description }},
		"a jq string": {rules("jq", "        expression: '.templates[\"{{param_x}}\"]'\n"),
			func(x *model.Collector) string { return x.Metrics[0].Expression }},
		"jq items": {rules("jq", "        items: '.[\"{{param_x}}\"][]'\n        expression: .x\n"),
			func(x *model.Collector) string { return x.Metrics[0].Items }},
		"a regex over the text": {rules("regex", "        expression: 'x{{param_(\\d+)'\n"),
			func(x *model.Collector) string { return x.Metrics[0].Expression }},
		"a label expression": {rules("jq", "        expression: .x\n        labels:\n          - name: region\n            expression: '.regions[\"{{param_region}}\"]'\n"),
			func(x *model.Collector) string { return x.Metrics[0].Labels[0].Expression }},
	} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", test.document))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if held := test.holds(&cfg.Collectors[0]); !strings.Contains(held, "{{param_") {
			t.Errorf("%s: kept as %q", name, held)
		}
		if err := ValidatePythonScripts("python3", cfg); err != nil {
			t.Errorf("%s: the interpreter refuses it: %v", name, err)
		}
	}
}

// A setting of a decoder or a transform the collector does not have is
// refused, as response.graphite is without the graphite decoder, rather than
// accepted and ignored; with the decoder or transform it is for, it loads.
func TestASettingOfAnotherDecoderOrTransformIsRefused(t *testing.T) {
	base := func(decoder, transform, response, rule string) string {
		document := "collectors:\n  - name: a\n    request: {type: http}\n"
		if decoder != "" {
			document += "    decoder: {type: " + decoder + "}\n"
		}
		return document + "    transform: " + transform + "\n" + response + rule
	}
	jqRule := "    metrics:\n      - name: m\n        expression: .x\n"
	for name, test := range map[string]struct{ document, want string }{
		"transform.script on jq":        {base("json", `{type: jq, script: "data = {'x': 1}"}`, "", jqRule), `collector "a" sets transform.script, which is the script of a python transform, but its transform is jq; set transform.type to python, or, to change the response before the jq rules read it, move the code to transform.pre_script`},
		"transform.libraries on jq":     {base("json", "{type: jq, libraries: [lxml]}", "", jqRule), `collector "a" sets transform.libraries, which are imported for the collector's Python code, but it has neither a python transform nor a pre_script`},
		"transform.required_libs on jq": {base("json", "{type: jq, required_libs: [lxml]}", "", jqRule), `collector "a" sets transform.required_libs, which are imported for the collector's Python code`},
		"response.csv with json":        {base("json", "{type: jq}", "    response: {csv: {delimiter: \";\", header: false}}\n", jqRule), `collector "a" sets response.csv, which applies to the csv decoder, but its decoder is json`},
		"response.csv.trim_space alone": {base("text", "{type: regex}", "    response: {csv: {trim_space: true}}\n", "    metrics:\n      - name: m\n        expression: 'x=(\\d+)'\n"), `sets response.csv, which applies to the csv decoder, but its decoder is text`},
		"response.namespaces on jq":     {base("json", "{type: jq}", "    response: {namespaces: {a: \"urn:x\"}}\n", jqRule), `collector "a" sets response.namespaces, which name the prefixes of an xpath transform's expressions, but its transform is jq`},
		"response.namespaces on python": {base("xml", "{type: python, script: \"metric(name='m', value=1)\"}", "    response: {namespaces: {a: \"urn:x\"}}\n", ""), `sets response.namespaces, which name the prefixes of an xpath transform's expressions, but its transform is python`},
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", test.document)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	for name, document := range map[string]string{
		"libraries for a pre_script":      base("json", "{type: jq, libraries: [lxml], pre_script: \"data = data\"}", "", jqRule),
		"response.csv with csv":           base("", "{type: csv}", "    response: {csv: {delimiter: \";\"}}\n", "    metrics:\n      - name: m\n        expression: x\n"),
		"response.csv, decoder undecided": base("", "{type: python, script: \"metric(name='m', value=1)\"}", "    response: {csv: {header: false}}\n", ""),
		"namespaces with xpath":           base("xml", "{type: xpath}", "    response: {namespaces: {a: \"urn:x\"}}\n", "    metrics:\n      - name: m\n        expression: //a:x\n"),
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// response.csv.delimiter is one character the CSV reader can split on. More
// than one, such as the two characters \t of a single-quoted '\t', a double
// quote and a line break are refused when the configuration loads, saying
// how a tab is written; a tab written "\t", and any other single character,
// load.
func TestTheCSVDelimiterIsOneCharacter(t *testing.T) {
	document := func(delimiter string) string {
		return "collectors:\n  - name: tsv\n    request: {type: http}\n    response:\n      csv:\n        delimiter: " + delimiter + "\n    transform: {type: csv}\n    metrics:\n      - name: cpu\n        expression: cpu\n"
	}
	for name, test := range map[string]struct{ written, shown string }{
		"backslash and t":           {`'\t'`, `'\t'`},
		"a word":                    {"tab", "'tab'"},
		"two characters":            {`";;"`, "';;'"},
		"a double quote":            {`'"'`, `'"'`},
		"a line feed":               {`"\n"`, `"\n"`},
		"a carriage return":         {`"\r"`, `"\r"`},
		"the replacement character": {`"\uFFFD"`, "'\uFFFD'"},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", document(test.written)))
		want := `collector "tsv" response.csv.delimiter ` + test.shown + ` must be one character, and not a double quote or a line break; for a tab, write delimiter: "\t" in double quotes, where YAML reads \t as the tab character`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", name, err, want)
		}
	}
	for name, test := range map[string]struct{ written, want string }{
		"a tab":                 {`"\t"`, "\t"},
		"a semicolon":           {`";"`, ";"},
		"a pipe":                {`"|"`, "|"},
		"a space":               {`" "`, " "},
		"a letter of two bytes": {`"§"`, "§"},
	} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", document(test.written)))
		if err != nil || cfg.Collectors[0].Response.CSV.Delimiter != test.want {
			t.Errorf("%s: %v", name, err)
		}
	}
}
