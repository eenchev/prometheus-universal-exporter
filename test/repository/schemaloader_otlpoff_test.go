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
		checked: []otlpValue{{`{"bad name": x}`, `otlp.headers "bad name" is not a header name`}, {`{a: "x\ny"}`, "otlp.headers a has the control character"}},
		noValue: []string{"[a]", "{a: [b]}", "x"},
	},
	"timeout":  {checked: []otlpValue{{"-5s", ""}, {"-1ns", ""}}, noValue: []string{"soon", "30", "[5s]"}},
	"interval": {checked: []otlpValue{{"-1s", ""}, {"500ms", "otlp.interval 500ms is under the least, 1s"}}, noValue: []string{"soon", "30", "[5s]"}},
	"tls": {
		checked: []otlpValue{{"{cert_file: /nonexistent/client.pem}", "otlp.tls sets only one of cert_file and key_file"}, {"{ca_file: /nonexistent/ca.pem}", "otlp.tls cannot be used"}},
		noValue: []string{"x", "{insecure_skip_verify: maybe}", "{unknown: 1}", "{ca_file: [a]}"},
	},
	"insecure_skip_verify": {noValue: []string{"maybe", "1", "[true]"}},
	"service_name":         {noValue: []string{"[a]", "{a: b}"}},
	"resource_attributes": {
		checked: []otlpValue{{"{service.name: x}", "otlp.resource_attributes sets service.name, which otlp.service_name sets"}},
		noValue: []string{"[a]", "{a: [b]}", "x"},
	},
	"probe_attributes":       {noValue: []string{"maybe", "1"}},
	"compression":            {checked: []otlpValue{{"zstd", ""}, {"GZIP", ""}, {`" "`, ""}, {"5", ""}, {"true", ""}}, noValue: []string{"[gzip]", "{a: b}"}},
	"max_pending_points":     {checked: []otlpValue{{"-1", ""}, {"-100000", ""}}, noValue: []string{"1.5", "many", "[1]"}},
	"unready_after_failures": {checked: []otlpValue{{"-1", ""}, {"-3", ""}}, noValue: []string{"1.5", "many", "[1]"}},
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
	for _, setting := range []string{"compression: none", "compression: gzip", `compression: ""`, "max_pending_points: 0", "max_pending_points: 5000", "unready_after_failures: 3", "timeout: 10s", "interval: 1m"} {
		key, value, _ := strings.Cut(setting, ": ")
		agree(t, schema, "otlp."+setting+", switched off", block(false, key, value), true)
		agree(t, schema, "otlp."+setting+", switched on", block(true, key, value), true)
	}
	// The switch is what the rule goes by: written as text, or left out,
	// the block is not one that is switched off.
	agree(t, schema, `otlp with enabled: "false"`, "otlp: {enabled: \"false\", compression: zstd}\n"+testutil.MinimalConfig, false)
	agree(t, schema, "otlp without enabled", "otlp: {compression: zstd}\n"+testutil.MinimalConfig, false)
}
