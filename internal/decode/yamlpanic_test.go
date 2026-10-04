package decode

import (
	"errors"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// A document the YAML library panics on is refused, with the library's own
// words and no panic: a merge into a mapping that has a key that is no text,
// where the merged mapping has a sequence for a key, makes the library hash
// what cannot be hashed. The panic left decodeYAML and took the probe with
// it. The failure is recognised by its text up to the runtime error's own
// words, which can hold numbers of the document, and by the function of the
// library that raised it, without a line.
func TestAYAMLDocumentTheLibraryPanicsOnIsRefused(t *testing.T) {
	for _, document := range []string{
		"a: 1\n2: 3\n<<: {[x]: 1}\n",
		"2: 3\n<<: {{k: v}: 1}\n",
	} {
		v, err := decodeYAML([]byte(document))
		if err == nil {
			t.Fatalf("%q decodes into %v, want it refused", document, v)
		}
		const said = "the YAML library failed on the document: runtime error"
		if !strings.HasPrefix(err.Error(), said+": hash of unhashable type") {
			t.Errorf("%q is refused with %q, want what the library failed with", document, err)
		}
		if same, want := model.SameFailureText(err), said+" (decode.go yaml.v3.(*decoder).mapping)"; same != want {
			t.Errorf("%q: the failure is recognised by %q, want %q", document, same, want)
		}
	}
}

// yamlPanickingAt decodes a mapping of 129 keys, which is handed to the
// library a part at a time, and panics as raise does when the first part is
// handed: a panic of the exporter's own code, in the middle of a decoding.
func yamlPanickingAt(raise func()) (any, error) {
	return yamlReading{large: yamlLargeMapping, hands: func(*yaml.Node) { raise() }}.decode([]byte(manyYAMLKeys(129)))
}

// A panic of the exporter's own code while a YAML document is decoded is
// the decode's failure too, and is not put on the library: it says that the
// exporter failed, where — the file, the line and the function, and no stack
// — with what, and that this is a defect to report, whether the panic is of
// a text, of an error or a runtime error, an index out of range in one
// function and a key that is no text set in a map of text keys in another
// (yamlMap.set), or is raised in a function of the standard library that
// the exporter called, which is not the place. It is recognised without the
// line, and a runtime error without its words, which hold the index: by the
// place, so that the two runtime errors are two failures to the log, and
// neither is the library's.
func TestAPanicOfTheExportersOwnWhileDecodingYAMLSaysSoAndWhere(t *testing.T) {
	const exporter, report = "the exporter failed on the YAML document (", "; this is a defect of the exporter and not of the document, please report it"
	const here = `yamlpanic_test\.go(:\d+)? decode\.TestAPanicOfTheExportersOwnWhileDecodingYAMLSaysSoAndWhere\.func\d+`
	none, index := []int{}, 5
	recognised := map[string]bool{}
	for name, tc := range map[string]struct {
		raise          func()
		place          string
		said, saidSame string
	}{
		"a text":                  {func() { panic("the walk lost its place") }, here, "the walk lost its place" + report, "the walk lost its place" + report},
		"an error":                {func() { panic(errors.New("no such part")) }, here, "no such part" + report, "no such part" + report},
		"an index out of range":   {func() { index = none[index] }, here, "runtime error: index out of range [5] with length 0" + report, "runtime error"},
		"in the standard library": {func() { _ = strings.Repeat("x", index-10) }, here, "strings: negative Repeat count" + report, "strings: negative Repeat count" + report},
		"a key that is no text":   {func() { (&yamlMap{text: map[string]any{}}).set(1, 2) }, `yamlkeys\.go(:\d+)? decode\.\(\*yamlMap\)\.set`, "interface conversion: interface {} is int, not string" + report, "runtime error"},
	} {
		v, err := yamlPanickingAt(tc.raise)
		if err == nil {
			t.Fatalf("%s: the document decodes into %v, want the panic as its failure", name, v)
		}
		text, same := err.Error(), model.SameFailureText(err)
		if !regexp.MustCompile(`^` + regexp.QuoteMeta(exporter) + strings.Replace(tc.place, `(:\d+)?`, `:\d+`, 1) + regexp.QuoteMeta("): "+tc.said) + `$`).MatchString(text) {
			t.Errorf("%s: the failure is %q, want the exporter's, at %s, with %q", name, text, tc.place, tc.said)
		}
		if !regexp.MustCompile(`^` + regexp.QuoteMeta(exporter) + strings.Replace(tc.place, `(:\d+)?`, ``, 1) + regexp.QuoteMeta("): "+tc.saidSame) + `$`).MatchString(same) {
			t.Errorf("%s: the failure is recognised by %q, want the place without its line and %q", name, same, tc.saidSame)
		}
		if strings.Contains(text, "goroutine ") || strings.Contains(text, "\n") || strings.Contains(text, "YAML library") {
			t.Errorf("%s: the failure is %q, want one line, no stack and nothing of the library", name, text)
		}
		if recognised[same] {
			t.Errorf("%s: the failure is recognised by %q, as another is", name, same)
		}
		recognised[same] = true
	}
}

// What a panic while decoding YAML is recognised by is its words, but for a
// runtime error, which is recognised by the place it was raised at: the
// library's panic of a text or an error by all of it, as it was; its runtime
// error, and the exporter's, by the file and the function without the line,
// so that one raised in another function is another failure, and one raised
// on another line of the same function, or with another index, the same.
func TestAPanicWhileDecodingYAMLIsRecognisedByItsPlace(t *testing.T) {
	var outOfRange runtime.Error
	func() {
		defer func() { outOfRange, _ = recover().(runtime.Error) }()
		none, index := []int{}, 7
		_ = none[index]
	}()
	if outOfRange == nil {
		t.Fatal("no runtime error was raised")
	}
	library := yamlPlace{file: "decode.go", line: 812, function: "yaml.v3.(*decoder).mapping", library: true}
	merge := yamlPlace{file: "decode.go", line: 966, function: "yaml.v3.(*decoder).merge", library: true}
	walk := yamlPlace{file: "yamlkeys.go", line: 547, function: "decode.(*yamlMap).set"}
	const ofLibrary, ofExporter, report = "the YAML library failed on the document: ", "the exporter failed on the YAML document (", "; this is a defect of the exporter and not of the document, please report it"
	const index = "runtime error: index out of range [7] with length 0"
	for name, tc := range map[string]struct {
		failed     any
		at         yamlPlace
		text, same string
	}{
		"the library, a text":           {"no more tokens", library, ofLibrary + "no more tokens", ofLibrary + "no more tokens"},
		"the library, an error":         {errors.New("did not find expected key"), library, ofLibrary + "did not find expected key", ofLibrary + "did not find expected key"},
		"the library, a runtime error":  {outOfRange, library, ofLibrary + index, ofLibrary + "runtime error (decode.go yaml.v3.(*decoder).mapping)"},
		"the library, in a merge":       {outOfRange, merge, ofLibrary + index, ofLibrary + "runtime error (decode.go yaml.v3.(*decoder).merge)"},
		"the exporter, a text":          {"no more parts", walk, ofExporter + "yamlkeys.go:547 decode.(*yamlMap).set): no more parts" + report, ofExporter + "yamlkeys.go decode.(*yamlMap).set): no more parts" + report},
		"the exporter, an error":        {errors.New("no such part"), walk, ofExporter + "yamlkeys.go:547 decode.(*yamlMap).set): no such part" + report, ofExporter + "yamlkeys.go decode.(*yamlMap).set): no such part" + report},
		"the exporter, a runtime error": {outOfRange, walk, ofExporter + "yamlkeys.go:547 decode.(*yamlMap).set): " + index + report, ofExporter + "yamlkeys.go decode.(*yamlMap).set): runtime error"},
	} {
		err := yamlPanicked(tc.failed, tc.at)
		if err.Error() != tc.text || model.SameFailureText(err) != tc.same {
			t.Errorf("%s: the failure is %q, recognised by %q, want %q, recognised by %q", name, err, model.SameFailureText(err), tc.text, tc.same)
		}
		moved := tc.at
		moved.line += 40
		if again := yamlPanicked(tc.failed, moved); model.SameFailureText(again) != tc.same {
			t.Errorf("%s: raised forty lines further it is recognised by %q, want %q as before", name, model.SameFailureText(again), tc.same)
		}
	}
}

// The package and the name of a function are told from the name the runtime
// gives it: a function, a method, a function within one, one with type
// arguments that hold a path of their own, the library's, whose path's dot
// is written %2e, and one of the standard library with and without a
// directory.
func TestAFunctionOfAPanicIsNamedWithItsPackage(t *testing.T) {
	const decode = "github.com/eenchev/prometheus-universal-exporter/internal/decode"
	for name, want := range map[string][2]string{
		"gopkg.in/yaml%2ev3.(*decoder).mapping":            {yamlLibrary, "yaml.v3.(*decoder).mapping"},
		"gopkg.in/yaml%2ev3.handleErr":                     {yamlLibrary, yamlRaisesAgain},
		decode + ".(*yamlMap).set":                         {decode, "decode.(*yamlMap).set"},
		decode + ".yamlReading.decode.func1":               {decode, "decode.yamlReading.decode.func1"},
		decode + ".keep[go.shape.struct { a/b.c }]":        {decode, "decode.keep[go.shape.struct { a/b.c }]"},
		decode + ".(*kept[go.shape.struct { a/b.c }]).add": {decode, "decode.(*kept[go.shape.struct { a/b.c }]).add"},
		"runtime.gopanic":                                  {"runtime", "runtime.gopanic"},
		"reflect.Value.SetMapIndex":                        {"reflect", "reflect.Value.SetMapIndex"},
		"internal/runtime/maps.fatal":                      {"internal/runtime/maps", "maps.fatal"},
		"encoding/base64.(*Encoding).DecodeString":         {"encoding/base64", "base64.(*Encoding).DecodeString"},
		"main": {"main", "main"},
	} {
		if in, function := yamlFunction(name); in != want[0] || function != want[1] {
			t.Errorf("%s is the function %q of the package %q, want %q of %q", name, function, in, want[1], want[0])
		}
	}
}
