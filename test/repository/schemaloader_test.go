//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// The schema an editor checks a configuration against and the exporter that
// loads it give the same verdict on the keys below. They had drifted: the
// exporter came to refuse a csv delimiter of more than one character, a size
// with a fraction and no unit, and settings beside a missing enabled, while
// the schema went on accepting the first two, so an editor showed as valid a
// configuration that did not start. Each document here goes through both, the
// committed schema and config.Load, and both must say what the table says.

// strictProblems checks what the validator of schema_test.go lets through
// and a validator of JSON Schema does not: maxLength, the length of a string
// in characters, so a character of several bytes is one; and a key left
// empty, which is null and none of the types a key of the schema takes.
func strictProblems(schema map[string]any, value any, path string) []string {
	var problems []string
	switch x := value.(type) {
	case nil:
		if types, typed := schema["type"]; typed {
			problems = append(problems, fmt.Sprintf("%s: null is not %v", path, types))
		}
	case string:
		if most, ok := schema["maxLength"].(float64); ok && float64(utf8.RuneCountInString(x)) > most {
			problems = append(problems, fmt.Sprintf("%s: %q is longer than %v", path, x, most))
		}
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		for key, item := range x {
			if sub, ok := properties[key].(map[string]any); ok {
				problems = append(problems, strictProblems(sub, item, path+"."+key)...)
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range x {
				problems = append(problems, strictProblems(items, item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	return problems
}

// verdicts are what the schema and the exporter say of a configuration: the
// problems the schema finds, and the error of loading it.
func verdicts(t *testing.T, schema map[string]any, document string) ([]string, error) {
	t.Helper()
	var doc any
	if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, document)
	}
	value := normalizeYAML(doc)
	problems := append(validateAgainstSchema(schema, value), strictProblems(schema, value, "")...)
	_, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
	return problems, err
}

// agree fails unless the schema and the exporter both accept the document,
// or both refuse it, as accepted says.
func agree(t *testing.T, schema map[string]any, name, document string, accepted bool) {
	t.Helper()
	problems, err := verdicts(t, schema, document)
	if (len(problems) == 0) != accepted || (err == nil) != accepted {
		t.Errorf("%s: want accepted %v by both; the schema says %v, the exporter %v\n%s", name, accepted, problems, err, document)
	}
}

// loadersAlone fails unless the schema accepts the document and the exporter
// refuses it with the message: what a schema cannot tell, which the
// descriptions say the exporter checks.
func loadersAlone(t *testing.T, schema map[string]any, name, document, message string) {
	t.Helper()
	problems, err := verdicts(t, schema, document)
	if len(problems) != 0 || err == nil || !strings.Contains(err.Error(), message) {
		t.Errorf("%s: want it past the schema and refused by the exporter with %q; the schema says %v, the exporter %v", name, message, problems, err)
	}
}

// csvCollector is a configuration of one csv collector with the delimiter as
// written.
func csvCollector(delimiter string) string {
	return "collectors:\n  - name: tsv\n    request: {type: http}\n    response:\n      csv:\n        delimiter: " + delimiter + "\n    transform: {type: csv}\n    metrics:\n      - name: cpu\n        expression: cpu\n"
}

// response.csv.delimiter is one character, and not a double quote, a line
// break, the NUL or the replacement character; left empty it is the default.
// A tab is one character when YAML reads it as one.
func TestSchemaAndExporterAgreeOnTheCSVDelimiter(t *testing.T) {
	schema := loadSchema(t)
	for written, accepted := range map[string]bool{
		`";"`: true, `'|'`: true, `','`: true, `'#'`: true, `" "`: true, `"\t"`: true, `"é"`: true, `"§"`: true, `"€"`: true, `"😀"`: true, `""`: true,
		`tab`: false, `'\t'`: false, `";;"`: false, `'||'`: false, `", "`: false, `'"'`: false, `"\""`: false, `"\n"`: false, `"\r"`: false, `"\r\n"`: false, `"\x00"`: false, `"�"`: false, `"éé"`: false,
	} {
		agree(t, schema, "delimiter: "+written, csvCollector(written), accepted)
	}
}

// sizedCollector is a configuration of one collector whose
// limits.max_output_bytes is as written.
func sizedCollector(size string) string {
	return "collectors:\n  - name: sized\n    request: {type: http}\n    transform: {type: regex}\n    limits:\n      max_output_bytes: " + size + "\n    metrics:\n      - name: v\n        expression: 'v=(\\d+)'\n"
}

// A size is a whole number of bytes or a number with a unit, as a YAML number
// or as text; a fraction needs a unit, and a sign, an exponent, an unknown
// unit and space around it are refused. The range, and a number YAML reads
// as a whole one where the exporter sees how it was written, are the
// exporter's alone to refuse.
func TestSchemaAndExporterAgreeOnSizes(t *testing.T) {
	schema := loadSchema(t)
	for written, accepted := range map[string]bool{
		"0": true, "1024": true, "'512'": true, "0x100": true, "1_000": true, "4611686018427387904": true,
		"10B": true, "10 B": true, "1.5B": true, "1k": true, "1K": true, "1kB": true, "1KB": true, "1.5 kb": true,
		"2Ki": true, "2KiB": true, "2kib": true, "0.5KiB": true, "0.0001KiB": true, "10MB": true, "64MiB": true, "64 MiB": true,
		"1.5MiB": true, "1G": true, "1.5GiB": true, "0.5GiB": true, "1T": true, "1TiB": true, "8388607TiB": true,

		"lots": false, "''": false, "MiB": false, "-1": false, "'-1'": false, "-1MiB": false, "-1.5MiB": false, "+5MiB": false,
		"1.5": false, "0.5": false, "'1.5'": false, "'1.0'": false, "'0.0'": false, ".5MiB": false, "5.MiB": false, "1.5.5MiB": false, "'1,5MiB'": false,
		"'1e3'": false, "1e3KiB": false, "'0x100'": false, "'1_000'": false, "8EiB": false, "1PiB": false, "10 XB": false, "10MiBs": false,
		"10  MiB": false, "' 10MiB'": false, "'10MiB '": false, "[1]": false, "{bytes: 1}": false, "true": false,
	} {
		agree(t, schema, "max_output_bytes: "+written, sizedCollector(written), accepted)
	}
	for written, message := range map[string]string{
		"8388608TiB":            `size "8388608TiB" is too large; a size is under 8EiB, which is 2^63 bytes`,
		"99999999999TiB":        `size "99999999999TiB" is too large`,
		"'9223372036854775808'": `size "9223372036854775808" is too large`,
		"1.0":                   `size "1.0" is not a whole number of bytes`,
		"1e3":                   `size "1e3" is not a number of bytes or a number with a unit`,
	} {
		loadersAlone(t, schema, "max_output_bytes: "+written, sizedCollector(written), message)
	}
}

// otlp.interval is a duration to both, and its least, 1s, is the exporter's
// to check: a duration is text to a schema. Switched off, the block is kept
// unchecked by both.
func TestSchemaAndExporterAgreeOnTheOTLPInterval(t *testing.T) {
	schema := loadSchema(t)
	otlp := func(enabled bool, interval string) string {
		return fmt.Sprintf("otlp:\n  enabled: %v\n  endpoint: http://collector.invalid:4318/v1/metrics\n  interval: %s\n", enabled, interval) + testutil.MinimalConfig
	}
	for written, accepted := range map[string]bool{"1s": true, "30s": true, "1m": true, "1h30m": true, "1.5s": true, "soon": false, "5": false, "1d": false, "[1s]": false} {
		agree(t, schema, "otlp.interval: "+written, otlp(true, written), accepted)
	}
	for _, written := range []string{"500ms", "999ms", "1ns", "-1s"} {
		loadersAlone(t, schema, "otlp.interval: "+written, otlp(true, written), "otlp.interval "+written+" is under the least, 1s")
		agree(t, schema, "otlp.interval: "+written+", switched off", otlp(false, written), true)
	}
	if description, _ := schema["properties"].(map[string]any)["otlp"].(map[string]any)["properties"].(map[string]any)["interval"].(map[string]any)["description"].(string); !strings.Contains(description, "At least 1s, which the exporter checks when the configuration loads") {
		t.Errorf("the description of otlp.interval does not say who checks its least: %q", description)
	}
}

// A block that enabled switches on — otlp, web.basic_auth — is refused by
// both when it sets any other key without saying enabled, and accepted by
// both with enabled: false, whatever the key: every key of each block is
// tried, so one added to a block is held to the rule in the schema too.
func TestSchemaAndExporterAgreeOnEnabled(t *testing.T) {
	schema := loadSchema(t)
	// A value of each key's type that says nothing else wrong.
	value := func(field reflect.StructField) string {
		switch key, _, _ := strings.Cut(field.Tag.Get("yaml"), ","); {
		case key == "compression":
			return model.OTLPCompressionGzip
		case field.Type == reflect.TypeOf(model.Duration(0)):
			return "10s"
		case field.Type.Kind() == reflect.Bool:
			return "true"
		case field.Type.Kind() == reflect.Int:
			return "1"
		case field.Type.Kind() == reflect.Map:
			return "{a: b}"
		case field.Type.Kind() == reflect.Struct:
			return "{insecure_skip_verify: true}"
		}
		return "x"
	}
	for block, t0 := range map[string]reflect.Type{"otlp": reflect.TypeOf(model.OTLPConfig{}), "web.basic_auth": reflect.TypeOf(model.ExporterBasicAuth{})} {
		open, closing := block+": {", "}\n"
		if outer, inner, nested := strings.Cut(block, "."); nested {
			open, closing = outer+": {"+inner+": {", "}}\n"
		}
		keys := 0
		for i := 0; i < t0.NumField(); i++ {
			key, _, _ := strings.Cut(t0.Field(i).Tag.Get("yaml"), ",")
			if key == "" || key == "-" || key == "enabled" {
				continue
			}
			keys++
			setting := key + ": " + value(t0.Field(i))
			agree(t, schema, block+" with "+key+" alone", open+setting+closing+testutil.MinimalConfig, false)
			agree(t, schema, block+" with "+key+" and an empty enabled", open+"enabled: , "+setting+closing+testutil.MinimalConfig, false)
			agree(t, schema, block+" with "+key+", switched off", open+"enabled: false, "+setting+closing+testutil.MinimalConfig, true)
		}
		if keys < 4 {
			t.Errorf("%s: only %d keys were tried", block, keys)
		}
		agree(t, schema, block+" empty", open+closing+testutil.MinimalConfig, true)
		agree(t, schema, block+" with enabled alone, off", open+"enabled: false"+closing+testutil.MinimalConfig, true)
	}
	for name, test := range map[string]struct {
		block    string
		accepted bool
	}{
		"otlp switched on":             {"otlp: {enabled: true, endpoint: 'http://collector.invalid:4318/v1/metrics'}\n", true},
		"otlp, the switch merged in":   {"x-on: &on {enabled: true}\notlp: {<<: *on, endpoint: 'http://collector.invalid:4318/v1/metrics'}\n", true},
		"otlp, a setting merged in":    {"x-otlp: &otlp {endpoint: 'http://collector.invalid:4318/v1/metrics'}\notlp: {<<: *otlp, interval: 10s}\n", false},
		"basic_auth switched on":       {"web: {basic_auth: {enabled: true, username: admin, password: s3cret}}\n", true},
		"basic_auth with two settings": {"web: {basic_auth: {username: admin, password: s3cret}}\n", false},
	} {
		agree(t, schema, name, test.block+testutil.MinimalConfig, test.accepted)
	}
}
