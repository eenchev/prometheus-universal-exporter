//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// otlpValue is a value of a key of the otlp block that a block switched on
// is refused for: by the schema and the exporter both, or, where a schema
// cannot tell, by the exporter alone, with the message.
type otlpValue struct {
	written string
	alone   string
}

// otlpKeys are the keys of the otlp block but enabled, each with the values
// a block that is switched on is refused for, checked, and the values that
// are none of the key's type, which no block may hold, noValue.
var otlpKeys = map[string]struct {
	checked []otlpValue
	noValue []string
}{
	"endpoint": {
		checked: []otlpValue{{"not-a-url", "otlp.endpoint must be an http or https URL"}, {"ftp://collector.invalid/v1/metrics", "otlp.endpoint must be an http or https URL"}, {`""`, "otlp.endpoint is required when OTLP is enabled"}},
		noValue: []string{"[a]", "{a: b}"},
	},
	"headers": {
		checked: []otlpValue{{`{"bad name": x}`, ""}, {`{Accept-Encoding: br}`, ""}, {`{a: "x\ny"}`, "otlp.headers a has the control character"}},
		noValue: []string{"[a]", "{a: [b]}", "x"},
	},
	"timeout": {checked: []otlpValue{{"-5s", ""}, {"-1ns", ""}}, noValue: []string{"soon", "30", "[5s]"}},
	// An interval of one part under the least is refused by both, and one
	// of several parts, whose sum a schema cannot tell, by the exporter.
	"interval": {checked: []otlpValue{{"-1s", ""}, {"500ms", ""}, {"0.5s", ""}, {"400ms400ms", "otlp.interval 800ms is under the least, 1s"}}, noValue: []string{"soon", "30", "[5s]"}},
	"tls": {
		checked: []otlpValue{{"{cert_file: /nonexistent/client.pem}", ""}, {"{key_file: /nonexistent/client.key}", ""}, {"{ca_file: /nonexistent/ca.pem}", "otlp.tls cannot be used"}},
		noValue: []string{"x", "{insecure_skip_verify: maybe}", "{unknown: 1}", "{ca_file: [a]}"},
	},
	"service_name": {noValue: []string{"[a]", "{a: b}"}},
	"resource_attributes": {
		checked: []otlpValue{{"{service.name: x}", "otlp.resource_attributes sets service.name, which otlp.service_name sets"}},
		noValue: []string{"[a]", "{a: [b]}", "x"},
	},
	"probe_attributes":       {noValue: []string{"maybe", "1"}},
	"exponential_histograms": {noValue: []string{"maybe", "1"}},
	"compression":            {checked: []otlpValue{{"zstd", ""}, {"GZIP", ""}, {`" "`, ""}, {"5", ""}, {"true", ""}}, noValue: []string{"[gzip]", "{a: b}"}},
	"max_pending_points":     {checked: []otlpValue{{"-1", ""}, {"-100000", ""}}, noValue: []string{"1.5", "many", "[1]"}},
	"unready_after_failures": {checked: []otlpValue{{"-1", ""}, {"-3", ""}, {"1001", ""}}, noValue: []string{"1.5", "many", "[1]"}},
	"batch_max_size":         {checked: []otlpValue{{"-1", ""}, {"-8192", ""}}, noValue: []string{"1.5", "many", "[1]"}},
	// The logs block is checked once it is switched on itself, in a block
	// switched on: an endpoint that is no URL by the exporter alone. A logs
	// block that sets a key without enabled, or a key it does not have,
	// is refused off and on, as the otlp block is.
	"logs": {
		checked: []otlpValue{
			{"{enabled: true, level: verbose}", ""}, {"{enabled: true, level: 5}", ""}, {"{enabled: true, interval: 500ms}", ""}, {"{enabled: true, interval: -1s}", ""},
			{"{enabled: true, max_pending_records: -1}", ""}, {"{enabled: true, endpoint: not-a-url}", "otlp.logs.endpoint must be an http or https URL"},
			{"{enabled: true, interval: 400ms400ms}", "otlp.logs.interval 800ms is under the least, 1s"},
		},
		noValue: []string{"x", "[a]", "{level: debug}", "{enabled: maybe}", "{unknown: 1}", "{enabled: true, interval: soon}", "{enabled: true, max_pending_records: many}"},
	},
	// The traces block is checked as the logs block is: once it is
	// switched on itself, in a block switched on; its threshold at least
	// 1ms, by both where it is one part and by the exporter alone where it
	// is several; its sample_ratio from 0 to 1 and its max_per_second not
	// negative, by both.
	"traces": {
		checked: []otlpValue{
			{"{enabled: true, threshold: 500us}", ""}, {"{enabled: true, threshold: -1s}", ""}, {"{enabled: true, interval: 500ms}", ""},
			{"{enabled: true, max_pending_spans: -1}", ""}, {"{enabled: true, endpoint: not-a-url}", "otlp.traces.endpoint must be an http or https URL"},
			{"{enabled: true, threshold: 400us400us}", "otlp.traces.threshold 800µs is under the least, 1ms"},
			{"{enabled: true, interval: 400ms400ms}", "otlp.traces.interval 800ms is under the least, 1s"},
			{"{enabled: true, sample_ratio: 1.5}", ""}, {"{enabled: true, sample_ratio: -0.1}", ""}, {"{enabled: true, max_per_second: -1}", ""},
		},
		noValue: []string{"x", "[a]", "{threshold: 1s}", "{enabled: maybe}", "{unknown: 1}", "{enabled: true, threshold: soon}", "{enabled: true, errors: maybe}", "{enabled: true, max_pending_spans: many}",
			"{enabled: true, parent_based: maybe}", "{enabled: true, sample_ratio: half}", "{enabled: true, sample_ratio: [1]}", "{enabled: true, max_per_second: 1.5}"},
	},
	"metric_names": {checked: []otlpValue{{"OTel", ""}, {"openmetrics", ""}, {"5", ""}}, noValue: []string{"[otel]", "{a: b}"}},
	// A scope written is held to a name, without a control character in
	// it or in its version.
	"scope": {
		checked: []otlpValue{{"{}", ""}, {`{version: "1"}`, ""}, {`{name: ""}`, ""}, {`{name: "a\tb"}`, ""}, {`{name: x, version: "1\n"}`, ""}},
		noValue: []string{"x", "[a]", "{unknown: 1}", "{name: [a]}"},
	},
	// The exporter_instance block is checked once it is switched on
	// itself, in a block switched on: an attribute that is service.name,
	// blanks alone or holds a control character, and a value that holds
	// one, by both. A key of otlp.resource_attributes is the exporter's
	// alone to refuse (TestTheExporterAloneRefusesAnExporterInstanceAttributeOfTheResource).
	"exporter_instance": {
		checked: []otlpValue{
			{"{enabled: true, attribute: service.name}", ""}, {`{enabled: true, attribute: " "}`, ""}, {`{enabled: true, attribute: "a\tb"}`, ""},
			{`{enabled: true, value: "pod\n0"}`, ""},
		},
		noValue: []string{"x", "[a]", "{attribute: a}", "{enabled: maybe}", "{unknown: 1}", "{enabled: true, attribute: [a]}", "{enabled: false, value: {a: b}}"},
	},
	// A size written as text is held to the least by both, as one
	// written as a number is.
	"batch_max_bytes": {
		checked: []otlpValue{{"1", ""}, {"65535", ""}, {"1KiB", ""}, {`"65535"`, ""}, {"63.9KiB", ""}, {"0.06MB", ""}},
		noValue: []string{"-1", "1.5", "lots", "[1]"},
	},
}

// An otlp block with enabled: false is kept unchecked, by the schema as by
// the exporter: its settings are there for later, and nothing uses them. The
// two agreed on that for its durations, and differed on three keys: the
// schema refused a compression that is not gzip or none, and a negative
// max_pending_points or unready_after_failures, in a block the exporter
// loads, so an editor flagged what the exporter takes. Every key of the
// block is put through both here, switched off and switched on, with each
// value a block that is on is refused for — by both, or by the exporter
// alone where a schema cannot tell, such as an endpoint that is no URL —
// and each such value is taken by both in a block that is off. What is not
// of the key's type at all — text for a number, a list for text, an unknown
// key of tls — is refused by both, off and on: it is not a setting kept for
// later but one that cannot be read. A value in order is taken by both, off
// and on, and a block that is on is still held to every one of its rules.
func TestSchemaAndExporterAgreeOnASwitchedOffOTLPBlock(t *testing.T) {
	schema := loadSchema(t)
	block := func(enabled bool, key, value string) string {
		lines := map[string]string{"endpoint": "http://collector.invalid:4318/v1/metrics", key: value}
		var document strings.Builder
		fmt.Fprintf(&document, "otlp:\n  enabled: %v\n", enabled)
		for _, name := range model.SortedKeys(lines) {
			fmt.Fprintf(&document, "  %s: %s\n", name, lines[name])
		}
		return document.String() + testutil.MinimalConfig
	}
	fields := reflect.TypeOf(model.OTLPConfig{})
	for i := 0; i < fields.NumField(); i++ {
		key, _, _ := strings.Cut(fields.Field(i).Tag.Get("yaml"), ",")
		if key == "enabled" {
			continue
		}
		row, listed := otlpKeys[key]
		if !listed {
			t.Errorf("otlp.%s is a key of the block and is not in the table: say what a block switched on is refused for of it, and what is no value of it", key)
			continue
		}
		var off, on []string
		for _, value := range row.checked {
			name := "otlp." + key + ": " + value.written
			agree(t, schema, name+", switched off", block(false, key, value.written), true)
			if value.alone == "" {
				agree(t, schema, name+", switched on", block(true, key, value.written), false)
				on = append(on, value.written+" refused by both")
			} else {
				loadersAlone(t, schema, name+", switched on", block(true, key, value.written), value.alone)
				on = append(on, value.written+" refused by the exporter")
			}
			off = append(off, value.written)
		}
		for _, written := range row.noValue {
			agree(t, schema, "otlp."+key+": "+written+", switched off", block(false, key, written), false)
			agree(t, schema, "otlp."+key+": "+written+", switched on", block(true, key, written), false)
		}
		if len(row.noValue) == 0 {
			t.Errorf("otlp.%s: the table has no value that is none of the key's type", key)
		}
		t.Logf("| %s | %s | %s | %s |", key, strings.Join(off, ", "), strings.Join(on, "; "), strings.Join(row.noValue, ", "))
	}
	if len(otlpKeys) != fields.NumField()-1 {
		t.Errorf("the table has %d keys, and the block %d beside enabled", len(otlpKeys), fields.NumField()-1)
	}
	// Values in order, off and on.
	for _, setting := range []string{"compression: none", "compression: gzip", `compression: ""`, "max_pending_points: 0", "max_pending_points: 5000", "unready_after_failures: 3", "unready_after_failures: 1000", "timeout: 10s", "interval: 1m", "batch_max_size: 0", "batch_max_size: 1", "batch_max_bytes: 0", "batch_max_bytes: 65536", "batch_max_bytes: 64KiB", "batch_max_bytes: 4MiB",
		"metric_names: otel", "metric_names: prometheus", `metric_names: ""`, "scope: {name: my-scope}", `scope: {name: my-scope, version: "1.2.3"}`, `scope: {name: "1"}`,
		"traces: {enabled: true}", "traces: {enabled: false, threshold: 1us, interval: -1s}", "traces: {enabled: true, threshold: 1ms, errors: true, interval: 1s, max_pending_spans: 0, endpoint: \"http://traces.invalid/v1/traces\"}",
		"traces: {enabled: true, parent_based: true, sample_ratio: 0.25, max_per_second: 10}", "traces: {enabled: true, sample_ratio: 1, max_per_second: 0}", "traces: {enabled: true, sample_ratio: 0}",
		"traces: {enabled: false, parent_based: true, sample_ratio: 2, max_per_second: -1}",
		"logs: {enabled: true}", "logs: {enabled: false, level: verbose, interval: -1s}", "logs: {enabled: true, level: DEBUG, interval: 1s, max_pending_records: 0, endpoint: \"http://logs.invalid/v1/logs\"}", `logs: {enabled: true, level: ""}`,
		"exporter_instance: {enabled: true}", `exporter_instance: {enabled: true, attribute: host.name, value: pod-0}`, `exporter_instance: {enabled: true, attribute: "", value: ""}`,
		"exporter_instance: {enabled: false, attribute: service.name}", "exporter_instance: {enabled: true, attribute: service.instance.id}"} {
		key, value, _ := strings.Cut(setting, ": ")
		agree(t, schema, "otlp."+setting+", switched off", block(false, key, value), true)
		agree(t, schema, "otlp."+setting+", switched on", block(true, key, value), true)
	}
	// The switch is what the rule goes by: written as text, or left out,
	// the block is not one that is switched off.
	agree(t, schema, `otlp with enabled: "false"`, "otlp: {enabled: \"false\", compression: zstd}\n"+testutil.MinimalConfig, false)
	agree(t, schema, "otlp without enabled", "otlp: {compression: zstd}\n"+testutil.MinimalConfig, false)
}

// otlp.exporter_instance.attribute is refused for a key of
// otlp.resource_attributes, written "" or not, which a schema cannot compare
// it with: the exporter alone refuses it, in a block switched on, and both
// take it in a block, or an exporter_instance block, switched off.
func TestTheExporterAloneRefusesAnExporterInstanceAttributeOfTheResource(t *testing.T) {
	schema := loadSchema(t)
	document := func(enabled, instance bool, value string) string {
		return fmt.Sprintf("otlp:\n  enabled: %v\n  endpoint: http://collector.invalid:4318/v1/metrics\n  resource_attributes: {host.name: %s}\n  exporter_instance: {enabled: %v, attribute: host.name}\n", enabled, value, instance) + testutil.MinimalConfig
	}
	for _, value := range []string{"a", `""`} {
		loadersAlone(t, schema, "host.name of resource_attributes, "+value, document(true, true, value), "otlp.exporter_instance.attribute is host.name, which otlp.resource_attributes sets too")
		agree(t, schema, "host.name of resource_attributes, "+value+", otlp switched off", document(false, true, value), true)
		agree(t, schema, "host.name of resource_attributes, "+value+", exporter_instance switched off", document(true, false, value), true)
	}
}
