package decode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// yamlFailureOf is the error the yaml decoder refuses a body with.
func yamlFailureOf(t *testing.T, body string) error {
	t.Helper()
	c := &model.Collector{Name: "doc", Decoder: model.DecoderConfig{Type: "yaml"}, Transform: model.TransformConfig{Type: "yq"}}
	_, err := Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body), Headers: http.Header{}}, c)
	if err == nil {
		t.Fatalf("yaml decoded %q", body)
	}
	return err
}

// excessiveAliasing is a document the YAML library refuses for what its
// aliases would expand to.
func excessiveAliasing() string {
	nine := func(of string) string { return "[" + strings.TrimSuffix(strings.Repeat(of+",", 9), ",") + "]" }
	var doc strings.Builder
	fmt.Fprintf(&doc, "a: &a %s\n", nine("x"))
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&doc, "%c: &%c %s\n", 'a'+i, 'a'+i, nine(fmt.Sprintf("*%c", 'a'+i-1)))
	}
	return doc.String()
}

// A YAML document that cannot be parsed names the line it failed on, which
// in a document that changes from scrape to scrape is another on each: the
// log recognises it as one failure all the same. Of each form the library
// writes a line in — a parser's error, a scanner's, one in a second
// document, a key written twice, which names the line of the first too, and
// several such keys in one error — two documents that fail the same way on
// different lines read differently, each naming its lines, and are
// recognised by one text: a parser's or a scanner's error by its problem
// without the words for the line, a list of problems with the mark where
// the lines were; a document that fails another way is recognised by
// another.
func TestYAMLFailuresThatDifferInTheirLineAreRecognisedAsOne(t *testing.T) {
	for name, tc := range map[string]struct {
		a, b, other  string
		wantA, wantB string
		same         string
	}{
		"a parser's error": {"a: 1\nb: 2\n c: 3\n", "a: 1\nb: 2\ne: 5\nf: 6\ng: 7\nh: 8\n c: 3\n", "a: 1\nb: 2\n- c\n",
			"YAML decode: yaml: line 3: mapping values are not allowed in this context", "YAML decode: yaml: line 7: mapping values are not allowed in this context",
			"YAML decode: yaml: mapping values are not allowed in this context"},
		"a scanner's error": {"a:\n\t- b\n", "x: 1\ny: 2\nz: 3\na:\n\t- b\n", "a:\n  - 'b\n",
			"YAML decode: yaml: line 2: found character that cannot start any token", "YAML decode: yaml: line 5: found character that cannot start any token",
			"YAML decode: yaml: found character that cannot start any token"},
		"a flow sequence left open": {"a: 1\nc: [1, 2\nd: 3\n", "a: 1\nb: 2\nx: 0\nc: [1, 2\nd: 3\n", "a: 1\nc: {k: 1\nd: 3\n",
			"YAML decode: yaml: line 1: did not find expected ',' or ']'", "YAML decode: yaml: line 3: did not find expected ',' or ']'",
			"YAML decode: yaml: did not find expected ',' or ']'"},
		"in a second document": {"a: 1\n---\nb: [\n", "a: 1\nc: 2\n---\n\nb: [\n", "a: 1\n---\nb: 'x\n",
			"YAML decode: yaml: line 3: did not find expected node content", "YAML decode: yaml: line 5: did not find expected node content",
			"YAML decode: yaml: did not find expected node content"},
		"a key written twice": {"a: 1\na: 2\n", "x: 1\na: 1\ny: 2\nz: 3\na: 2\n", "b: 1\nb: 2\n",
			"YAML decode: yaml: unmarshal errors:\n  line 2: mapping key \"a\" already defined at line 1", "YAML decode: yaml: unmarshal errors:\n  line 5: mapping key \"a\" already defined at line 2",
			"YAML decode: yaml: unmarshal errors:\n  line #: mapping key \"a\" already defined at line #"},
		"two keys written twice": {"a: 1\nb: 2\na: 3\nb: 4\n", "x: 1\na: 1\nb: 2\ny: 2\na: 3\nz: 3\nb: 4\n", "a: 1\nb: 2\na: 3\n",
			"YAML decode: yaml: unmarshal errors:\n  line 3: mapping key \"a\" already defined at line 1\n  line 4: mapping key \"b\" already defined at line 2",
			"YAML decode: yaml: unmarshal errors:\n  line 5: mapping key \"a\" already defined at line 2\n  line 7: mapping key \"b\" already defined at line 3",
			"YAML decode: yaml: unmarshal errors:\n  line #: mapping key \"a\" already defined at line #\n  line #: mapping key \"b\" already defined at line #"},
	} {
		a, b, other := yamlFailureOf(t, tc.a), yamlFailureOf(t, tc.b), yamlFailureOf(t, tc.other)
		if a.Error() != tc.wantA || b.Error() != tc.wantB {
			t.Errorf("%s: the failures are\n%v\n%v\nwant\n%s\n%s", name, a, b, tc.wantA, tc.wantB)
		}
		if model.SameFailureText(a) != tc.same || model.SameFailureText(b) != tc.same {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", name, a, b, model.SameFailureText(a), model.SameFailureText(b), tc.same)
		}
		if model.SameFailureText(a) == model.SameFailureText(other) {
			t.Errorf("%s: the failures\n%v\n%v\nare recognised as one, by %s", name, a, other, model.SameFailureText(a))
		}
	}
}

// The errors the library writes without a line are recognised by their text
// as it is, and are the errors they were: an anchor nothing defines, which
// is another failure for another name, an anchor that holds itself, a
// document whose aliases expand to too much, a merge of something that is
// no mapping, a value that does not fit its tag, and a character a document
// may not hold.
func TestYAMLFailuresWithoutALineAreRecognisedByTheirText(t *testing.T) {
	for body, want := range map[string]string{
		"a: 1\nb: *nope\n":       "yaml: unknown anchor 'nope' referenced",
		"a: 1\nc: 2\nb: *gone\n": "yaml: unknown anchor 'gone' referenced",
		"a: &x {b: *x}\n":        "yaml: anchor 'x' value contains itself",
		excessiveAliasing():      "yaml: document contains excessive aliasing",
		"a: 1\nb:\n  <<: 5\n":    "yaml: map merge requires map or sequence of maps as the value",
		"a: !!int foo\n":         "yaml: cannot decode !!str `foo` as a !!int",
		"a: !!binary '***'\n":    "yaml: !!binary value contains invalid base64 data",
		"a: \x01\n":              "yaml: control characters are not allowed",
		"? [a, b]\n: 1\n":        `yaml: invalid map key: []interface {}{"a", "b"}`,
	} {
		_, err := decodeYAML([]byte(body))
		if err == nil || err.Error() != want || model.SameFailureText(err) != want {
			t.Errorf("%q failed with %v, recognised by %q; want both %q", body, err, model.SameFailureText(err), want)
		}
		if again := yamlFailure(err); again != err { //nolint:errorlint // the error itself: one without a line is handed on as it is
			t.Errorf("%q: %v is given the text %q to be recognised by", body, err, model.SameFailureText(again))
		}
	}
}

// Only the forms the library writes a line in are read for one: a problem
// that starts with a line and a colon, and, of a key written twice, the line
// it ends with. Anything else — no digits, no colon, the words in the
// middle of a text, a key that holds them — is left as it is, so a text of
// another form is recognised by the whole of it, and an error that is none
// of the library's is handed on untouched.
func TestOnlyTheYAMLLibrarysOwnLineFormsAreLeftOut(t *testing.T) {
	for problem, want := range map[string]string{
		"line 12: did not find expected key":                "line #: did not find expected key",
		"line 0: x":                                         "line #: x",
		"line 12 did not find expected key":                 "line 12 did not find expected key",
		"line : did not find expected key":                  "line : did not find expected key",
		"line x: did not find expected key":                 "line x: did not find expected key",
		"line 12:did not find expected key":                 "line 12:did not find expected key",
		"on line 12: did not find expected key":             "on line 12: did not find expected key",
		"did not find expected key":                         "did not find expected key",
		"":                                                  "",
		"line 3: value 17 of line 4: is odd":                "line #: value 17 of line 4: is odd",
		`line 4: mapping key "k" already defined at line 2`: `line #: mapping key "k" already defined at line #`,
		`line 4: mapping key "k already defined at line 9" already defined at line 2`: `line #: mapping key "k already defined at line 9" already defined at line #`,
		`line 4: mapping key "k" already defined at line two`:                         `line #: mapping key "k" already defined at line two`,
		`line 4: mapping key "k" already defined at line `:                            `line #: mapping key "k" already defined at line `,
		`line 4: field k already defined at line 2`:                                   `line #: field k already defined at line 2`,
		`mapping key "k" already defined at line 2`:                                   `mapping key "k" already defined at line 2`,
	} {
		if got := withoutYAMLLines(problem); got != want {
			t.Errorf("%q is recognised by %q, want %q", problem, got, want)
		}
	}
	for _, err := range []error{
		errors.New("the body holds more than one YAML document"),
		errors.New("line 3: not the library's"),
		errors.New("yaml: on line 3 something"),
		&yaml.TypeError{Errors: []string{"no line here", "nor line 4 here"}},
		&yaml.TypeError{},
	} {
		if got := yamlFailure(err); got != err || model.SameFailureText(got) != err.Error() { //nolint:errorlint // the error itself
			t.Errorf("%q is given the text %q to be recognised by", err, model.SameFailureText(got))
		}
	}
	mixed := yamlFailure(&yaml.TypeError{Errors: []string{"line 3: cannot unmarshal !!str `x` into int", "no line here"}})
	if want := "yaml: unmarshal errors:\n  line #: cannot unmarshal !!str `x` into int\n  no line here"; model.SameFailureText(mixed) != want {
		t.Errorf("%q is recognised by %q, want %q", mixed, model.SameFailureText(mixed), want)
	}
	if want := "yaml: unmarshal errors:\n  line 3: cannot unmarshal !!str `x` into int\n  no line here"; mixed.Error() != want {
		t.Errorf("the error reads %q, want %q", mixed, want)
	}
}

// decodeYAMLBeforeLinesWereLeftOut is decodeYAML as it was before a YAML
// error was given the text it is recognised by.
func decodeYAMLBeforeLinesWereLeftOut(body []byte) (any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if err := decoder.Decode(&root); errors.Is(err, io.EOF) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	for {
		var next yaml.Node
		err := decoder.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !emptyYAMLDocument(&next) {
			return nil, errors.New("the body holds more than one YAML document (separated by ---); multi-document YAML is not supported")
		}
	}
	timestampsAsText(&root, map[*yaml.Node]bool{})
	var v any
	if err := root.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// A YAML document decodes into what it decoded into, and one that is
// refused is refused in the words it was: every YAML fixture, every example
// configuration and the configurations under configs, whole and cut off
// after each of their first sixty lines and in the middle of it, which
// leaves over a hundred of them open in a sequence or a quoted text; and
// the documents of the tests above. Only what a refusal is recognised by differs, and only
// for one that names a line. Under the race detector a file is cut off
// after every sixth of those lines, a sixth of the documents, with a sixth
// of each outcome to find among them.
func TestYAMLDecodesAndIsRefusedAsBeforeItsLinesWereLeftOut(t *testing.T) {
	var documents []string
	every := alloctest.UnlessRaced(1, 6)
	for _, pattern := range []string{"../../testdata/yaml/*", "../../examples/*.yaml", "../../configs/*.yaml"} {
		files, err := filepath.Glob(pattern)
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v, %d files", pattern, err, len(files))
		}
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			documents = append(documents, string(raw))
			lines := strings.SplitAfter(string(raw), "\n")
			for cut := 1; cut < len(lines) && cut <= 60; cut += every {
				// Cut after the line, and in the middle of it.
				whole := strings.Join(lines[:cut], "")
				documents = append(documents, whole, whole[:len(whole)-len(lines[cut-1])/2])
			}
		}
	}
	documents = append(documents, "", "a: 1\n", "a: 1\n---\n", "a: 1\n---\nb: 2\n", "a: 1\n  b: 2\n", "a:\n\t- b\n", "a: 1\na: 2\n", "a: *nope\n", "a: &x {b: *x}\n",
		excessiveAliasing(), "a: 1\nb:\n  <<: 5\n", "a: !!int foo\n", "a: \x01\n", "? [a, b]\n: 1\n", "updated: 2024-06-01\n", "a: 1\n---\nb: [\n")
	decoded, refused, moved := 0, 0, 0
	for _, document := range documents {
		want, wantErr := decodeYAMLBeforeLinesWereLeftOut([]byte(document))
		got, err := decodeYAML([]byte(document))
		if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q decodes into %v, %v; it was %v, %v", document, got, err, want, wantErr)
		}
		if err == nil {
			decoded++
			continue
		}
		refused++
		var problems *yaml.TypeError
		if errors.As(err, &problems) != errors.As(wantErr, &problems) {
			t.Fatalf("%q: %v is no longer the kind of error it was", document, err)
		}
		if model.SameFailureText(err) != model.SameFailureText(wantErr) {
			moved++
			if !strings.Contains(err.Error(), "line ") {
				t.Fatalf("%q: %v names no line, and is recognised by %q where it was by its text", document, err, model.SameFailureText(err))
			}
		} else if strings.Contains(err.Error(), "yaml: line ") {
			t.Fatalf("%q: %v names a line, and is still recognised by it", document, err)
		}
	}
	if decoded < 1000/every || refused < 100/every || moved < 90/every {
		t.Errorf("%d documents decoded and %d were refused, %d of those recognised otherwise than before: they do not cover all three", decoded, refused, moved)
	}
}

// The library writes no line for an error on a document's first line: `yaml:
// problem`, where on any other line it is `yaml: line N: problem`. The two
// are one failure to the log, so a mistake that moves to the first line of a
// document, or from it, is a repeat as one that moves among the other lines
// is. Marked and not left out, the line made `yaml: line #: problem` of the
// one and left the other as it was, and the log took them for two failures.
func TestAYAMLFailureOnTheFirstLineIsTheOneOnAnyOtherLine(t *testing.T) {
	for name, tc := range map[string]struct {
		first, later, other  string
		wantFirst, wantLater string
	}{
		"a parser's error": {"a: b: c\n", "x: 1\ny: 2\na: b: c\n", "a: [1\n",
			"yaml: mapping values are not allowed in this context", "yaml: line 3: mapping values are not allowed in this context"},
		"a scanner's error": {"a: @b\n", "x: 1\ny: @b\n", "a: 'b",
			"yaml: found character that cannot start any token", "yaml: line 2: found character that cannot start any token"},
		"a quote left open": {"a: 'b", "x: 1\ny: 2\nz: 3\na: 'b", "a: b: c\n",
			"yaml: found unexpected end of stream", "yaml: line 4: found unexpected end of stream"},
	} {
		t.Run(name, func(t *testing.T) {
			_, first := decodeYAML([]byte(tc.first))
			_, later := decodeYAML([]byte(tc.later))
			_, other := decodeYAML([]byte(tc.other))
			if first == nil || later == nil || other == nil {
				t.Fatalf("the documents decode: %v, %v, %v", first, later, other)
			}
			if first.Error() != tc.wantFirst || later.Error() != tc.wantLater {
				t.Fatalf("the errors read\n%s\n%s\nwant\n%s\n%s", first, later, tc.wantFirst, tc.wantLater)
			}
			if a, b := model.SameFailureText(first), model.SameFailureText(later); a != b || a != tc.wantFirst {
				t.Fatalf("the errors are recognised by\n%s\n%s\nwant both by %s", a, b, tc.wantFirst)
			}
			if model.SameFailureText(other) == model.SameFailureText(first) {
				t.Fatalf("another failure, %v, is recognised by the same text", other)
			}
		})
	}
}
