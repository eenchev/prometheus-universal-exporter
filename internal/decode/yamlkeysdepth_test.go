package decode

import (
	"fmt"
	"math/rand/v2"
	"runtime/metrics"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlListedByCalls is yamlLearnt.listed as it was, a call for each node and
// for what each alias stands for: the oracle for the one that keeps a list.
func yamlListedByCalls(l *yamlLearnt, n *yaml.Node) uint64 {
	switch n.Kind {
	case yaml.ScalarNode:
		return 0
	case yaml.AliasNode:
		if n.Alias == nil {
			return 0
		}
		return yamlListedByCalls(l, n.Alias)
	case yaml.MappingNode:
		if twice, ok := l.twice[n]; ok {
			return twice.text
		}
	}
	if n.Anchor != "" {
		if text, known := l.lists[n]; known {
			return text
		}
		if l.lists == nil {
			l.lists = map[*yaml.Node]uint64{}
		}
		l.lists[n] = 0
	}
	var text uint64
	for _, child := range n.Content {
		text = yamlPlus(text, yamlListedByCalls(l, child))
	}
	if n.Anchor != "" {
		l.lists[n] = text
	}
	return text
}

// yamlAnchorsInAMappingWrittenTwice is a document whose anchors are written
// in a mapping with a key written twice, which is read no further, each a
// sequence of an alias of the one before it, and the last aliased outside:
// looked through from that alias, each anchor is met in the one after it.
func yamlAnchorsInAMappingWrittenTwice(anchors int) string {
	var doc strings.Builder
	doc.WriteString("x:\n  d: 1\n  d: 2\n  c:\n  - &a0 [x]\n")
	for i := 1; i <= anchors; i++ {
		fmt.Fprintf(&doc, "  - &a%d [*a%d]\n", i, i-1)
	}
	fmt.Fprintf(&doc, "y: *a%d\n", anchors)
	return doc.String()
}

// yamlStacks is how much memory the goroutines' stacks take.
func yamlStacks() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/stacks:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// yamlStackOf is how much the stacks grew by while a call ran, on a
// goroutine of its own, which starts with a few kilobytes.
func yamlStackOf(call func()) uint64 {
	grown := make(chan uint64)
	go func() {
		before := yamlStacks()
		call()
		grown <- max(yamlStacks(), before) - before
	}()
	return <-grown
}

// How much text the problems of a document come to is added up with what
// each alias stands for, in a stack no deeper than the document is nested:
// 40,000 anchors written in a mapping with a key written twice, each a
// sequence of an alias of the one before, are met one inside the other from
// the alias of the last, and were a call each, an 8 MB stack for an 860 kB
// body and more than a goroutine may have for a larger one, which no recover
// catches. Looking through the document, and decoding it, now takes under 2
// MB; and the document comes to the text it came to, one key written twice.
// The document, a chain far deeper than one may be written, is now refused for
// its depth before it is decoded (adding up the depth is as shallow a stack as
// adding up the problems), where it was refused for its aliases.
func TestTheProblemsOfAYAMLDocumentAreAddedUpInAStackNoDeeperThanItIsNested(t *testing.T) {
	anchors := 40000
	if raceDetector {
		anchors = 10000
	}
	body := yamlAnchorsInAMappingWrittenTwice(anchors)
	root := parsedYAML(t, body)
	var text uint64
	looking := yamlStackOf(func() {
		learnt := yamlLearnt{large: yamlLargeMapping}
		learnt.learn(root, true)
		if learnt.way(root) != yamlByLibrary {
			t.Error("the document is not the library's to decode")
		}
		text = learnt.listed(root)
	})
	if text != yamlProblemText+1 {
		t.Errorf("the problems come to %d bytes of text, want the %d of one key written twice", text, yamlProblemText+1)
	}
	var err error
	decoding := yamlStackOf(func() { _, err = decodeYAML([]byte(body)) })
	if !isYAMLTooDeep(err) {
		t.Errorf("the document is refused with %v, want its depth", err)
	}
	if !raceDetector && (looking > 1<<20 || decoding > 2<<20) {
		t.Errorf("looking through %d anchors that each hold the one before grew the stack by %d kB, and decoding the document by %d kB, want under 1 MB and 2 MB", anchors, looking>>10, decoding>>10)
	}
	// By calls it was the same text.
	before := yamlLearnt{large: yamlLargeMapping}
	before.learn(root, true)
	if was := yamlListedByCalls(&before, root); was != text {
		t.Errorf("the problems come to %d bytes of text, and by a call for each node to %d", text, was)
	}
}

// What the problems of a document come to is what it was when each node,
// and what each alias stands for, was a call: over the YAML files of the
// repository, the documents of the YAML error tests, those written for the
// forms of a merge, a key and an alias, 30,000 drawn at random, one in ten
// with keys written twice, and documents written for the order the anchors
// are met in — anchors in a mapping with a key written twice aliased from
// outside it, in their order, against it and many times, an anchor that
// holds an alias of what holds it, of itself, and of a mapping written
// twice, in and outside a mapping written twice — whatever counts as a large
// mapping.
func TestTheProblemsOfAYAMLDocumentComeToWhatTheyDidByACallForEachNode(t *testing.T) {
	twice := "{k: 1, k: 2}"
	documents := append(append(yamlFixtureDocuments(t), hostileYAMLDocuments()...), yamlWrittenDocuments()...)
	documents = append(documents,
		yamlAnchorsInAMappingWrittenTwice(50),
		"x: {d: 1, d: 2, c: [&a "+twice+", &b [*a, *a], &c [*b, *a, *b]]}\ny: [*c, *b, *a, *c]\n",
		"x: {d: 1, d: 2, c: [&a "+twice+", &b [*a, *a], &c [*b, *a, *b]]}\ny: [*a, *b, *c, *a]\n",
		"b: &b\n  m: {d: 1, d: 2, c: &a [*b]}\ny: *a\n",
		"b: &b\n  m: {d: 1, d: 2, c: &a [*b]}\n  z: *a\ny: *a\n",
		"b: &b\n  m: {d: 1, d: 2, c: &a [*b, "+twice+"]}\n  z: [*a, *a]\ny: [*a, *b]\n",
		"a: &a [*a, "+twice+"]\nb: [*a, *a]\n",
		"a: &a {m: "+twice+", l: [*a]}\nb: *a\nc: *a\n",
		"m: &m "+twice+"\nl: &l [*m, *m, &n [*m]]\no: [*l, *n, *l]\n",
		"x: {d: 1, d: 2, c: &c [&m "+twice+", *m]}\ny: &y [*c, *m]\nz: [*y, *y, *c]\n",
		"s: &s text\nx: {d: 1, d: 2, c: &c [*s, &t more]}\ny: [*c, *t, *s]\n",
	)
	maker := yamlMaker{random: rand.New(rand.NewPCG(16, 17))} //nolint:gosec // documents for a test
	generated := 30000
	if raceDetector {
		generated = 1000
	}
	for range generated {
		documents = append(documents, maker.document())
	}
	withProblems, withAliases, differentTexts := 0, 0, map[uint64]bool{}
	for _, document := range documents {
		root, _ := yamlDocumentOf([]byte(document))
		if root == nil {
			continue
		}
		for _, large := range []int{0, 4, yamlLargeMapping} {
			now, before := yamlLearnt{large: large}, yamlLearnt{large: large}
			now.learn(root, true)
			before.learn(root, true)
			text, was := now.listed(root), yamlListedByCalls(&before, root)
			if text != was {
				t.Fatalf("%q, large past %d keys: the problems come to %d bytes of text, and by a call for each node to %d", document, large, text, was)
			}
			if len(now.lists) != len(before.lists) {
				t.Fatalf("%q, large past %d keys: %d anchors were added up, and by a call for each node %d", document, large, len(now.lists), len(before.lists))
			}
			for anchor, text := range now.lists {
				if was, known := before.lists[anchor]; !known || was != text {
					t.Fatalf("%q, large past %d keys: the anchor %q on line %d comes to %d bytes of text, and by a call for each node to %d", document, large, anchor.Anchor, anchor.Line, text, was)
				}
			}
			if large == yamlLargeMapping && text > 0 {
				withProblems++
				differentTexts[text] = true
				if _, aliasOrMerge := yamlNodes(root); aliasOrMerge {
					withAliases++
				}
			}
		}
	}
	if !raceDetector && (withProblems < 1000 || withAliases < 500 || len(differentTexts) < 30) {
		t.Errorf("%d documents have keys written twice, %d of them aliases or merges, and they come to %d different texts: the documents do not cover it", withProblems, withAliases, len(differentTexts))
	}
}
