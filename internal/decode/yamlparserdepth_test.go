package decode

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A YAML document nested deeper than a response may be is refused by one of
// two: the library's parser, where it is nested past 10,000 levels one way,
// by indentation alone or in brackets alone, and the exporter, where the
// parser lets it through, nested both ways or through an alias
// (yamlTooDeep). The parser's refusal read `yaml: exceeded max depth of
// 10000`, which does not say what the 10000 bounds, and the exporter's said
// how deep a response may nest: one mistake in two wordings, and two
// failures to the log. The parser's is now given the exporter's words
// (yamlParserDepth), after the line the parser names when it names one.

// yamlTooDeepText is what a document nested deeper than a response may be
// is refused with, and recognised by.
const yamlTooDeepText = "yaml: the document is nested more than 10000 deep, counting what its aliases stand for; a response may nest 10000 deep at most"

// yamlLibraryRefusal is the error the YAML library's parser refuses body
// with, as the library words it, or nil for a body it parses.
func yamlLibraryRefusal(body []byte) error {
	var root yaml.Node
	return yaml.NewDecoder(bytes.NewReader(body)).Decode(&root)
}

// yamlParserDepthText is the library's refusal of a document for its depth,
// in the forms it writes it: after the line, or after none.
var yamlParserDepthText = regexp.MustCompile(`^yaml: (line [0-9]+: )?exceeded max depth of 10000$`)

// yamlDepthRefusalNow is an error of the decoder as it was, as it reads
// now: the parser's refusal of a document for its depth in the exporter's
// words, after the line the parser named, recognised without the line; and
// any other error itself. It is the oracle's part of this change for the
// tests that compare the decoder with what it was (yamlcut_test.go,
// yamlproblems_test.go).
func yamlDepthRefusalNow(was error) error {
	if was == nil {
		return nil
	}
	found := yamlParserDepthText.FindStringSubmatch(was.Error())
	if found == nil {
		return was
	}
	return model.SameFailureAs(errors.New("yaml: "+found[1]+strings.TrimPrefix(yamlTooDeepText, "yaml: ")), yamlTooDeepText)
}

// A document nested 10,001 deep is refused with one message whichever of
// the two refuses it — in block sequences, block mappings, flow sequences
// and flow mappings, which the parser refuses, and nested both ways, under
// keys, and through an alias, which the exporter does — and is recognised
// by one text, so the failure log sees one failure. Where the parser names
// a line, the message names it, in the library's place for a line: the line
// the level too many opens on, or, of levels of indentation, that of the
// last key or value written before them when that is not the document's
// first line. A document that is too deep on its first line has none, as
// the library writes none for that line. And a document nested 10,000 deep
// decodes, each way.
func TestAYAMLDocumentTooDeepIsRefusedTheSameWhicheverRefusesIt(t *testing.T) {
	bracketed := func(open, shut string, levels int) string {
		return strings.Repeat(open, levels) + "7" + strings.Repeat(shut, levels) + "\n"
	}
	// A mapping under each key, a line each: a body as long as the square of
	// its levels, fifty megabytes.
	byLines := func(levels int) string {
		var body strings.Builder
		for i := range levels {
			body.WriteString(strings.Repeat(" ", i))
			body.WriteString("a:\n")
		}
		return body.String() + strings.Repeat(" ", levels) + "7\n"
	}
	// Sequences by indentation on two lines, the second going on inside the
	// first's innermost.
	twoLines := func(first, second int) string {
		return strings.Repeat("- ", first) + "\n" + strings.Repeat(" ", 2*first) + strings.Repeat("- ", second) + "7\n"
	}
	documents := []struct {
		name, body string
		// parser says the library's parser refuses the document, and line
		// is the line it names, 0 for none.
		parser bool
		line   int
		// decodes is the document nested 10,000 deep, which decodes.
		decodes string
	}{
		{"block sequences", strings.Repeat("- ", 10001) + "7\n", true, 0, strings.Repeat("- ", 10000) + "7\n"},
		{"block sequences on two lines", twoLines(5000, 5001), true, 2, twoLines(5000, 5000)},
		{"block sequences on the line after their key, which is the line the parser names", "a: 1\nb:\n  " + strings.Repeat("- ", 10000) + "7\n", true, 2, "a: 1\nb:\n  " + strings.Repeat("- ", 9999) + "7\n"},
		{"block mappings, each the key of the one around it", strings.Repeat("? ", 10001) + "a\n", true, 0, ""},
		{"block mappings, a line each", byLines(10001), true, 10001, byLines(10000)},
		{"flow sequences", bracketed("[", "]", 10001), true, 0, bracketed("[", "]", 10000)},
		{"flow sequences, a line each", bracketed("[\n", "]", 10001), true, 10001, bracketed("[\n", "]", 10000)},
		{"flow sequences that do not end", strings.Repeat("[", 10001), true, 0, ""},
		{"flow mappings", bracketed("{a: ", "}", 10001), true, 0, bracketed("{a: ", "}", 10000)},
		{"flow mappings on the third line", "a: 1\nb: 2\nc: " + bracketed("{a: ", "}", 10001), true, 3, "a: 1\nb: 2\nc: " + bracketed("{a: ", "}", 9999)},
		{"flow mappings under a key, 10,000 of them", "a: 1\nb: 2\nc: " + bracketed("{a: ", "}", 10000), false, 0, ""},
		{"flow sequences inside block sequences", strings.Repeat("- ", 5001) + bracketed("[", "]", 5000), false, 0, strings.Repeat("- ", 5000) + bracketed("[", "]", 5000)},
		{"flow sequences inside block sequences, 10,000 of each", strings.Repeat("- ", 10000) + bracketed("[", "]", 10000), false, 0, ""},
		{"a sequence under a key, 10,000 block sequences", "a:\n" + strings.Repeat("- ", 10000) + "7\n", false, 0, "a:\n" + strings.Repeat("- ", 9999) + "7\n"},
		{"an alias of flow sequences", "p: &x " + bracketed("[", "]", 10000) + "q: *x\n", false, 0, "p: &x " + bracketed("[", "]", 9999) + "q: *x\n"},
	}
	if raceDetector {
		// One of each kind, and none of fifty megabytes: under the race
		// detector parsing a document this deep takes a tenth of a second,
		// and decoding one at the limit a third.
		documents = slices.DeleteFunc(documents, func(document struct {
			name, body string
			parser     bool
			line       int
			decodes    string
		}) bool {
			return !slices.Contains([]string{"block sequences", "block sequences on two lines", "flow mappings", "flow sequences inside block sequences", "an alias of flow sequences"}, document.name)
		})
		for i := range documents[1:] {
			documents[i+1].decodes = ""
		}
	}
	for _, document := range documents {
		// Which of the two refuses the document is what the table says: the
		// library's parser in its own words, or not at all.
		library := yamlLibraryRefusal([]byte(document.body))
		if wantLibrary := "yaml: exceeded max depth of 10000"; document.line > 0 {
			wantLibrary = fmt.Sprintf("yaml: line %d: exceeded max depth of 10000", document.line)
			if library == nil || library.Error() != wantLibrary {
				t.Fatalf("%s: the library's parser says %.200v, want %s", document.name, library, wantLibrary)
			}
		} else if (library != nil) != document.parser || library != nil && library.Error() != wantLibrary {
			t.Fatalf("%s: the library's parser says %.200v, want it to refuse the document (%v) with %s", document.name, library, document.parser, wantLibrary)
		}
		want := yamlTooDeepText
		if document.line > 0 {
			want = fmt.Sprintf("yaml: line %d: %s", document.line, strings.TrimPrefix(yamlTooDeepText, "yaml: "))
		}
		_, err := decodeYAML([]byte(document.body))
		if err == nil || err.Error() != want {
			t.Fatalf("%s: %.300v, want the document refused with %s", document.name, err, want)
		}
		if same := model.SameFailureText(err); same != yamlTooDeepText {
			t.Fatalf("%s: the failure is recognised by %.300s, want %s", document.name, same, yamlTooDeepText)
		}
		if document.decodes == "" {
			continue
		}
		value, err := decodeYAML([]byte(document.decodes))
		if err != nil || valueDepth(value) != MaxDepth {
			t.Fatalf("%s, nested %d deep: decoded nested %d deep (%.300v)", document.name, MaxDepth, valueDepth(value), err)
		}
	}
	// As a response's decoder answers: after what says which decoder it is.
	c := &model.Collector{Name: "deep", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "yaml"}}
	for body, want := range map[string]string{
		strings.Repeat("- ", 10001) + "7\n":                         "YAML decode: " + yamlTooDeepText,
		"a: 1\nb: " + bracketed("[", "]", 10001):                    "YAML decode: yaml: line 2: " + strings.TrimPrefix(yamlTooDeepText, "yaml: "),
		strings.Repeat("- ", 5001) + bracketed("[", "]", 5000):      "YAML decode: " + yamlTooDeepText,
		"a: 1\nb:\n  " + strings.Repeat("- ", 10000) + "7\nc: [1\n": "YAML decode: yaml: line 2: " + strings.TrimPrefix(yamlTooDeepText, "yaml: "),
	} {
		_, err := Decode(&fetch.HTTPResponse{StatusCode: 200, Body: []byte(body), Headers: http.Header{}}, c)
		if err == nil || err.Error() != want {
			t.Fatalf("%.40q...: %.300v, want %s", body, err, want)
		}
		if same := model.SameFailureText(err); same != "YAML decode: "+yamlTooDeepText {
			t.Fatalf("%.40q...: the failure is recognised by %.300s, want %s", body, same, "YAML decode: "+yamlTooDeepText)
		}
	}
	// A document nested as deep as it may be that the parser refuses for
	// something else is refused in the library's words for that.
	body := []byte("a: 1\nb:\n  " + strings.Repeat("- ", 9999) + "7\nc: [1\nd:")
	library := yamlLibraryRefusal(body)
	_, err := Decode(&fetch.HTTPResponse{StatusCode: 200, Body: body, Headers: http.Header{}}, c)
	if library == nil || err == nil || err.Error() != "YAML decode: "+library.Error() || !strings.Contains(err.Error(), "did not find expected ',' or ']'") {
		t.Fatalf("a document with a sequence that does not end: %.300v, want the library's %.300v", err, library)
	}
}

// Only the parser's refusal for the depth is given the exporter's words: the
// whole of the library's text for it, after a line or after none, with the
// depth that is MaxDepth. Any other text — another depth, other words
// before or after, a line that is no number — is left as the library wrote
// it, and read for its line as it was: a library that came to word the
// refusal otherwise, or to bound a document elsewhere, is not taken for
// this one.
func TestOnlyTheParsersOwnDepthRefusalIsGivenTheExportersWords(t *testing.T) {
	for problem, wantLine := range map[string]string{
		"exceeded max depth of 10000":             "",
		"line 1: exceeded max depth of 10000":     "1",
		"line 10001: exceeded max depth of 10000": "10001",
	} {
		line, deep := yamlParserDepth(problem)
		if !deep || line != wantLine {
			t.Errorf("%q: line %q, recognised %v, want the parser's refusal for the depth at line %q", problem, line, deep, wantLine)
		}
		err := yamlFailure(errors.New("yaml: " + problem))
		want := yamlTooDeepText
		if wantLine != "" {
			want = "yaml: line " + wantLine + ": " + strings.TrimPrefix(yamlTooDeepText, "yaml: ")
		}
		if err.Error() != want || model.SameFailureText(err) != yamlTooDeepText {
			t.Errorf("%q reads %v, recognised by %s, want %s, recognised by %s", problem, err, model.SameFailureText(err), want, yamlTooDeepText)
		}
	}
	for _, problem := range []string{
		"", "exceeded max depth", "exceeded max depth of 1000", "exceeded max depth of 100000", "exceeded max depth of 20000", "exceeded max depth of 10000 ", "exceeded max depth of 10000\n", "Exceeded max depth of 10000",
		" exceeded max depth of 10000", "line : exceeded max depth of 10000", "line x: exceeded max depth of 10000", "line -3: exceeded max depth of 10000", "line 3 : exceeded max depth of 10000", "line 3:exceeded max depth of 10000",
		"line 3: line 4: exceeded max depth of 10000", "line 3: while increasing flow level: exceeded max depth of 10000", "while increasing flow level: exceeded max depth of 10000", "lines 3: exceeded max depth of 10000",
		"line 3: exceeded max depth of 10000: more", "line 3: did not find expected key", "did not find expected key", "unknown anchor 'exceeded max depth of 10000' referenced", "line 3: found character that cannot start any token",
		"line 3.5: exceeded max depth of 10000", "line 0: exceeded max depth of 10000", "line 007: exceeded max depth of 10000", "line +7: exceeded max depth of 10000", "line ３: exceeded max depth of 10000", "document contains excessive aliasing", "the document is nested more than 10000 deep",
	} {
		if line, deep := yamlParserDepth(problem); deep || line != "" {
			t.Errorf("%q is taken for the parser's refusal for the depth, at line %q", problem, line)
		}
		// And the error is what it was made into before the refusal was
		// recognised: itself, or itself recognised without its line.
		library := errors.New("yaml: " + problem)
		got, was := yamlFailure(library), yamlFailureBeforeDepthWasRecognised(library)
		if got.Error() != was.Error() || model.SameFailureText(got) != model.SameFailureText(was) || (got == library) != (was == library) { //nolint:errorlint // whether it is the library's error itself, wrapped in nothing
			t.Errorf("%q reads %v, recognised by %s; it read %v, recognised by %s", problem, got, model.SameFailureText(got), was, model.SameFailureText(was))
		}
	}
	// An error that is not the library's at all is no refusal of its parser.
	for _, text := range []string{"exceeded max depth of 10000", "yamls: exceeded max depth of 10000", "YAML: exceeded max depth of 10000", "json: exceeded max depth of 10000"} {
		other := errors.New(text)
		if got := yamlFailure(other); got != other { //nolint:errorlint // the error itself, wrapped in nothing
			t.Errorf("%q, which is no error of the YAML library, reads %v", text, got)
		}
	}
}

// yamlFailureBeforeDepthWasRecognised is yamlFailure as it was before it
// recognised the parser's refusal of a document for its depth.
func yamlFailureBeforeDepthWasRecognised(err error) error {
	if problems, ok := err.(*yaml.TypeError); ok { //nolint:errorlint // as yamlFailure reads it
		return yamlProblems(problems)
	}
	const library = "yaml: "
	text := err.Error()
	if short, same, cut := yamlPartCut(text); cut {
		return model.SameFailureAs(errors.New(short), same)
	}
	problem, ok := strings.CutPrefix(text, library)
	if !ok {
		return err
	}
	if same := withoutYAMLLines(problem); same != problem {
		return model.SameFailureAs(err, library+strings.TrimPrefix(same, "line "+model.MovingMark+": "))
	}
	return err
}
