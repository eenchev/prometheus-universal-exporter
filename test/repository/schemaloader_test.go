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
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
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
	problems := schemaProblems(t, schema, document)
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

// refusedByBoth fails unless the schema refuses the document and the
// exporter refuses it with the message.
func refusedByBoth(t *testing.T, schema map[string]any, name, document, message string) {
	t.Helper()
	problems, err := verdicts(t, schema, document)
	if len(problems) == 0 || err == nil || !strings.Contains(err.Error(), message) {
		t.Errorf("%s: want it refused by the schema and by the exporter with %q; the schema says %v, the exporter %v\n%s", name, message, problems, err, document)
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
// limits.max_response_bytes is as written: a size that is held to nothing
// but being one. limits.max_output_bytes, which the sizes below were first
// written at, has a least of its own (limitedCollector).
func sizedCollector(size string) string {
	return limitedCollector("max_response_bytes", size)
}

// limitedCollector is a configuration of one collector whose limit of the
// key is as written.
func limitedCollector(key, written string) string {
	return "collectors:\n  - name: sized\n    request: {type: http}\n    transform: {type: regex}\n    limits:\n      " + key + ": " + written + "\n    metrics:\n      - name: v\n        expression: 'v=(\\d+)'\n"
}

// A size is a whole number of bytes or a number with a unit, as a YAML number
// or as text; a fraction needs a unit, and a sign, an exponent, an unknown
// unit and space around the text are refused. A number YAML reads as one is
// the whole number it equals however it is written, with an exponent or a
// fraction of zero too, to the exporter as to the schema, which is handed
// the number and not its spelling: the exporter once refused 1e3 and 1.0,
// which the schema took. The range, and a fraction past the digits the
// number a schema is handed holds, are the exporter's alone to refuse.
func TestSchemaAndExporterAgreeOnSizes(t *testing.T) {
	schema := loadSchema(t)
	for written, accepted := range map[string]bool{
		"0": true, "1024": true, "'512'": true, "0x100": true, "1_000": true, "4611686018427387904": true,
		"10B": true, "10 B": true, "1.5B": true, "1k": true, "1K": true, "1kB": true, "1KB": true, "1.5 kb": true,
		"2Ki": true, "2KiB": true, "2kib": true, "0.5KiB": true, "0.0001KiB": true, "10MB": true, "64MiB": true, "64 MiB": true,
		"1.5MiB": true, "1G": true, "1.5GiB": true, "0.5GiB": true, "1T": true, "1TiB": true, "8388607TiB": true,
		"1e3": true, "1E3": true, "1e+3": true, "+1e3": true, "1.0": true, "1.": true, "1000.000": true, "2.5e2": true, "2.50e2": true,
		"0.5e1": true, ".5e1": true, "10e-1": true, "0.0": true, "0e0": true, "-0.0": true, "-0": true, "+1000": true,
		"1_000.0": true, "1_0e2": true, "9007199254740993.0": true, "9.223372036854775807e18": true, "9223372036854775807": true,

		"lots": false, "''": false, "MiB": false, "-1": false, "'-1'": false, "-1MiB": false, "-1.5MiB": false, "+5MiB": false,
		"1.5": false, "0.5": false, "'1.5'": false, "'1.0'": false, "'0.0'": false, ".5MiB": false, "5.MiB": false, "1.5.5MiB": false, "'1,5MiB'": false,
		"'1e3'": false, "1e3KiB": false, "'0x100'": false, "'1_000'": false, "8EiB": false, "1PiB": false, "10 XB": false, "10MiBs": false,
		"10  MiB": false, "' 10MiB'": false, "'10MiB '": false, "[1]": false, "{bytes: 1}": false, "true": false,
		"1e-1": false, "1.5e0": false, "2.55e1": false, "15e-1": false, ".5": false, "-1e3": false, "-1.0": false, "-.5e1": false,
		".inf": false, "+.inf": false, "-.inf": false, ".nan": false, "1e400": false, "0x1p3": false,
		`"1e3"`: false, "'1E3'": false, "'1e+3'": false, `"1.0"`: false, "'1.'": false, "'2.5e2'": false, "'+1000'": false, "'0e0'": false, "!!str 1e3": false,
	} {
		agree(t, schema, "max_response_bytes: "+written, sizedCollector(written), accepted)
	}
	for written, message := range map[string]string{
		"8388608TiB":              `size "8388608TiB" is too large; a size is under 8EiB, which is 2^63 bytes`,
		"99999999999TiB":          `size "99999999999TiB" is too large`,
		"'9223372036854775808'":   `size "9223372036854775808" is too large`,
		"9223372036854775808":     `size 9223372036854775808 is too large; a size is under 8EiB, which is 2^63 bytes`,
		"99999999999999999999":    `size "99999999999999999999" is too large`,
		"9223372036854775808.0":   `size 9223372036854775808.0 is too large; a size is under 8EiB, which is 2^63 bytes`,
		"9.223372036854775808e18": `size 9.223372036854775808e18 is too large; a size is under 8EiB, which is 2^63 bytes`,
		"1e19":                    `size 1e19 is too large; a size is under 8EiB, which is 2^63 bytes`,
		"1e300":                   `size 1e300 is too large; a size is under 8EiB, which is 2^63 bytes`,
		// A float64 holds some 17 digits: past them a fraction is none to
		// what a schema is handed, and the exporter reads what is written.
		"1.00000000000000000001": `size "1.00000000000000000001" is not a whole number of bytes`,
		"9007199254740992.5":     `size "9007199254740992.5" is not a whole number of bytes`,
		"1e-400":                 `size "1e-400" is not a number of bytes or a number with a unit`,
	} {
		loadersAlone(t, schema, "max_response_bytes: "+written, sizedCollector(written), message)
	}
	// A whole number under 0 is refused by both, and by the exporter as
	// negative however it is written.
	for _, written := range []string{"-1", "-1e3", "-1.0", "-1e19"} {
		refusedByBoth(t, schema, "max_response_bytes: "+written, sizedCollector(written), "size "+written+" is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB")
	}
}

// A key of a whole number takes a number YAML writes with a point or an
// exponent as the whole number it is, to the exporter as to the schema, which
// is handed the number and not its spelling: in the configuration, at a
// collector's limit and its retries, which the schema holds to at most 10,
// and in the static target file. The
// exporter reads the number by its digits, where it converted the float64
// nearest to it, so that 9.223372036854775807e18 was refused as negative on
// amd64 and 1e19 as no whole number. A whole number past what the key's int
// holds, under -2^63 or from 2^63, is refused by the exporter in one message,
// and the schemas, which have no maximum for a whole number, take it from
// 2^63: the range is the exporter's alone to refuse, as a size's is, and so
// is a fraction past the digits the float64 a schema is handed holds.
func TestSchemaAndExporterAgreeOnWholeNumbersWrittenWithAPointOrAnExponent(t *testing.T) {
	schema, targetSchema := loadSchema(t), loadSchemaFile(t, staticTargetsSchemaFile)
	retried := func(written string) string {
		return strings.Replace(limitedCollector("max_metrics", "0"), "    request: {type: http}\n", "    request: {type: http, retry: {attempts: "+written+"}}\n", 1)
	}
	targets := func(written string) string {
		return "interval: 1m\nconcurrency: " + written + "\ntargets:\n  - {name: t, collector: sized, target: 'http://x'}\n"
	}
	targetVerdicts := func(written string) ([]string, error) {
		problems := schemaProblems(t, targetSchema, targets(written))
		file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targets(written)))
		if err == nil {
			err = config.ValidateStaticTargets(file)
		}
		return problems, err
	}
	pastTheRange := func(key, written string) string {
		return key + " is " + written + ", which is past the whole numbers it holds, -9223372036854775808 to 9223372036854775807; write a whole number in that range"
	}
	for written, accepted := range map[string]bool{
		"1e3": true, "1.0": true, "1000.000": true, "+2e0": true, "9007199254740993.0": true, "9.223372036854775807e18": true, "0.0": true, "-0.0": true,
		"1.5": false, "1e-1": false, "-1.0": false, "-1e3": false, "-1e19": false, "-9.223372036854775808e18": false, ".inf": false, ".nan": false,
	} {
		agree(t, schema, "max_metrics: "+written, limitedCollector("max_metrics", written), accepted)
		if problems, err := targetVerdicts(written); (len(problems) == 0) != accepted || (err == nil) != accepted {
			t.Errorf("concurrency: %s: want accepted %v by both; the schema says %v, the exporter %v", written, accepted, problems, err)
		}
	}
	// A retry's attempts are at most 10, to both, however the number is
	// written.
	for _, written := range []string{"2.0", "2e0", "+2.0", "1e1"} {
		agree(t, schema, "attempts: "+written, retried(written), true)
	}
	for _, written := range []string{"11.0", "1.1e1", "9.223372036854775807e18"} {
		refusedByBoth(t, schema, "attempts: "+written, retried(written), `collector "sized" request.retry.attempts must be from 0 to 10`)
	}
	for _, written := range []string{"1e19", "9.223372036854775808e18", "9223372036854775808.0", "1e300"} {
		loadersAlone(t, schema, "max_metrics: "+written, limitedCollector("max_metrics", written), pastTheRange("max_metrics", written))
		refusedByBoth(t, schema, "attempts: "+written, retried(written), pastTheRange("attempts", written))
		if problems, err := targetVerdicts(written); len(problems) != 0 || err == nil || !strings.Contains(err.Error(), pastTheRange("concurrency", written)) {
			t.Errorf("concurrency: %s: want it past the schema and refused by the exporter as past the range; the schema says %v, the exporter %v", written, problems, err)
		}
	}
	// A float64 holds some 17 digits: past them a fraction is none to
	// what a schema is handed, and the exporter reads what is written.
	for _, written := range []string{"1.00000000000000000001", "9007199254740992.5"} {
		loadersAlone(t, schema, "max_metrics: "+written, limitedCollector("max_metrics", written), "max_metrics is "+written+", which is not a whole number")
	}
}

// limits.max_output_bytes is a size that, when it is set, is at least the 38
// bytes a Python script that emits no metric answers in
// (transform.MinPythonOutputBytes), to the exporter and, where the size is written as a number, to the schema,
// which is handed the number however it is written: 37 and 1 are refused by
// both, with an exponent and with a fraction of zero too, and 38, and 0,
// which is the default, are taken by both. A size written as text, in
// quotes or with a unit, is text of the pattern of a size to a schema,
// which cannot tell how many bytes it comes to: one that is too small is the
// exporter's alone to refuse, and one that comes to no byte at all is the
// default to both.
func TestSchemaAndExporterAgreeOnTheLeastOutputLimit(t *testing.T) {
	schema := loadSchema(t)
	const least = transform.MinPythonOutputBytes
	limited := func(written string) string { return limitedCollector("max_output_bytes", written) }
	tooSmall := func(size int) string {
		return fmt.Sprintf(`collector "sized" limits.max_output_bytes is %d, and a Python script that emits no metric answers in %d bytes, so no transform's script could answer within it; set at least %d, or leave it out, or 0, for the default, 1MiB`, size, least, least)
	}
	for written, accepted := range map[string]bool{
		"0": true, "0.0": true, "0e0": true, "-0": true, "-0.0": true, "0x0": true,
		"38": true, "39": true, "3.8e1": true, "38.0": true, "38.": true, "+38": true, "0x26": true, "380e-1": true,
		"1e2": true, "1024": true, "1048576": true, "4611686018427387904": true,
		"'38'": true, "38B": true, "38 B": true, "38.9B": true, "0.04KiB": true, "1k": true, "64MiB": true,
		// No byte at all is the default, with a unit too.
		"'0'": true, "0B": true, "0.9B": true, "0.0001KiB": true,
	} {
		agree(t, schema, "max_output_bytes: "+written, limited(written), accepted)
	}
	for written, size := range map[string]int{
		"1": 1, "+1": 1, "1.0": 1, "1e0": 1, "10": 10, "1e1": 10, "26": 26, "26.0": 26, "2.6e1": 26, "0x1a": 26, "27": 27,
		"37": 37, "37.0": 37, "3.7e1": 37, "370e-1": 37, "0x25": 37, "+37": 37, "3_7": 37,
	} {
		refusedByBoth(t, schema, "max_output_bytes: "+written, limited(written), tooSmall(size))
	}
	for written, size := range map[string]int{
		"'1'": 1, `"26"`: 26, "'37'": 37, "1B": 1, "20B": 20, "26B": 26, "26 b": 26, "37B": 37, "37.9B": 37, "1.5B": 1, "0.01KiB": 10, "0.02KiB": 20, "0.000026MB": 26,
	} {
		loadersAlone(t, schema, "max_output_bytes: "+written, limited(written), tooSmall(size))
	}
	// What is no size is no output limit either, to both, as it was.
	for _, written := range []string{"-1", "-38", "37.5", "38.5", "3.75e1", "lots", "'3.8e1'", "'38.0'", "38 bytes", "[38]", "true"} {
		agree(t, schema, "max_output_bytes: "+written, limited(written), false)
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
