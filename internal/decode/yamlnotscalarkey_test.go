package decode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// yamlNotScalarKeyWords is the problem of a document with a mapping key that
// is a sequence or a mapping, after the line and what the key is.
const yamlNotScalarKeyWords = "; only scalar keys (text, numbers, booleans, null) can be read"

// yamlFirstNotScalarKey is the first mapping key under n, in the order the
// document is written, that is a sequence or a mapping or an alias of one,
// and nil when there is none: found here apart from the decoder, which
// looks for it as it looks through the document for other things.
func yamlFirstNotScalarKey(n *yaml.Node) *yaml.Node {
	for i, child := range n.Content {
		kind := child.Kind
		if kind == yaml.AliasNode && child.Alias != nil {
			kind = child.Alias.Kind
		}
		if n.Kind == yaml.MappingNode && i%2 == 0 && (kind == yaml.SequenceNode || kind == yaml.MappingNode) {
			return child
		}
		if child.Kind != yaml.AliasNode {
			if key := yamlFirstNotScalarKey(child); key != nil {
				return key
			}
		}
	}
	return nil
}

// yamlNotScalarKeyText is the error a document is refused with for key, a
// key that is a sequence or a mapping or an alias of one, written out here
// apart from the decoder, and the text that failure is recognised by.
func yamlNotScalarKeyText(key *yaml.Node) (text, same string) {
	what := map[yaml.Kind]string{yaml.SequenceNode: "a sequence", yaml.MappingNode: "a mapping"}[key.Kind]
	if key.Kind == yaml.AliasNode {
		what = "an alias of " + map[yaml.Kind]string{yaml.SequenceNode: "a sequence", yaml.MappingNode: "a mapping"}[key.Alias.Kind]
	}
	return fmt.Sprintf("yaml: the mapping key at line %d is %s%s", key.Line, what, yamlNotScalarKeyWords),
		"yaml: the mapping key at line # is " + what + yamlNotScalarKeyWords
}

// yamlNotScalarKeyLines adds to own the lines of the keys under n that are
// a sequence or a mapping, and to aliased, for the keys that are an alias of
// one, the line of what the alias stands for: where the library says such a
// key is.
func yamlNotScalarKeyLines(n *yaml.Node, own, aliased map[int]bool) {
	for i, child := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			switch {
			case child.Kind == yaml.SequenceNode || child.Kind == yaml.MappingNode:
				own[child.Line] = true
			case child.Kind == yaml.AliasNode && child.Alias != nil && child.Alias.Kind != yaml.ScalarNode:
				aliased[child.Alias.Line] = true
			}
		}
		if child.Kind != yaml.AliasNode {
			yamlNotScalarKeyLines(child, own, aliased)
		}
	}
}

// yamlOfAKeyProblem is the problems the library lists for a key that is a
// sequence or a mapping: one of no text written twice, and one decoded into
// text.
var yamlOfAKeyProblem = regexp.MustCompile("^  line ([0-9]+): (?:mapping key \"\" already defined at line ([0-9]+)|cannot unmarshal !!(?:seq|map|[^ ]+ ``) into string)$")

// yamlOfAKey reports whether err is an error the library refused the
// document under root with for a key that is a sequence or a mapping, read
// from its text here apart from the decoder: its words for a key that is no
// key, its panic in a merge, and a list with a problem of such a key, on the
// lines of such keys.
func yamlOfAKey(root *yaml.Node, err error) bool {
	if err == nil || root == nil || yamlFirstNotScalarKey(root) == nil {
		return false
	}
	text := err.Error()
	if strings.HasPrefix(text, "yaml: invalid map key: ") || strings.HasPrefix(text, "the YAML library failed on the document: ") {
		return true
	}
	listed, ok := strings.CutPrefix(text, "yaml: unmarshal errors:")
	if !ok {
		return false
	}
	own, aliased := map[int]bool{}, map[int]bool{}
	yamlNotScalarKeyLines(root, own, aliased)
	for _, problem := range strings.Split(listed, "\n")[1:] {
		found := yamlOfAKeyProblem.FindStringSubmatch(problem)
		if found == nil {
			continue
		}
		later, _ := strconv.Atoi(found[1])
		earlier, _ := strconv.Atoi(found[2])
		// A key written twice is two that are sequences, or mappings; an
		// alias is told apart by its anchor's name.
		if found[2] != "" && own[later] && own[earlier] || found[2] == "" && (own[later] || aliased[later]) {
			return true
		}
	}
	return false
}

// yamlKeyRefusalNow is what a document with a mapping key that is a
// sequence or a mapping is refused with since such a key is refused in its
// own words, for an oracle that tells what decoding the document came to
// before: was, unless the document has such a key and was refused for it.
// A document that decoded, and one refused for anything else, are left as
// they were.
func yamlKeyRefusalNow(body []byte, was error) error {
	root, err := yamlDocumentOf(body)
	if err != nil || !yamlOfAKey(root, was) {
		return was
	}
	text, same := yamlNotScalarKeyText(yamlFirstNotScalarKey(root))
	return model.SameFailureAs(errors.New(text), same)
}

// keyRefusalNow is yamlKeyRefusalNow for the outcome of decoding the parsed
// document root, which may have been the library failing outright.
func (o yamlOutcome) keyRefusalNow(root *yaml.Node) yamlOutcome {
	if o.failed == nil && !yamlOfAKey(root, o.err) || yamlFirstNotScalarKey(root) == nil {
		return o
	}
	text, same := yamlNotScalarKeyText(yamlFirstNotScalarKey(root))
	return yamlOutcome{err: model.SameFailureAs(errors.New(text), same)}
}

// yamlRefusalBefore is the error a document with keys written twice, the
// way refused without the library, was refused with before a key that is a
// sequence or a mapping was refused in its own words.
func yamlRefusalBefore(root *yaml.Node, large int) error {
	learnt := yamlLearnt{large: large}
	learnt.learn(root, true)
	return learnt.refusal(root)
}

// decodeYAMLBeforeKeys is decodeYAML as it was before a mapping key that is
// a sequence or a mapping was refused in its own words, the oracle of the
// tests that show every other document decodes, and is refused, as it was.
func decodeYAMLBeforeKeys(body []byte) (v any, err error) {
	defer func() {
		if failed := recover(); failed != nil {
			v, err = nil, yamlPanicked(failed, yamlPanicPlace())
		}
	}()
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if err := decoder.Decode(&root); errors.Is(err, io.EOF) {
		return nil, nil
	} else if err != nil {
		return nil, yamlFailure(err)
	}
	for {
		var next yaml.Node
		err := decoder.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, yamlFailure(err)
		}
		if !emptyYAMLDocument(&next) {
			return nil, errors.New("the body holds more than one YAML document (separated by ---); multi-document YAML is not supported")
		}
	}
	timestampsAsText(&root, map[*yaml.Node]bool{})
	r := yamlReading{large: yamlLargeMapping}
	learnt := yamlLearnt{large: r.large}
	learnt.learn(&root, true)
	if learnt.tooDeep(&root) {
		return nil, yamlTooDeep()
	}
	switch learnt.way(&root) {
	case yamlByParts:
		walk := yamlWalk{large: r.large, hands: r.hands, aliases: learnt.aliases, known: map[*yaml.Node]yamlKnown{}, expanding: map[*yaml.Node]bool{}}
		walk.null = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
		walk.learn(&root)
		v, err := walk.value(&root)
		if err == nil && len(walk.problems) > 0 {
			err = &yaml.TypeError{Errors: walk.problems}
		}
		if err != nil {
			return nil, yamlFailure(err)
		}
		return v, nil
	case yamlRefused:
		return nil, learnt.refusal(&root)
	}
	var decoded any
	if err := root.Decode(&decoded); err != nil {
		return nil, yamlFailure(err)
	}
	return decoded, nil
}

// A mapping key that is a sequence or a mapping, or an alias of one, is
// refused in words that say so and where, as the first such key in the
// document: two different ones in a mapping were refused as one key written
// twice, `mapping key "" already defined`, for the library tells keys apart
// by their text and these have none; one alone, or the same one twice, in
// the library's words of its own types, `invalid map key: []interface
// {}{"a", "b"}`, with no line; and one in a mapping merged into another the
// library panicked on. A sequence tagged as text is a sequence still; a key
// beside a large mapping, which the walk decodes, and beside keys written
// twice in a large mapping, which are refused without the library, is
// refused the same; a document nested too deep is refused for that first,
// as before.
func TestAYAMLMappingKeyThatIsASequenceOrAMappingIsRefusedForWhatItIs(t *testing.T) {
	const sequence, mapping = "a sequence", "a mapping"
	large := manyYAMLKeys(129)
	for _, tc := range []struct {
		document, what string
		line           int
	}{
		{"? [a, b]\n: 1\n? [c, d]\n: 2\n", sequence, 1},
		{"? {x: 1}\n: 1\n? {y: 2}\n: 2\n", mapping, 1},
		{"{[a]: 1, [b]: 2}\n", sequence, 1},
		{"? [a, b]\n: 1\n", sequence, 1},
		{"? {x: 1}\n: 1\n", mapping, 1},
		{"? [a, b]\n: 1\n? [a, b]\n: 2\n", sequence, 1},
		{"? []\n: 1\n", sequence, 1},
		{"x: 1\ny: 2\n? {}\n: 1\n", mapping, 3},
		{"top:\n  nested:\n    ? [a]\n    : 1\n", sequence, 3},
		{"- 1\n- {? [a] : 1}\n", sequence, 2},
		{"a: &s [1]\nb:\n  ? *s\n  : 2\n", "an alias of " + sequence, 3},
		{"a: &m {k: v}\n? *m\n: 2\n", "an alias of " + mapping, 2},
		{"? !!str [a]\n: 1\nb: 2\n", sequence, 1},
		{"a: 1\n2: 3\n<<: {[x]: 1}\n", sequence, 3},
		{"2: 3\n<<: {{k: v}: 1}\n", mapping, 2},
		{"? {? [inner] : 1}\n: 1\n", mapping, 1},
		{"first: [1]\nkeys:\n  ? [a]\n  : 1\n  ? {b: 2}\n  : 2\n", sequence, 3},
		{large + "? [a]\n: 1\n", sequence, 130},
		{large + "b: 1\nb: 2\n? {m: 1}\n: 1\n? {n: 2}\n: 2\n", mapping, 132},
	} {
		v, err := decodeYAML([]byte(tc.document))
		want := fmt.Sprintf("yaml: the mapping key at line %d is %s%s", tc.line, tc.what, yamlNotScalarKeyWords)
		if err == nil || err.Error() != want {
			t.Errorf("%.200q decodes into %v, %v; want it refused with %q", tc.document, v, err, want)
			continue
		}
		if same, want := model.SameFailureText(err), "yaml: the mapping key at line # is "+tc.what+yamlNotScalarKeyWords; same != want {
			t.Errorf("%.200q: the failure is recognised by %q, want %q", tc.document, same, want)
		}
	}
	deep := strings.Repeat("[", MaxDepth) + "{[a]: 1}" + strings.Repeat("]", MaxDepth)
	if _, err := decodeYAML([]byte(deep)); err == nil || model.SameFailureText(err) != yamlTooDeepText {
		t.Errorf("a key that is a sequence nested too deep is refused with %.300v, want it refused for its depth", err)
	}
}

// The failure of a key that is a sequence or a mapping is one to the log
// wherever in the document the key is, on its first line or another, and
// whatever it holds; a key that is a mapping is another failure, and so is
// an alias of a sequence.
func TestAYAMLKeyThatIsNoScalarIsOneFailureWhereverItIs(t *testing.T) {
	same := func(document string) string {
		_, err := decodeYAML([]byte(document))
		return model.SameFailureText(err)
	}
	one := same("? [a]\n: 1\n")
	for _, document := range []string{"x: 1\n? [a]\n: 1\n", "x: 1\ny: 2\nz: 3\n\n\n? [b, c, d]\n: 1\n", "{a: 1, [x]: 2}\n", manyYAMLKeys(200) + "? [q]\n: 1\n"} {
		if got := same(document); got != one {
			t.Errorf("%.200q is recognised by %q, want %q", document, got, one)
		}
	}
	for _, document := range []string{"? {a: 1}\n: 1\n", "a: &s [1]\n? *s\n: 1\n"} {
		if got := same(document); got == one {
			t.Errorf("%q is recognised as the failure of a key that is a sequence, %q", document, got)
		}
	}
}

// Every other YAML document decodes, and is refused, exactly as it was
// before a key that is a sequence or a mapping was refused in its own
// words, and every document that has such a key was refused before, so
// none that decoded is refused now: the oracle is decodeYAML as it was,
// over the documents of the YAML tests here and a thousand generated ones
// (three hundred under the race detector) of mappings, sequences, scalars
// of every tag, aliases, merges, keys written twice, large mappings and
// keys that are sequences and mappings, each piece as likely as the next.
func TestEveryOtherYAMLDocumentDecodesAsItDidBeforeKeysThatAreNoScalarWereRefused(t *testing.T) {
	documents := []string{
		"", "---\n", "a: 1\n", "a: 1\na: 2\n", "a: [1, 2]\nb: {c: d}\n", "? a\n: 1\n", "? 1\n: x\n? true\n: y\n? ~\n: z\n",
		"a: &x 1\nb: *x\n", "a: &m {k: v}\nb:\n  <<: *m\n  c: 1\n", "a: 1\nb: *nope\n", "a: &x {b: *x}\n",
		"a: !!int foo\n", "a: !!binary '***'\n", "updated: 2024-06-01\n", "a: 1\n---\nb: 2\n", "a: [1\n",
		"a: 1\nb:\n  <<: 5\n", "!!str 5: x\n", "? |\n  block\n: 1\n", "[1, 2, {a: b}]\n", manyYAMLKeys(129),
		manyYAMLKeys(200) + "a: 1\n", strings.Repeat("a: 1\n", 60), excessiveAliasing(),
	}
	random := yamlGenerated{rand.New(rand.NewPCG(23, 27))} //nolint:gosec // documents for a test
	for range alloctest.UnlessRaced(1000, 300) {
		var document strings.Builder
		if random.IntN(8) == 0 {
			document.WriteString(manyYAMLKeys(129))
		}
		for range 1 + random.IntN(4) {
			fmt.Fprintf(&document, "%s: %s\n", random.key(0), random.value(0))
		}
		documents = append(documents, document.String())
	}
	counted := map[string]int{}
	for _, document := range documents {
		v, err := decodeYAML([]byte(document))
		was, wasErr := decodeYAMLBeforeKeys([]byte(document))
		root, _ := yamlDocumentOf([]byte(document))
		if root != nil && yamlFirstNotScalarKey(root) != nil {
			counted["with a key that is no scalar"]++
			if yamlOfAKey(root, wasErr) {
				counted["refused for a key that is no scalar"]++
				if err == nil || !strings.HasPrefix(err.Error(), "yaml: the mapping key at line ") {
					t.Errorf("%.300q decodes into %.300v, %v; want it refused for its key", document, v, err)
				}
				continue
			}
		}
		now, before := yamlOutcome{value: v, err: err}, yamlOutcome{value: was, err: wasErr}
		if !now.same(before) {
			t.Errorf("%.300q is\n%.1000s\nand was\n%.1000s", document, now.text(), before.text())
		}
		if err == nil {
			counted["decoded"]++
		} else {
			counted["refused"]++
		}
	}
	t.Logf("the documents are %v", counted)
	for _, kind := range []string{"with a key that is no scalar", "refused for a key that is no scalar", "decoded", "refused"} {
		if counted[kind] < 20 {
			t.Errorf("the documents are %v, too few of them %s", counted, kind)
		}
	}
}

// yamlGenerated makes the pieces of generated documents.
type yamlGenerated struct {
	*rand.Rand
}

// value is a value of a generated document: a scalar of some tag, an alias,
// an anchored value, a sequence, a mapping or a mapping with a merge key,
// and a scalar past depth 3.
func (g yamlGenerated) value(depth int) string {
	scalars := []string{"1", "text", "~", "2024-06-01", "!!str 7", "*a"}
	if depth > 3 {
		return scalars[g.IntN(len(scalars))]
	}
	switch g.IntN(len(scalars) + 4) {
	case 0:
		return "&a " + g.value(depth+1)
	case 1:
		return "[" + g.value(depth+1) + ", " + g.value(depth+1) + "]"
	case 2:
		return "{" + g.key(depth+1) + ": " + g.value(depth+1) + ", " + g.key(depth+1) + ": " + g.value(depth+1) + "}"
	case 3:
		return "{<<: " + g.value(depth+1) + ", k: 1}"
	}
	return scalars[g.IntN(len(scalars))]
}

// key is a key of a generated document: one of a few scalars, so that some
// are written twice, and one time in twelve each a sequence, a mapping, an
// alias and a merge key.
func (g yamlGenerated) key(depth int) string {
	switch g.IntN(12) {
	case 0:
		return "[" + g.value(depth+1) + "]"
	case 1:
		return "{k: " + g.value(depth+1) + "}"
	case 2:
		return "*a "
	case 3:
		return "<<"
	}
	return []string{"a", "b", "c", "1", "true", "~", "'x'", "!!str 5"}[g.IntN(8)]
}
