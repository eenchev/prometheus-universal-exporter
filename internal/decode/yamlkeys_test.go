package decode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// yamlDocumentOf parses a body as decodeYAML does, to the document it then
// decodes: nothing for an empty body, and the error decodeYAML refuses a
// body with before it decodes anything.
func yamlDocumentOf(body []byte) (*yaml.Node, error) {
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
	return &root, nil
}

// yamlOutcome is what decoding a document came to: a value, an error, or
// the library failing outright.
type yamlOutcome struct {
	value  any
	err    error
	failed any
}

// text is the outcome written out, with the type of every value, and what
// an error is recognised by.
func (o yamlOutcome) text() string {
	switch {
	case o.failed != nil:
		return fmt.Sprintf("failed outright: %v", o.failed)
	case o.err != nil:
		return fmt.Sprintf("error %q, recognised by %q", o.err, model.SameFailureText(o.err))
	}
	return fmt.Sprintf("%#v", o.value)
}

// same reports whether two outcomes are one: values equal with their types,
// errors equal in their text, in what they are recognised by and in being a
// list of problems or not.
func (o yamlOutcome) same(other yamlOutcome) bool {
	if o.failed != nil || o.err != nil || other.failed != nil || other.err != nil {
		var problems *yaml.TypeError
		return o.text() == other.text() && errors.As(o.err, &problems) == errors.As(other.err, &problems)
	}
	return sameYAMLValue(o.value, other.value)
}

// sameYAMLValue reports whether two decoded values are equal, as
// reflect.DeepEqual does but for a NaN, which is equal to nothing: a NaN is
// the same as a NaN, and the values of the keys that are NaN, which no key
// finds again, are the same when they are written out the same in some
// order.
func sameYAMLValue(a, b any) bool {
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return false
	}
	nan := func(v any) bool {
		number, is := v.(float64)
		return is && number != number
	}
	switch a := a.(type) {
	case float64:
		return a == b.(float64) || nan(a) && nan(b)
	case []any:
		b := b.([]any)
		if len(a) != len(b) || (a == nil) != (b == nil) {
			return false
		}
		for i := range a {
			if !sameYAMLValue(a[i], b[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		b := b.(map[string]any)
		if len(a) != len(b) || (a == nil) != (b == nil) {
			return false
		}
		for key, value := range a {
			if other, has := b[key]; !has || !sameYAMLValue(value, other) {
				return false
			}
		}
		return true
	case map[any]any:
		b := b.(map[any]any)
		if len(a) != len(b) || (a == nil) != (b == nil) {
			return false
		}
		var ofNaN, otherOfNaN []string
		for key, value := range a {
			if nan(key) {
				ofNaN = append(ofNaN, fmt.Sprintf("%#v", value))
			} else if other, has := b[key]; !has || !sameYAMLValue(value, other) {
				return false
			}
		}
		for key, value := range b {
			if nan(key) {
				otherOfNaN = append(otherOfNaN, fmt.Sprintf("%#v", value))
			}
		}
		slices.Sort(ofNaN)
		slices.Sort(otherOfNaN)
		return slices.Equal(ofNaN, otherOfNaN)
	}
	return reflect.DeepEqual(a, b)
}

// yamlByTheLibraryAlone decodes a parsed document as decodeYAML did when it
// handed every document to the library.
func yamlByTheLibraryAlone(root *yaml.Node) (outcome yamlOutcome) {
	defer func() {
		if failed := recover(); failed != nil {
			outcome = yamlOutcome{failed: failed}
		}
	}()
	var v any
	if err := root.Decode(&v); err != nil {
		return yamlOutcome{err: yamlFailure(err)}
	}
	return yamlOutcome{value: v}
}

// yamlWith decodes a parsed document as decodeYAML does now, with a mapping
// large past so many keys.
func yamlWith(root *yaml.Node, large int, hands func(*yaml.Node)) (outcome yamlOutcome) {
	defer func() {
		if failed := recover(); failed != nil {
			outcome = yamlOutcome{failed: failed}
		}
	}()
	v, err := yamlReading{large: large, hands: hands}.value(root)
	return yamlOutcome{value: v, err: err}
}

// yamlWayOf is the way a parsed document is decoded.
func yamlWayOf(root *yaml.Node, large int) int {
	learnt := yamlLearnt{large: large}
	learnt.learn(root, true)
	return learnt.way(root)
}

// yamlNodes counts the nodes of a parsed document, and reports whether any
// is an alias or a merge key.
func yamlNodes(n *yaml.Node) (nodes int, aliasOrMerge bool) {
	nodes = 1
	aliasOrMerge = n.Kind == yaml.AliasNode || yamlMerges(n)
	for _, child := range n.Content {
		under, either := yamlNodes(child)
		nodes += under
		aliasOrMerge = aliasOrMerge || either
	}
	return nodes, aliasOrMerge
}

// largestYAMLMapping is how many keys the largest mapping has that the
// library reads when it decodes n, through its aliases too.
func largestYAMLMapping(n *yaml.Node, seen map[*yaml.Node]bool) int {
	if n == nil || seen[n] {
		return 0
	}
	seen[n] = true
	largest := largestYAMLMapping(n.Alias, seen)
	if n.Kind == yaml.MappingNode {
		largest = max(largest, len(n.Content)/2)
	}
	for _, child := range n.Content {
		largest = max(largest, largestYAMLMapping(child, seen))
	}
	return largest
}

// onlyKeysWrittenTwice reports whether an error is a list of problems that
// are all keys written twice.
func onlyKeysWrittenTwice(err error) bool {
	var problems *yaml.TypeError
	if !errors.As(err, &problems) {
		return false
	}
	for _, problem := range problems.Errors {
		if !strings.Contains(problem, ": mapping key ") && !strings.HasPrefix(problem, "... and ") {
			return false
		}
	}
	return true
}

// yamlMaker writes YAML documents of everything a document can hold, drawn
// at random.
type yamlMaker struct {
	random *rand.Rand
	// anchors are the anchors written so far, maps those of mappings and
	// scalars those of scalars.
	anchors, maps, scalars []string
	next                   int
	// twice is whether a mapping may write a key twice.
	twice bool
}

// yamlMadeScalars are scalars of every kind YAML reads: numbers in every
// base, floats, booleans and what only looks like one, null in every
// spelling, text plain and quoted, a text of two lines, dates and
// timestamps, values with a tag that fits, with one that does not, and with
// one of the document's own, and integers past int64.
var yamlMadeScalars = []string{
	"1", "2", "-7", "0x10", "017", "0o17", "0b11", "1_000", "1.5", "1e3", ".inf", "-.inf", ".nan",
	"true", "false", "yes", "on", "~", "null", "Null", "NULL", "text", "two words", `"quoted"`, "'single'", `""`, `"5"`, `"<<"`, "<<",
	`"line one\nline two"`, "2024-06-01", "2001-12-14t21:59:43.10-05:00", "2001-12-14 21:59:43.10 -5",
	"!!str 5", `!!int "5"`, "!!float 1", "!!binary aGVsbG8=", `!!null ""`, "!thing x", "!!timestamp 2001-01-01", "!!merge x",
	"18446744073709551615", "9223372036854775808", "-9223372036854775809",
	"1", "text", "~", "true", "1.5", "x", "y", "z", "3", "4", "text", "more text", "1", "2",
}

// yamlMadeMistakes are values the library refuses.
var yamlMadeMistakes = []string{"!!int foo", `!!float "x"`, "!!bool yes", `!!binary "@@@"`, "!!null foo", "!!timestamp nope"}

// yamlMadeKey is a key as it is written, what the library tells it from
// another by, and whether it is a merge key.
type yamlMadeKey struct {
	text, same string
	merge      bool
}

// yamlMadeKeys are keys of every kind: text plain and quoted, numbers that
// are one number written two ways, a float, booleans, null in three
// spellings, a date, keys with a tag, a sequence and a mapping, the merge
// key plain and tagged, and a quoted "<<", which is no merge key.
var yamlMadeKeys = []yamlMadeKey{
	{"a", "s:a", false}, {"b", "s:b", false}, {"c", "s:c", false}, {"d", "s:d", false}, {"e", "s:e", false}, {"f", "s:f", false},
	{"g", "s:g", false}, {"h", "s:h", false}, {"a", "s:a", false}, {"b", "s:b", false}, {"c", "s:c", false}, {"k", "s:k", false},
	{"i", "s:i", false}, {"j", "s:j", false}, {"l", "s:l", false}, {"m", "s:m", false}, {"n", "s:n", false}, {"o", "s:o", false},
	{"p", "s:p", false}, {"q", "s:q", false}, {"r", "s:r", false}, {"s", "s:s", false}, {"t", "s:t", false}, {"u", "s:u", false},
	{`"a"`, "s:a", false}, {"'b'", "s:b", false}, {"1", "s:1", false}, {"2", "s:2", false}, {"0x1", "s:0x1", false}, {"1.5", "s:1.5", false},
	{"true", "s:true", false}, {"~", "s:~", false}, {"null", "s:null", false}, {`!!null ""`, "s:", false}, {"2024-06-01", "s:2024-06-01", false},
	{"!!str 5", "s:5", false}, {`!!int "5"`, "s:5", false}, {"!!merge x", "s:x", false}, {"[a, b]", "seq", false}, {"{x: 1}", "map", false},
	{`"<<"`, "s:<<", false}, {"!!str <<", "s:<<", false}, {"<<", "s:<<", true}, {"<<", "s:<<", true}, {"<<", "s:<<", true}, {"!!merge <<", "s:<<", true},
}

func (m *yamlMaker) chance(percent int) bool {
	return m.random.IntN(100) < percent
}

// name is a name for an anchor, which no alias can be written of before
// the anchor is known.
func (m *yamlMaker) name() string {
	m.next++
	return fmt.Sprintf("n%d", m.next)
}

// anchor is a new anchor, of a mapping, a sequence or a scalar, known from
// here on.
func (m *yamlMaker) anchor(of yaml.Kind) string {
	return m.known(m.name(), of)
}

// known makes an anchor known.
func (m *yamlMaker) known(name string, of yaml.Kind) string {
	m.anchors = append(m.anchors, name)
	switch of {
	case yaml.MappingNode:
		m.maps = append(m.maps, name)
	case yaml.ScalarNode:
		m.scalars = append(m.scalars, name)
	}
	return name
}

// key draws one more key of a mapping whose keys so far are taken, or none.
func (m *yamlMaker) key(taken map[string]bool) (yamlMadeKey, bool) {
	key := yamlMadeKeys[m.random.IntN(len(yamlMadeKeys))]
	switch {
	case taken[key.same] && !m.twice:
		return key, false
	case (key.same == "seq" || key.same == "map") && !m.chance(5):
		// A key that is a sequence or a mapping fails the document, so few
		// have one.
		return key, false
	case len(m.scalars) > 0 && m.chance(5):
		// An alias as a key, of a scalar and now and then of a sequence or a
		// mapping, which is no key.
		name := m.scalars[m.random.IntN(len(m.scalars))]
		if m.chance(10) {
			name = m.anchors[m.random.IntN(len(m.anchors))]
		}
		key = yamlMadeKey{"*" + name + " ", "alias:" + name, false}
	case m.chance(3) && !key.merge && key.same != "seq" && key.same != "map":
		key.text = "&" + m.anchor(yaml.ScalarNode) + " " + key.text
	}
	taken[key.same] = true
	return key, true
}

// merged is a value for a merge key: an alias of a mapping, a mapping, a
// sequence of both, and what may not be merged, an alias of something else,
// a scalar and a sequence of scalars.
func (m *yamlMaker) merged() string {
	alias := func() string {
		if len(m.anchors) > 0 && m.chance(4) {
			return "*" + m.anchors[m.random.IntN(len(m.anchors))]
		}
		if len(m.maps) > 0 {
			return "*" + m.maps[m.random.IntN(len(m.maps))]
		}
		return m.mapping(1)
	}
	switch at := m.random.IntN(100); {
	case at < 45:
		return alias()
	case at < 65:
		return m.mapping(1)
	case at < 90:
		items := make([]string, 1+m.random.IntN(3))
		for i := range items {
			if m.chance(25) {
				items[i] = m.mapping(1)
			} else {
				items[i] = alias()
			}
		}
		return "[" + strings.Join(items, ", ") + "]"
	case at < 98:
		return m.mapping(1)
	}
	return []string{"5", "[1, 2]", "~"}[m.random.IntN(3)]
}

// mapping is a mapping in flow style.
func (m *yamlMaker) mapping(depth int) string {
	var pairs []string
	taken := map[string]bool{}
	for range m.random.IntN(6) {
		key, ok := m.key(taken)
		switch {
		case !ok:
		case key.merge:
			pairs = append(pairs, key.text+": "+m.merged())
		case m.chance(5):
			// A key without a value, which is null.
			pairs = append(pairs, key.text+":")
		default:
			pairs = append(pairs, key.text+": "+m.flow(depth))
		}
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// flow is a value on one line: a scalar, an alias, a sequence or a mapping
// in flow style, with an anchor on some and a tag on a few. An anchor is
// sometimes known to what it holds, which may then hold itself.
func (m *yamlMaker) flow(depth int) string {
	at := m.random.IntN(100)
	if depth == 0 && at >= 10 && at < 50 {
		at = 99
	}
	switch {
	case at < 10 && len(m.anchors) > 0:
		return "*" + m.anchors[m.random.IntN(len(m.anchors))]
	case at < 50 && depth > 0:
		kind := yaml.SequenceNode
		if at < 32 {
			kind = yaml.MappingNode
		}
		// An anchor is written before what it is of, and now and then known
		// to it, which may then hold itself.
		anchor, name := "", ""
		if m.chance(30) {
			name = m.name()
			if anchor = "&" + name + " "; m.chance(5) {
				m.known(name, kind)
				name = ""
			}
		}
		var made string
		switch {
		case kind == yaml.MappingNode:
			if made = m.mapping(depth - 1); m.chance(4) {
				made = "!!set " + made
			}
		case m.chance(4):
			made = "!!omap [a: 1, b: 2]"
		default:
			items := make([]string, m.random.IntN(4))
			for i := range items {
				items[i] = m.flow(depth - 1)
			}
			made = "[" + strings.Join(items, ", ") + "]"
		}
		if name != "" {
			m.known(name, kind)
		}
		return anchor + made
	case at < 51 && m.chance(40):
		return yamlMadeMistakes[m.random.IntN(len(yamlMadeMistakes))]
	}
	scalar := yamlMadeScalars[m.random.IntN(len(yamlMadeScalars))]
	if m.chance(8) {
		scalar = "&" + m.anchor(yaml.ScalarNode) + " " + scalar
	}
	return scalar
}

// after is what follows a key's colon or an item's dash in block style: a
// value on the line, a text of several lines, or a mapping or a sequence on
// the lines below, with an anchor on some.
func (m *yamlMaker) after(indent string, depth int, merge bool) string {
	at := m.random.IntN(100)
	switch {
	case merge:
		return " " + m.merged() + "\n"
	case at < 30 && depth > 0:
		kind := yaml.SequenceNode
		if at < 20 {
			kind = yaml.MappingNode
		}
		anchor, name := "", ""
		if m.chance(25) {
			name = m.name()
			if anchor = " &" + name; m.chance(5) {
				m.known(name, kind)
				name = ""
			}
		}
		var made string
		if kind == yaml.MappingNode {
			made = m.block(indent+"  ", depth-1)
		} else {
			made = m.list(indent+"  ", depth-1)
		}
		if name != "" {
			m.known(name, kind)
		}
		return anchor + "\n" + made
	case at < 34:
		return " |\n" + indent + "  first line\n" + indent + "  second line\n"
	case at < 37:
		return " >\n" + indent + "  folded\n" + indent + "  text\n"
	case at < 40:
		return "\n"
	}
	return " " + m.flow(depth) + "\n"
}

// block is a mapping in block style.
func (m *yamlMaker) block(indent string, depth int) string {
	var doc strings.Builder
	taken := map[string]bool{}
	for range 1 + m.random.IntN(6) {
		if key, ok := m.key(taken); ok || doc.Len() == 0 {
			doc.WriteString(indent)
			doc.WriteString(key.text)
			doc.WriteString(":")
			doc.WriteString(m.after(indent, depth, key.merge))
		}
	}
	return doc.String()
}

// list is a sequence in block style.
func (m *yamlMaker) list(indent string, depth int) string {
	var doc strings.Builder
	for range 1 + m.random.IntN(4) {
		doc.WriteString(indent)
		doc.WriteString("-")
		doc.WriteString(m.after(indent, depth, false))
	}
	return doc.String()
}

// document is one document; one in ten may write a key twice.
func (m *yamlMaker) document() string {
	m.anchors, m.maps, m.scalars, m.next = m.anchors[:0], m.maps[:0], m.scalars[:0], 0
	m.twice = m.chance(10)
	switch at := m.random.IntN(10); {
	case at < 1:
		return m.flow(3) + "\n"
	case at < 3:
		return m.list("", 2)
	}
	return m.block("", 2)
}

// yamlFixtureDocuments are the YAML files of the repository, whole and cut
// off after each of their first sixty lines and in the middle of it.
func yamlFixtureDocuments(t *testing.T) []string {
	t.Helper()
	var documents []string
	for _, pattern := range []string{"../../testdata/yaml/*", "../../testdata/chart/*.yaml", "../../testdata/chart/*/*.yaml", "../../examples/*.yaml", "../../examples/*/*.yaml",
		"../../configs/*.yaml", "../../charts/*/*.yaml", "../../charts/*/templates/*.yaml"} {
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
			for cut := 1; cut < len(lines) && cut <= 60; cut++ {
				whole := strings.Join(lines[:cut], "")
				documents = append(documents, whole, whole[:len(whole)-len(lines[cut-1])/2])
			}
		}
	}
	return documents
}

// yamlWrittenDocuments are documents written for what the generated ones
// may not come to: each form of a merge, of a key and of an alias beside
// the others.
func yamlWrittenDocuments() []string {
	return []string{
		"", "a: 1\n", "a: 1\n---\n", "a: 1\n---\nb: 2\n", "a: *nope\n", "a: &x {b: *x}\n", excessiveAliasing(), "updated: 2024-06-01\n",
		"base: &base {a: 1, b: 2}\nmore: &more {b: 3, c: 4}\nall:\n  <<: [*base, *more]\n  c: 5\n  d: 6\n",
		"base: &base {a: 1, b: {x: 1, y: 2}}\nall: {<<: *base, a: 2}\n",
		"base: &base {a: 1, <<: {z: 26, a: 3}}\nall: {<<: *base, b: 2}\n",
		"base: &base {a: 1, b: 2}\nall: {b: 0, <<: *base, c: 3}\n",
		"all: {a: 1, <<: {a: 2, b: 3, <<: {b: 4, c: 5}}}\n",
		"all: {a: 1, <<: [{a: 2, b: 3}, {b: 4, c: 5}]}\n",
		"all: {a: 1, <<: 5}\n", "all: {a: 1, <<: [1, 2]}\n", "all: {a: 1, <<: ~}\n", "s: &s [1, 2]\nall: {a: 1, <<: *s}\n", "s: &s text\nall: {a: 1, <<: [*s]}\n",
		"all: {a: 1, \"<<\": {b: 2}}\n", "all: {a: 1, !!merge <<: {b: 2}}\n", "all: {a: 1, !!str <<: {b: 2}}\n",
		"all: {a: 1, <<: {\"<<\": 5, b: 2}}\n", "all: {1: one, <<: {\"<<\": 5, b: 2}}\n",
		"all: {a: 1, <<: {~: 1, 5: 2, 1.5: 3, true: 4, b: 5}}\n", "all: {1: one, <<: {~: 1, 5: 2, 1.5: 3, true: 4, 1: 5, \"1\": 6}}\n",
		"all: {a: 1, <<: {[x]: 1}}\n", "all: {a: 1, <<: {{x: 1}: 1}}\n", "all: {1: one, <<: {[x]: 1}}\n", "? !!str [a]\n: 1\n<<: {b: 2}\n", "? !!str [a]\n: 1\nb: 2\n",
		"? !!str [a]\n: {b: 1, c: 2}\nd: 3\n", "? !!str {a: 1, b: 2}\n: {c: 1, d: 2}\ne: {f: 1, g: 2}\n", "s: &s !!str [a, b]\n? *s\n: {c: 1, d: 2}\ne: 3\n",
		"all: {a: 1, <<: {a: !!int foo, b: 2}}\n", "all: {a: 1, <<: {a: {k: 1, k: 2}, b: 2}}\n", "all: {a: 1, <<: {c: {k: 1, k: 2}, b: 2}}\n",
		"all: {a: 1, <<: [{b: 1}, 5]}\n", "m: &m {a: 1, b: 2}\nall: {<<: [*m, *m], c: 3}\n",
		// A merge into a mapping with a key that is a sequence or a mapping:
		// the library reads the key once more, fails on what is in it, or
		// fails outright, and the walk refuses the document for what it meets.
		"{? !!str {k: {? !!null [a] : x}} : x, <<: x}\n", "{? !!str [a] : x, <<: x}\n", "{? !!str [!!int foo] : x, <<: {b: 1}}\n", "a: &a {? !!str [*a] : x, <<: {b: 1}}\n",
		"{? !!str [a] : x, <<: {b: !!int foo}}\n", "{? !!str [a] : x, <<: {b: 1}}\n", "{1: x, <<: [{b: 1}, {[a]: 1}, 5]}\n", "m: &m {[a]: 1}\nall: {1: x, <<: *m}\n",
		"s: &s !!str [!!int foo]\nall: {? *s : x, <<: {b: 1}}\n",
		"big: &big {a: 1, b: 2, c: 3, d: 4, e: 5, f: 6}\nall: {b: 0, <<: *big, g: 7}\nlist: [*big, *big]\n",
		"1: a\n0x1: b\n01: c\n", "1: a\n\"1\": b\n1.0: c\n", "~: 1\nnull: 2\n", ".nan: 1\n.NaN: 2\nx: .nan\n", "? [a, b]\n: 1\nc: 2\n", "? {a: 1, b: 2}\n: 1\nc: 2\n",
		"s: &s text\n*s : 1\nb: 2\n", "s: &s [1, 2]\n*s : 1\nb: 2\n", "s: &s {a: 1, b: 2}\n? *s\n: 1\nb: 2\n",
		"a: &a [1, *a]\nb: 2\n", "a: &a {b: 1, c: *a}\n", "a: &a {b: 1, c: [*a]}\n", "a: &a {b: 1, <<: *a}\n", "a: &a {b: 1, ? *a : 2}\n", "a: &a [{b: 1, c: 2}, [*a]]\n",
		"? {a: 1, a: 2}\n: {b: 1, b: 2}\nc: {d: 1, d: 2}\n", "m: &m {a: 1, a: 2}\nl: [*m, *m]\n", "a: {b: 1, b: 2, c: {d: 1, d: 2}}\ne: {f: 1, f: 2}\n",
		"!!set {a, b, c}\n", "!!omap [a: 1, b: 2]\n", "!!null {a: 1, b: 2}\n", "!!str [a, b]\n", "!!int {a: 1, b: 2}\n", "a: !!map {b: 1, c: 2}\nd: !!seq [1, 2]\n",
		"a: {}\nb: []\nc: {d: {}, e: []}\n", "- {a: 1, b: 2}\n- [c, d]\n- e\n-\n- ~\n", "{a: 1, b: 2}\n", "[{a: 1, b: 2}, {c: 3, d: 4}]\n", "text\n", "5\n", "~\n",
		"a: |\n  one\n  two\nb: >\n  three\n  four\nc: \"five\n  six\"\nd: 7\n",
	}
}

// yamlMergesBesideAKeyThatIsNoKey reports whether a mapping under n, or
// under what an alias there stands for, has a merge key and a key that is a
// sequence or a mapping, of its own or of a mapping merged into it: the
// documents the library fails outright on, or refuses for what is in that
// key, and that the walk may refuse with another error.
func yamlMergesBesideAKeyThatIsNoKey(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	if n == nil || seen[n] {
		return false
	}
	seen[n] = true
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if yamlMerges(n.Content[i]) && yamlHasAKeyThatIsNoKey(n, map[*yaml.Node]bool{}) {
				return true
			}
		}
	}
	if yamlMergesBesideAKeyThatIsNoKey(n.Alias, seen) {
		return true
	}
	for _, child := range n.Content {
		if yamlMergesBesideAKeyThatIsNoKey(child, seen) {
			return true
		}
	}
	return false
}

// yamlHasAKeyThatIsNoKey reports whether mapping n, or a mapping merged
// into it, alone or in a sequence, has a key that is a sequence or a
// mapping, or an alias of one.
func yamlHasAKeyThatIsNoKey(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if seen[n] {
		return false
	}
	seen[n] = true
	if n.Kind == yaml.SequenceNode {
		for _, merged := range n.Content {
			if yamlHasAKeyThatIsNoKey(merged, seen) {
				return true
			}
		}
		return false
	}
	if n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if yamlMerges(key) {
			if yamlHasAKeyThatIsNoKey(n.Content[i+1], seen) {
				return true
			}
			continue
		}
		if key.Kind == yaml.AliasNode && key.Alias != nil {
			key = key.Alias
		}
		if key.Kind == yaml.SequenceNode || key.Kind == yaml.MappingNode {
			return true
		}
	}
	return false
}

// yamlHandsOfADocument is more parts than the library is handed of any
// document of these tests: those whose aliases expand to too much are
// refused well before.
const yamlHandsOfADocument = 10000

// yamlOtherError is a way the error for keys written twice in a large
// mapping may differ from the library's, or none.
func yamlOtherError(root *yaml.Node, library yamlOutcome) string {
	switch _, aliasOrMerge := yamlNodes(root); {
	case library.failed != nil:
		return "the library fails outright"
	case library.err != nil && !onlyKeysWrittenTwice(library.err):
		return "the library met another error"
	case aliasOrMerge:
		return "an alias or a merge"
	}
	return ""
}

// A document is decoded into what the YAML library decodes it into, and
// refused in the library's words, whatever counts as a large mapping: with
// one past no key, past one, two and four keys, which makes every document
// with a mapping one the library is not given whole, and past 128, which is
// what decodeYAML counts and which one drawn document in five is decoded
// with once more, beside a mapping of 129 keys. Compared are the values
// with their types, the maps' among them, and the errors' texts, what they
// are recognised by and whether they are a list of problems; over the YAML
// files of the repository, whole and cut off after each of their first
// sixty lines and in the middle of it, the documents the tests of the YAML
// errors are written of, some eighty written for the forms of a merge, a
// key and an alias, and 30,000 drawn at random of nested mappings and
// sequences in flow and in block style, anchors and aliases of scalars,
// sequences and mappings, some holding themselves, merges of an alias, a
// mapping, a sequence of both, a scalar and a sequence of scalars, keys a
// merge replaces and keys it leaves, merges within merged mappings, the
// merge key tagged and a quoted "<<", tags that fit and that do not, a
// document's own tag, sets and ordered maps, null in every spelling as key
// and as value, keys that are numbers, booleans, null, dates, aliases,
// sequences and mappings, empty mappings and sequences, timestamps and
// texts of several lines.
//
// A document without a large mapping, and with few keys written twice if
// any, is the library's to decode, and is what it was. One with a large
// mapping and no key written twice is decoded a part at a time, into the
// same, and the library is handed no mapping of more keys than counts as
// large, nor more than 10,000 parts; the documents the library fails
// outright on, a merge into a mapping one of whose keys, or of a merged
// mapping's, is a sequence or a mapping, are refused where the walk decodes
// that mapping, and so are those the library refuses for what is in such a
// key, with an error that may be another than the library's. One with keys
// written twice and a large mapping is refused without the library: with
// the library's error
// where that is of keys written twice alone and the document has no alias
// and no merge, and with an error of its keys written twice otherwise.
func TestAYAMLDocumentIsDecodedIntoWhatTheLibraryMakesOfItWhateverIsALargeMapping(t *testing.T) {
	documents := append(append(yamlFixtureDocuments(t), hostileYAMLDocuments()...), yamlWrittenDocuments()...)
	written := len(documents)
	maker := yamlMaker{random: rand.New(rand.NewPCG(16, 17))} //nolint:gosec // documents for a test
	generated := 30000
	if raceDetector {
		generated = 3000
	}
	for range generated {
		documents = append(documents, maker.document())
	}
	counted := map[string]int{}
	large := "{" + manyYAMLPairs(129, "", ", ") + "}"
	// check holds a document, decoded with a mapping large past so many
	// keys, against what the library alone makes of it.
	mostHands := 0
	check := func(document string, root *yaml.Node, library yamlOutcome, large int) {
		// The library is handed no mapping of more keys than that, but for
		// a pair alone; and no more parts than a document whose aliases are
		// refused has before it is, where one whose aliases are followed
		// without end would be handed parts until the memory is gone.
		hands := 0
		got := yamlWith(root, large, func(handed *yaml.Node) {
			if pairs := largestYAMLMapping(handed, map[*yaml.Node]bool{}); pairs > max(large, 1) {
				t.Fatalf("%q, large past %d keys: the library is handed a mapping of %d", document, large, pairs)
			}
			if hands++; hands > yamlHandsOfADocument {
				t.Fatalf("%q, large past %d keys: the library is handed more than %d parts", document, large, yamlHandsOfADocument)
			}
		})
		mostHands = max(mostHands, hands)
		switch way := yamlWayOf(root, large); {
		case way == yamlByLibrary:
			counted["by the library"]++
			if !got.same(library) {
				t.Fatalf("%q, large past %d keys and the library's to decode, is\n%s\nand by the library alone\n%s", document, large, got.text(), library.text())
			}
		case way == yamlByParts && library.failed != nil:
			// In a part handed to the library it fails as it did.
			if got.same(library) {
				counted["in parts, failing outright as the library does"]++
				return
			}
			// The document is refused, or the library fails outright on
			// another key of a part; with which error is not promised.
			counted["in parts, where the library fails outright"]++
			if got.err == nil && got.failed == nil || !yamlMergesBesideAKeyThatIsNoKey(root, map[*yaml.Node]bool{}) {
				t.Fatalf("%q, large past %d keys, is\n%s\nand the library fails outright with %v", document, large, got.text(), library.failed)
			}
		case way == yamlByParts && library.err != nil && (got.err != nil || got.failed != nil) && !got.same(library) && yamlMergesBesideAKeyThatIsNoKey(root, map[*yaml.Node]bool{}):
			// The library read such a key once more for the merge and refused
			// the document for what is in it; the walk refuses it too, for what
			// it meets first.
			counted["in parts, refused with another error than the library's"]++
		case way == yamlByParts:
			counted["in parts"]++
			if got.err != nil {
				counted["in parts and refused"]++
			}
			if !got.same(library) {
				t.Fatalf("%q, large past %d keys, is in parts\n%s\nand by the library alone\n%s", document, large, got.text(), library.text())
			}
		case got.err == nil || !onlyKeysWrittenTwice(got.err) || got.failed != nil:
			t.Fatalf("%q, large past %d keys, has keys written twice and is\n%s", document, large, got.text())
		case got.same(library):
			counted["refused as the library does"]++
		default:
			other := yamlOtherError(root, library)
			if other == "" {
				t.Fatalf("%q, large past %d keys, is refused with\n%s\nand by the library alone\n%s", document, large, got.text(), library.text())
			}
			counted["refused otherwise: "+other]++
		}
	}
	for i, document := range documents {
		root, err := yamlDocumentOf([]byte(document))
		if root == nil {
			// Refused before anything is decoded, or empty.
			got, gotErr := decodeYAML([]byte(document))
			if got != nil || (err == nil) != (gotErr == nil) || err != nil && (err.Error() != gotErr.Error() || model.SameFailureText(err) != model.SameFailureText(gotErr)) {
				t.Fatalf("%q decodes into %v, %v; parsed alone it is refused with %v", document, got, gotErr, err)
			}
			counted["not parsed"]++
			continue
		}
		library := yamlByTheLibraryAlone(root)
		if i >= written {
			counted["drawn: "+strings.SplitN(library.text(), " ", 2)[0]]++
		}
		for _, large := range []int{0, 1, 2, 4, yamlLargeMapping} {
			check(document, root, library, large)
		}
		if yamlWayOf(root, yamlLargeMapping) != yamlByLibrary {
			t.Fatalf("%q is not the library's to decode", document)
		}
		// One document in five beside a mapping of 129 keys, which makes it
		// one that is decoded in parts as decodeYAML counts.
		beside := ""
		switch top := root.Content[0]; {
		case i < written || i%5 != 0 || top.Style&yaml.FlowStyle != 0:
		case top.Kind == yaml.MappingNode:
			beside = document + "beside: " + large + "\n"
		case top.Kind == yaml.SequenceNode:
			beside = document + "- " + large + "\n"
		}
		if root, _ := yamlDocumentOf([]byte(beside)); root != nil {
			counted["beside a mapping of 129 keys"]++
			if yamlWayOf(root, yamlLargeMapping) == yamlByLibrary {
				t.Fatalf("%q is the library's to decode", beside)
			}
			check(beside, root, yamlByTheLibraryAlone(root), yamlLargeMapping)
		}
	}
	t.Logf("%d documents, of at most %d parts: %v", len(documents), mostHands, counted)
	for outcome, least := range map[string]int{
		"not parsed": 100, "by the library": 30000, "beside a mapping of 129 keys": 5000, "in parts": 50000, "in parts and refused": 5000,
		"in parts, where the library fails outright": 10, "in parts, refused with another error than the library's": 4,
		"refused as the library does": 3000, "refused otherwise: the library met another error": 100, "refused otherwise: an alias or a merge": 100,
		"drawn: error": 3000, "drawn: map[string]interface": 5000, "drawn: map[interface": 2000, "drawn: []interface": 2000,
	} {
		if counted[outcome] < least && !raceDetector {
			t.Errorf("%d documents were %s, fewer than %d: the documents do not cover it", counted[outcome], outcome, least)
		}
	}
}

// manyYAMLKeys is a mapping of so many different keys in block style, each
// `key<i>: <i>`.
func manyYAMLKeys(count int) string {
	return manyYAMLPairs(count, "", "\n")
}

// manyYAMLPairs is so many pairs of different keys, `key<i>: value`, or
// `key<i>: <i>` without a value, each ended as said.
func manyYAMLPairs(count int, value, end string) string {
	var doc strings.Builder
	for i := range count {
		if value == "" {
			fmt.Fprintf(&doc, "key%d: %d%s", i, i, end)
		} else {
			fmt.Fprintf(&doc, "key%d: %s%s", i, value, end)
		}
	}
	return doc.String()
}

// parsedYAML is a body parsed as decodeYAML parses it, which the test fails
// without.
func parsedYAML(t *testing.T, body string) *yaml.Node {
	t.Helper()
	root, err := yamlDocumentOf([]byte(body))
	if root == nil {
		t.Fatalf("%.200q is not parsed: %v", body, err)
	}
	return root
}

// A document is the library's to decode unless it has a mapping of more
// than 128 keys or keys written twice whose problems come to more than 64
// kB of text, counted as 64 bytes and the key for each: a mapping of 128
// keys, nested mappings of 128 keys each, a key of one byte and one of two
// written 45 times (990 problems), one of three bytes written 44 times and a
// key of 30 kB written twice are decoded, and refused, by the library as
// they were; a mapping of 129 keys is decoded in parts; a key of one byte
// written 46 times, one of three bytes written 45 times, a key of 30 kB
// written three times, a key written twice in a mapping of 129 keys, and a
// mapping of ten keys written twice that 30 aliases stand for, whose
// problems the library would list 31 times, are refused without it.
func TestAYAMLDocumentIsTheLibrarysToDecodeUntilAMappingIsLargeOrItsProblemsMany(t *testing.T) {
	long := strings.Repeat("k", 30000)
	aliased := "m: &m {" + strings.Repeat("a: 1, a: 2, ", 5) + "}\nl: [" + strings.Repeat("*m, ", 30) + "]\n"
	for name, tc := range map[string]struct {
		body string
		way  int
	}{
		"a mapping of 128 keys":                   {manyYAMLKeys(128), yamlByLibrary},
		"a mapping of 129 keys":                   {manyYAMLKeys(129), yamlByParts},
		"a mapping of 129 keys in a list":         {"- 1\n- {" + manyYAMLPairs(129, "", ", ") + "}\n", yamlByParts},
		"mappings of 128 keys in one of 128":      {manyYAMLPairs(128, "{"+manyYAMLPairs(128, "", ", ")+"}", "\n"), yamlByLibrary},
		"a key written twice":                     {"a: 1\na: 2\n", yamlByLibrary},
		"a key written 45 times":                  {strings.Repeat("a: 1\n", 45), yamlByLibrary},
		"a key written 46 times":                  {strings.Repeat("a: 1\n", 46), yamlRefused},
		"a key of two bytes written 45 times":     {strings.Repeat("ab: 1\n", 45), yamlByLibrary},
		"a key of three bytes written 44 times":   {strings.Repeat("abc: 1\n", 44), yamlByLibrary},
		"a key of three bytes written 45 times":   {strings.Repeat("abc: 1\n", 45), yamlRefused},
		"a long key written twice":                {strings.Repeat("? "+long+"\n: 1\n", 2), yamlByLibrary},
		"a long key written three times":          {strings.Repeat("? "+long+"\n: 1\n", 3), yamlRefused},
		"a key written twice among 129":           {manyYAMLKeys(128) + "key5: 0\n", yamlRefused},
		"a key written twice beside 129":          {"a: {b: 1, b: 2}\nc:\n  " + strings.ReplaceAll(manyYAMLKeys(129), "\n", "\n  "), yamlRefused},
		"a key written twice, and a list":         {"a: {b: 1, b: 2}\nc: [" + strings.Repeat("{d: 1}, ", 500) + "]\n", yamlByLibrary},
		"a mapping written twice that 30 aliases": {aliased, yamlRefused},
		"a key written twice around 129":          {"a: {x: &big {" + manyYAMLPairs(129, "", ", ") + "}, x: 1}\nb: *big\n", yamlRefused},
	} {
		root := parsedYAML(t, tc.body)
		if way := yamlWayOf(root, yamlLargeMapping); way != tc.way {
			t.Errorf("%s: the document is decoded the way %d, want %d", name, way, tc.way)
			continue
		}
		library, got := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, nil)
		whole, err := decodeYAML([]byte(tc.body))
		if now := (yamlOutcome{value: whole, err: err}); !now.same(got) {
			t.Errorf("%s: decodeYAML gives\n%.300s\nand the document decoded\n%.300s", name, now.text(), got.text())
		}
		if strings.Contains(name, "alias") {
			// The library lists the mapping's problems again at each alias.
			if got.err == nil || !onlyKeysWrittenTwice(got.err) || got.same(library) {
				t.Errorf("%s: the document is refused with\n%.300s\nand by the library with\n%.300s", name, got.text(), library.text())
			}
			continue
		}
		if !got.same(library) {
			t.Errorf("%s: the document is\n%.300s\nand by the library alone\n%.300s", name, got.text(), library.text())
		}
	}
	// The ten keys written twice of a mapping 30 aliases stand for are 45
	// problems, which the library lists 31 times; the error is of the
	// mapping as it is written.
	_, err := decodeYAML([]byte(aliased))
	if want := "\n  line 1: mapping key \"a\" already defined at line 1"; err == nil || err.Error() != "yaml: unmarshal errors:"+strings.Repeat(want, 10)+"\n  ... and 35 more problems" {
		t.Errorf("a mapping written twice that 30 aliases stand for is refused with\n%v", err)
	}
}

// Once a document is parsed, refusing it for a key written many times
// allocates the same few kilobytes whether the key is written 1200 times or
// 100,000: the error, a table of the mapping's different keys, and no text
// for each two of them. The library is handed nothing.
func TestRefusingAYAMLKeyWrittenManyTimesAllocatesNoMoreForMoreOfThem(t *testing.T) {
	for _, count := range []int{1200, 100000} {
		root := parsedYAML(t, strings.Repeat("a: 1\n", count))
		var got yamlOutcome
		allocated := allocatedBy(func() {
			got = yamlWith(root, yamlLargeMapping, func(*yaml.Node) { t.Error("the document is handed to the library") })
		})
		allocations := testing.AllocsPerRun(3, func() { yamlWith(root, yamlLargeMapping, nil) })
		if got.err == nil || !strings.HasSuffix(got.err.Error(), fmt.Sprintf("\n  ... and %d more problems", count*(count-1)/2-10)) {
			t.Fatalf("a key written %d times is refused with %.1000v", count, got.err)
		}
		if !raceDetector && (allocated > 16<<10 || allocations > 100) {
			t.Errorf("refusing a key written %d times allocated %d bytes in %.0f allocations, want at most 16 kB in 100", count, allocated, allocations)
		}
	}
}

// keysWrittenTwice is a mapping of count pairs in block style whose keys
// are drawn from the first so many of names, so that most are written
// several times.
func keysWrittenTwice(random *rand.Rand, names []string, count int) string {
	var doc strings.Builder
	for i := range count {
		fmt.Fprintf(&doc, "%s: %d\n", names[random.IntN(len(names))], i)
	}
	return doc.String()
}

// Keys written twice in a document the library is not given are refused
// with the error the library made of them, to the letter, and recognised
// by the same text: one key written 46 to 60, 100, 200 and 400 times; 200
// mappings of 129 to 400 pairs whose keys are drawn from one to fifteen
// names, some of them names that hold the library's own words, a quote, a
// line break and the mark, some keys that are sequences and mappings, with
// different keys among them, so that the first ten problems, their count
// and the ten different problems the failure is recognised by are those of
// one key, of a few and of more than ten; the same mappings under a key,
// in a list of several, and after mappings with a key written twice of
// their own; a key of 30 kB written three and four times; and a list of
// 3000 small mappings that each write a key twice. The library's error is
// the oracle, for documents it can still decode.
func TestKeysWrittenTwiceInALargeYAMLMappingAreRefusedAsTheLibraryRefusedThem(t *testing.T) {
	names := []string{"a", "b", "c", `"x already defined at line 7"`, `"line 5: x"`, `"a\n  line 9: boo"`, `"#"`, "3", "'it''s'", "[s]", "{m: 1}", "~", "true", "2024-06-01", "!!str 5", `"quoted \"q\""`, "d", "e"}
	var documents []string
	for count := 46; count <= 60; count++ {
		documents = append(documents, strings.Repeat("a: 1\n", count))
	}
	documents = append(documents, strings.Repeat("a: 1\n", 100), strings.Repeat("a: 1\n", 200), strings.Repeat("a: 1\n", 400))
	random := rand.New(rand.NewPCG(46, 60)) //nolint:gosec // documents for a test
	mappings := 200
	if raceDetector {
		mappings = 40
	}
	for i := range mappings {
		kinds := 1 + random.IntN(15)
		from := random.IntN(len(names) - kinds + 1)
		mapping := keysWrittenTwice(random, names[from:from+kinds], 129+random.IntN(272))
		if i%3 == 0 {
			// Different keys among them.
			lines := strings.SplitAfter(mapping, "\n")
			for at := range lines {
				if random.IntN(3) == 0 && lines[at] != "" {
					lines[at] = fmt.Sprintf("only%d: 0\n", at)
				}
			}
			mapping = strings.Join(lines, "")
		}
		indented := "  " + strings.ReplaceAll(strings.TrimSuffix(mapping, "\n"), "\n", "\n  ") + "\n"
		switch i % 4 {
		case 0:
			documents = append(documents, mapping)
		case 1:
			documents = append(documents, "under:\n"+indented+"after: {a: 1, a: 2}\n")
		case 2:
			documents = append(documents, "- {b: 1, b: 2, b: 3}\n-\n"+indented+"- {c: 1, c: 2}\n-\n"+indented)
		case 3:
			documents = append(documents, "first: {a: 1, z: 2, a: 3}\nsecond: [{y: 1, y: 2}, 5]\nthird:\n"+indented)
		}
	}
	long := strings.Repeat("k", 30000)
	documents = append(documents, strings.Repeat("? "+long+"\n: 1\n", 3), strings.Repeat("? "+long+"\n: 1\n", 4), itemsWritingTwice(strings.Repeat("ab", 1500)))
	recognised := map[int]int{}
	for _, document := range documents {
		root := parsedYAML(t, document)
		if way := yamlWayOf(root, yamlLargeMapping); way != yamlRefused {
			t.Fatalf("%.200q is decoded the way %d", document, way)
		}
		library, got := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, func(*yaml.Node) { t.Fatalf("%.200q is handed to the library", document) })
		if library.err == nil || !got.same(library) {
			t.Fatalf("%.300q is refused with\n%.1500s\nand by the library with\n%.1500s", document, got.text(), library.text())
		}
		recognised[strings.Count(model.SameFailureText(got.err), "\n")]++
	}
	// One problem, a few, ten, and ten with the mark for more.
	if !raceDetector && (recognised[1] < 15 || recognised[10] < 10 || recognised[11] < 50 || len(recognised) < 11) {
		t.Errorf("the failures are recognised by so many lines: %v; they do not cover one, ten and more", recognised)
	}
}

// The problems of a mapping the library does not read are not counted
// among those the error says there are more of: the library reads no
// further into a mapping with a key written twice, and not the value of a
// key that is such a mapping. Beside a mapping of 129 keys, which makes the
// document one that is refused without the library: a key written six
// times in a mapping that holds one with a key written five times is 15
// problems, not 25; a third mapping within, with a key written four times,
// adds none, and a mapping after them, which is read, adds its own; a key
// that is a mapping with a key written six times, whose value writes one
// five times, is 15; and all of them in one document are 46. The error is
// the library's to the letter.
func TestTheProblemsOfAYAMLMappingTheLibraryDoesNotReadAreNotCounted(t *testing.T) {
	six, five, four := strings.Repeat("b: 1, ", 6), strings.Repeat("d: 1, ", 5), strings.Repeat("f: 1, ", 4)
	beside := "beside: {" + manyYAMLPairs(129, "", ", ") + "}\n"
	within := "a: {" + six + "c: {" + five + "}}\n"
	twiceWithin := "a: {" + six + "c: {" + five + "e: {" + four + "}}}\ng: {h: 1, h: 2}\n"
	asKey := "? {" + six + "}\n: {" + five + "}\n"
	for name, tc := range map[string]struct {
		body string
		more int
	}{
		"a mapping within one written twice":       {within + beside, 5},
		"after the large mapping":                  {beside + within, 5},
		"two within, and one after":                {twiceWithin + beside, 6},
		"the value of a key that is written twice": {asKey + beside, 5},
		"all of them":                              {"first:\n  " + strings.ReplaceAll(twiceWithin, "\n", "\n  ") + "\n" + asKey + "last: [{" + six + "c: {" + five + "}}]\n" + beside, 36},
	} {
		root := parsedYAML(t, tc.body)
		if way := yamlWayOf(root, yamlLargeMapping); way != yamlRefused {
			t.Errorf("%s: the document is decoded the way %d", name, way)
			continue
		}
		library, got := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, func(*yaml.Node) { t.Errorf("%s: the document is handed to the library", name) })
		if library.err == nil || !got.same(library) {
			t.Errorf("%s: the document is refused with\n%.1500s\nand by the library with\n%.1500s", name, got.text(), library.text())
		}
		if want := fmt.Sprintf("\n  ... and %d more problems", tc.more); got.err == nil || !strings.HasSuffix(got.err.Error(), want) {
			t.Errorf("%s: the document is refused with\n%v\nwant it to end with %q", name, got.err, want)
		}
	}
}

// A document with a large mapping is decoded in time linear in it: the
// library, which compares every two keys of a mapping it is given, is
// handed the 20,000 keys of a mapping 128 at a time, 157 times, and never a
// mapping of more, whatever the values are — numbers, small mappings, lists,
// aliases of a mapping or mappings of 300 keys themselves — and looking
// through the document for keys written twice compares no two keys of a
// mapping of more than twenty-four. The document decodes into what the library
// makes of it, which for 20,000 keys is checked by its size and its values,
// since the library alone needs seconds for it.
func TestALargeYAMLMappingIsHandedToTheLibraryAPartAtATime(t *testing.T) {
	keys := 20000
	if raceDetector {
		keys = 4000
	}
	inner := "{" + manyYAMLPairs(300, "", ", ") + "}"
	for name, tc := range map[string]struct {
		body  string
		hands int
		// last is the value of the last key, where the document is too large
		// for the library alone.
		last any
	}{
		"numbers":            {manyYAMLKeys(keys), keys/yamlLargeMapping + 1, keys - 1},
		"small mappings":     {manyYAMLPairs(keys, "{a: 1, b: [2, 3]}", "\n"), keys/yamlLargeMapping + 1, map[string]any{"a": 1, "b": []any{2, 3}}},
		"aliases":            {"one: &one {a: 1, b: 2}\n" + manyYAMLPairs(keys, "*one", "\n"), (keys+1)/yamlLargeMapping + 1, map[string]any{"a": 1, "b": 2}},
		"mappings of 300":    {manyYAMLPairs(200, inner, "\n"), 200 * (1 + 300/yamlLargeMapping + 1), nil},
		"a list of mappings": {"- " + inner + "\n- [" + inner + ", 5]\n- {a: " + inner + "}\n", 3*(300/yamlLargeMapping+1) + 2, nil},
	} {
		root := parsedYAML(t, tc.body)
		learnt := yamlLearnt{large: yamlLargeMapping}
		learnt.learn(root, true)
		nodes, _ := yamlNodes(root)
		if way := learnt.way(root); way != yamlByParts || learnt.compared > nodes {
			t.Errorf("%s: the document is decoded the way %d, and %d pairs of keys were compared in looking through its %d nodes", name, way, learnt.compared, nodes)
			continue
		}
		hands, largest := 0, 0
		got := yamlWith(root, yamlLargeMapping, func(handed *yaml.Node) {
			hands++
			largest = max(largest, largestYAMLMapping(handed, map[*yaml.Node]bool{}))
		})
		if got.err != nil || got.failed != nil || hands > tc.hands || largest > yamlLargeMapping {
			t.Errorf("%s: the library was handed %d parts, the largest a mapping of %d keys, want at most %d parts: %v, %v", name, hands, largest, tc.hands, got.err, got.failed)
			continue
		}
		if tc.last == nil {
			if library := yamlByTheLibraryAlone(root); !got.same(library) {
				t.Errorf("%s: the document decodes into another value than the library's", name)
			}
			continue
		}
		if whole, _ := got.value.(map[string]any); len(whole) < keys || !reflect.DeepEqual(whole[fmt.Sprintf("key%d", keys-1)], tc.last) || !reflect.DeepEqual(whole["key127"], whole["key128"]) && whole["key128"] != 128 {
			t.Errorf("%s: the document decodes into %d keys, the last %#v", name, len(whole), whole[fmt.Sprintf("key%d", keys-1)])
		}
	}
}

// Looking through a document allocates nothing where its mappings have
// twenty-four keys or fewer, whose keys are compared two by two as the library
// compares them, and decoding such a document allocates what the library
// alone does; with mappings of more keys, which a table of the keys is made
// for, it allocates one table for the document, however many mappings there
// are. A thousand items of six keys and a nested mapping are looked through
// with fewer comparisons than the document has nodes.
func TestLookingThroughAYAMLDocumentCostsOneWalkOverIt(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	small := typicalYAML(1000)
	root := parsedYAML(t, small)
	var learnt yamlLearnt
	if allocations := testing.AllocsPerRun(10, func() {
		learnt = yamlLearnt{large: yamlLargeMapping}
		learnt.learn(root, true)
		if learnt.way(root) != yamlByLibrary {
			t.Fatal("the document is not the library's to decode")
		}
	}); allocations != 0 {
		t.Errorf("looking through a document of small mappings allocates %.0f times, want none", allocations)
	}
	if nodes, _ := yamlNodes(root); learnt.compared == 0 || learnt.compared > 2*nodes {
		t.Errorf("%d pairs of keys were compared in a document of %d nodes", learnt.compared, nodes)
	}
	body := []byte(small)
	alone := testing.AllocsPerRun(5, func() {
		root, _ := yamlDocumentOf(body)
		if outcome := yamlByTheLibraryAlone(root); outcome.err != nil {
			t.Fatal(outcome.err)
		}
	})
	now := testing.AllocsPerRun(5, func() {
		if _, err := decodeYAML(body); err != nil {
			t.Fatal(err)
		}
	})
	if now > alone {
		t.Errorf("decoding a document of small mappings allocates %.0f times, and the library alone %.0f", now, alone)
	}
	// Mappings of 40 keys: a table of the keys, made once.
	wide := strings.Repeat("- {"+manyYAMLPairs(40, "", ", ")+"}\n", 500)
	root = parsedYAML(t, wide)
	if allocations := testing.AllocsPerRun(10, func() {
		learnt = yamlLearnt{large: yamlLargeMapping}
		learnt.learn(root, true)
	}); allocations > 20 || learnt.compared != 0 {
		t.Errorf("looking through 500 mappings of 40 keys allocates %.0f times and compares %d pairs of keys, want one table for them all", allocations, learnt.compared)
	}
}

// typicalYAML is a document as an API answers it: a list of so many items
// of six keys and a nested mapping, under a key.
func typicalYAML(items int) string {
	var doc strings.Builder
	doc.WriteString("status: ok\ncount: 3\nitems:\n")
	for i := range items {
		fmt.Fprintf(&doc, "  - id: item-%d\n    region: eu-%d\n    kind: k%d\n    value: %d\n    size: %d\n    extra:\n      a: 1\n      b: two\n      c: [1, 2, 3]\n", i, i%7, i%3, i, i*1024)
	}
	return doc.String()
}

// The library refuses a document whose aliases expand to too much, and an
// anchor that holds itself; a document with a large mapping is refused for
// the same, in the same words, and one the library decodes is decoded:
//
//   - the document of the tests of the YAML errors, nine levels of nine
//     aliases each, beside a mapping of 200 keys;
//   - a sequence of a thousand numbers that 120 to 160 aliases stand for,
//     which the library refuses from 139 aliases on, beside a mapping of 129
//     keys: refused from the same number on;
//   - the same sequence with 200 aliases, and one of 150 numbers with 400,
//     which the library refuses alone and decodes beside a mapping of 3000
//     keys, since the aliases are then a smaller share of the document:
//     decoded, so neither an alias nor the sequence of them is handed to the
//     library to judge apart from the rest;
//   - an anchor that holds itself through a large mapping, in a sequence
//     beside one, after one and as a merge of one, and never a mapping of
//     more than 128 keys handed to the library on the way.
//
// The library is handed no more than 10,000 parts of any of them: the nine
// levels are refused after 55, and followed they would be 387 million nodes.
func TestAliasesBesideALargeYAMLMappingAreRefusedAsTheLibraryRefusesThem(t *testing.T) {
	thousand := "d: &d [" + strings.Repeat("1, ", 1000) + "]\n"
	aliases := func(count int) string { return "l: [" + strings.Repeat("*d, ", count) + "]\n" }
	large := "{" + manyYAMLPairs(200, "", ", ")
	documents := map[string]string{
		"nine levels of nine aliases":       "big:\n  " + strings.ReplaceAll(manyYAMLKeys(200), "\n", "\n  ") + "\n" + excessiveAliasing(),
		"200 aliases beside 3000 keys":      "big: {" + manyYAMLPairs(3000, "", ", ") + "}\n" + thousand + aliases(200),
		"400 aliases of 150 beside 3000":    "big: {" + manyYAMLPairs(3000, "", ", ") + "}\nd: &d [" + strings.Repeat("1, ", 150) + "]\n" + aliases(400),
		"itself in a large mapping":         "a: &a " + large + "self: *a}\n",
		"itself in a list in one":           "a: &a " + large + "self: [1, *a]}\n",
		"itself as a merge of one":          "a: &a " + large + "<<: *a}\n",
		"itself as a key of one":            "a: &a " + large + "? *a : 1}\n",
		"itself beside a large mapping":     "a: &a [[*a], " + large + "}]\n",
		"itself after a large mapping":      "a: &a [" + large + "}, [*a]]\n",
		"itself in a mapping after one":     "a: &a [" + large + "}, {b: {c: *a}}]\n",
		"itself, and a large mapping apart": "big: " + large + "}\na: &a [1, [*a]]\n",
	}
	step := 4
	if raceDetector {
		step = 20
	}
	for count := 120; count <= 160; count += step {
		documents[fmt.Sprintf("%d aliases beside 129 keys", count)] = manyYAMLKeys(129) + thousand + aliases(count)
	}
	refused, decoded := 0, 0
	for name, document := range documents {
		root := parsedYAML(t, document)
		if way := yamlWayOf(root, yamlLargeMapping); way != yamlByParts {
			t.Errorf("%s: the document is decoded the way %d", name, way)
			continue
		}
		library := yamlByTheLibraryAlone(root)
		hands := 0
		got := yamlWith(root, yamlLargeMapping, func(handed *yaml.Node) {
			if pairs := largestYAMLMapping(handed, map[*yaml.Node]bool{}); pairs > yamlLargeMapping {
				t.Errorf("%s: the library is handed a mapping of %d keys", name, pairs)
			}
			// Aliases that are not refused are followed until the memory is
			// gone: nine levels of nine are 387 million nodes.
			if hands++; hands > yamlHandsOfADocument {
				t.Fatalf("%s: the library is handed more than %d parts: the aliases are not refused", name, yamlHandsOfADocument)
			}
		})
		if !got.same(library) {
			t.Errorf("%s: the document is\n%.300s\nand by the library alone\n%.300s", name, got.text(), library.text())
		}
		switch {
		case strings.HasPrefix(name, "itself") && (got.err == nil || !strings.Contains(got.err.Error(), "yaml: anchor 'a' value contains itself")):
			t.Errorf("%s: the document is %.300s", name, got.text())
		case strings.HasPrefix(name, "nine") && (got.err == nil || got.err.Error() != "yaml: document contains excessive aliasing"):
			t.Errorf("%s: the document is %.300s", name, got.text())
		case (strings.HasPrefix(name, "200") || strings.HasPrefix(name, "400")) && got.err != nil:
			t.Errorf("%s: the document is refused: %v", name, got.err)
		case strings.Contains(name, "aliases beside 129") && got.err != nil:
			refused++
		case strings.Contains(name, "aliases beside 129"):
			decoded++
		}
	}
	if refused < 4/step+1 || decoded < 4/step+1 {
		t.Errorf("%d documents of 120 to 160 aliases were refused and %d decoded: they do not cover both", refused, decoded)
	}
	// Alone, the parts with 200 and with 400 aliases are refused by the
	// library.
	if alone := yamlByTheLibraryAlone(parsedYAML(t, "d: &d ["+strings.Repeat("1, ", 150)+"]\n"+aliases(400))); alone.err == nil || alone.err.Error() != "yaml: document contains excessive aliasing" {
		t.Errorf("a sequence of 150 that 400 aliases stand for is, alone, %.300s", alone.text())
	}
	if alone := yamlByTheLibraryAlone(parsedYAML(t, thousand+aliases(200))); alone.err == nil || alone.err.Error() != "yaml: document contains excessive aliasing" {
		t.Errorf("a sequence of a thousand that 200 aliases stand for is, alone, %.300s", alone.text())
	}
}

// A merge into a mapping of 300 keys, of a mapping of 300 keys, and of
// several, is what the library makes of it: a key the mapping has keeps
// its value wherever the merge key is written, of two merged mappings the
// earlier gives a key they share, a merged mapping's own merge is applied
// after its keys, a mapping merged twice changes nothing, the value of a
// key that is left out is not decoded, so a mistake in it does not fail
// the document, and what may not be merged is refused in the library's
// words.
func TestAMergeIntoALargeYAMLMappingIsWhatTheLibraryMakesOfIt(t *testing.T) {
	flow := func(prefix string, count int) string {
		var doc strings.Builder
		for i := range count {
			fmt.Fprintf(&doc, "%s%d: %s-%d, ", prefix, i, prefix, i)
		}
		return doc.String()
	}
	base := "base: &base {" + flow("b", 300) + "shared: of base, 7: seven}\n"
	more := "more: &more {" + flow("m", 200) + "shared: of more, b7: of more, <<: {deep: 1, m3: deep, b8: deep}}\n"
	for name, document := range map[string]string{
		"an alias":                 base + "all: {b5: mine, <<: *base, own: 1, b9: mine}\n",
		"into a large mapping":     base + "all: {" + flow("a", 300) + "<<: *base, b9: mine}\n",
		"a small one into a large": "all: {" + flow("a", 300) + "<<: {a5: theirs, z: 26}, own: mine}\n",
		"a sequence":               base + more + "all: {<<: [*more, *base], own: 1}\n",
		"a sequence, turned":       base + more + "all: {<<: [*base, *more, {own: theirs, last: 1}], own: 1}\n",
		"merged twice":             base + "all: {<<: [*base, *base], own: 1}\n",
		"a merge within a merge":   base + more + "top: &top {<<: *more, t: 1, m4: of top}\nall: {<<: [*top, *base], deep: mine}\n",
		"text keys and others":     base + "all: {1: one, <<: *base, 7: mine}\ntext: {a: 1, <<: *base}\n",
		"a value left out":         "all: {" + flow("a", 300) + "<<: {a5: !!int foo, z: 26}}\n",
		"a value not left out":     "all: {" + flow("a", 300) + "<<: {a5: 1, z: !!int foo}}\n",
		"a scalar":                 "all: {" + flow("a", 300) + "<<: 5}\n",
		"a sequence of a scalar":   base + "all: {" + flow("a", 300) + "<<: [*base, 5]}\n",
		"an alias of a sequence":   "s: &s [1, 2]\nall: {" + flow("a", 300) + "<<: *s}\n",
		"a quoted key":             base + "all: {" + flow("a", 300) + "\"<<\": *base}\n",
		"a quoted key merged":      "all: {" + flow("a", 300) + "<<: {\"<<\": 5, z: 26}}\n",
		"a null key merged":        "all: {" + flow("a", 300) + "<<: {~: 5, z: 26, 5: five, 1.5: half}}\n",
	} {
		root := parsedYAML(t, document)
		if way := yamlWayOf(root, yamlLargeMapping); way != yamlByParts {
			t.Errorf("%s: the document is decoded the way %d", name, way)
			continue
		}
		library, got := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, nil)
		if library.failed != nil || !got.same(library) {
			t.Errorf("%s: the document is\n%.400s\nand by the library alone\n%.400s", name, got.text(), library.text())
		}
	}
}

// BenchmarkYAMLDecode is the decoding of a document as an API answers it,
// a thousand items of small mappings, by the library alone, as it was, and
// as it is now, with the document looked through first; of a mapping of
// 5,000 different keys both ways, and of 20,000 and 200,000 as it is now.
func BenchmarkYAMLDecode(b *testing.B) {
	typical := []byte(typicalYAML(1000))
	alone := func(b *testing.B, body []byte) {
		b.ReportAllocs()
		for b.Loop() {
			root, _ := yamlDocumentOf(body)
			if outcome := yamlByTheLibraryAlone(root); outcome.err != nil {
				b.Fatal(outcome.err)
			}
		}
	}
	now := func(b *testing.B, body []byte) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := decodeYAML(body); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("typical/library-alone", func(b *testing.B) { alone(b, typical) })
	b.Run("typical/now", func(b *testing.B) { now(b, typical) })
	b.Run("keys=5000/library-alone", func(b *testing.B) { alone(b, []byte(manyYAMLKeys(5000))) })
	for _, keys := range []int{5000, 20000, 200000} {
		b.Run(fmt.Sprintf("keys=%d/now", keys), func(b *testing.B) { now(b, []byte(manyYAMLKeys(keys))) })
	}
}
