package config

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
	"gopkg.in/yaml.v3"
)

// Every key the configuration reads is in the schema, at every depth, and the
// schema has no key the configuration does not read.
func TestConfigSchemaCoversEveryKey(t *testing.T) {
	var walk func(t reflect.Type, schema map[string]any, path string)
	walk = func(typ reflect.Type, schema map[string]any, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			switch typ.Kind() {
			case reflect.Pointer:
				typ = typ.Elem()
			case reflect.Slice:
				typ, schema = typ.Elem(), schema["items"].(map[string]any)
			case reflect.Map:
				typ = typ.Elem()
				if sub, ok := schema["additionalProperties"].(map[string]any); ok {
					schema = sub
				}
			}
		}
		if typ.Kind() != reflect.Struct || typ == durationType {
			return
		}
		properties := schema["properties"].(map[string]any)
		seen := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			key, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
			if key == "" || key == "-" {
				continue
			}
			seen[key] = true
			sub, ok := properties[key].(map[string]any)
			if !ok {
				t.Errorf("%s.%s is read from the configuration but missing from the schema", path, key)
				continue
			}
			walk(typ.Field(i).Type, sub, path+"."+key)
		}
		for key := range properties {
			if !seen[key] {
				t.Errorf("%s.%s is in the schema but not read from the configuration", path, key)
			}
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s allows unknown keys, but the configuration rejects them", path)
		}
	}
	walk(reflect.TypeOf(model.Config{}), configSchema(), "")
}

// The request types offered are the ones this build carries.
func TestConfigSchemaListsTheBuiltRequestTypes(t *testing.T) {
	collector := configSchema()["properties"].(map[string]any)["collectors"].(map[string]any)["items"].(map[string]any)
	requestType := collector["properties"].(map[string]any)["request"].(map[string]any)["properties"].(map[string]any)["type"].(map[string]any)
	if !reflect.DeepEqual(requestType["enum"], fetch.BuiltRequestTypes()) {
		t.Fatalf("enum=%v, built=%v", requestType["enum"], fetch.BuiltRequestTypes())
	}
}

// The pattern of a duration key takes the texts the exporter reads as a
// duration that is not negative, and the pattern of a block that is switched
// off the texts it reads as a duration at all: some 9700 texts — a sign or
// none, and one or two numbers with units, well written and not — get from
// each pattern the verdict the exporter's own reading gives. A negative
// duration under a nanosecond, -.5ns, is among them and others are added:
// the pattern refuses it for its digits, and the exporter, which once read
// it as the zero Go rounds it to, reads it as negative. Where they part is a
// duration too long to be held, which is written as one. Beside the pattern
// there was before, which took any sign and a unit without a number, nothing
// is newly taken.
func TestTheDurationPatternsTakeWhatTheExporterReads(t *testing.T) {
	pattern, signed := regexp.MustCompile(durationPattern), regexp.MustCompile(signedDurationPattern)
	before := regexp.MustCompile(`^[-+]?(0|([0-9]*(\.[0-9]*)?(ns|us|µs|μs|ms|s|m|h))+)$`)
	signs := []string{"", "+", "-", "--", "+-", " "}
	numbers := []string{"", "0", "00", "5", "05", "10", "1.5", ".5", "5.", "0.0", ".0", "0.", ".", "1e3", "1_0"}
	units := []string{"", "ns", "us", "µs", "μs", "ms", "s", "m", "h", "d", "S", " s"}
	second := []string{"", "0s", "30m", "0.5s", ".0m", "5", "m", "-1s", "1d"}
	texts, taken, takenSigned := 0, 0, 0
	check := func(text string) {
		texts++
		var d model.Duration
		err := d.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: text})
		if got := signed.MatchString(text); got != (err == nil) {
			t.Errorf("%q: the signed pattern takes it: %v; the exporter reads it: %v", text, got, err)
		}
		want := err == nil && d >= 0
		got := pattern.MatchString(text)
		if got != want {
			t.Errorf("%q: the pattern takes it: %v; the exporter reads it as %v, %v", text, got, d, err)
		}
		if got && !before.MatchString(text) {
			t.Errorf("%q is taken, and was not before", text)
		}
		if got {
			taken++
		}
		if err == nil {
			takenSigned++
		}
	}
	for _, sign := range signs {
		for _, number := range numbers {
			for _, unit := range units {
				for _, rest := range second {
					check(sign + number + unit + rest)
				}
			}
		}
	}
	// Under a nanosecond: negative when written so, zero or more otherwise.
	for _, text := range []string{"-0.4ns", "-.5ns", "-0.0000000001s", "-0s0s0s0.1ns", "0.4ns", "+.5ns", "0.0000000001s", "0s0s0s0.1ns", "-0s0s0s0.0ns"} {
		check(text)
	}
	// Too long to be held, a duration is still written as one: both patterns
	// take what the exporter then refuses, which a pattern cannot tell.
	for _, text := range []string{"2562048h", "9223372036854775808ns", strings.Repeat("9", 30) + "h", "+2562048h"} {
		var d model.Duration
		if err := d.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: text}); err == nil || !strings.Contains(err.Error(), "is not a duration") || !pattern.MatchString(text) || !signed.MatchString(text) {
			t.Errorf("%q: the exporter reads it with %v; the pattern takes it: %v, the signed one: %v", text, err, pattern.MatchString(text), signed.MatchString(text))
		}
	}
	check("2562047h47m16.854775807s")
	if texts < 9000 || taken < 500 || takenSigned <= taken {
		t.Fatalf("%d texts tried, %d of them durations and %d not negative", texts, takenSigned, taken)
	}
	t.Logf("%d texts tried, %d of them durations and %d not negative", texts, takenSigned, taken)
}

// Every duration key of the three schemas has the one description of a
// duration (durationSchema): text of the duration pattern, or of the signed
// one in the otlp block, which is kept unchecked while it is switched off,
// or the whole number 0 and no other number. YAML reads an unquoted 0 as a
// number, which the exporter reads as the duration it spells, and the
// schemas took text alone. The keys are found by their type, so a duration
// key added to a file is held to this without anyone adding it here.
func TestEveryDurationKeyTakesTextOrTheNumberZero(t *testing.T) {
	signed := map[string]bool{".otlp.timeout": true, ".otlp.interval": true}
	keys := 0
	var walk func(typ reflect.Type, schema map[string]any, path string)
	walk = func(typ reflect.Type, schema map[string]any, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			if typ.Kind() == reflect.Slice {
				schema = schema["items"].(map[string]any)
			}
			typ = typ.Elem()
		}
		if typ == durationType {
			keys++
			want := durationPattern
			if signed[path] {
				want = signedDurationPattern
			}
			if !reflect.DeepEqual(schema["type"], []string{"string", "integer"}) || schema["minimum"] != 0 || schema["maximum"] != 0 || schema["pattern"] != want {
				t.Errorf("%s is described as %v from %v to %v with the pattern %v; want text or the number 0, and the pattern %s", path, schema["type"], schema["minimum"], schema["maximum"], schema["pattern"], want)
			}
			return
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		properties := schema["properties"].(map[string]any)
		for i := 0; i < typ.NumField(); i++ {
			key, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
			if key == "" || key == "-" {
				continue
			}
			walk(typ.Field(i).Type, properties[key].(map[string]any), path+"."+key)
		}
	}
	walk(reflect.TypeOf(model.Config{}), configSchema(), "")
	walk(reflect.TypeOf(collectorFile{}), collectorFileSchema(), "")
	walk(reflect.TypeOf(model.StaticTargetFile{}), staticTargetsSchema(), "")
	if keys < 18 {
		t.Fatalf("only %d duration keys were found", keys)
	}
	// A key that is no duration is not given a number by the way: a size
	// takes whole numbers as it did, and a pattern of another key makes it
	// text alone.
	collector := configSchema()["properties"].(map[string]any)["collectors"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if got := collector["name"].(map[string]any)["type"]; got != "string" {
		t.Errorf("collectors[].name takes %v", got)
	}
	if got := collector["limits"].(map[string]any)["properties"].(map[string]any)["max_output_bytes"].(map[string]any); !reflect.DeepEqual(got["type"], []string{"integer", "string"}) || got["maximum"] != nil {
		t.Errorf("limits.max_output_bytes is described as %v", got)
	}
}

// The keys of a value_map are refused by the schema's pattern exactly when
// the exporter refuses them: over every character there is, a key with the
// character before it, after it or inside it, and the character alone, is
// taken by the pattern when strings.TrimSpace leaves it as it is, which is
// what the exporter asks (transform.checkValueRules). The empty key is the
// length's to refuse. The pattern is written for Go and for JavaScript
// alike, so it holds no escape only one of them reads: none but \t, \n, \v,
// \f, \r, \s, \S and \x with two digits.
func TestTheValueMapKeyPatternRefusesWhatTrimSpaceTakesOff(t *testing.T) {
	rule := valueMapKeys()
	if rule["minLength"] != 1 {
		t.Fatalf("the least length of a key is %v", rule["minLength"])
	}
	pattern := regexp.MustCompile(rule["pattern"].(string))
	blanks, keys := 0, 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if unicode.IsSpace(r) {
			blanks++
		} else if r > 0x3000 && r%97 != 0 {
			// Past the last blank, one character in 97 is tried.
			continue
		}
		for _, key := range []string{string(r), string(r) + "up", "up" + string(r), "u" + string(r) + "p", string(r) + "u\np" + string(r)} {
			keys++
			if got, want := pattern.MatchString(key), strings.TrimSpace(key) == key; got != want {
				t.Errorf("%q (U+%04X): the pattern takes it: %v; it is left as it is by TrimSpace: %v", key, r, got, want)
			}
		}
	}
	if blanks != 25 || keys < 100000 {
		t.Fatalf("%d blanks and %d keys were tried", blanks, keys)
	}
	for _, key := range []string{"up", "*", "a b", "a\tb", "1", "a\n\nb", "\u200Bup", "up\uFEFF"} {
		if !pattern.MatchString(key) {
			t.Errorf("%q is refused", key)
		}
	}
	for _, escape := range regexp.MustCompile(`\\.`).FindAllString(rule["pattern"].(string), -1) {
		if !slices.Contains([]string{`\t`, `\n`, `\v`, `\f`, `\r`, `\s`, `\S`, `\x`}, escape) {
			t.Errorf("the pattern has the escape %s, which Go and JavaScript may not read alike", escape)
		}
	}
}

// An optional key's pattern takes nothing at all and what the key's own
// pattern takes, and no other text; its values are the key's own and "",
// which is added to a list of its own, not to the one the exporter checks
// values against.
func TestAnOptionalKeyTakesItsValuesAndTheEmptyString(t *testing.T) {
	for _, own := range []string{transform.MetricsPrefixRE.String(), targetNameRE.String(), `^[a-zA-Z_:][a-zA-Z0-9_:]*$`, `^a|b$`} {
		pattern, optional := regexp.MustCompile(own), regexp.MustCompile(optionalPattern(own))
		for _, text := range []string{"", " ", "a", "b", "grafana", "grafana_", "vendor_eu", "bad-name", "_x", "a:b", "0", "\n", "a\n", "é", "^$"} {
			if got, want := optional.MatchString(text), text == "" || pattern.MatchString(text); got != want {
				t.Errorf("%s: %q is taken: %v, want %v", optionalPattern(own), text, got, want)
			}
		}
	}
	before := slices.Clone(model.DecoderTypes)
	values := optionalEnum(model.DecoderTypes)
	if !slices.Equal(values, append(slices.Clone(before), "")) || !slices.Equal(model.DecoderTypes, before) {
		t.Errorf("the values are %v, and the decoder types %v", values, model.DecoderTypes)
	}
}
