package decode

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// yamlMergedKeys is a mapping of so many different keys in block style under
// a key and an anchor of that name, each `k<i>: <i>`, from the key numbered
// from on.
func yamlMergedKeys(name string, from, count int) string {
	var doc strings.Builder
	fmt.Fprintf(&doc, "%s: &%s\n", name, name)
	for i := from; i < from+count; i++ {
		fmt.Fprintf(&doc, "  k%06d: %d\n", i, i)
	}
	return doc.String()
}

// yamlMergeDocument is a document a merge of a large mapping is measured
// and compared with, and how many mappings as large as its first it decodes
// into, its merges counted.
type yamlMergeDocument struct {
	name, body string
	mappings   float64
}

// yamlMergeDocuments are a mapping of so many keys under an anchor beside a
// small mapping, the same with the small mapping merging the large one, and
// with the merged pairs written out in it; several small mappings that each
// merge the large one, four of them, or two of a mapping of more than 50,000
// keys, whose aliases the library refuses at four; two large mappings that
// share half their keys, and a small one that merges a list of both; and a
// large mapping without and with a merge of a small one that has one of its
// keys.
func yamlMergeDocuments(keys int) []yamlMergeDocument {
	base := yamlMergedKeys("base", 0, keys)
	other := yamlMergedKeys("other", keys/2, keys)
	var own strings.Builder
	for i := range keys {
		fmt.Fprintf(&own, "  own%06d: %d\n", i, i)
	}
	several := 4
	if keys > 50000 {
		several = 2
	}
	var merges strings.Builder
	for i := range several {
		fmt.Fprintf(&merges, "copy%d:\n  <<: *base\n  extra: %d\n", i, i)
	}
	small := "small: &small {a: 1, b: 2, own000005: theirs}\nlarge:\n"
	return []yamlMergeDocument{
		{"no-merge", base + "copy:\n  extra: 1\n", 1},
		{"one-merge", base + "copy:\n  <<: *base\n  extra: 1\n", 2},
		{"written-out", base + strings.Replace(base, "base: &base", "copy:", 1) + "  extra: 1\n", 2},
		{fmt.Sprintf("%d-merges", several), base + merges.String(), float64(1 + several)},
		{"two-sources/no-merge", base + other + "copy:\n  extra: 1\n", 2},
		{"two-sources", base + other + "copy:\n  <<: [*base, *other]\n  extra: 1\n", 3.5},
		{"large-merges-small/no-merge", small + own.String(), 1},
		{"large-merges-small", small + own.String() + "  <<: *small\n", 1},
	}
}

// yamlKeyNodes are the nodes of a parsed document that are the key of a
// pair of a mapping.
func yamlKeyNodes(n *yaml.Node, keys map[*yaml.Node]bool) map[*yaml.Node]bool {
	for i, child := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			keys[child] = true
		}
		yamlKeyNodes(child, keys)
	}
	return keys
}

// yamlMergedHands counts how the keys of a document are handed to the
// library where a pair is not handed as a pair of its mapping: together, as
// a sequence made of the keys of a run of pairs of a merged mapping, and
// alone, as the one key of a mapping made for it; and how many runs of
// merged pairs were handed together and then, since a key of the run is
// not decoded so, one at a time, which the first key of the run handed
// alone right after the run tells.
type yamlMergedHands struct {
	keys                    map[*yaml.Node]bool
	together, alone, turned int
	// longest is the most keys handed together, and first the first key of
	// the run handed last, if nothing was handed since.
	longest int
	first   *yaml.Node
}

func (h *yamlMergedHands) handed(n *yaml.Node) {
	first := h.first
	h.first = nil
	if len(n.Content) == 0 || !h.keys[n.Content[0]] {
		return
	}
	switch {
	case n.Kind == yaml.SequenceNode:
		h.together++
		h.longest = max(h.longest, len(n.Content))
		h.first = n.Content[0]
	case n.Kind == yaml.MappingNode && len(n.Content) == 2 && n.Content[1].Tag == "!!null" && n.Content[1].Line == 0:
		h.alone++
		if n.Content[0] == first {
			h.turned++
		}
	}
}

// yamlMergeMaker writes documents of mappings that merge one another, drawn
// at random: two to six mappings in flow style, a pair on each line, each
// under an anchor, of no key to a few, of 126 to 131, of 254 to 259 and of
// up to 430, whose keys are drawn from the same few hundred so that two
// mappings share some; each may merge, before, between or after its own
// keys, an alias of an earlier mapping, a mapping written in place, which
// may merge another itself, a list of one to three of both, and what may not
// be merged, a scalar, an alias of a scalar or of a sequence, and a list
// with a scalar in it.
type yamlMergeMaker struct {
	random *rand.Rand
	// maps are the anchors of the mappings written so far.
	maps []string
	// odd is how many keys of a hundred are no plain text, and mistakes is
	// whether a value may be one the library lists a problem for, 1, or one
	// that ends the decoding too, 2.
	odd, mistakes int
	doc           strings.Builder
}

func (m *yamlMergeMaker) chance(percent int) bool {
	return m.random.IntN(100) < percent
}

// key is the key numbered i as it is written, with the colon that follows
// it, or none where the mapping has a key the library does not tell from
// it. One in a few, in some documents, is no plain text: a number, the same
// number written in another base, which is another key to the library and
// decodes into the same, the number as text, a float, a boolean, null in
// two spellings, `.nan`, a quoted "<<", a tagged text, a date, an alias of a
// text, and now and then a key no document decodes with: a sequence, plain
// and tagged as text, and a text tagged as a number.
func (m *yamlMergeMaker) key(i int, taken map[string]bool) (string, bool) {
	spelt := "k" + strconv.Itoa(i)
	same := "s:" + spelt
	if m.chance(m.odd) {
		number := strconv.Itoa(i % 40)
		switch m.random.IntN(16) {
		case 0, 1:
			spelt, same = number, "s:"+number
		case 2:
			spelt = fmt.Sprintf("0x%x", i%40)
			same = "s:" + spelt
		case 3:
			spelt, same = `"`+number+`"`, "s:"+number
		case 4:
			spelt, same = "1.5", "s:1.5"
		case 5:
			spelt, same = "true", "s:true"
		case 6:
			spelt, same = "~", "s:~"
		case 7:
			spelt, same = ".nan", "s:.nan"
		case 8:
			spelt, same = `"<<"`, "s:<<"
		case 9:
			spelt, same = "!!str 7", "s:7"
		case 10:
			spelt, same = "2024-06-01", "s:2024-06-01"
		case 11:
			spelt, same = "*s ", "alias:s"
		case 12:
			spelt, same = `!!null ""`, "s:"
		case 13:
			if m.chance(4) {
				spelt, same = "? !!str [a] ", "seq"
			}
		case 14:
			if m.chance(4) {
				spelt, same = "[a]", "seq"
			}
		case 15:
			if m.chance(4) {
				spelt = "!!int " + spelt
			}
		}
	}
	if taken[same] {
		return "", false
	}
	taken[same] = true
	return spelt + ": ", true
}

// value is a value for the key numbered i: the number, a text, null, a
// small mapping and a list, an alias of a text and of a mapping written
// before, which may be a large one, a small mapping that merges one, and, in
// some documents, a value that does not fit its tag, which ends the
// decoding, and a mapping with a key that is a sequence tagged as text,
// which is a problem listed at the end.
func (m *yamlMergeMaker) value(i int) string {
	switch at := m.random.IntN(200); {
	case at < 12:
		return "text"
	case at < 18:
		return []string{"~", "null", `""`}[m.random.IntN(3)]
	case at < 28:
		return "{a: 1, b: [2, 3]}"
	case at < 34:
		return "[1, [2, 3]]"
	case at < 40:
		return "*s"
	case at < 41 && len(m.maps) > 0:
		return "*" + m.maps[m.random.IntN(len(m.maps))]
	case at < 42 && len(m.maps) > 0:
		return "{c: 3, <<: *" + m.maps[m.random.IntN(len(m.maps))] + "}"
	case at < 44 && m.mistakes > 0:
		return "{? !!str [a] : 1, b: 2}"
	case at < 45 && m.mistakes > 1:
		return []string{"!!int foo", `!!binary "@@@"`}[m.random.IntN(2)]
	}
	return strconv.Itoa(i)
}

// small is a mapping of no key to four in flow style on one line, which
// may merge another.
func (m *yamlMergeMaker) small(depth int) string {
	var pairs []string
	taken := map[string]bool{"s:<<": true}
	i := m.random.IntN(300)
	for range m.random.IntN(5) {
		i += 1 + m.random.IntN(3)
		if key, ok := m.key(i, taken); ok {
			pairs = append(pairs, key+m.value(i))
		}
	}
	if depth > 0 && m.chance(30) {
		pairs = append(pairs, "<<: "+m.merged(depth-1))
	}
	m.random.Shuffle(len(pairs), func(a, b int) { pairs[a], pairs[b] = pairs[b], pairs[a] })
	return "{" + strings.Join(pairs, ", ") + "}"
}

// merged is a value for a merge key.
func (m *yamlMergeMaker) merged(depth int) string {
	one := func() string {
		if len(m.maps) > 0 && m.chance(75) {
			return "*" + m.maps[m.random.IntN(len(m.maps))]
		}
		return m.small(depth)
	}
	switch at := m.random.IntN(100); {
	case at < 55:
		return one()
	case at < 96:
		items := make([]string, 1+m.random.IntN(3))
		for i := range items {
			items[i] = one()
		}
		return "[" + strings.Join(items, ", ") + "]"
	case at < 97:
		return []string{"5", "~"}[m.random.IntN(2)]
	case at < 98:
		return "*s"
	case at < 99:
		return "*l"
	}
	return "[" + one() + ", 5]"
}

// mapping writes a mapping of up to so many keys under a key and an anchor
// of the name.
func (m *yamlMergeMaker) mapping(name string, size int) {
	fmt.Fprintf(&m.doc, "%s: &%s {\n", name, name)
	taken := map[string]bool{}
	merge := -1
	if m.chance(75) {
		merge = m.random.IntN(size + 1)
		taken["s:<<"] = true
	}
	i := m.random.IntN(200)
	for at := 0; at <= size; at++ {
		if at == merge {
			fmt.Fprintf(&m.doc, "  <<: %s,\n", m.merged(2))
		}
		i += 1 + m.random.IntN(2)
		if key, ok := m.key(i, taken); ok && at < size {
			fmt.Fprintf(&m.doc, "  %s%s,\n", key, m.value(i))
		}
	}
	m.doc.WriteString("  }\n")
	m.maps = append(m.maps, name)
}

// document is one document.
func (m *yamlMergeMaker) document() string {
	m.doc.Reset()
	m.maps = m.maps[:0]
	m.odd = []int{0, 0, 0, 3, 30}[m.random.IntN(5)]
	m.mistakes = max(0, m.random.IntN(8)-5)
	m.doc.WriteString("s: &s text\nl: &l [1, 2]\n")
	for at := range 2 + m.random.IntN(5) {
		size := 0
		switch drawn := m.random.IntN(100); {
		case drawn < 30:
			size = m.random.IntN(7)
		case drawn < 45:
			size = 126 + m.random.IntN(6)
		case drawn < 50:
			size = 254 + m.random.IntN(6)
		case drawn < 60:
			size = 129 + m.random.IntN(300)
		default:
			size = 7 + m.random.IntN(40)
		}
		m.mapping("m"+strconv.Itoa(at), size)
	}
	return m.doc.String()
}

// yamlWrittenMerges are documents written for what a run of merged pairs
// handed to the library together must not change: two keys of a merged
// mapping that decode into the same, of which the first is taken; keys that
// are `.nan`, each of which is a key of its own; a null among text keys,
// which is left out, alone, between others and before a value that does not
// fit its tag; a key that does not fit its tag after a value with a problem
// and after one that ends the decoding; values with problems, which are
// listed in the order they are written; the value of a key that is left
// out, of the mapping's own and of an earlier merged mapping's, which is
// not decoded; the merge key's text as a key; and a merged mapping's own
// merge, of one that merges a third.
func yamlWrittenMerges() []string {
	problem := func(of string) string { return "{? !!str [" + of + "] : 1, z: 26}" }
	return []string{
		"all: {1: one, <<: {0x2: a, 2: b, 02: c, 3: d}}\n",
		"all: {1: one, <<: [{0x2: a, 5: e}, {2: b, 6: f}]}\n",
		"all: {1: one, <<: [{.nan: a, .NaN: b, 2: c}, {.nan: d, 2: e}]}\n",
		"all: {.nan: own, <<: {.nan: a, 1: b}}\n",
		"all: {a: 1, <<: {~: 3}}\n", "all: {a: 1, <<: {b: 2, ~: 3, c: 4}}\n", "all: {a: 1, <<: {b: !!int foo, ~: 3}}\n", "all: {a: 1, <<: {~: 3, b: !!int foo}}\n",
		"all: {1: one, <<: {b: 2, ~: 3, c: 4, null: 5}}\n",
		"all: {a: 1, <<: {b: " + problem("x") + ", !!int k: 3, c: " + problem("y") + "}}\n",
		"all: {a: 1, <<: {b: !!int foo, !!int k: 3}}\n", "all: {a: 1, <<: {!!int k: 3, b: !!int foo}}\n", "all: {1: one, <<: {b: 2, !!int k: 3}}\n",
		"all: {a: 1, <<: {b: " + problem("x") + ", c: 2,\n  d: " + problem("y") + ",\n  e: 5,\n  f: " + problem("z") + "}}\n",
		"all: {a: 1, <<: [{b: " + problem("x") + "},\n  {b: " + problem("y") + ", c: " + problem("z") + "}]}\n",
		"all: {b: 0, <<: {b: !!int foo, c: 1}}\n", "all: {<<: {b: !!int foo, c: 1}, b: 0}\n", "all: {a: 0, <<: [{b: 1}, {b: !!int foo, c: 2}]}\n",
		"all: {a: 0, <<: [{b: 1}, {c: 2, b: " + problem("x") + "}, {c: !!int foo, d: 4}]}\n",
		"all: {a: 1, <<: {b: 2, \"<<\": 5, c: 3}}\n", "all: {1: one, <<: {b: 2, \"<<\": 5, c: 3}}\n", "all: {a: 1, <<: {b: 2, !!str <<: 5, c: 3}}\n",
		"a: &a {x: 1, y: 2}\nb: &b {y: 3, z: 4, <<: *a}\nc: &c {<<: *b, z: 5, w: 6}\nall: {<<: [*c, *a], v: 7}\n",
		"a: &a {x: 1, y: 2}\nb: &b {<<: [*a, {y: 3, u: 4}], z: 4}\nall: {u: mine, <<: [{z: first}, *b]}\n",
		"s: &s text\nall: {a: 1, <<: {b: 2, *s : 3, c: 4, [x]: 5}}\n", "s: &s text\nall: {1: one, <<: {b: 2, *s : 3, c: 4}}\n",
		"big: &big {a: 1, b: 2, c: 3}\nall: {z: 0, <<: {p: 1, q: *big, r: 3, s: {t: *big}, u: 5}}\n",
	}
}

// A document with a merge is decoded into what the YAML library makes of
// it, and refused in its words, where the pairs of a merged mapping are
// handed to the library a run at a time, and into what it was decoded into
// when each key and each value was handed alone (yamlWalkBefore): over the
// documents written for the forms of a merge, a key and an alias, those
// written for what a run must not change, and 200 drawn at random of two to
// six mappings that merge one another — of a few keys, of 126 to 131, which
// is a run and a pair more or less, of 254 to 259 and of up to 430, sharing
// some of their keys; the merge key before, between and after the mapping's
// own keys; an alias, a mapping written in place and a list of one to three
// as what is merged; a merged mapping that merges another, through an alias
// too; keys that are numbers, floats, booleans, null, `.nan`, dates, aliases
// and sequences, and one number written two ways; values that are aliases of
// a large mapping, small mappings that merge one, values that do not fit
// their tag and mappings with a problem; and a scalar, an alias of a scalar
// and of a sequence, and a list with a scalar as what is merged, which is
// refused. Each is decoded with a mapping large past 128 keys and past one,
// two or four, which makes every mapping of it one the walk decodes: a
// document drawn past one of the three, and a written one past each, so that
// what it was written for is met in runs of one pair, of two and of four.
//
// Compared are the values with their types, and the errors' texts, what
// they are recognised by and whether they are a list of problems. Against
// the walk as it was every document is the same. Against the library it is
// the same but where it was not before: where a mapping with a merge has a
// key that is a sequence or a mapping, of its own or of a merged mapping,
// which the walk refuses with the error it meets first. The library is
// handed no mapping of more keys than counts as large, and no more keys
// together than that.
//
// What the test costs is the documents drawn, each decoded by the library,
// which compares every two keys of a mapping, and four times by a walk: 200
// are a second. Under the race detector, where one costs fifty milliseconds,
// four are drawn: each outcome counted below is still met.
func TestAYAMLDocumentWithAMergeIsWhatTheLibraryAndTheWalkBeforeMakeOfIt(t *testing.T) {
	documents := append(yamlWrittenMerges(), yamlWrittenDocuments()...)
	written := len(documents)
	maker := yamlMergeMaker{random: rand.New(rand.NewPCG(27, 128))} //nolint:gosec // documents for a test
	for range alloctest.UnlessRaced(200, 4) {
		documents = append(documents, maker.document())
	}
	counted := map[string]int{}
	for i, document := range documents {
		root, _ := yamlDocumentOf([]byte(document))
		if root == nil {
			continue
		}
		library := yamlByTheLibraryAlone(root)
		keys := yamlKeyNodes(root, map[*yaml.Node]bool{})
		bounds := []int{yamlLargeMapping, []int{1, 2, 4}[i%3]}
		if i < written {
			bounds = []int{yamlLargeMapping, 1, 2, 4}
		}
		for _, large := range bounds {
			if yamlWayOf(root, large) != yamlByParts {
				continue
			}
			hands := yamlMergedHands{keys: keys}
			got := yamlWith(root, large, func(handed *yaml.Node) {
				if pairs := largestYAMLMapping(handed, map[*yaml.Node]bool{}); pairs > max(large, 1) {
					t.Fatalf("%q, large past %d keys: the library is handed a mapping of %d", document, large, pairs)
				}
				hands.handed(handed)
			})
			if hands.longest > max(large, 1) {
				t.Fatalf("%q, large past %d keys: the library is handed %d keys together", document, large, hands.longest)
			}
			if before := yamlWithTheWalkBefore(root, large); !got.same(before) {
				t.Fatalf("%q, large past %d keys, is\n%.1000s\nand as the walk decoded it before\n%.1000s", document, large, got.text(), before.text())
			}
			counted["runs of merged pairs handed together"] += hands.together - hands.turned
			counted["runs of merged pairs handed one at a time"] += hands.turned
			switch {
			case got.same(library) && got.err == nil:
				counted["decoded"]++
				if whole, _ := got.value.(map[string]any); len(whole) > 0 {
					for _, value := range whole {
						if _, general := value.(map[any]any); general {
							counted["decoded, with a mapping of keys that are no text"]++
							break
						}
					}
				}
			case got.same(library) && onlyProblems(got.err):
				counted["refused for its problems"]++
			case got.same(library) && got.err.Error() == errYAMLMergesMap.Error():
				counted["refused for what it merges"]++
			case got.same(library):
				counted["refused otherwise"]++
			case (got.err != nil || got.failed != nil) && yamlMergesBesideAKeyThatIsNoKey(root, map[*yaml.Node]bool{}):
				counted["refused with another error than the library's, or where it fails outright"]++
			default:
				t.Fatalf("%q, large past %d keys, is\n%.1000s\nand by the library alone\n%.1000s", document, large, got.text(), library.text())
			}
		}
	}
	t.Logf("%d documents: %v", len(documents), counted)
	for _, outcome := range []struct {
		name         string
		plain, raced int
	}{
		{"decoded", 160, 50},
		{"decoded, with a mapping of keys that are no text", 34, 10},
		{"refused for its problems", 42, 8},
		{"refused for what it merges", 36, 5},
		{"refused otherwise", 44, 16},
		{"refused with another error than the library's, or where it fails outright", 7, 6},
		{"runs of merged pairs handed together", 24000, 500},
		{"runs of merged pairs handed one at a time", 88, 7},
	} {
		if least := alloctest.UnlessRaced(outcome.plain, outcome.raced); counted[outcome.name] < least {
			t.Errorf("%d documents, or runs of them, were %s, fewer than %d: the documents do not cover it", counted[outcome.name], outcome.name, least)
		}
	}
}

// yamlMergedAliases is a document of a sequence of so many numbers under an
// anchor, a mapping of so many pairs whose values are aliases of it, and so
// many small mappings that each merge the mapping and have no key of their
// own: nearly everything the library decodes for a merge of it is decoded
// for an alias.
func yamlMergedAliases(numbers, pairs, merges int) string {
	var doc strings.Builder
	fmt.Fprintf(&doc, "d: &d %s\nbase: &base {%s}\n", yamlNumbers(numbers), manyYAMLPairs(pairs, "*d", ", "))
	for i := range merges {
		fmt.Fprintf(&doc, "m%d: {<<: *base}\n", i)
	}
	return doc.String()
}

// yamlMergedBeforeARefusal is a document of 144 numbers under an anchor, a
// mapping of twelve pairs whose values are aliases of them, twelve small
// mappings that each merge it, which is as many as are decoded, and one that
// merges another mapping of twelve such pairs: the document is refused for
// its aliases in that last merge. With a mistake, the value of the pair
// numbered so is one that does not fit its tag; the mapping is written where
// it is merged into one that has the key itself, so that the value is not
// read there. The library decodes a third less for it than for 200 numbers,
// five pairs and 33 merges, which are refused at the same pair.
func yamlMergedBeforeARefusal(mistake int) string {
	var doc strings.Builder
	fmt.Fprintf(&doc, "d: &d %s\nbase: &base {%s}\nfirst: {", yamlNumbers(144), manyYAMLPairs(12, "*d", ", "))
	if mistake >= 0 {
		fmt.Fprintf(&doc, "key%d: 0, ", mistake)
	}
	doc.WriteString("<<: &last {")
	for i := range 12 {
		if i == mistake {
			fmt.Fprintf(&doc, "key%d: !!int foo, ", i)
		} else {
			fmt.Fprintf(&doc, "key%d: *d, ", i)
		}
	}
	doc.WriteString("}}\n")
	for i := range 12 {
		fmt.Fprintf(&doc, "m%d: {<<: *base}\n", i)
	}
	doc.WriteString("m: {<<: *last}\n")
	return doc.String()
}

// The aliases of a merged mapping are counted pair by pair, in the order of
// the pairs, as they were when each pair was handed to the library alone,
// so that a document is refused for its aliases at the same merge as the
// library refuses it, and as the walk refused it before: small mappings
// that each merge a mapping of 130 pairs whose values are aliases of ten
// numbers are decoded up to nineteen of them and refused from the twentieth
// on, and of twenty pairs of fifty numbers, with a mapping large past four
// keys, from the thirteenth. The values of a run of the 130 pairs are more
// than the library could be handed in one part without refusing them
// itself, so they are handed in two.
//
// And what a value of a merged mapping fails with comes before the refusal
// for the aliases of a later pair of its run, as it did. A document that is
// refused for its aliases at the seventh pair of the last mapping it
// merges, in runs of four pairs: with a value that does not fit its tag in
// the pair before that one or two before, which are handed to the library
// only when the seventh is refused, or in an earlier run, the document is
// refused for the value; with it in the seventh or later, where it is not
// read, for the aliases. Under the race detector the merges are one fewer
// than are refused and as many, and the value is in the sixth pair and in
// the seventh.
//
// And the library is handed nothing of a merged mapping apart from the rest
// of the document that it would refuse for its aliases where the document
// is none it refuses. One merge of six pairs whose values are aliases of
// 200 numbers, in runs of five pairs: the five values of a run are 1,011
// nodes to the library handed together, 1,005 of them for an alias, and are
// handed in two parts. One merge of two pairs of 998 numbers: a value is
// 1,001 nodes handed as the one item of a part, and is handed as the alias
// it is. Both documents are decoded, as they were.
func TestTheAliasesOfAMergedYAMLMappingAreRefusedWhereTheyWere(t *testing.T) {
	const aliasing = "yaml: document contains excessive aliasing"
	check := func(document string, large int) yamlOutcome {
		t.Helper()
		root := parsedYAML(t, document)
		got := yamlWith(root, large, nil)
		if before := yamlWithTheWalkBefore(root, large); !got.same(before) {
			t.Fatalf("%.300q, large past %d keys, is\n%.300s\nand as the walk decoded it before\n%.300s", document, large, got.text(), before.text())
		}
		if library := yamlByTheLibraryAlone(root); !got.same(library) {
			t.Fatalf("%.300q, large past %d keys, is\n%.300s\nand by the library alone\n%.300s", document, large, got.text(), library.text())
		}
		return got
	}
	for _, tc := range []struct{ large, pairs, numbers, refusedFrom int }{{yamlLargeMapping, 130, 10, 20}, {4, 20, 50, 13}} {
		for merges := tc.refusedFrom - alloctest.UnlessRaced(3, 1); merges <= tc.refusedFrom+alloctest.UnlessRaced(1, 0); merges++ {
			switch got := check(yamlMergedAliases(tc.numbers, tc.pairs, merges), tc.large); {
			case got.err == nil && merges >= tc.refusedFrom:
				t.Errorf("%d merges of %d pairs of %d numbers are decoded, want them refused from %d on", merges, tc.pairs, tc.numbers, tc.refusedFrom)
			case got.err != nil && (merges < tc.refusedFrom || got.err.Error() != aliasing):
				t.Errorf("%d merges of %d pairs of %d numbers are refused with %v, want them refused for the aliases from %d on", merges, tc.pairs, tc.numbers, got.err, tc.refusedFrom)
			}
		}
	}
	const refusedAt = 6
	for mistake := refusedAt - alloctest.UnlessRaced(3, 1); mistake <= refusedAt+alloctest.UnlessRaced(1, 0); mistake++ {
		want := "yaml: cannot decode !!str `foo` as a !!int"
		if mistake >= refusedAt {
			want = aliasing
		}
		if got := check(yamlMergedBeforeARefusal(mistake), 4); got.err == nil || got.err.Error() != want {
			t.Errorf("with a value that does not fit its tag in pair %d of the last merged mapping the document is refused with %v, want %q", mistake, got.err, want)
		}
	}
	for _, tc := range []struct{ large, pairs, numbers int }{{5, 6, 200}, {1, 2, 998}} {
		if got := check(yamlMergedAliases(tc.numbers, tc.pairs, 1), tc.large); got.err != nil {
			t.Errorf("one merge of %d pairs of %d numbers, large past %d keys, is refused with %v, want it decoded", tc.pairs, tc.numbers, tc.large, got.err)
		}
	}
}

// A merge of a mapping of many runs of pairs is what the walk made of it
// when each merged pair was handed to the library alone, value and error:
// the documents the merge is measured with, of a mapping of 5,000 keys,
// which is 39 runs of yamlLargeMapping and a part of one, merged once, four
// times, and in a list with another that shares half its keys, and of a
// large mapping that merges a small one; a large mapping that merges another
// through an alias, with keys of its own before and after, merged in a list
// with a third into a mapping that has two of the keys itself; a mapping of
// 5,000 keys that are numbers, some of them one number written twice, a null
// and a `.nan` among them, merged into a mapping of keys that are no text
// and into one of text keys; values with a problem at both ends of a merged
// mapping, which are listed in their order, once for the mapping and once
// for the merge; and a list of what is merged that ends with a scalar,
// which is refused after the mappings before it were merged. The merged
// mappings have the keys they are to have.
//
// The walk as it was is the one oracle here: the library alone, which
// compares every two keys of a mapping, takes ten times as long as it for
// the mapping merged once, and seconds for one of 20,000 keys. What a merge
// is decoded into does not depend on how many runs it is, past the first,
// one between and the last, so the mappings are no larger than keeps the
// test to a third of a second. Under the race detector they are of 400 keys,
// three runs of yamlLargeMapping and a part of one.
func TestAMergeOfAVeryLargeYAMLMappingIsWhatTheWalkMadeOfItBefore(t *testing.T) {
	keys := alloctest.UnlessRaced(5000, 400)
	base, other := yamlMergedKeys("base", 0, keys), yamlMergedKeys("other", keys/2, keys)
	var numbers strings.Builder
	numbers.WriteString("numbers: &numbers\n  ~: none\n  .nan: no number\n  0x10: sixteen\n")
	for i := range keys {
		fmt.Fprintf(&numbers, "  %d: %d\n", i, i)
	}
	problem := "{? !!str [a] : 1, z: 26}"
	documents := yamlMergeDocuments(keys)
	documents = append(documents,
		yamlMergeDocument{name: "a merge within a merge", body: base + other + "mid: &mid\n  only: mid\n  <<: *base\n  k000003: mid\n" +
			"all:\n  k000001: mine\n  <<: [*mid, *other]\n  only: mine\n"},
		yamlMergeDocument{name: "keys that are numbers", body: numbers.String() + "any:\n  1.5: mine\n  16: mine\n  <<: *numbers\n  7: mine\ntext:\n  a: mine\n  <<: *numbers\n  \"7\": mine\n"},
		yamlMergeDocument{name: "values with a problem", body: "base: &base\n  first: " + problem + "\n" + strings.SplitN(base, "\n", 2)[1] + "  last: " + problem + "\n" +
			"copy:\n  <<: *base\n  extra: 1\n"},
		yamlMergeDocument{name: "a scalar after a mapping", body: base + "copy:\n  <<: [*base, 5]\n  extra: 1\n"},
	)
	for _, document := range documents {
		if !strings.Contains(document.body, "<<") {
			continue
		}
		root := parsedYAML(t, document.body)
		if way := yamlWayOf(root, yamlLargeMapping); way != yamlByParts {
			t.Errorf("%s: the document is decoded the way %d", document.name, way)
			continue
		}
		got, before := yamlWith(root, yamlLargeMapping, nil), yamlWithTheWalkBefore(root, yamlLargeMapping)
		if !got.same(before) {
			t.Errorf("%s: the document is\n%.600s\nand as the walk decoded it before\n%.600s", document.name, got.text(), before.text())
			continue
		}
		whole, _ := got.value.(map[string]any)
		entries := func(key string) int {
			switch merged := whole[key].(type) {
			case map[string]any:
				return len(merged)
			case map[any]any:
				return len(merged)
			}
			return 0
		}
		switch document.name {
		case "one-merge":
			if copied, _ := whole["copy"].(map[string]any); len(copied) != keys+1 || copied["extra"] != 1 || copied[fmt.Sprintf("k%06d", keys-1)] != keys-1 {
				t.Errorf("%s: the mapping that merges %d keys has %d", document.name, keys, len(copied))
			}
		case "two-sources":
			// The earlier of the two gives a key they share: its values are
			// numbers either way, so the keys are counted.
			if entries("copy") != keys+keys/2+1 {
				t.Errorf("%s: the mapping that merges two of %d keys, half of them shared, has %d", document.name, keys, entries("copy"))
			}
		case "a merge within a merge":
			if all, _ := whole["all"].(map[string]any); len(all) != keys+keys/2+1 || all["only"] != "mine" || all["k000001"] != "mine" || all["k000003"] != "mid" || all["k000002"] != 2 {
				t.Errorf("%s: the mapping has %d keys, and those set beside the merges are %v, %v and %v", document.name, len(all), all["only"], all["k000001"], all["k000003"])
			}
		case "keys that are numbers":
			// Among keys of any type a null and a `.nan` are keys and the number
			// written a second way is the one the mapping has; among text keys
			// that is a key of its own and a null is none.
			if entries("any") != keys+3 || entries("text") != keys+3 {
				t.Errorf("%s: the mappings that merge %d numbers have %d and %d keys", document.name, keys, entries("any"), entries("text"))
			}
		case "values with a problem":
			if got.err == nil || strings.Count(got.err.Error(), "into string") != 4 || !strings.Contains(got.err.Error(), fmt.Sprintf("line 2: cannot unmarshal !!str `` into string\n  line %d: ", keys+3)) {
				t.Errorf("%s: the document is refused with %.600v, want the two problems listed twice", document.name, got.err)
			}
		case "a scalar after a mapping":
			if got.err == nil || got.err.Error() != errYAMLMergesMap.Error() {
				t.Errorf("%s: the document is refused with %.300v", document.name, got.err)
			}
		}
	}
}

// A merge of a large mapping costs no more than the mapping does: decoding
// a parsed document of a mapping of 5,000 keys and a small one that merges
// it allocates no more than twice the bytes of the same document without
// the merge, and no more than two and a quarter times the allocations; with
// four small mappings that each merge it, no more than five times the bytes
// and six times the allocations. A call of the library for each merged key
// and one for each value were 2.7 times the bytes and 3.5 times the
// allocations, and 7.7 and 11 times for the four; in runs it is 1.7 and 2.0
// times, and 3.8 and 5.1. The factors of a mapping of 5,000 keys are those
// of one of 20,000 within half of a hundredth of them, either way: what a
// pair allocates does not depend on how many there are, and a map of four
// times the keys is as full.
//
// And the library is called for a run of merged pairs, not for a pair: for
// the keys of 128 pairs and for their values, two calls for every 128 keys
// of each merge beside the one for every 128 keys of the mapping itself,
// where it was two for every merged pair. Under the race detector, which no
// allocation is measured under, the mapping is of 1,000 keys and the calls
// are counted alike.
func TestAMergeOfALargeYAMLMappingCostsNoMoreThanTheMapping(t *testing.T) {
	keys := alloctest.UnlessRaced(5000, 1000)
	var alone float64
	var memoryAlone uint64
	for _, document := range yamlMergeDocuments(keys) {
		switch document.name {
		case "no-merge":
			if !raceDetector {
				root := parsedYAML(t, document.body)
				alone, memoryAlone = alloctest.Once(2, func() { yamlWith(root, yamlLargeMapping, nil) })
			}
			continue
		case "one-merge", "4-merges":
		default:
			continue
		}
		root := parsedYAML(t, document.body)
		merges := int(document.mappings) - 1
		hands, longest := 0, 0
		got := yamlWith(root, yamlLargeMapping, func(handed *yaml.Node) {
			hands++
			if handed.Kind == yaml.SequenceNode {
				longest = max(longest, len(handed.Content))
			}
		})
		if most := (1+2*merges)*(keys/yamlLargeMapping+1) + 4*(merges+1); got.err != nil || hands > most || longest > yamlLargeMapping {
			t.Errorf("%s: the library was handed %d parts, the longest a sequence of %d items, want at most %d parts of %d: %v", document.name, hands, longest, most, yamlLargeMapping, got.err)
		}
		if raceDetector {
			continue
		}
		mostMemory := uint64(1+merges) * memoryAlone
		if memory := alloctest.BytesAtMost(1, mostMemory, func() { yamlWith(root, yamlLargeMapping, nil) }); memory > mostMemory {
			t.Errorf("%s: decoding the document allocates %d bytes, and without the merge %d: more than %d times as much", document.name, memory, memoryAlone, 1+merges)
		}
		most := (1 + 1.25*float64(merges)) * alone
		if now := alloctest.AllocsAtMost(1, most, func() { yamlWith(root, yamlLargeMapping, nil) }); now > most {
			t.Errorf("%s: decoding the document allocates %.0f times, and without the merge %.0f: more than %.2f times as often", document.name, now, alone, most/alone)
		}
	}
	if !raceDetector && alone == 0 {
		t.Error("the document without the merge was not measured")
	}
}

// BenchmarkYAMLMerge is the decoding of a document with a merge of a large
// mapping, of 20,000 and of 200,000 keys, beside the same document without
// the merge and with the merged pairs written out: time, and bytes and
// allocations to be divided by the bytes of the body, which are reported.
func BenchmarkYAMLMerge(b *testing.B) {
	for _, keys := range []int{20000, 200000} {
		for _, document := range yamlMergeDocuments(keys) {
			body := []byte(document.body)
			b.Run(fmt.Sprintf("keys=%d/%s", keys, document.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := decodeYAML(body); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(len(body)), "body-bytes")
			})
		}
	}
}
