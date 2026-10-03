package config

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
