//go:build !select_request_types || request_type_http

package repository

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

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
	for _, written := range []string{"500ms", "999ms", "1ns"} {
		loadersAlone(t, schema, "otlp.interval: "+written, otlp(true, written), "otlp.interval "+written+" is under the least, 1s")
		agree(t, schema, "otlp.interval: "+written+", switched off", otlp(false, written), true)
	}
	// A negative interval is refused by both, and kept by both in a block
	// that is switched off.
	agree(t, schema, "otlp.interval: -1s", otlp(true, "-1s"), false)
	agree(t, schema, "otlp.interval: -1s, switched off", otlp(false, "-1s"), true)
	if description, _ := schema["properties"].(map[string]any)["otlp"].(map[string]any)["properties"].(map[string]any)["interval"].(map[string]any)["description"].(string); !strings.Contains(description, "At least 1s, which the exporter checks when the configuration loads") {
		t.Errorf("the description of otlp.interval does not say who checks its least: %q", description)
	}
}

// durationKey is a duration key of one of the three files that have a
// schema: check gives, for a duration as written, what that file's schema
// finds wrong with the document setting the key to it, and the error of the
// exporter loading the document.
type durationKey struct {
	name  string
	check func(t *testing.T, written string) ([]string, error)
}

// lastWords is an error without the path of the file it is about, which
// differs from one temporary directory to the next.
func lastWords(err error) string {
	text := err.Error()
	return text[strings.LastIndex(text, ".yaml")+1:]
}

// A duration is refused by a schema exactly when the exporter refuses it for
// how it is written: no key takes a negative duration, and the schemas took
// any sign, so an editor showed timeout: -5s as valid in a file the exporter
// then refused. Each duration below is put, in each of the three files, in
// a key of each kind — one that takes zero, one whose zero is its default
// and whose least is 1s, and the target file's interval, which is required —
// through that file's committed schema and its loader. A + is taken by both
// before any duration and a - before a zero only; how long a duration must
// be stays the exporter's to check, a duration being text to a schema. A
// block that is switched off holds any duration, to both. An unquoted 0,
// which YAML reads as a number, is the duration zero to both, with a sign
// or without, and no other number is a duration to either. The two things
// both cannot say alike are named as such at the end: a duration too long to
// be held, and a zero that is a number to YAML and is not written 0.
func TestSchemaAndExporterAgreeOnDurations(t *testing.T) {
	configSchema, collectorFileSchema, targetsSchema := loadSchema(t), loadSchemaFile(t, collectorFileSchemaFile), loadSchemaFile(t, staticTargetsSchemaFile)
	inConfig := func(document func(written string) string) func(*testing.T, string) ([]string, error) {
		return func(t *testing.T, written string) ([]string, error) {
			t.Helper()
			return verdicts(t, configSchema, document(written))
		}
	}
	// A collector file is loaded as the configuration listing it is.
	inCollectorFile := func(t *testing.T, written string) ([]string, error) {
		t.Helper()
		document := strings.Replace(testutil.CollectorsDocument("demo"), "      path: /status\n", "      path: /status\n      retry:\n        backoff: "+written+"\n", 1)
		dir := t.TempDir()
		testutil.WriteIn(t, dir, "collectors.yaml", document)
		_, err := config.Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [collectors.yaml]\n"))
		return schemaProblems(t, collectorFileSchema, document), err
	}
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", testutil.MinimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	inTargets := func(document func(written string) string) func(*testing.T, string) ([]string, error) {
		return func(t *testing.T, written string) ([]string, error) {
			t.Helper()
			file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document(written)))
			if err == nil {
				err = config.ValidateStaticTargets(file)
			}
			if err == nil {
				err = config.ValidateStaticTargetsAgainst(file, cfg)
			}
			return schemaProblems(t, targetsSchema, document(written)), err
		}
	}
	otlp := func(key string, enabled bool) func(string) string {
		return func(written string) string {
			return fmt.Sprintf("otlp:\n  enabled: %v\n  endpoint: http://collector.invalid:4318/v1/metrics\n  %s: %s\n", enabled, key, written) + testutil.MinimalConfig
		}
	}
	target := func(setting string) func(string) string {
		return func(written string) string {
			return "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\n" + setting + written + "\n"
		}
	}
	takesZero := []durationKey{
		{"the configuration's collectors[].cache.ttl", inConfig(func(written string) string {
			return strings.Replace(testutil.MinimalConfig, "    request:\n", "    cache:\n      ttl: "+written+"\n    request:\n", 1)
		})},
		{"the configuration's otlp.timeout", inConfig(otlp("timeout", true))},
		{"a collector file's collectors[].request.retry.backoff", inCollectorFile},
		{"the target file's targets[].request.retry.backoff", inTargets(target("    request:\n      retry:\n        backoff: "))},
	}
	zeroIsTheDefault := []durationKey{
		{"the configuration's otlp.interval", inConfig(otlp("interval", true))},
		{"the target file's targets[].interval", inTargets(target("    interval: "))},
	}
	required := []durationKey{
		{"the target file's interval", inTargets(func(written string) string {
			return "interval: " + written + "\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\n"
		})},
	}
	switchedOff := []durationKey{
		{"otlp.timeout, switched off", inConfig(otlp("timeout", false))},
		{"otlp.interval, switched off", inConfig(otlp("interval", false))},
	}
	every := slices.Concat(takesZero, zeroIsTheDefault, required)

	// expect fails unless every key gets the verdicts: accepted by both,
	// refused by both, or, with a message, past the schema and refused by
	// the exporter with it.
	expect := func(keys []durationKey, durations []string, accepted bool, message string) {
		t.Helper()
		for _, key := range keys {
			for _, written := range durations {
				problems, err := key.check(t, written)
				switch {
				case message != "":
					if len(problems) != 0 || err == nil || !strings.Contains(err.Error(), message) {
						t.Errorf("%s: %s: want it past the schema and refused by the exporter with %q; the schema says %v, the exporter %v", key.name, written, message, problems, err)
					}
				case (len(problems) == 0) != accepted || (err == nil) != accepted:
					t.Errorf("%s: %s: want accepted %v by both; the schema says %v, the exporter %v", key.name, written, accepted, problems, err)
				}
			}
		}
	}
	aSecondOrMore := []string{"1s", "5s", "+5s", "'+1m'", "1h30m", "1.5h", "5.s", "1m0.5s", "+1h0m0s"}
	zero := []string{"0s", "'0'", "+0s", "-0s", "'+0'", "'-0'", "0h0m", "-0h0m0s", "-0.0s", "-.0s", "-0.m"}
	underASecond := []string{".5s", "+.5s", "500ms", "0.999s", "1ns", "1us", "1µs", "1μs", "0h0m0.1s"}
	negative := []string{"-5s", "'-5s'", "-1ns", "-1h30m", "-0h1m", "-.5s", "-1.5h", "-0m0.001s"}
	notADuration := []string{"5", "'5'", "'-5'", "''", "s", ".s", "+s", "-s", "'-'", "1d", "5 s", "' 5s'", "'5s '", "--5s", "+-5s", "1s1", "5S", "soon", "'0.0'", "'00'", "30", "-5", "1.5", "0.5", "true", "[5s]", "{s: 5}"}
	// Written negative, and so small that Go rounds them to zero.
	negativeUnderANanosecond := []string{"-0.4ns", "-.5ns", "-0.0000000001s", "-0s0s0s0.1ns"}
	// Longer than a duration can be, 2^63 - 1 nanoseconds.
	tooLong := []string{"2562048h", "9223372036854775808ns", strings.Repeat("9", 30) + "h"}

	expect(every, aSecondOrMore, true, "")
	expect(every, negative, false, "")
	// However small, what is written negative is negative: the exporter
	// read these as the zero they round to, and took them where the schema
	// did not. It now refuses each in the words it has for -1ns there.
	expect(every, negativeUnderANanosecond, false, "")
	for _, key := range every {
		_, asNegative := key.check(t, "-1ns")
		for _, written := range negativeUnderANanosecond {
			if _, err := key.check(t, written); err == nil || asNegative == nil || lastWords(err) != lastWords(asNegative) {
				t.Errorf("%s: %s is refused with %v, and -1ns with %v", key.name, written, err, asNegative)
			}
		}
	}
	expect(every, notADuration, false, "")
	expect(slices.Concat(takesZero, zeroIsTheDefault), zero, true, "")
	expect(takesZero, underASecond, true, "")
	// How long a duration must be is the exporter's alone to say.
	expect(zeroIsTheDefault, underASecond, false, "is under the least, 1s")
	expect(required, underASecond, false, "is under the least, 1s")
	expect(required, zero, false, "interval is required")
	// Switched off, a block is kept unchecked: whatever is a duration.
	expect(switchedOff, slices.Concat(aSecondOrMore, zero, underASecond, negative, negativeUnderANanosecond), true, "")
	expect(switchedOff, notADuration, false, "")
	// YAML reads an unquoted 0 as a number, and the exporter reads it as the
	// duration it spells, with a sign too: the schemas take the number 0
	// beside the text, where they refused it as no text.
	unquotedZero := []string{"0", "+0", "-0"}
	expect(slices.Concat(takesZero, zeroIsTheDefault, switchedOff), unquotedZero, true, "")
	expect(required, unquotedZero, false, "interval is required")

	// The two disagreements on how a duration is written, each named here
	// and documented (docs/CONFIGURATION.md, Editor support), and no other.
	// A duration too long to be held is well written, so no pattern can
	// refuse it, and is no duration to the exporter; the longest one is
	// taken by both.
	expect(slices.Concat(every, switchedOff), tooLong, false, "is not a duration")
	expect(switchedOff, []string{"2562047h47m16.854775807s"}, true, "")
	// And a zero YAML reads as a number that is not written 0: a schema is
	// handed the number, 0, which it takes, and the exporter reads what is
	// written, which has no unit and is not 0.
	expect(slices.Concat(every, switchedOff), zerosOfAnotherSpelling, false, "is not a duration")
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
