//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// wholeNumberPlace is a key of a whole number in one of the files, with a
// document that writes it as VALUE, and the number it loaded as.
type wholeNumberPlace struct {
	name, key, document string
	// load reads the document, and get is the key's value in what it read.
	load func(t *testing.T, document string) (any, error)
	get  func(loaded any) int
}

func loadWholeNumberConfiguration(t *testing.T, document string) (any, error) {
	return Load(testutil.WriteFile(t, "config.yaml", document))
}

// loadWholeNumberCollectorFile loads the document as a collector file, its
// path left out of the error for the errors of two files to compare.
func loadWholeNumberCollectorFile(t *testing.T, document string) (any, error) {
	collectors := testutil.WriteIn(t, t.TempDir(), "collectors.yaml", document)
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", "collector_files: ["+collectors+"]\n"))
	if err != nil {
		return nil, errors.New(strings.ReplaceAll(err.Error(), "collector file "+collectors+": ", ""))
	}
	return cfg, nil
}

func loadWholeNumberTargetFile(t *testing.T, document string) (any, error) {
	return LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
}

// wholeNumberPlaces are keys of a whole number of each kind of file: the
// configuration, a collector file and the static target file, a pointer
// among them.
func wholeNumberPlaces() []wholeNumberPlace {
	collector := func(loaded any) *model.Collector { return &loaded.(*model.Config).Collectors[0] }
	return []wholeNumberPlace{
		{"limits.max_metrics", "max_metrics", checkedCollector("", "    limits: {max_metrics: VALUE}\n", ""), loadWholeNumberConfiguration, func(l any) int { return collector(l).Limits.MaxMetrics }},
		{"max_concurrent_probes", "max_concurrent_probes", checkedCollector("", "    max_concurrent_probes: VALUE\n", ""), loadWholeNumberConfiguration, func(l any) int { return collector(l).MaxConcurrentProbes }},
		{"request.retry.attempts", "attempts", checkedCollector("      retry: {attempts: VALUE}\n", "", ""), loadWholeNumberConfiguration, func(l any) int { return collector(l).Request.Retry.Attempts }},
		{"otlp.max_pending_points", "max_pending_points", "otlp:\n  enabled: false\n  max_pending_points: VALUE\n" + checkedCollector("", "", ""), loadWholeNumberConfiguration, func(l any) int { return l.(*model.Config).OTLP.MaxPendingPoints }},
		{"limits.max_cache_entries in a collector file", "max_cache_entries", checkedCollector("", "    limits: {max_cache_entries: VALUE}\n", ""), loadWholeNumberCollectorFile, func(l any) int { return collector(l).Limits.MaxCacheEntries }},
		{"max_concurrent_probes in a collector file", "max_concurrent_probes", checkedCollector("", "    max_concurrent_probes: VALUE\n", ""), loadWholeNumberCollectorFile, func(l any) int { return collector(l).MaxConcurrentProbes }},
		{"concurrency of the target file", "concurrency", "interval: 1m\nconcurrency: VALUE\ntargets:\n  - {name: t, collector: a, target: http://x}\n", loadWholeNumberTargetFile, func(l any) int { return l.(*model.StaticTargetFile).Concurrency }},
		{"a target's request.retry.attempts", "attempts", "interval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {retry: {attempts: VALUE}}\n", loadWholeNumberTargetFile, func(l any) int {
			if attempts := l.(*model.StaticTargetFile).Targets[0].Request.Retry.Attempts; attempts != nil {
				return *attempts
			}
			return -1
		}},
	}
}

// A whole number written with a point or an exponent at a key of a whole
// number is the number written there, read by its digits: it loads exactly
// as the number written with digits alone does, value, verdict and message
// alike, so that the key's own checks apply to it in their own words. It was
// the float64 nearest to it, converted: 9007199254740993.0 loaded as the
// number below it, and 9.223372036854775807e18 was refused as negative on
// amd64, where the conversion of 2^63 wraps around. One no int holds is
// refused once, naming the key, the number as written and the range, where
// 1e19 was "expected a whole number" and -1e19 and 9.223372036854775808e18
// a negative number, and so is one past a float64's range, 1e400, which the
// YAML decoder takes for text and which was "expected a whole number, not a
// string"; a fraction, however small, is refused as it was, and a number in
// quotes is text, as it was.
func TestAWholeNumberWrittenWithAPointOrAnExponentIsTheNumberItIsAtEveryKeyOfOne(t *testing.T) {
	for _, place := range wholeNumberPlaces() {
		written := func(value string) string { return strings.Replace(place.document, "VALUE", value, 1) }
		before, _, _ := strings.Cut(place.document, "VALUE")
		line := 1 + strings.Count(before, "\n")
		key := place.key
		for float, digits := range map[string]string{
			"9.223372036854775807e18": "9223372036854775807", "9007199254740993.0": "9007199254740993", "9.007199254740993e15": "9007199254740993",
			"1e3": "1000", "1.0": "1", "2.0": "2", "+3e0": "3", "0.0": "0", "-1.0": "-1", "-9.223372036854775808e18": "-9223372036854775808",
		} {
			loaded, err := place.load(t, written(float))
			want, wantErr := place.load(t, written(digits))
			switch {
			case fmt.Sprint(err) != fmt.Sprint(wantErr):
				t.Errorf("%s: %s: %v; written %s: %v", place.name, float, err, digits, wantErr)
			case err == nil && place.get(loaded) != place.get(want):
				t.Errorf("%s: %s is %d, and %s %d", place.name, float, place.get(loaded), digits, place.get(want))
			}
		}
		for _, value := range []string{"9.223372036854775808e18", "1e19", "-1e19", "9223372036854775808.0", "1e300", "1e400", "-1e400", "1_0e400", "1.5e400"} {
			_, err := place.load(t, written(value))
			want := fmt.Sprintf("line %d: %s is %s, which is past the whole numbers it holds, -9223372036854775808 to 9223372036854775807; write a whole number in that range", line, key, value)
			if err == nil || !strings.Contains(err.Error(), want) || strings.Count(err.Error(), "line ") != 1 {
				t.Errorf("%s: %s: %v; want %q alone", place.name, value, err, want)
			}
		}
		for _, value := range []string{"1.5", "1.00000000000000000001", "9007199254740992.5", "-0.5"} {
			_, err := place.load(t, written(value))
			want := fmt.Sprintf("line %d: %s is %s, which is not a whole number", line, key, value)
			if err == nil || err.Error() != want {
				t.Errorf("%s: %s: %v; want %q", place.name, value, err, want)
			}
		}
		// Quoted, or tagged as text, a number is text, which is no whole
		// number, as it was.
		for _, value := range []string{`"1e400"`, `'1e3'`, "!!str 1e400"} {
			_, err := place.load(t, written(value))
			want := fmt.Sprintf("line %d: expected a whole number, not a string", line)
			if err == nil || err.Error() != want {
				t.Errorf("%s: %s: %v; want %q", place.name, value, err, want)
			}
		}
	}
}

// wholeNumberDocument is a document of two keys of a whole number, one an
// int and one a pointer to one, as the files have.
type wholeNumberDocument struct {
	A int  `yaml:"a"`
	B *int `yaml:"b"`
}

// loadAsItWas is what the load made of a document of wholeNumberDocument
// before it read a whole number written with a point or an exponent by its
// digits: what the decoder made of it, and the check the load had of a
// number with a fraction, copied from it, which went by the float64.
func loadAsItWas(document []byte) (wholeNumberDocument, error) {
	var out wholeNumberDocument
	err := yaml.Unmarshal(document, &out)
	var doc yaml.Node
	if yaml.Unmarshal(document, &doc) != nil {
		return out, err
	}
	key, n := doc.Content[0].Content[0].Value, doc.Content[0].Content[1]
	if n.ShortTag() != "!!float" {
		return out, err
	}
	value, parseErr := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64)
	if parseErr != nil || value == math.Trunc(value) {
		return out, err
	}
	problem := fmt.Sprintf("line %d: %s is %s, which is not a whole number", n.Line, key, n.Value)
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		return out, &yaml.TypeError{Errors: append(slices.Clone(typeErr.Errors), problem)}
	}
	return out, &yaml.TypeError{Errors: []string{problem}}
}

// The load as it was, the YAML decoder and the check of a fraction it had,
// is the oracle of every number the change does not concern: over numbers
// written every way YAML writes one, at an int and at a pointer to one, a
// number of the tag !!int — digits, a sign, hex, octal, binary, underscores,
// past the range — loads with the value and the messages it had, and so does
// one of the tag !!float that is a whole number the float64 the decoder makes
// of it holds, and infinity and not-a-number. What changes is of a number of
// the tag !!float, and of one past the range of a float64, 1e300 written
// with digits enough, which the decoder tags !!str, alone: a whole number the float64 does not hold exactly is
// the number written, where it was the float64's, wrapped around past 2^63;
// one past the range of an int is refused in one message of the load's, where
// it wrapped around or was refused as no whole number, or as text past a
// float64; and a fraction the
// float64 rounds away is refused as other fractions are.
func TestOnlyANumberWithAPointOrAnExponentThatAFloatDoesNotHoldIsReadOtherwise(t *testing.T) {
	var written []string
	for _, sign := range []string{"", "+", "-"} {
		for _, number := range []string{"0", "7", "1000", "0x1F", "0o17", "017", "0b101", "1_000", "9007199254740993", "9223372036854775807", "9223372036854775808", "18446744073709551616"} {
			written = append(written, sign+number)
		}
		for _, whole := range []string{"0", "1", "1_0", "9007199254740993", "9223372036854775807", "9223372036854775808", "18446744073709551615"} {
			for _, fraction := range []string{"", ".", ".0", ".5", ".000000000000000000001"} {
				for _, exponent := range []string{"", "e0", "e3", "e-1", "e15", "e18", "e19", "e300"} {
					if fraction+exponent != "" {
						written = append(written, sign+whole+fraction+exponent)
					}
				}
			}
		}
	}
	written = append(written, ".inf", "-.inf", ".nan", "!!float 7", "!!float 9007199254740993", "!!int 7")
	var same, exact, pastTheRange, fractions []string
	for _, value := range written {
		for _, key := range []string{"a", "b"} {
			document := []byte(key + ": " + value + "\n")
			was, wasErr := loadAsItWas(document)
			var now wholeNumberDocument
			nowErr := withValueProblems(yaml.Unmarshal(document, &now), document, &now)
			var doc yaml.Node
			if err := yaml.Unmarshal(document, &doc); err != nil {
				t.Fatalf("%s: %v", document, err)
			}
			n := doc.Content[0].Content[1]
			whole, isWhole := model.ReadWholeNumber(n.Value)
			number, held := whole.Int(64)
			_, notANumber := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64)
			unchanged := fmt.Sprint(nowErr) == fmt.Sprint(wasErr) && reflect.DeepEqual(now, was)
			// A number past a float64's range the decoder takes for text.
			pastAFloat := n.ShortTag() == "!!str" && errors.Is(notANumber, strconv.ErrRange)
			switch {
			case (n.ShortTag() != "!!float" && !pastAFloat) || (notANumber != nil && !isWhole):
				same = append(same, value)
				if !unchanged {
					t.Errorf("%s: was %+v, %v; is %+v, %v", document, was, wasErr, now, nowErr)
				}
			case isWhole && held:
				if got := valueAt(now, key); nowErr != nil || got != int(number) {
					t.Errorf("%s: is %d, %v; want the number written, %d", document, got, nowErr, number)
				}
				if unchanged {
					same = append(same, value)
				} else if key == "a" {
					exact = append(exact, fmt.Sprintf("%s (was %d, %v)", value, was.A, wasErr))
				}
			case isWhole:
				want := fmt.Sprintf("line 1: %s is %s, which is past the whole numbers it holds, -9223372036854775808 to 9223372036854775807; write a whole number in that range", key, n.Value)
				if nowErr == nil || nowErr.Error() != (&yaml.TypeError{Errors: []string{want}}).Error() {
					t.Errorf("%s: %v; want %q alone", document, nowErr, want)
				}
				pastTheRange = append(pastTheRange, value)
			default:
				want := fmt.Sprintf("line 1: %s is %s, which is not a whole number", key, n.Value)
				if nowErr == nil || nowErr.Error() != (&yaml.TypeError{Errors: []string{want}}).Error() {
					t.Errorf("%s: was %v, is %v; want %q alone", document, wasErr, nowErr, want)
				}
				if unchanged {
					same = append(same, value)
				} else if key == "a" {
					fractions = append(fractions, fmt.Sprintf("%s (was %d, %v)", value, was.A, wasErr))
				}
			}
		}
	}
	if len(same) < len(written)/2 || len(exact) == 0 || len(pastTheRange) < len(written)/10 || len(fractions) == 0 {
		t.Errorf("of %d spellings at two keys, %d load as they did, %d are now the number written, %d are past the range and %d fractions a float64 does not hold: the spellings no longer make each kind", len(written), len(same), len(exact), len(pastTheRange), len(fractions))
	}
	t.Logf("%d spellings at two keys: %d load as they did; %d past the range are refused as such; now the number written:\n%s\nnow refused as fractions:\n%s", len(written), len(same), len(pastTheRange), strings.Join(exact, "\n"), strings.Join(fractions, "\n"))
}

// valueAt is the key's value in the document, -1 where the pointer is nil.
func valueAt(d wholeNumberDocument, key string) int {
	if key == "a" {
		return d.A
	}
	if d.B == nil {
		return -1
	}
	return *d.B
}

// A whole number written with a point or an exponent is written into the key
// the YAML decoder decodes it into, wherever the document puts it: the load
// of the document is the load of the same document with every such number
// written with digits alone, which the decoder reads, value for value. Each
// number is one a float64 does not hold (9007199254740993.0), so that one
// written into another key, or into none, shows. A key that is an alias is
// the key its anchor holds, not the anchor's name: limits: {*m : 7.0}, the
// anchor &max_labels_per_metric holding max_metrics, set
// max_labels_per_metric. Such a key keeps out the key a merged mapping sets,
// as the decoder has it, and is a key of the merged mapping it is written
// in; a merge of a list of mappings, aliases or written there, takes each key
// from the first that sets it, and a merged mapping that merges one in turn
// its own keys first; a mapping that is an alias, used in two places, is
// written into at both; and the entries of a list that is an alias, and
// entries that are aliases, are the entries they are.
func TestAWholeNumberIsWrittenIntoTheKeyTheDecoderDecodesItInto(t *testing.T) {
	// FLOAT1 to FLOAT3 are numbers a float64 does not hold, STATUS a status.
	collector := func(name, limits string) string {
		return "  - name: " + name + "\n    request: {type: http, accept_status: STATUSES}\n    decoder: {type: json}\n    transform: {type: jq}\n    limits: " + limits + "\n    metrics: [{name: m, expression: .x}]\n"
	}
	for name, document := range map[string]string{
		"an alias key whose anchor is named for another key": "x-key: &max_labels_per_metric max_metrics\ncollectors:\n" + collector("a", "{*max_labels_per_metric : FLOAT1}"),
		"an alias key whose anchor names no key":             "x-key: &k max_metrics\ncollectors:\n" + collector("a", "{*k : FLOAT1, max_help_length: FLOAT2}"),
		"an alias key beside a merge key":                    "x-key: &k max_metrics\nx-d: &d {max_metrics: FLOAT1, max_help_length: FLOAT2}\ncollectors:\n" + collector("a", "{<<: *d, *k : FLOAT3}"),
		"an alias key of a merged mapping":                   "x-key: &k max_metrics\nx-d: &d {*k : FLOAT1, max_help_length: FLOAT2}\ncollectors:\n" + collector("a", "{<<: *d}"),
		"an alias key of a merged mapping overridden":        "x-key: &k max_metrics\nx-d: &d {*k : FLOAT1, max_help_length: FLOAT2}\ncollectors:\n" + collector("a", "{<<: *d, max_metrics: FLOAT3}"),
		"a merge of a list of aliases":                       "x-a: &a {max_metrics: FLOAT1}\nx-b: &b {max_metrics: FLOAT2, max_help_length: FLOAT3}\ncollectors:\n" + collector("a", "{<<: [*a, *b]}"),
		"a merge of a list of mappings":                      "collectors:\n" + collector("a", "{<<: [{max_metrics: FLOAT1}, {max_metrics: FLOAT2, max_help_length: FLOAT3}]}"),
		"a merge of a mapping that merges another":           "x-b: &b {max_metrics: FLOAT2, max_help_length: FLOAT3}\nx-a: &a {<<: *b, max_metrics: FLOAT1}\ncollectors:\n" + collector("a", "{<<: *a}"),
		"a mapping that is an alias, in two places":          "x-d: &d {max_metrics: FLOAT1, max_help_length: FLOAT2}\ncollectors:\n" + collector("a", "*d") + collector("b", "*d"),
		"a mapping in two places, merged into each":          "x-d: &d {max_metrics: FLOAT1, max_help_length: FLOAT2}\ncollectors:\n" + collector("a", "{<<: *d}") + collector("b", "{<<: *d, max_metrics: FLOAT3}"),
		"a list that is an alias":                            "x-s: &s [STATUS, 2xx]\ncollectors:\n" + strings.Replace(collector("a", "{max_metrics: FLOAT1}"), "STATUSES", "*s", 1),
		"entries that are aliases":                           "x-s: &s STATUS\ncollectors:\n" + strings.Replace(collector("a", "{max_metrics: FLOAT1}"), "STATUSES", "[*s, 2xx, *s]", 1),
		"a collector that is an alias":                       "x-c: &c {name: a, request: {type: http, accept_status: STATUSES}, decoder: {type: json}, transform: {type: jq}, limits: {max_metrics: FLOAT1}, metrics: [{name: m, expression: .x}]}\ncollectors: [*c]\n",
	} {
		written := func(numbers ...string) string {
			document := strings.ReplaceAll(document, "STATUSES", "[STATUS]")
			return strings.NewReplacer("FLOAT1", numbers[0], "FLOAT2", numbers[1], "FLOAT3", numbers[2], "STATUS", numbers[3]).Replace(document)
		}
		loaded, err := Load(testutil.WriteFile(t, "config.yaml", written("9007199254740993.0", "9.007199254740995e15", "900719925474099.7e1", "5.03e2")))
		want, wantErr := Load(testutil.WriteFile(t, "config.yaml", written("9007199254740993", "9007199254740995", "9007199254740997", "503")))
		if err != nil || wantErr != nil {
			t.Errorf("%s: %v; written with digits: %v", name, err, wantErr)
			continue
		}
		for i := range want.Collectors {
			got, wanted := &loaded.Collectors[i], &want.Collectors[i]
			if got.Limits != wanted.Limits || !slices.Equal(got.Request.AcceptStatus, wanted.Request.AcceptStatus) {
				t.Errorf("%s: collector %s has %+v and %q; written with digits, %+v and %q", name, got.Name, got.Limits, got.Request.AcceptStatus, wanted.Limits, wanted.Request.AcceptStatus)
			}
		}
	}
}

// A key that is an alias is named in a message by the key its anchor holds,
// the key the value is decoded into, at the line the alias is written on:
// a fraction written through *k, the anchor &k holding max_metrics, is
// max_metrics's, as is a key with nothing after its colon. So it is in a
// mapping of keys the file names: in transform.labels, *z, the anchor
// holding zone, with nothing after its colon is the key zone's, and *n, the
// anchor holding ~, is a key YAML reads as none, which the decoder drops
// with its value. Each loaded without a word.
func TestAnAliasKeyIsNamedByTheKeyItStandsFor(t *testing.T) {
	document := "x-key: &k max_metrics\n" + checkedCollector("", "    limits:\n      *k : 1.5\n", "")
	_, err := Load(testutil.WriteFile(t, "config.yaml", document))
	if want := "line 9: max_metrics is 1.5, which is not a whole number"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error %v, want %q", err, want)
	}
	document = "x-key: &k max_metrics\n" + checkedCollector("", "    limits:\n      *k :\n", "")
	_, err = Load(testutil.WriteFile(t, "config.yaml", document))
	if want := "line 9: max_metrics has nothing after its colon, which YAML reads as no value at all"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error %v, want %q", err, want)
	}
	for labels, want := range map[string]string{
		"{*z : }":   `line 8: labels key "zone" has nothing after its colon, which YAML reads as no value at all`,
		"{*n : up}": "line 8: labels has the key ~, which YAML reads as no key at all, so the entry would be dropped",
	} {
		document := "x-z: &z zone\nx-n: &n ~\n" + strings.Replace(checkedCollector("", "", ""), "    transform: {type: jq}\n", "    transform: {type: jq, labels: "+labels+"}\n", 1)
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("labels: %s: error %v, want %q", labels, err, want)
		}
	}
}
