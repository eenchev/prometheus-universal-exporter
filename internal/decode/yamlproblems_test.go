package decode

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// writtenTwice is the problem the YAML library has with a key written twice,
// as it lists it and as it is recognised.
func writtenTwice(key string, line, first int) string {
	return fmt.Sprintf("\n  line %d: mapping key %q already defined at line %d", line, key, first)
}

const (
	yamlProblemsSaid   = "YAML decode: yaml: unmarshal errors:"
	sameWrittenTwiceA  = "\n  line #: mapping key \"a\" already defined at line #"
	sameWrittenTwiceB  = "\n  line #: mapping key \"b\" already defined at line #"
	sameAndMoreProblem = "\n  ... and # more problems"
)

// itemsWritingTwice is a list of items that each write their key twice: the
// keys name an item's key each, in the order of the list.
func itemsWritingTwice(keys string) string {
	var doc strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&doc, "- {%c: 1, %c: 2}\n", key, key)
	}
	return doc.String()
}

// The YAML library lists a key written k times as a problem for each two of
// them, so its list grows with the square of the document. The error lists
// the first ten problems as the library words them and says how many more
// there were: a document with ten problems or fewer fails in the library's
// own words, as it did; one with eleven names ten and one more; and 1200
// lines of one key, 6 kB that the library makes 719,400 problems and 40 MB
// of text of, fail in under a kilobyte. The error is still the library's
// list of problems to errors.As.
func TestAYAMLDocumentsProblemsAreListedToTheTenthAndCounted(t *testing.T) {
	firstTen := writtenTwice("a", 2, 1) + writtenTwice("a", 3, 1) + writtenTwice("a", 4, 1) + writtenTwice("a", 5, 1) +
		writtenTwice("a", 3, 2) + writtenTwice("a", 4, 2) + writtenTwice("a", 5, 2) +
		writtenTwice("a", 4, 3) + writtenTwice("a", 5, 3) + writtenTwice("a", 5, 4)
	for name, tc := range map[string]struct{ body, want string }{
		"ten problems":    {strings.Repeat("a: 1\n", 5), yamlProblemsSaid + firstTen},
		"eleven problems": {strings.Repeat("a: 1\n", 5) + "b: 1\nb: 2\n", yamlProblemsSaid + firstTen + "\n  ... and 1 more problem"},
		"twelve problems": {strings.Repeat("a: 1\n", 5) + "b: 1\nb: 2\nc: 1\nc: 2\n", yamlProblemsSaid + firstTen + "\n  ... and 2 more problems"},
		"fifteen problems of one key": {strings.Repeat("a: 1\n", 6), yamlProblemsSaid +
			writtenTwice("a", 2, 1) + writtenTwice("a", 3, 1) + writtenTwice("a", 4, 1) + writtenTwice("a", 5, 1) + writtenTwice("a", 6, 1) +
			writtenTwice("a", 3, 2) + writtenTwice("a", 4, 2) + writtenTwice("a", 5, 2) + writtenTwice("a", 6, 2) +
			writtenTwice("a", 4, 3) + "\n  ... and 5 more problems"},
	} {
		err := yamlFailureOf(t, tc.body)
		if err.Error() != tc.want {
			t.Errorf("%s: the failure reads\n%v\nwant\n%s", name, err, tc.want)
		}
		_, was := decodeYAMLBeforeLinesWereLeftOut([]byte(tc.body))
		if listed := strings.Count(was.Error(), "\n"); (listed <= yamlProblemsShown) != (err.Error() == "YAML decode: "+was.Error()) {
			t.Errorf("%s: the failure reads\n%v\nand the library's, of %d problems,\n%v", name, err, listed, was)
		}
		var problems *yaml.TypeError
		if !errors.As(err, &problems) {
			t.Errorf("%s: %v is no longer a list of problems", name, err)
		}
	}
	err := yamlFailureOf(t, strings.Repeat("a: 1\n", 1200))
	if text := err.Error(); len(text) > 1000 || !strings.HasPrefix(text, yamlProblemsSaid+writtenTwice("a", 2, 1)+writtenTwice("a", 3, 1)) || !strings.HasSuffix(text, writtenTwice("a", 11, 1)+"\n  ... and 719390 more problems") {
		t.Errorf("a key written 1200 times fails with %d bytes of text: %.2000s", len(text), text)
	}
	if want := yamlProblemsSaid + sameWrittenTwiceA; model.SameFailureText(err) != want {
		t.Errorf("a key written 1200 times is recognised by %d bytes of text, want %q", len(model.SameFailureText(err)), want)
	}
}

// A failure is recognised by what its problems are, not by how many times
// each is listed: a list of items that each write a key twice has one
// problem an item, and is one failure when it grows by an item, to two, to
// three or to twelve, of which the error counts two; a key written twice,
// three times and six times is one failure too. A different problem among
// them is another failure, the same however many items have it. Ten
// different problems are told apart; past ten, the failure is recognised by
// the first ten and that there are more.
func TestAYAMLFailureIsRecognisedByItsProblemsNotByHowManyTimesEachIsListed(t *testing.T) {
	distinct := func(keys string) string {
		var same strings.Builder
		same.WriteString(yamlProblemsSaid)
		for _, key := range keys {
			fmt.Fprintf(&same, "\n  line #: mapping key %q already defined at line #", string(key))
		}
		return same.String()
	}
	for body, want := range map[string]string{
		itemsWritingTwice("aa"):                      yamlProblemsSaid + sameWrittenTwiceA,
		itemsWritingTwice("aaa"):                     yamlProblemsSaid + sameWrittenTwiceA,
		itemsWritingTwice("aaaaaaaaaaaa"):            yamlProblemsSaid + sameWrittenTwiceA,
		"a: 1\na: 2\n":                               yamlProblemsSaid + sameWrittenTwiceA,
		"a: 1\na: 2\na: 3\n":                         yamlProblemsSaid + sameWrittenTwiceA,
		strings.Repeat("a: 1\n", 6):                  yamlProblemsSaid + sameWrittenTwiceA,
		itemsWritingTwice("aab"):                     yamlProblemsSaid + sameWrittenTwiceA + sameWrittenTwiceB,
		itemsWritingTwice("abababababababab"):        yamlProblemsSaid + sameWrittenTwiceA + sameWrittenTwiceB,
		itemsWritingTwice("baa"):                     yamlProblemsSaid + sameWrittenTwiceB + sameWrittenTwiceA,
		strings.Repeat("a: 1\n", 5) + "b: 1\nb: 2\n": yamlProblemsSaid + sameWrittenTwiceA + sameWrittenTwiceB,
		itemsWritingTwice("abcdefghi"):               distinct("abcdefghi"),
		itemsWritingTwice("abcdefghij"):              distinct("abcdefghij"),
		itemsWritingTwice("abcdefghijk"):             distinct("abcdefghij") + sameAndMoreProblem,
		itemsWritingTwice("abcdefghijkl"):            distinct("abcdefghij") + sameAndMoreProblem,
		itemsWritingTwice("aabbccddeeffgghhiijjkk"):  distinct("abcdefghij") + sameAndMoreProblem,
		itemsWritingTwice("abcdefghijaaaaajjjjj"):    distinct("abcdefghij"),
	} {
		err := yamlFailureOf(t, body)
		if got := model.SameFailureText(err); got != want {
			t.Errorf("%q fails with\n%v\nrecognised by\n%s\nwant\n%s", body, err, got, want)
		}
		// The count of the problems left out is no part of what the failure is.
		if listed := strings.Count(body, "\n"); strings.HasPrefix(body, "- ") && listed > yamlProblemsShown && !strings.Contains(err.Error(), fmt.Sprintf("\n  ... and %d more problem", listed-yamlProblemsShown)) {
			t.Errorf("%q fails with\n%v\nwant the %d problems past the tenth counted", body, err, listed-yamlProblemsShown)
		}
	}
	twelve := yamlFailureOf(t, itemsWritingTwice("aaaaaaaaaaaa"))
	if want := yamlProblemsSaid + writtenTwice("a", 1, 1); !strings.HasPrefix(twelve.Error(), want) || !strings.HasSuffix(twelve.Error(), writtenTwice("a", 10, 10)+"\n  ... and 2 more problems") {
		t.Errorf("twelve items fail with\n%v", twelve)
	}
	// A list of problems that names no line, which the library has none of
	// for a document decoded into plain values, is recognised alike.
	for _, tc := range []struct {
		problems []string
		same     string
	}{
		{[]string{"no line here", "no line here"}, "yaml: unmarshal errors:\n  no line here"},
		{[]string{"no line here", "nor here", "no line here"}, "yaml: unmarshal errors:\n  no line here\n  nor here"},
		{[]string{"line 3: x", "line #: x"}, "yaml: unmarshal errors:\n  line #: x"},
	} {
		library := &yaml.TypeError{Errors: tc.problems}
		err := yamlFailure(library)
		if err.Error() != library.Error() || model.SameFailureText(err) != tc.same {
			t.Errorf("%q reads %q and is recognised by %q, want %q", tc.problems, err, model.SameFailureText(err), tc.same)
		}
	}
	eleven := &yaml.TypeError{Errors: strings.Fields("a b c d e f g h i j k")}
	err := yamlFailure(eleven)
	if want := "yaml: unmarshal errors:\n  a\n  b\n  c\n  d\n  e\n  f\n  g\n  h\n  i\n  j\n  ... and 1 more problem"; err.Error() != want {
		t.Errorf("eleven problems read %q, want %q", err, want)
	}
	if want := "yaml: unmarshal errors:\n  a\n  b\n  c\n  d\n  e\n  f\n  g\n  h\n  i\n  j\n  ... and # more problems"; model.SameFailureText(err) != want {
		t.Errorf("eleven problems are recognised by %q, want %q", model.SameFailureText(err), want)
	}
	if len(eleven.Errors) != 11 {
		t.Errorf("the library's list was changed: %q", eleven.Errors)
	}
}

// A document with many problems costs what the library makes of it and no
// more: a key written 400 times, 2 kB, is 79,800 problems, which the library
// allocates some 13 MB for. Decoding it, reading the error and asking what
// it is recognised by allocate no more than a tenth over that: the text of
// the whole list, 4 MB, is never made.
func TestAYAMLDocumentsManyProblemsCostNoMoreThanTheLibraryMadeOfThem(t *testing.T) {
	body := []byte(strings.Repeat("a: 1\n", 400))
	c := &model.Collector{Name: "doc", Decoder: model.DecoderConfig{Type: "yaml"}, Transform: model.TransformConfig{Type: "yq"}}
	var library, text int
	// The library alone is measured once: what decoding takes is bounded
	// by a tenth over it.
	_, base := alloctest.Once(1, func() {
		_, err := decodeYAMLBeforeLinesWereLeftOut(body)
		var problems *yaml.TypeError
		if !errors.As(err, &problems) {
			t.Fatalf("the library refused the document with %v", err)
		}
		library = len(problems.Errors)
	})
	now := alloctest.BytesAtMost(1, base+base/10, func() {
		_, err := Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{}}, c)
		if err == nil {
			t.Fatal("the document decoded")
		}
		text = len(err.Error()) + len(model.SameFailureText(err))
	})
	if library != 400*399/2 || text > 2000 {
		t.Fatalf("the library listed %d problems, and the error and what it is recognised by are %d bytes", library, text)
	}
	if now > base+base/10 {
		t.Errorf("decoding allocated %d bytes, and the library alone %d", now, base)
	}
}

// yamlFailureBeforeProblemsWereCollapsed and withoutYAMLLinesBefore are
// yamlFailure and withoutYAMLLines as they were when every problem listed
// was kept, each on a line of its own.
func yamlFailureBeforeProblemsWereCollapsed(err error) error {
	var problems *yaml.TypeError
	if errors.As(err, &problems) {
		same := make([]string, len(problems.Errors))
		moved := false
		for i, problem := range problems.Errors {
			same[i] = withoutYAMLLinesBefore(problem)
			moved = moved || same[i] != problem
		}
		if !moved {
			return err
		}
		return model.SameFailureAs(err, strings.Replace(err.Error(), problems.Error(), (&yaml.TypeError{Errors: same}).Error(), 1))
	}
	const library = "yaml: "
	text := err.Error()
	problem, ok := strings.CutPrefix(text, library)
	if !ok {
		return err
	}
	if same := withoutYAMLLinesBefore(problem); same != problem {
		return model.SameFailureAs(err, library+same)
	}
	return err
}

func withoutYAMLLinesBefore(problem string) string {
	const starts, ends = "line ", " already defined at line "
	rest, ok := strings.CutPrefix(problem, starts)
	if !ok {
		return problem
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 || !strings.HasPrefix(rest[digits:], ": ") {
		return problem
	}
	rest = rest[digits:]
	if at := strings.LastIndex(rest, ends); at >= 0 && strings.HasPrefix(rest, ": mapping key ") {
		if first := rest[at+len(ends):]; first != "" && strings.Trim(first, "0123456789") == "" {
			rest = rest[:at] + ends + model.MovingMark
		}
	}
	return starts + model.MovingMark + rest
}

// hostileYAMLDocuments are documents written to be mistaken: keys that hold
// the library's own words for a line, the mark, a line break; the errors
// that name no line; errors on the first line, which the library names no
// line for, and on later ones.
func hostileYAMLDocuments() []string {
	return []string{
		"a: 1\nb: 2\na: 3\n",
		"x: 0\ny: 0\na: 1\nb: 2\n\n\na: 3\n",
		"\"x already defined at line 7\": 1\n\"x already defined at line 7\": 2\n",
		"\"x already defined at line 9\": 1\n\"x already defined at line 9\": 2\n",
		"k already defined at line 12: 1\nk already defined at line 12: 2\n",
		"k already defined at line 13: 1\nk already defined at line 13: 2\n",
		"\"line 5: x\": 1\n\"line 5: x\": 2\n",
		"\"a\\n  line 9: boo\": 1\n\"a\\n  line 9: boo\": 2\n",
		"\"#\": 1\n\"#\": 2\n",
		"3: 1\n3: 2\n",
		"\n4: 1\n4: 2\n",
		"a: 1\na: 2\na: 3\n",
		"a: 1\na: 2\na: 3\na: 4\n",
		"a: 1\nb: 1\na: 2\nb: 2\n",
		"- {a: 1, a: 2}\n- {a: 1, a: 2}\n",
		"- {a: 1, a: 2}\n- {a: 1, a: 2}\n- {a: 1, a: 2}\n",
		"? [a]\n: 1\n? [a]\n: 2\n",
		"a: *line5\n",
		"a: *x\n",
		"a: !!int abc\n",
		"a: !!binary \"@@@\"\n",
		"a: !!float 'line 5: x'\n",
		"a:\n  <<: 1\n",
		"a: b: c\n",
		"x: 1\na: b: c\n",
		"x: 1\ny: 2\na: b: c\n",
		"\tx: 1\n",
		"a: 1\n\tx: 1\n",
		"[a, b\n",
		"x: 1\ny: 1\nz: [a, b\n",
		"a: \"line 7: x\nb: 2\n",
		"a: \x01\n",
		"a: \xff\n",
		"a: 1\n---\nb: [\n",
		"a: &a [*a]\n",
		strings.Repeat("k: v\n", 2000) + "a: b: c\n",
		"%YAML 9.9\n---\na: 1\n",
		"%TAG !a! tag:a\n%TAG !a! tag:b\n---\na: 1\n",
		"a:\n  b: 1\n c: 2\n",
		strings.Repeat("[", 20000),
	}
}

// A document is refused in the words it was, and recognised by what it was,
// unless it has more than ten problems, which are then counted, or a problem
// listed more than once, which is then recognised once: forty documents
// written to be mistaken and two thousand made of a few lines drawn at
// random, of a mapping, of a list and of both, some of them mistaken, against the decoder as it was before a YAML
// error was read at all and against the reading of it before the problems
// were collapsed, but for a scanner's or a parser's error, which is now
// recognised without the words for its line. And each problem is read for
// its lines as it was.
func TestAYAMLFailureWithFewDifferentProblemsReadsAndIsRecognisedAsItWas(t *testing.T) {
	documents := hostileYAMLDocuments()
	// The lines of a mapping, those of a list, and lines of every kind, of
	// which many cannot be parsed.
	kinds := [][]string{
		{"a: 1\n", "a: 2\n", "b: 1\n", "b: 2\n", "c: [1, 2]\n", "\n", "h:\n  i: 1\n  i: 2\n", "\"line 5: x\": 1\n"},
		{"- {a: 1, a: 2}\n", "- {b: 1, b: 2}\n", "- {a: 1, b: 2}\n", "- {a: 1, a: 2, a: 3}\n", "- c\n", "\n"},
		{"a: 1\n", "a: 2\n", "- {a: 1, a: 2}\n", " d: 1\n", "\t- e\n", "f: [1, 2\n", "g: *nope\n", "\n", "j: 'k\n", "---\n", "l: !!int m\n"},
	}
	random := rand.New(rand.NewPCG(14, 15)) //nolint:gosec // documents for a test
	for i := range 2100 {
		lines := kinds[i%len(kinds)]
		var doc strings.Builder
		for range 1 + random.IntN(12) {
			doc.WriteString(lines[random.IntN(len(lines))])
		}
		documents = append(documents, doc.String())
	}
	refused, same, collapsed, counted := 0, 0, 0, 0
	for _, document := range documents {
		_, was := decodeYAMLBeforeLinesWereLeftOut([]byte(document))
		_, err := decodeYAML([]byte(document))
		if (err == nil) != (was == nil) {
			t.Fatalf("%q is refused with %v; it was with %v", document, err, was)
		}
		if err == nil {
			continue
		}
		refused++
		listed, different := 0, map[string]bool{}
		var problems *yaml.TypeError
		if errors.As(was, &problems) {
			listed = len(problems.Errors)
			for _, problem := range problems.Errors {
				different[withoutYAMLLinesBefore(problem)] = true
			}
		}
		if errors.As(err, &problems) != errors.As(was, &problems) {
			t.Fatalf("%q: %v is no longer the kind of error it was", document, err)
		}
		if listed > yamlProblemsShown {
			counted++
			if text := err.Error(); !strings.HasPrefix(was.Error(), text[:strings.LastIndex(text, "\n")+1]) || !strings.Contains(text[strings.LastIndex(text, "\n"):], fmt.Sprintf("\n  ... and %d more problem", listed-yamlProblemsShown)) {
				t.Fatalf("%q is refused with\n%v\nwant the first ten of\n%v\nand the others counted", document, err, was)
			}
			continue
		}
		if err.Error() != was.Error() {
			t.Fatalf("%q is refused with\n%v\nit was with\n%v", document, err, was)
		}
		before := model.SameFailureText(yamlFailureBeforeProblemsWereCollapsed(was))
		// A scanner's or a parser's error has since lost the words for its
		// line where it had the mark for it, to be the failure it is on the
		// document's first line, which the library names no line for.
		if marked := "yaml: line " + model.MovingMark + ": "; strings.HasPrefix(before, marked) {
			before = "yaml: " + before[len(marked):]
		}
		if len(different) < listed {
			collapsed++
			if got := model.SameFailureText(err); got == before || strings.Count(got, "\n") != len(different) {
				t.Fatalf("%q, with %d problems of which %d differ, is recognised by\n%s", document, listed, len(different), got)
			}
			continue
		}
		same++
		if got := model.SameFailureText(err); got != before {
			t.Fatalf("%q is recognised by\n%s\nit was by\n%s", document, got, before)
		}
	}
	if refused < 1000 || same < 500 || collapsed < 300 || counted < 100 {
		t.Errorf("%d documents were refused: %d as they were, %d with a problem listed more than once and %d with more than ten: they do not cover all three", refused, same, collapsed, counted)
	}
	problems := []string{"", "line ", "line 1", "line 1:", "line 1: ", "line 12: x", "line 12:x", "line x: y", "on line 3: x", "line 3: line 4: x",
		`line 4: mapping key "k" already defined at line 2`, `line 4: mapping key "k" already defined at line `, `line 4: mapping key "k" already defined at line 2x`,
		`line 4: mapping key "k already defined at line 9" already defined at line 2`, `line 4: field k already defined at line 2`, `mapping key "k" already defined at line 2`,
		`line 4: mapping key "#" already defined at line #`, "line #: x"}
	for _, piece := range []string{"line ", "7", ": ", "mapping key ", `"k"`, " already defined at line ", "12", "#", "x"} {
		for _, problem := range problems[:18] {
			problems = append(problems, problem+piece, piece+problem)
		}
	}
	for _, problem := range problems {
		if got, was := withoutYAMLLines(problem), withoutYAMLLinesBefore(problem); got != was {
			t.Errorf("%q is read as %q; it was as %q", problem, got, was)
		}
		if got := string(appendWithoutYAMLLines([]byte("kept "), problem)); got != "kept "+withoutYAMLLinesBefore(problem) {
			t.Errorf("%q is added to a text as %q", problem, got)
		}
	}
}
