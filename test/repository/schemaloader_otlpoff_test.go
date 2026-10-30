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
	"metric_names": {checked: []otlpValue{{"OTel", ""}, {"openmetrics", ""}, {"5", ""}}, noValue: []string{"[otel]", "{a: b}"}},
	// A scope written is held to a name, without a control character in
	// it or in its version.
	"scope": {
		checked: []otlpValue{{"{}", ""}, {`{version: "1"}`, ""}, {`{name: ""}`, ""}, {`{name: "a\tb"}`, ""}, {`{name: x, version: "1\n"}`, ""}},
		noValue: []string{"x", "[a]", "{unknown: 1}", "{name: [a]}"},
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
		"logs: {enabled: true}", "logs: {enabled: false, level: verbose, interval: -1s}", "logs: {enabled: true, level: DEBUG, interval: 1s, max_pending_records: 0, endpoint: \"http://logs.invalid/v1/logs\"}", `logs: {enabled: true, level: ""}`} {
		key, value, _ := strings.Cut(setting, ": ")
		agree(t, schema, "otlp."+setting+", switched off", block(false, key, value), true)
		agree(t, schema, "otlp."+setting+", switched on", block(true, key, value), true)
	}
	// The switch is what the rule goes by: written as text, or left out,
	// the block is not one that is switched off.
	agree(t, schema, `otlp with enabled: "false"`, "otlp: {enabled: \"false\", compression: zstd}\n"+testutil.MinimalConfig, false)
	agree(t, schema, "otlp without enabled", "otlp: {compression: zstd}\n"+testutil.MinimalConfig, false)
}
