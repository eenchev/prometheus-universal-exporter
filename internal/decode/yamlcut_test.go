package decode

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// numbersFrom is a flow sequence of so many numbers counted up from one,
// `[5, 6, 7]`, and the Go syntax the YAML library writes it in when it is a
// key that is no key.
func numbersFrom(from, count int) (sequence, syntax string) {
	numbers := make([]string, count)
	for i := range numbers {
		numbers[i] = strconv.Itoa(from + i)
	}
	listed := strings.Join(numbers, ", ")
	return "[" + listed + "]", "[]interface {}{" + listed + "}"
}

// A mapping key that is a sequence or a mapping is refused with the whole
// key in the library's error, written as Go writes a value: a key of
// 200,000 numbers was an error of 1.4 MB, logged as one line and answered to
// the scraper. The error holds the first 256 bytes of what the library wrote
// of the key and says how long that was, and is under 300 bytes: beside a
// key that is no text, where the library decodes the document; beside a
// mapping of 200 keys, where the walk hands the pair to the library; and for
// a key that is itself a mapping of 20,000 keys, which the walk decodes and
// refuses in the library's words. It is recognised by the same text with the
// mark in place of the length, so a key of 150,000 numbers that starts the
// same is the same failure to the log, and one that starts with another
// number is another.
func TestAYAMLKeyThatIsACollectionIsRefusedWithItsStartAndItsLength(t *testing.T) {
	const said = "yaml: invalid map key: "
	key, syntax := numbersFrom(1, 200000)
	want := said + syntax[:yamlPartBytes] + fmt.Sprintf("... (%d bytes)", len(syntax))
	same := said + syntax[:yamlPartBytes] + "... (# bytes)"
	for name, document := range map[string]string{
		"beside a key that is no text": "1: x\n? " + key + "\n: x\n",
		"beside a mapping of 200 keys": manyYAMLKeys(200) + "1: x\n? " + key + "\n: x\n",
	} {
		v, err := decodeYAML([]byte(document))
		if err == nil {
			t.Fatalf("%s: the document decodes into %.100v", name, v)
		}
		if text := err.Error(); text != want || len(text) > 300 {
			t.Errorf("%s: the error is %d bytes, %.400q, want %q", name, len(text), text, want)
		}
		if got := model.SameFailureText(err); got != same {
			t.Errorf("%s: the failure is recognised by %d bytes, %.400q, want %q", name, len(got), got, same)
		}
	}
	shorter, _ := numbersFrom(1, 150000)
	other, _ := numbersFrom(2, 200000)
	_, errShorter := decodeYAML([]byte("1: x\n? " + shorter + "\n: x\n"))
	_, errOther := decodeYAML([]byte("1: x\n? " + other + "\n: x\n"))
	if errShorter == nil || errOther == nil {
		t.Fatalf("the documents are refused with %.300v and %.300v", errShorter, errOther)
	}
	if got := model.SameFailureText(errShorter); got != same || !strings.HasSuffix(errShorter.Error(), "... (1088909 bytes)") {
		t.Errorf("a key of 150,000 numbers is refused with %.400q, recognised by %.400q, want its own length and %q", errShorter, got, same)
	}
	if got := model.SameFailureText(errOther); got == same || len(got) > 300 {
		t.Errorf("a key that starts with another number is recognised by %d bytes, %.400q, want another text", len(got), got)
	}
	// The walk's own refusal, of a key it decodes itself.
	_, err := decodeYAML([]byte("1: x\n? {" + manyYAMLPairs(20000, "", ", ") + "}\n: x\n"))
	if err == nil {
		t.Fatal("a key that is a mapping of 20,000 keys is decoded")
	}
	if text := err.Error(); len(text) > 300 || !regexp.MustCompile(`^yaml: invalid map key: map\[string\]interface \{\}\{"key0":0, "key1":1, "key10":10, .*\.\.\. \(\d{6} bytes\)$`).MatchString(text) {
		t.Errorf("a key that is a mapping of 20,000 keys is refused with %d bytes, %.400q", len(text), text)
	}
	if got := model.SameFailureText(err); len(got) > 300 || !strings.HasSuffix(got, "... (# bytes)") {
		t.Errorf("a key that is a mapping of 20,000 keys is recognised by %d bytes, %.400q", len(got), got)
	}
}

// A key written twice is in each of its problems, and an error lists ten of
// them: a key of 100 kB written three times, in the explicit form a key of
// more than 1,024 characters takes, was an error of 300 kB, and written six
// times one of 1 MB. Each problem holds the first 64 bytes of the key,
// quoted, and its length, as model.QuoteValue cuts a value, and still says
// where the key was first written; ten such problems and the count of the
// rest are under 1,500 bytes, and the error is still a list of problems to
// errors.As. The failure is recognised by one problem with the mark for the
// length and the lines, so keys of 100 kB and of 50 kB that start the same,
// in one document or in two, are one failure, and a key that starts
// otherwise another. The library's own list, which a document with a short
// list is refused with, reads the same as the refusal made without the
// library, for keys of 65 bytes, of 200 and of 30 kB; a key of 64 bytes
// reads as the library wrote it.
func TestAYAMLKeyWrittenTwiceIsNamedByItsStartAndItsLength(t *testing.T) {
	long := strings.Repeat("k", 100000)
	problem := func(line, first int, size string) string {
		return fmt.Sprintf("\n  line %d: mapping key %q... (%s bytes) already defined at line %d", line, long[:yamlKeyBytes], size, first)
	}
	const said = "yaml: unmarshal errors:"
	same := said + "\n  line #: mapping key " + strconv.Quote(long[:yamlKeyBytes]) + "... (# bytes) already defined at line #"
	v, err := decodeYAML([]byte(strings.Repeat("? "+long+"\n: 1\n", 3)))
	if err == nil {
		t.Fatalf("a key written three times decodes into %.100v", v)
	}
	if want := said + problem(3, 1, "100000") + problem(5, 1, "100000") + problem(5, 3, "100000"); err.Error() != want {
		t.Errorf("a key of 100 kB written three times is refused with %d bytes, %.1000q, want %q", len(err.Error()), err, want)
	}
	if got := model.SameFailureText(err); got != same {
		t.Errorf("a key of 100 kB written three times is recognised by %d bytes, %.1000q, want %q", len(got), got, same)
	}
	var problems *yaml.TypeError
	if !errors.As(err, &problems) || len(problems.Errors) != 3 {
		t.Errorf("%.300v is no longer a list of three problems", err)
	}
	if !strings.Contains(err.Error(), model.QuoteValue(long)) {
		t.Errorf("the key is not cut as model.QuoteValue cuts a value, %s: %.300v", model.QuoteValue(long), err)
	}
	_, err = decodeYAML([]byte(strings.Repeat("? "+long+"\n: 1\n", 6)))
	if err == nil {
		t.Fatal("a key written six times is decoded")
	}
	if text := err.Error(); len(text) > 1500 || strings.Count(text, long[:yamlKeyBytes]+`"... (100000 bytes) already defined at line `) != yamlProblemsShown || !strings.HasSuffix(text, "\n  ... and 5 more problems") || !errors.As(err, &problems) {
		t.Errorf("a key of 100 kB written six times is refused with %d bytes, %.2000q, want ten problems named and five counted", len(text), text)
	}
	if got := model.SameFailureText(err); got != same {
		t.Errorf("a key of 100 kB written six times is recognised by %.1000q, want %q", got, same)
	}
	half, other := long[:50000], "j"+long[:99999]
	for name, tc := range map[string]struct {
		document string
		same     bool
	}{
		"a key of 50 kB that starts the same":     {strings.Repeat("? "+half+"\n: 1\n", 2), true},
		"both keys in one document":               {strings.Repeat("? "+long+"\n: 1\n", 2) + strings.Repeat("? "+half+"\n: 1\n", 2), true},
		"a key that starts otherwise":             {strings.Repeat("? "+other+"\n: 1\n", 2), false},
		"the first 64 bytes of the key, as a key": {strings.Repeat(long[:yamlKeyBytes]+": 1\n", 2), false},
		"a key that starts the same, of 65 bytes": {strings.Repeat(long[:yamlKeyBytes+1]+": 1\n", 2), true},
		"a key of 200 bytes the library refuses":  {"under: {" + long[:200] + ": 1, " + long[:200] + ": 2}\n", true},
	} {
		_, err := decodeYAML([]byte(tc.document))
		if err == nil {
			t.Fatalf("%s: the document is decoded", name)
		}
		if got := model.SameFailureText(err); (got == same) != tc.same || len(err.Error()) > 500 {
			t.Errorf("%s: refused with %d bytes, %.600q, recognised by %.300q; the same failure as the key of 100 kB: want %v", name, len(err.Error()), err, got, tc.same)
		}
	}
	// The library's list and the refusal made without it read the same.
	for _, size := range []int{yamlKeyBytes - 1, yamlKeyBytes, yamlKeyBytes + 1, 200, 30000} {
		for _, key := range []string{long[:size], strings.Repeat("é", size/2) + long[:size%2], `"` + strings.Repeat(`\"`, size/2) + `"`, `"` + strings.Repeat(`\x01 `, size/2) + `"`} {
			root := parsedYAML(t, strings.Repeat("? "+key+"\n: 1\n", 3))
			library, refused := yamlByTheLibraryAlone(root), yamlWith(root, 0, nil)
			if yamlWayOf(root, 0) != yamlRefused || library.err == nil || !refused.same(library) {
				t.Fatalf("a key of %d bytes written three times is refused without the library with\n%.1000s\nand by the library with\n%.1000s", size, refused.text(), library.text())
			}
			value := root.Content[0].Content[0].Value
			if cut := strings.Contains(library.err.Error(), " bytes) already defined at line "); cut != (len(value) > yamlKeyBytes) || !strings.Contains(library.err.Error(), "mapping key "+model.QuoteValue(value)+" already") {
				t.Errorf("a key of %d bytes is named as %.300q, want it as %s", len(value), library.err, model.QuoteValue(value))
			}
			uncut := yamlFailureUncut(&yaml.TypeError{Errors: []string{fmt.Sprintf(yamlWrittenTwiceUncut, 3, value, 1)}})
			now := yamlFailure(&yaml.TypeError{Errors: []string{fmt.Sprintf(yamlWrittenTwiceUncut, 3, value, 1)}})
			if len(value) <= yamlKeyBytes && (now.Error() != uncut.Error() || model.SameFailureText(now) != model.SameFailureText(uncut)) {
				t.Errorf("a key of %d bytes reads %q, recognised by %q; it read %q, recognised by %q", len(value), now, model.SameFailureText(now), uncut, model.SameFailureText(uncut))
			}
		}
	}
}

// A scalar that does not fit its tag, and the name of an anchor that is not
// known or that holds itself, are in the library's error whole: a value of a
// megabyte tagged !!int was an error of a megabyte. The error holds the
// first 256 bytes of the part, what closes it, and its length, and then
// what the library says after it, the tag the value was to fit; the failure
// is recognised by the same with the mark for the length. A value that
// itself holds the library's words for what follows is cut where the value
// ends, and a part of 256 bytes or fewer reads as the library wrote it.
func TestAYAMLValueOrAnchorOfAnyLengthIsNamedByItsStartAndItsLength(t *testing.T) {
	long := strings.Repeat("a", 1000000)
	head := long[:yamlPartBytes]
	holds := "x` as a !!bool " + long[:500]
	for name, tc := range map[string]struct{ document, want string }{
		"a value tagged !!int":               {"a: !!int " + long + "\n", "yaml: cannot decode !!str `" + head + "`... (1000000 bytes) as a !!int"},
		"a block of text tagged !!float":     {"a: !!float |\n  " + long + "\n", "yaml: cannot decode !!str `" + head + "`... (1000001 bytes) as a !!float"},
		"a value that holds the words":       {"a: !!int \"" + holds + "\"\n", "yaml: cannot decode !!str `" + holds[:yamlPartBytes] + "`... (515 bytes) as a !!int"},
		"an anchor that is not known":        {"a: *" + long + "\n", "yaml: unknown anchor '" + head + "'... (1000000 bytes) referenced"},
		"an anchor that holds itself":        {"a: &" + long + " [*" + long + "]\n", "yaml: anchor '" + head + "'... (1000000 bytes) value contains itself"},
		"an anchor the walk finds in itself": {"a: &" + long + " {" + manyYAMLPairs(129, "", ", ") + "self: *" + long + "}\n", "yaml: anchor '" + head + "'... (1000000 bytes) value contains itself"},
	} {
		v, err := decodeYAML([]byte(tc.document))
		if err == nil {
			t.Fatalf("%s: the document decodes into %.100v", name, v)
		}
		if err.Error() != tc.want {
			t.Errorf("%s: refused with %d bytes, %.600q, want %q", name, len(err.Error()), err, tc.want)
		}
		if got, want := model.SameFailureText(err), regexp.MustCompile(`\(\d+ bytes\)`).ReplaceAllString(tc.want, "(# bytes)"); got != want {
			t.Errorf("%s: recognised by %d bytes, %.600q, want %q", name, len(got), got, want)
		}
	}
	for _, size := range []int{1, yamlPartBytes - 1, yamlPartBytes} {
		for _, document := range []string{"a: !!int " + long[:size] + "\n", "a: *" + long[:size] + "\n", "a: &" + long[:size] + " [*" + long[:size] + "]\n"} {
			_, err := decodeYAML([]byte(document))
			_, was := decodeYAMLUncut(yamlReading{large: yamlLargeMapping}, []byte(document))
			if err == nil || err.Error() != was.Error() || model.SameFailureText(err) != model.SameFailureText(was) || strings.Contains(err.Error(), " bytes)") {
				t.Errorf("a part of %d bytes is refused with %q, and was with %q", size, err, was)
			}
		}
	}
	_, err := decodeYAML([]byte("a: !!int " + long[:yamlPartBytes+1] + "\n"))
	if want := "yaml: cannot decode !!str `" + head + "`... (257 bytes) as a !!int"; err == nil || err.Error() != want {
		t.Errorf("a value of 257 bytes is refused with %q, want %q", err, want)
	}
}

// A text is cut between two characters, never inside one: a key written
// twice whose 64th byte is inside a character of two, of three or of four
// bytes is cut before that character, exactly as model.QuoteValue cuts it;
// so are a value that does not fit its tag at its 256th byte, and an error
// over 2,000 bytes at the byte the whole is cut at. What is left is valid
// UTF-8, and the length it says is that of the whole.
func TestAYAMLErrorIsCutBetweenCharacters(t *testing.T) {
	for _, character := range []string{"é", "€", "😀"} {
		for lead := range len(character) {
			key := strings.Repeat("x", lead) + strings.Repeat(character, 40)
			_, err := decodeYAML([]byte(strings.Repeat("? "+key+"\n: 1\n", 2)))
			if err == nil {
				t.Fatalf("a key of %q written twice is decoded", character)
			}
			kept := lead + (yamlKeyBytes-lead)/len(character)*len(character)
			want := fmt.Sprintf("yaml: unmarshal errors:\n  line 3: mapping key %q... (%d bytes) already defined at line 1", key[:kept], len(key))
			if text := err.Error(); text != want || !utf8.ValidString(text) || !strings.Contains(text, model.QuoteValue(key)) || yamlKeyText(key, false) != model.QuoteValue(key) {
				t.Errorf("a key of %q after %d bytes is refused with %q, want %q", character, lead, text, want)
			}
			if got := model.SameFailureText(err); !utf8.ValidString(got) || !strings.Contains(got, strconv.Quote(key[:kept])+"... (# bytes)") {
				t.Errorf("a key of %q after %d bytes is recognised by %q", character, lead, got)
			}
			value := strings.Repeat("x", lead) + strings.Repeat(character, 200)
			_, err = decodeYAML([]byte("a: !!int " + value + "\n"))
			kept = lead + (yamlPartBytes-lead)/len(character)*len(character)
			want = fmt.Sprintf("yaml: cannot decode !!str `%s`... (%d bytes) as a !!int", value[:kept], len(value))
			if err == nil || err.Error() != want || !utf8.ValidString(err.Error()) || !utf8.ValidString(model.SameFailureText(err)) {
				t.Errorf("a value of %q after %d bytes is refused with %q, want %q", character, lead, err, want)
			}
			whole := errors.New(strings.Repeat("x", lead) + strings.Repeat(character, 1500))
			bounded := boundedFailure(whole)
			mark := fmt.Sprintf("... (%d bytes)", len(whole.Error()))
			kept = lead + (maxFailureBytes-len(mark)-lead)/len(character)*len(character)
			if text := bounded.Error(); text != whole.Error()[:kept]+mark || len(text) > maxFailureBytes || !utf8.ValidString(text) || !utf8.ValidString(model.SameFailureText(bounded)) {
				t.Errorf("an error of %q after %d bytes is cut to %d bytes, %q, want %d of it and %q", character, lead, len(text), text, kept, mark)
			}
		}
	}
	// A text that is no UTF-8 where it is cut is cut there.
	if got := headOf(strings.Repeat("\x80", 100), 64); got != 64 {
		t.Errorf("a text of bytes that start no character is cut at %d, want 64", got)
	}
	for _, text := range []string{"", "a", strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("é", 32), strings.Repeat("é", 33), "a" + strings.Repeat("é", 32), strings.Repeat("😀", 16), "abc" + strings.Repeat("😀", 16), "a\"\n" + strings.Repeat("€", 30)} {
		if got := yamlKeyText(text, false); got != model.QuoteValue(text) {
			t.Errorf("the key %q is named %s; model.QuoteValue names it %s", text, got, model.QuoteValue(text))
		}
	}
}

// yamlKeyNamed is a problem with a key written twice, in a list of them.
var yamlKeyNamed = regexp.MustCompile(`^  line \d+: mapping key (".*") already defined at line \d+$`)

// yamlHasNoLongPart reports whether an error of the decoder as it was holds
// no part that is now cut: a list of problems none of which names a key of
// more than 64 bytes, or another error of 256 bytes or fewer in all.
func yamlHasNoLongPart(text string) bool {
	problems, list := strings.CutPrefix(text, "yaml: unmarshal errors:\n")
	if !list {
		return len(text) <= yamlPartBytes
	}
	for _, problem := range strings.Split(problems, "\n") {
		if named := yamlKeyNamed.FindStringSubmatch(problem); named != nil {
			if key, err := strconv.Unquote(named[1]); err != nil || len(key) > yamlKeyBytes {
				return false
			}
		}
	}
	return true
}

// A YAML error with no long part reads as it did, to the letter, is
// recognised by what it was, and is the kind of error it was to errors.As;
// and a document that decodes decodes into what it did. The decoder as it
// was before any error was cut (yamluncut_test.go) is the oracle, over the
// YAML files of the repository, whole and cut off in their first sixty
// lines, the documents written for the errors, for the forms of a merge, a
// key and an alias, 6,000 drawn at random, and 150 mappings of 129 pairs or
// more whose keys are written several times, of short names, of names that
// hold the library's own words and of names of 64 bytes; each decoded with
// a mapping large past no key, one, two, four and 128 keys, so that each is
// decoded by the library, in parts and refused without the library. An
// error within the bound on the whole is also the very error the decoder
// made, untouched by the bound. The few errors with a long part say how
// long it was.
func TestAYAMLFailureWithNoLongPartReadsAndIsRecognisedAsItWas(t *testing.T) {
	documents := append(append(yamlFixtureDocuments(t), hostileYAMLDocuments()...), yamlWrittenDocuments()...)
	maker := yamlMaker{random: rand.New(rand.NewPCG(20, 26))} //nolint:gosec // documents for a test
	generated, mappings := 6000, 150
	if raceDetector {
		generated, mappings = 600, 30
	}
	for range generated {
		documents = append(documents, maker.document())
	}
	names := []string{"a", "b", "c", `"x already defined at line 7"`, `"line 5: x"`, `"a\n  line 9: boo"`, `"#"`, "3", "[s]", "{m: 1}", "~", "!!str 5", strings.Repeat("n", yamlKeyBytes), strings.Repeat("é", yamlKeyBytes/2), "d", "e"}
	random := rand.New(rand.NewPCG(20, 27)) //nolint:gosec // documents for a test
	for i := range mappings {
		kinds := 1 + random.IntN(len(names)-1)
		from := random.IntN(len(names) - kinds + 1)
		mapping := keysWrittenTwice(random, names[from:from+kinds], 129+random.IntN(100))
		if i%2 == 0 {
			mapping = "under:\n  " + strings.ReplaceAll(strings.TrimSuffix(mapping, "\n"), "\n", "\n  ") + "\nafter: {a: 1, a: 2}\n"
		}
		documents = append(documents, mapping)
	}
	counted := map[string]int{}
	for _, document := range documents {
		for _, large := range []int{0, 1, 2, 4, yamlLargeMapping} {
			reading := yamlReading{large: large}
			was, wasErr := decodeYAMLUncut(reading, []byte(document))
			got, gotErr := reading.decode([]byte(document))
			if (gotErr == nil) != (wasErr == nil) {
				t.Fatalf("%.300q, large past %d keys, is refused with %.500v; it was with %.500v", document, large, gotErr, wasErr)
			}
			if gotErr == nil {
				counted["decoded"]++
				if !sameYAMLValue(got, was) {
					t.Fatalf("%.300q, large past %d keys, decodes into %.500v; it did into %.500v", document, large, got, was)
				}
				continue
			}
			if !yamlHasNoLongPart(wasErr.Error()) {
				counted["refused with a long part"]++
				// The part is cut to its start, and its length said in up to
				// some twenty bytes more.
				if text := gotErr.Error(); !strings.Contains(text, " bytes)") || len(text) > len(wasErr.Error())+len("... (1000000 bytes)") || !strings.Contains(model.SameFailureText(gotErr), "... (# bytes)") {
					t.Fatalf("%.300q, large past %d keys, is refused with %d bytes, %.500v; it was with %d bytes", document, large, len(gotErr.Error()), gotErr, len(wasErr.Error()))
				}
				continue
			}
			var problems *yaml.TypeError
			if gotErr.Error() != wasErr.Error() || model.SameFailureText(gotErr) != model.SameFailureText(wasErr) || errors.As(gotErr, &problems) != errors.As(wasErr, &problems) {
				t.Fatalf("%.300q, large past %d keys, is refused with\n%v\nrecognised by\n%s\nit was with\n%v\nrecognised by\n%s", document, large, gotErr, model.SameFailureText(gotErr), wasErr, model.SameFailureText(wasErr))
			}
			if errors.As(gotErr, &problems) {
				counted["refused with a list of problems"]++
				if root, _ := yamlDocumentOf([]byte(document)); root != nil && yamlWayOf(root, large) == yamlRefused {
					counted["refused without the library"]++
				}
			} else {
				counted["refused otherwise"]++
			}
			if bounded := boundedFailure(gotErr); bounded != gotErr { //nolint:errorlint // the very error, not one that wraps it
				t.Fatalf("%.300q, large past %d keys: the error of %d bytes is not the decoder's own once bounded: %v", document, large, len(gotErr.Error()), bounded)
			}
		}
	}
	t.Logf("%d documents: %v", len(documents), counted)
	for outcome, least := range map[string]int{"decoded": 20000, "refused with a list of problems": 2000, "refused without the library": 500, "refused otherwise": 8000} {
		if counted[outcome] < least && !raceDetector {
			t.Errorf("%d documents were %s, fewer than %d: the documents do not cover it", counted[outcome], outcome, least)
		}
	}
	if counted["refused with a long part"] > counted["refused with a list of problems"]/20 {
		t.Errorf("%d documents were refused with a long part, of %d refused with a list of problems: the documents are to be of errors that are not cut", counted["refused with a long part"], counted["refused with a list of problems"])
	}
}

// heapHeldAfter is how many bytes more the heap holds once made has run, a
// collection later, while what it returned is still held: the least of
// three runs, since the heap of a test holds what other code left on it.
// What the run before returned is held through both measurements of a run,
// so that letting go of it does not hide what the run's own is holding.
func heapHeldAfter(made func() error) (held uint64, err error) {
	held = ^uint64(0)
	for range 3 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&before)
		next := made()
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(err)
		err = next
		grown := uint64(0)
		if after.HeapAlloc > before.HeapAlloc {
			grown = after.HeapAlloc - before.HeapAlloc
		}
		held = min(held, grown)
	}
	return held, err
}

// The error of a document refused for a long part holds the short text and
// nothing of the long one: not the library's error, whose text is the whole
// key, nor its list of problems, nor a text that the short one is a part
// of. With the error still held and the document let go of, the heap is
// under 16 kB larger than before the document was decoded, where the error
// of a key of 200,000 numbers held 1.4 MB, that of a key of 100 kB written
// three times 400 kB, that of a key of 21 kB the library lists three
// problems of 84 kB, and that of a value of a megabyte a megabyte. The
// failure log keeps what a failure is recognised by for as long as it is
// remembered, and whoever holds the error holds this much.
func TestACutYAMLErrorHoldsNothingOfTheDocument(t *testing.T) {
	if raceDetector {
		t.Skip("what the heap holds is not measured under the race detector")
	}
	key, _ := numbersFrom(1, 200000)
	long := strings.Repeat("k", 1000000)
	for name, document := range map[string]string{
		"a key of 200,000 numbers":            "1: x\n? " + key + "\n: x\n",
		"a key of 100 kB written three times": strings.Repeat("? "+long[:100000]+"\n: 1\n", 3),
		"a key of 21 kB the library lists":    strings.Repeat("? "+long[:21000]+"\n: 1\n", 3),
		"a value of a megabyte tagged !!int":  "a: !!int " + long + "\n",
		"an anchor of a megabyte":             "a: *" + long + "\n",
	} {
		for _, through := range []string{"decodeYAML", "Decode"} {
			held, err := heapHeldAfter(func() error {
				if through == "Decode" {
					return yamlFailureOf(t, document)
				}
				_, err := decodeYAML([]byte(document))
				return err
			})
			if err == nil {
				t.Fatalf("%s: the document is decoded", name)
			}
			if text, same := err.Error(), model.SameFailureText(err); held > 16<<10 || len(text) > 600 || len(same) > 600 {
				t.Errorf("%s, through %s: with the error held the heap holds %d bytes more; the error is %d bytes and is recognised by %d", name, through, held, len(text), len(same))
			}
		}
	}
}

// The words of a panic while a document is decoded are whatever was raised:
// a text of 10,000 bytes is said by its first 256 and its length, the
// library's panic and the exporter's own alike, and the exporter's still
// ends by saying that it is a defect to report. The failure is recognised by
// the same with the mark for the length. A panic of 256 bytes or fewer, and
// a runtime error, read and are recognised as they were.
func TestTheWordsOfALongPanicWhileDecodingYAMLAreCut(t *testing.T) {
	library := yamlPlace{file: "decode.go", line: 812, function: "yaml.v3.(*decoder).mapping", library: true}
	walk := yamlPlace{file: "yamlkeys.go", line: 547, function: "decode.(*yamlMap).set"}
	const ofLibrary, ofExporter, report = "the YAML library failed on the document: ", "the exporter failed on the YAML document (", "; this is a defect of the exporter and not of the document, please report it"
	long := strings.Repeat("p", 10000)
	head := long[:yamlPartBytes]
	for name, tc := range map[string]struct {
		failed     any
		at         yamlPlace
		text, same string
	}{
		"the library, a text":   {long, library, ofLibrary + head + "... (10000 bytes)", ofLibrary + head + "... (# bytes)"},
		"the library, an error": {errors.New(long), library, ofLibrary + head + "... (10000 bytes)", ofLibrary + head + "... (# bytes)"},
		"the exporter, a text":  {long, walk, ofExporter + "yamlkeys.go:547 decode.(*yamlMap).set): " + head + "... (10000 bytes)" + report, ofExporter + "yamlkeys.go decode.(*yamlMap).set): " + head + "... (# bytes)" + report},
		"the exporter, an error": {fmt.Errorf("no such part: %s", long), walk, ofExporter + "yamlkeys.go:547 decode.(*yamlMap).set): no such part: " + long[:yamlPartBytes-14] + "... (10014 bytes)" + report,
			ofExporter + "yamlkeys.go decode.(*yamlMap).set): no such part: " + long[:yamlPartBytes-14] + "... (# bytes)" + report},
	} {
		err := yamlPanicked(tc.failed, tc.at)
		if err.Error() != tc.text || model.SameFailureText(err) != tc.same {
			t.Errorf("%s: the failure is %q, recognised by %q, want %q, recognised by %q", name, err, model.SameFailureText(err), tc.text, tc.same)
		}
	}
	v, err := yamlPanickingAt(func() { panic(long) })
	if err == nil {
		t.Fatalf("the document decodes into %.100v, want the panic as its failure", v)
	}
	if text := err.Error(); len(text) > 600 || !strings.HasPrefix(text, ofExporter) || !strings.HasSuffix(text, "): "+head+"... (10000 bytes)"+report) || !strings.HasSuffix(model.SameFailureText(err), "): "+head+"... (# bytes)"+report) {
		t.Errorf("a panic of 10,000 bytes is the failure %q, recognised by %q", text, model.SameFailureText(err))
	}
	var outOfRange runtime.Error
	func() {
		defer func() { outOfRange, _ = recover().(runtime.Error) }()
		none, index := []int{}, 7
		_ = none[index]
	}()
	for _, failed := range []any{"no more tokens", errors.New("did not find expected key"), outOfRange, long[:yamlPartBytes], errors.New(long[:yamlPartBytes]), 5, nil} {
		for _, at := range []yamlPlace{library, walk} {
			err, was := yamlPanicked(failed, at), yamlPanickedUncut(failed, at)
			if err.Error() != was.Error() || model.SameFailureText(err) != model.SameFailureText(was) {
				t.Errorf("a panic of %.60v is the failure %q, recognised by %q; it was %q, recognised by %q", failed, err, model.SameFailureText(err), was, model.SameFailureText(was))
			}
		}
	}
}
