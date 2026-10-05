package decode

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// MaxDepth is how deep a decoded value nests at most, whichever decoder made
// it, and what follows a value as deep as it goes — the hand-over to a
// Python script, both ways — takes it from here (jsonvalue.go). These tests
// hold the decoders to it: of each format that nests, a document nested
// MaxDepth deep is decoded and one nested a level deeper is refused.

// valueDepth is how deep the lists and mappings of v nest at their deepest.
func valueDepth(v any) int {
	deepest := 0
	switch x := v.(type) {
	case []any:
		for _, value := range x {
			deepest = max(deepest, valueDepth(value))
		}
	case map[string]any:
		for _, value := range x {
			deepest = max(deepest, valueDepth(value))
		}
	default:
		return 0
	}
	return deepest + 1
}

// jsonValueAsItWas is JSONValue as it was when a YAML document decoded
// nested deeper than a JSON one, and a value was read three times as deep as
// a document.
func jsonValueAsItWas(body []byte) (value any, rest []byte, err error) {
	d := jsonDecoder{data: body, depth: 10000 - 30000}
	d.space()
	d.sizeCache()
	if value, err = d.value(); err != nil {
		return nil, nil, err
	}
	return value, body[d.pos:], nil
}

// A JSON document nests MaxDepth deep and no deeper, in arrays and in
// objects, and so does a value read as a worker's answer is read, by
// JSONValue, whatever lies around it: a pre-script may leave what a decoder
// made, and no decoder makes a value nested deeper. JSONValue read a value
// to 30,000 deep while a YAML document decoded that deep; what it reads of a
// value within MaxDepth, and refuses of a line that is no JSON, is what it
// read and refused then.
func TestAJSONValueNestsAsDeepAsADecodedValue(t *testing.T) {
	if MaxDepth != 10000 {
		t.Fatalf("MaxDepth is %d, want the 10,000 levels the JSON decoder reads", MaxDepth)
	}
	for _, shape := range []struct{ open, shut string }{{"[", "]"}, {`{"a":`, "}"}} {
		nested := func(depth int) []byte {
			return []byte(strings.Repeat(shape.open, depth) + "7" + strings.Repeat(shape.shut, depth))
		}
		if v, err := decodeJSON(nested(MaxDepth)); err != nil || valueDepth(v) != MaxDepth {
			t.Fatalf("a document nested %d deep in %s: %v", MaxDepth, shape.open, err)
		}
		if _, err := decodeJSON(nested(MaxDepth + 1)); err == nil || !strings.Contains(err.Error(), "arrays and objects nested more than 10000 deep") {
			t.Fatalf("a document nested %d deep in %s: %v, want it refused for its depth", MaxDepth+1, shape.open, err)
		}
		for _, depth := range []int{1, MaxDepth - 1, MaxDepth} {
			v, rest, err := JSONValue(append(nested(depth), ", next"...))
			if err != nil || valueDepth(v) != depth || string(rest) != ", next" {
				t.Fatalf("a value nested %d deep in %s: read nested %d deep, with %q after it and the error %v", depth, shape.open, valueDepth(v), rest, err)
			}
		}
		for _, depth := range []int{MaxDepth + 1, MaxDepth + 2, 3 * MaxDepth} {
			if _, _, err := JSONValue(nested(depth)); err == nil || !strings.Contains(err.Error(), "arrays and objects nested more than 10000 deep") {
				t.Fatalf("a value nested %d deep in %s is read with the error %v, deeper than any decoder makes one", depth, shape.open, err)
			}
			if _, _, err := jsonValueAsItWas(nested(depth)); err != nil {
				t.Fatalf("a value nested %d deep in %s was not read: %v", depth, shape.open, err)
			}
		}
	}
	for _, body := range []string{
		`7`, `"text", 1`, `[1, 2.5, "a", null, true, {"k": [1e400, 12345678901234567890123]}]}`, `{"a": {"b": [[], {}]}, "a": 2} `, ` [1,`, `{"a" 1}`, `tru`, ``, `]`, `[1 2]`,
		strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth) + `, "metrics": []}`, strings.Repeat(`{"a":[`, MaxDepth/2) + strings.Repeat("]}", MaxDepth/2) + "x", strings.Repeat("[", MaxDepth),
	} {
		v, rest, err := JSONValue([]byte(body))
		wasV, wasRest, wasErr := jsonValueAsItWas([]byte(body))
		if !reflect.DeepEqual(v, wasV) || string(rest) != string(wasRest) || (err == nil) != (wasErr == nil) || err != nil && err.Error() != wasErr.Error() {
			t.Errorf("%.60q is read as %.80v with %.40q after it and the error %v, and was as %.80v with %.40q and %v", body, v, rest, err, wasV, wasRest, wasErr)
		}
	}
}

// yamlDepthVerdict is what decoding a YAML document came to, for a table of
// documents nested about as deep as one may be.
func yamlDepthVerdict(outcome yamlOutcome) string {
	switch {
	case outcome.failed != nil:
		return fmt.Sprint("failed outright: ", outcome.failed)
	case outcome.err == nil:
		return "decoded"
	case isYAMLTooDeep(outcome.err):
		return "too deep"
	case outcome.err.Error() == yamlTooDeepAsItWas:
		return "too deep through its aliases"
	case outcome.err.Error() == "yaml: exceeded max depth of 10000":
		return "the parser's bound"
	case strings.HasSuffix(outcome.err.Error(), `mapping key "d" already defined at line 3`):
		return "a key written twice"
	}
	return outcome.err.Error()
}

// A YAML document decodes into a value nested MaxDepth deep at most, as a
// JSON document does, counting the same: the sequences and mappings one
// inside another, an alias as what it stands for. One nested deeper is
// refused before it is decoded, saying so, whether or not it has an alias.
//
// The library's parser does not see to that. It bounds what is nested by
// indentation and what is nested in brackets apart, at 10,000 each, so
// brackets inside indentation decoded nested 20,000 deep; and a level of
// indentation is two collections where a sequence is written under a
// mapping's key, no further in than the key. Only a document with an alias
// was held to a bound on the whole, and that one counted the library's
// calls, a document from three levels less deep than MaxDepth. A document
// the parser refuses, nested past its bound one way, was refused in the
// library's words, and is refused as every other that is too deep
// (yamlparserdepth_test.go).
//
// Each document of the table is decoded now and as it was (yamldepthdiff_test.go),
// and where it decodes, into what the library alone makes of it, nested as
// deep as the table says.
func TestNoYAMLDocumentNestsDeeperThanMaxDepth(t *testing.T) {
	const decoded, tooDeep, parser, aliases, twice = "decoded", "too deep", "the parser's bound", "too deep through its aliases", "a key written twice"
	indented := func(levels int) string { return strings.Repeat("- ", levels) }
	bracketed := func(levels int, inside string) string {
		return strings.Repeat("[", levels) + inside + strings.Repeat("]", levels)
	}
	// A mapping whose value is a sequence under its key, whose entry is such
	// a mapping again: two collections for each level of indentation, which
	// costs a body as long as the square of the levels, so a few hundred of
	// them, around brackets for the rest.
	const levels = 300
	underKeys := func(inside string) string {
		lines := []string{"k:"}
		for i := range levels {
			lines = append(lines, strings.Repeat("  ", i)+"- k:")
		}
		return strings.Join(append(lines, strings.Repeat("  ", levels)+"- "+inside, ""), "\n")
	}
	const under = 2 * (levels + 1)
	documents := []struct {
		name, body string
		// depth is how deep the value nests where the document decodes now.
		depth    int
		now, was string
	}{
		{"sequences by indentation, 9,999", indented(MaxDepth-1) + "7\n", MaxDepth - 1, decoded, decoded},
		{"sequences by indentation, 10,000", indented(MaxDepth) + "7\n", MaxDepth, decoded, decoded},
		{"sequences by indentation, 10,001", indented(MaxDepth+1) + "7\n", 0, tooDeep, parser},
		{"sequences in brackets, 9,999", bracketed(MaxDepth-1, "7") + "\n", MaxDepth - 1, decoded, decoded},
		{"sequences in brackets, 10,000", bracketed(MaxDepth, "7") + "\n", MaxDepth, decoded, decoded},
		{"sequences in brackets, 10,000, the innermost empty", bracketed(MaxDepth, "") + "\n", MaxDepth, decoded, decoded},
		{"sequences in brackets, 10,001", bracketed(MaxDepth+1, "7") + "\n", 0, tooDeep, parser},
		{"mappings in brackets, 10,000", yamlFlowMap(MaxDepth) + "\n", MaxDepth, decoded, decoded},
		{"mappings in brackets, 10,001", yamlFlowMap(MaxDepth+1) + "\n", 0, tooDeep, parser},
		{"brackets inside indentation, 5,000 and 4,999", indented(MaxDepth/2) + bracketed(MaxDepth/2-1, "7") + "\n", MaxDepth - 1, decoded, decoded},
		{"brackets inside indentation, 5,000 and 5,000", indented(MaxDepth/2) + bracketed(MaxDepth/2, "7") + "\n", MaxDepth, decoded, decoded},
		{"brackets inside indentation, 5,000 and 5,001", indented(MaxDepth/2) + bracketed(MaxDepth/2+1, "7") + "\n", 0, tooDeep, decoded},
		{"brackets inside indentation, 5,001 and 5,000", indented(MaxDepth/2+1) + bracketed(MaxDepth/2, "7") + "\n", 0, tooDeep, decoded},
		{"brackets inside indentation, 10,000 and 1", indented(MaxDepth) + "[7]\n", 0, tooDeep, decoded},
		{"brackets inside indentation, 10,000 and 10,000", indented(MaxDepth) + bracketed(MaxDepth, "7") + "\n", 0, tooDeep, decoded},
		{"sequences under keys, 602", underKeys("7"), under, decoded, decoded},
		{"sequences under keys around brackets, 9,999", underKeys(bracketed(MaxDepth-under-1, "7")), MaxDepth - 1, decoded, decoded},
		{"sequences under keys around brackets, 10,000", underKeys(bracketed(MaxDepth-under, "7")), MaxDepth, decoded, decoded},
		{"sequences under keys around brackets, 10,001", underKeys(bracketed(MaxDepth-under+1, "7")), 0, tooDeep, decoded},
		{"sequences under keys around brackets, 10,602", underKeys(bracketed(MaxDepth, "7")), 0, tooDeep, decoded},
		{"sequences under keys around indentation around brackets, 20,000", underKeys(indented(MaxDepth-under) + bracketed(MaxDepth, "7")), 0, tooDeep, decoded},
		// With an alias a document was held to a bound, three levels under
		// this one where the alias is on the way to the innermost value.
		{"an alias of sequences, 9,997", "p: &x " + yamlFlowSeq(MaxDepth-4) + "\nq: *x\n", MaxDepth - 3, decoded, decoded},
		{"an alias of sequences, 9,998", "p: &x " + yamlFlowSeq(MaxDepth-3) + "\nq: *x\n", MaxDepth - 2, decoded, aliases},
		{"an alias of sequences, 10,000", "p: &x " + yamlFlowSeq(MaxDepth-1) + "\nq: *x\n", MaxDepth, decoded, aliases},
		{"an alias of sequences, 10,001", "p: &x " + yamlFlowSeq(MaxDepth) + "\nq: *x\n", 0, tooDeep, aliases},
		{"an alias beside brackets inside indentation, 9,998", "a: &a 1\nb: *a\nc:\n  " + indented(MaxDepth/2-2) + bracketed(MaxDepth/2-1, "7") + "\n", MaxDepth - 2, decoded, decoded},
		{"an alias beside brackets inside indentation, 10,000", "a: &a 1\nb: *a\nc:\n  " + indented(MaxDepth/2) + bracketed(MaxDepth/2-1, "7") + "\n", MaxDepth, decoded, aliases},
		{"an alias beside brackets inside indentation, 10,001", "a: &a 1\nb: *a\nc:\n  " + indented(MaxDepth/2) + bracketed(MaxDepth/2, "7") + "\n", 0, tooDeep, aliases},
		// A mapping merged counts as it is written, inside the mapping it is
		// merged into, where its keys are that mapping's own in the value.
		{"a merge of mappings, 9,997 written", "p: &x " + yamlFlowMap(MaxDepth-5) + "\nq:\n  <<: *x\n", MaxDepth - 4, decoded, decoded},
		{"a merge of mappings, 10,000 written", "p: &x " + yamlFlowMap(MaxDepth-2) + "\nq:\n  <<: *x\n", MaxDepth - 1, decoded, aliases},
		{"a merge of mappings, 10,001 written", "p: &x " + yamlFlowMap(MaxDepth-1) + "\nq:\n  <<: *x\n", 0, tooDeep, aliases},
		// Anchors that each hold an alias of the one before, which the library
		// decodes in a call for the sequence and one for the alias: both were
		// counted, and a link is one level now.
		{"a chain of 4,997 aliases, 5,001", yamlHiddenChain(MaxDepth/2 - 3), 0, twice, twice},
		{"a chain of 4,998 aliases, 5,002", yamlHiddenChain(MaxDepth/2 - 2), 0, twice, aliases},
		{"a chain of 9,996 aliases, 10,000", yamlHiddenChain(MaxDepth - 4), 0, twice, aliases},
		{"a chain of 9,997 aliases, 10,001", yamlHiddenChain(MaxDepth - 3), 0, tooDeep, aliases},
	}
	if raceDetector {
		// A document at the bound and one over it, without an alias and with
		// one: under the race detector each takes a third of a second.
		documents = slices.DeleteFunc(documents, func(document struct {
			name, body string
			depth      int
			now, was   string
		}) bool {
			return !slices.Contains([]string{"brackets inside indentation, 5,000 and 5,000", "brackets inside indentation, 5,000 and 5,001", "sequences under keys around brackets, 10,001",
				"an alias of sequences, 10,000", "an alias of sequences, 10,001"}, document.name)
		})
	}
	for _, document := range documents {
		value, err := decodeYAML([]byte(document.body))
		if got := yamlDepthVerdict(yamlOutcome{value: value, err: err}); got != document.now {
			t.Errorf("%s: %.200s, want %s", document.name, got, document.now)
			continue
		}
		root, refused := yamlDocumentOf([]byte(document.body))
		// A document the parser refuses was refused in the library's words.
		was := yamlOutcome{err: yamlLibraryRefusal([]byte(document.body))}
		if refused == nil {
			was = yamlValueAsItWas(yamlReading{large: yamlLargeMapping}, root)
		}
		if got := yamlDepthVerdict(was); got != document.was {
			t.Errorf("%s: it was %.200s, want %s", document.name, got, document.was)
		}
		if err != nil {
			continue
		}
		if library := yamlByTheLibraryAlone(root); !library.same(yamlOutcome{value: value}) || valueDepth(value) != document.depth {
			t.Errorf("%s: decoded nested %d deep, want %d deep and what the library makes of it, which is nested %d deep (%.100v)", document.name, valueDepth(value), document.depth, valueDepth(library.value), library.err)
		}
	}
}
