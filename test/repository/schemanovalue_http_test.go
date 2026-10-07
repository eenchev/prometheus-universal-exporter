//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"strings"
	"testing"
)

// The rule that a value is not written as none is of every key alike, the
// ones the exporter requires and the ones it does not: in a configuration
// that loads, each of some keys written with nothing, with null and with ~
// is refused by the schema and by the exporter, the same configuration
// loading with the key left out where the key is optional. A bare dash
// among a list's entries — the rule its author meant to write — is refused
// where it stands, first, in the middle or last, with its place in the list
// and its line, and so are two of them, each for itself.
func TestSchemaAndExporterAgreeOnAKeyOrAnEntryWrittenWithNoValue(t *testing.T) {
	schema := loadSchema(t)
	const base = "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: prometheus\n"
	agree(t, schema, "the configuration as it is", base, true)
	for _, value := range []string{"", " null", " ~", " Null", " NULL"} {
		for _, setting := range []string{
			"    name_escaping:%s\n", "    metrics_prefix:%s\n", "    coalesce:%s\n", "    max_concurrent_probes:%s\n", "    limits:%s\n", "    limits:\n      max_metrics:%s\n",
			"    cache:%s\n", "    cache:\n      ttl:%s\n", "    response:%s\n", "    metrics:%s\n", "    error_handling:\n      on_fetch_error:%s\n",
		} {
			agree(t, schema, strings.TrimSpace(fmt.Sprintf(setting, value)), base+fmt.Sprintf(setting, value), false)
		}
		for _, document := range []string{
			strings.Replace(base, "name: demo", "name:"+value, 1), strings.Replace(base, "type: http", "type:"+value, 1), strings.Replace(base, "type: prometheus", "type:"+value, 1),
			strings.Replace(base, "    request:\n      type: http\n", "    request:"+value+"\n", 1), strings.Replace(base, "    transform:\n      type: prometheus\n", "    transform:"+value+"\n", 1),
			"collectors:" + value + "\n", "collector_files:" + value + "\n" + base, "web:" + value + "\n" + base, "otlp:" + value + "\n" + base,
			"otlp:\n  enabled:" + value + "\n" + base, "otlp:\n  enabled: false\n  endpoint:" + value + "\n" + base, "web:\n  basic_auth:" + value + "\n" + base,
		} {
			agree(t, schema, "a key written with"+value, document, false)
		}
		// A value of a mapping, which the exporter took for "" or for 0.
		for _, setting := range []string{"      labels: {site:%s }\n", "      labels:\n        site:%s\n", "      rename: {up:%s }\n", "      rename_labels: {job:%s }\n"} {
			agree(t, schema, strings.TrimSpace(fmt.Sprintf(setting, value)), base+fmt.Sprintf(setting, value), false)
		}
		agree(t, schema, "a value_map's value written with"+value, strings.Replace(jqCollector, "        # metric\n", "        value_map: {up:"+value+" , down: 0}\n", 1), false)
		agree(t, schema, "a header written with"+value, strings.Replace(jqCollector, "      # request\n", "      headers:\n        Accept:"+value+"\n", 1), false)
	}
	// A list's entry.
	rules := func(entries ...string) string {
		return base + "    metrics:\n" + strings.Join(entries, "")
	}
	up, load := "      - name: up\n", "      - name: load\n        expression: '^node_load1$'\n"
	for _, dash := range []string{"      -\n", "      - null\n", "      - ~\n"} {
		agree(t, schema, "the rules without the dash", rules(up, load), true)
		how := "is written " + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(dash), "-"))
		if dash == "      -\n" {
			how = "is a dash with nothing after it"
		}
		for name, tc := range map[string]struct {
			document string
			want     []string
		}{
			"first":         {rules(dash, up, load), []string{"line 8: metrics entry 1 " + how}},
			"in the middle": {rules(up, dash, load), []string{"line 9: metrics entry 2 " + how}},
			"last":          {rules(up, load, dash), []string{"line 11: metrics entry 3 " + how}},
			"alone":         {rules(dash), []string{"line 8: metrics entry 1 " + how}},
			"twice":         {rules(dash, up, dash), []string{"line 8: metrics entry 1 " + how, "line 10: metrics entry 3 " + how}},
		} {
			problems, err := verdicts(t, schema, tc.document)
			if len(problems) == 0 || err == nil {
				t.Errorf("a dash %s, %q: want it refused by both; the schema says %v, the exporter %v", name, dash, problems, err)
				continue
			}
			for _, want := range tc.want {
				if want += ", which YAML reads as no value at all, so the entry would be left out without a word; write a metric rule there, or take the entry out"; !strings.Contains(err.Error(), want) {
					t.Errorf("a dash %s, %q: the exporter says %v\nwant %s", name, dash, err, want)
				}
			}
		}
	}
	for setting, want := range map[string]string{
		"      include: [~, up]\n":                                       "line 7: include entry 1 is written ~, which YAML reads as no value at all, so the entry would be left out without a word; write a value there, or take the entry out",
		"      exclude:\n        - up\n        -\n":                      "line 9: exclude entry 2 is a dash with nothing after it",
		"      remove_labels: [null]\n":                                  "line 7: remove_labels entry 1 is written null",
		"      libraries:\n        -\n":                                  "line 8: libraries entry 1 is a dash with nothing after it",
		"    metrics:\n      - name: up\n        labels:\n          -\n": "line 10: labels entry 1 is a dash with nothing after it, which YAML reads as no value at all, so the entry would be left out without a word; write a label there, or take the entry out",
	} {
		problems, err := verdicts(t, schema, base+setting)
		if len(problems) == 0 || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want it refused by both, by the exporter with %q; the schema says %v, the exporter %v", setting, want, problems, err)
		}
	}
	for _, document := range []string{"collectors:\n  -\n", "collectors:\n  -\n" + strings.TrimPrefix(base, "collectors:\n"), base + "  - ~\n", "collector_files:\n  -\n" + base} {
		problems, err := verdicts(t, schema, document)
		if len(problems) == 0 || err == nil || !strings.Contains(err.Error(), "which YAML reads as no value at all, so the entry would be left out without a word") {
			t.Errorf("want it refused by both; the schema says %v, the exporter %v\n%s", problems, err, document)
		}
	}
	// What is written as text that is empty is a value, as it was.
	for _, setting := range []string{"    name_escaping: \"\"\n", "    metrics_prefix: ''\n", "    limits: {}\n", "    metrics: []\n", "      labels: {site: \"\"}\n", "    cache: {}\n"} {
		agree(t, schema, strings.TrimSpace(setting), base+setting, true)
	}
}
