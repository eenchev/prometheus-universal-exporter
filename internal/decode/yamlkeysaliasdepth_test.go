package decode

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlFlowSeq is a flow sequence nested d deep, `[[[x]]]`, which the parser
// bounds at 10,000 (max_flow_level).
func yamlFlowSeq(d int) string { return strings.Repeat("[", d) + "x" + strings.Repeat("]", d) }

// yamlFlowMap is a flow mapping nested d deep, `{k: {k: x}}`.
func yamlFlowMap(d int) string { return strings.Repeat("{k: ", d) + "x" + strings.Repeat("}", d) }

// yamlAliasChain is a hidden chain of links anchors, each a sequence, a
// mapping or a merge holding an alias of the one before: `a0: &a0 [x]`, `a1:
// &a1 [*a0]`, ... Each anchor is used once, so the document expands to no
// more than twice its size and the library's aliasing guard does not trip on
// its size, but the library decodes it by following every alias, a stack as
// deep as the chain is long.
func yamlAliasChain(kind string, links int) string {
	var b strings.Builder
	switch kind {
	case "seq":
		b.WriteString("a0: &a0 [x]\n")
		for i := 1; i <= links; i++ {
			fmt.Fprintf(&b, "a%d: &a%d [*a%d]\n", i, i, i-1)
		}
	case "map":
		b.WriteString("a0: &a0 {k: x}\n")
		for i := 1; i <= links; i++ {
			fmt.Fprintf(&b, "a%d: &a%d {k: *a%d}\n", i, i, i-1)
		}
	case "merge":
		b.WriteString("a0: &a0 {k: x}\n")
		for i := 1; i <= links; i++ {
			fmt.Fprintf(&b, "a%d: &a%d {<<: *a%d}\n", i, i, i-1)
		}
	}
	return b.String()
}

// yamlDepthOf is how deep a parsed document is nested, counting what its
// aliases stand for.
func yamlDepthOf(root *yaml.Node) uint64 {
	l := yamlLearnt{large: yamlLargeMapping}
	l.learn(root, true)
	return l.deepest(root)
}

// isYAMLTooDeep reports whether an error is the refusal of a document nested
// deeper than a response may be.
func isYAMLTooDeep(err error) bool {
	return err != nil && err.Error() == "yaml: the document is nested more than 10000 deep, counting what its aliases stand for; a response may nest 10000 deep at most"
}

// A YAML document nested deeper than a response may be, counting what its
// aliases stand for, is refused before anything decodes it, on every way the
// document is decoded, the library's own included. A single alias of a deeply
// nested anchor is a chain the library decodes: at the bound, which is the
// JSON decoder's and counts the sequences and mappings of the value, it is
// decoded into what the library makes of it, and one level over the bound the
// library still decodes it, deep stack and all, but the exporter refuses it
// for its depth. The alias itself is no level, and neither are the document
// and the scalar innermost, which the bound counted when it was the depth of
// the library's calls: a document was then refused from three levels less
// deep. The chain runs through a sequence, through a mapping and through a
// merge key, whose value counts as it is written, one level in.
func TestAYAMLDocumentNestedThroughItsAliasesDeeperThanAResponseMayNestIsRefused(t *testing.T) {
	// A single alias of a deeply nested anchor: the library decodes it (its
	// aliases expand to no more than twice its size), and so does the exporter
	// up to the bound.
	for _, nest := range []func(int) string{yamlFlowSeq, yamlFlowMap} {
		// 9999 levels of nesting under the key of a mapping come to a depth of
		// exactly the bound, which the value the alias stands for has too; one
		// level more is over it.
		at := parsedYAML(t, "p: &x "+nest(9999)+"\nq: *x\n")
		if got := yamlDepthOf(at); got != MaxDepth {
			t.Fatalf("the accepted document is %d deep, want the bound of %d", got, MaxDepth)
		}
		library, ours := yamlByTheLibraryAlone(at), yamlWith(at, yamlLargeMapping, nil)
		if ours.err != nil || !library.same(ours) {
			t.Errorf("at the bound the document decodes to %s, and the library makes %s of it", ours.text(), library.text())
		}
		if value, _ := ours.value.(map[string]any); valueDepth(value["p"]) != MaxDepth-1 || valueDepth(value["q"]) != MaxDepth-1 {
			t.Errorf("at the bound the values under the mapping nest %d and %d deep, want %d", valueDepth(value["p"]), valueDepth(value["q"]), MaxDepth-1)
		}

		over := parsedYAML(t, "p: &x "+nest(10000)+"\nq: *x\n")
		if got := yamlDepthOf(over); got != MaxDepth+1 {
			t.Fatalf("the document over the bound is %d deep, want %d", got, MaxDepth+1)
		}
		if library := yamlByTheLibraryAlone(over); library.err != nil || library.failed != nil {
			t.Errorf("the library does not decode the document over the bound, it makes %s of it", library.text())
		}
		if ours := yamlWith(over, yamlLargeMapping, nil); !isYAMLTooDeep(ours.err) {
			t.Errorf("the document over the bound is refused with %s, want the depth", ours.text())
		}
	}

	// A merge key's value is a node the library decodes through, so a merge of
	// a deeply nested mapping counts as it is written, a mapping inside the
	// one it is merged into: at the bound it is decoded, and over it refused,
	// where the library decodes it.
	merge := parsedYAML(t, "p: &x "+yamlFlowMap(9998)+"\nq:\n  <<: *x\n")
	if got := yamlDepthOf(merge); got != MaxDepth {
		t.Fatalf("the merge of a nested mapping is %d deep, want the bound of %d", got, MaxDepth)
	}
	if library, ours := yamlByTheLibraryAlone(merge), yamlWith(merge, yamlLargeMapping, nil); ours.err != nil || !library.same(ours) {
		t.Errorf("the merge at the bound decodes to %s, and the library makes %s of it", ours.text(), library.text())
	}
	merge = parsedYAML(t, "p: &x "+yamlFlowMap(9999)+"\nq:\n  <<: *x\n")
	if got := yamlDepthOf(merge); got != MaxDepth+1 {
		t.Fatalf("the merge of a nested mapping is %d deep, want %d", got, MaxDepth+1)
	}
	if library := yamlByTheLibraryAlone(merge); library.err != nil || library.failed != nil {
		t.Errorf("the library does not decode the merge over the bound, it makes %s of it", library.text())
	}
	if ours := yamlWith(merge, yamlLargeMapping, nil); !isYAMLTooDeep(ours.err) {
		t.Errorf("the merge over the bound is refused with %s, want the depth", ours.text())
	}

	// A hidden chain through a sequence, a mapping and a merge: the library,
	// which decodes a chain of this shape by re-reading each anchor in place,
	// refuses it for its aliases well before the bound; up to the bound the
	// exporter refuses it the same way, and over it for its depth instead.
	for _, kind := range []string{"seq", "map", "merge"} {
		// 9998 links come to a depth of exactly the bound, a link being one
		// level, and 9999 to one over it.
		at := parsedYAML(t, yamlAliasChain(kind, 9998))
		if got := yamlDepthOf(at); got != MaxDepth {
			t.Fatalf("the %s chain at the bound is %d deep, want %d", kind, got, MaxDepth)
		}
		// The library is not run on this chain under the race detector, where
		// the exporter's answer is held to the refusal the library is known to
		// make of it. The library compares every two of the chain's 9,999 keys
		// before it decodes any, for each of the three chains a quarter of a
		// second in a plain run and two and a half under the detector, which
		// runs every test twice; and the chain cannot be shorter there, as
		// other tests' inputs are, since its length is the bound. What stands
		// in for the library is no measurement but one refusal, the same in
		// every build: the plain run holds the exporter's answer to the
		// library's, and the run under the detector holds the same answer to
		// that refusal, so a refusal that were not the library's would fail
		// the one or the other.
		library, ours := yamlOutcome{err: errYAMLAliasing}, yamlWith(at, yamlLargeMapping, nil)
		if !raceDetector {
			library = yamlByTheLibraryAlone(at)
		}
		if isYAMLTooDeep(ours.err) {
			t.Errorf("the %s chain at the bound is refused for its depth, want the library's %s", kind, library.text())
		}
		if !library.same(ours) {
			t.Errorf("the %s chain at the bound is %s, and the library makes %s of it", kind, ours.text(), library.text())
		}

		over := parsedYAML(t, yamlAliasChain(kind, 9999))
		if got := yamlDepthOf(over); got != MaxDepth+1 {
			t.Fatalf("the %s chain over the bound is %d deep, want %d", kind, got, MaxDepth+1)
		}
		if ours := yamlWith(over, yamlLargeMapping, nil); !isYAMLTooDeep(ours.err) {
			t.Errorf("the %s chain over the bound is refused with %s, want the depth", kind, ours.text())
		}
	}
}

// A YAML document without aliases is nested as deep as it is written, which
// the one look through it finds: it is not gone through a second time for its
// depth, and one within the bound is decoded as it always was. 9,990 levels of
// sequences, which the library decodes, the exporter decodes the same; and a
// document with an anchor that nothing is an alias of, whose depth a second
// look would have kept, is found as deep as it is without one.
func TestADeeplyNestedYAMLDocumentWithoutAliasesIsDecodedAsBefore(t *testing.T) {
	root := parsedYAML(t, strings.Repeat("- ", 9990)+"x\n")
	learnt := yamlLearnt{large: yamlLargeMapping}
	learnt.learn(root, true)
	if learnt.aliases || learnt.nested != 9990 || learnt.inside != 0 {
		t.Fatalf("a document of 9990 sequences is found with an alias (%v), nested %d deep, and looked through to %d levels from its end", learnt.aliases, learnt.nested, learnt.inside)
	}
	library, ours := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, nil)
	if isYAMLTooDeep(ours.err) {
		t.Fatalf("a document of 9990 sequences and no alias is refused for its depth: %v", ours.err)
	}
	if !library.same(ours) {
		t.Errorf("the nested document decodes to %s, and the library makes %s of it", ours.text(), library.text())
	}

	root = parsedYAML(t, "a: &unused [[1], {b: [2, [3]]}]\nc: {d: 4}\n")
	learnt = yamlLearnt{large: yamlLargeMapping}
	learnt.learn(root, true)
	if learnt.tooDeep(root) || learnt.nested != 5 || learnt.depths != nil {
		t.Errorf("a document nested 5 deep with no alias is found nested %d deep, and the depths of %d anchors were added up for it, want none", learnt.nested, len(learnt.depths))
	}
	if depth := learnt.deepest(root); depth != 5 || len(learnt.depths) != 1 {
		t.Errorf("gone through for its aliases the document is %d deep, with the depths of %d anchors kept, want 5 and 1", depth, len(learnt.depths))
	}
}

// A self-containing YAML anchor has no finite depth: the depth is added up
// without looping on it, counting the anchor met again as nothing, and the
// document is left to the refusal it had, the library's `anchor 'a' value
// contains itself`, rather than the depth. It is so for an anchor that holds
// an alias of itself alone and for one at the end of a chain.
func TestASelfContainingYAMLAnchorKeepsItsError(t *testing.T) {
	documents := []string{
		"a: &a [*a]\nb: *a\n",
		"a: &a {k: *a}\n",
		"a0: &a0 [x]\na1: &a1 [*a0, *a1]\nb: *a1\n",
	}
	for _, document := range documents {
		root := parsedYAML(t, document)
		// The adding up terminates (it would hang or overflow on a cycle it
		// did not break).
		_ = yamlDepthOf(root)
		library, ours := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, nil)
		if isYAMLTooDeep(ours.err) {
			t.Errorf("%q is refused for its depth, want the library's %s", document, library.text())
		}
		if !library.same(ours) {
			t.Errorf("%q is %s, and the library makes %s of it", document, ours.text(), library.text())
		}
	}
}

// A YAML document whose aliases expand to too much, but that is not nested
// deep, keeps the library's `document contains excessive aliasing`: nine
// levels of nine aliases each, which expand to the ninth power but are only
// eighteen deep, are refused for their aliases as they were, not for their
// depth.
func TestExcessivelyAliasedShallowYAMLDocumentsKeepTheirError(t *testing.T) {
	var b strings.Builder
	b.WriteString("a0: &a0 [x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [", i, i)
		for j := 0; j < 9; j++ {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*a%d", i-1)
		}
		b.WriteString("]\n")
	}
	root := parsedYAML(t, b.String())
	if got := yamlDepthOf(root); got > MaxDepth {
		t.Fatalf("the shallow document is %d deep, which is over the bound", got)
	}
	library, ours := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, nil)
	if library.err == nil || !strings.Contains(library.err.Error(), "excessive aliasing") {
		t.Fatalf("the library does not refuse the document for its aliases, it makes %s of it", library.text())
	}
	if !library.same(ours) {
		t.Errorf("the document is %s, and the library makes %s of it", ours.text(), library.text())
	}
}

// The depth bound refuses no document of the differential corpus: none of the
// repository's YAML files, the error tests' documents, those written for the
// forms of a merge, a key and an alias, or the random ones comes near a depth
// of 10,000, with or without an alias, so the bound changes none of them. And
// the two ways the depth is found agree: a document without an alias is as
// deep in the one look through it as it is when its nodes are added up.
func TestTheDepthBoundRefusesNoDocumentOfTheDifferentialCorpus(t *testing.T) {
	documents := append(append(yamlFixtureDocuments(t), hostileYAMLDocuments()...), yamlWrittenDocuments()...)
	maker := yamlMaker{random: rand.New(rand.NewPCG(25, 26))} //nolint:gosec // documents for a test
	generated := 3000
	if raceDetector {
		generated = 1000
	}
	for range generated {
		documents = append(documents, maker.document())
	}
	var deepest uint64
	aliased, plain := 0, 0
	for _, document := range documents {
		root, _ := yamlDocumentOf([]byte(document))
		if root == nil {
			continue
		}
		learnt := yamlLearnt{large: yamlLargeMapping}
		learnt.learn(root, true)
		if learnt.tooDeep(root) {
			t.Errorf("%.120q is refused for its depth", document)
		}
		depth := learnt.deepest(root)
		deepest = max(deepest, depth)
		if learnt.aliases {
			aliased++
		} else {
			plain++
		}
		if depth > 100 || learnt.inside != 0 || depth < uint64(learnt.nested) || !learnt.aliases && depth != uint64(learnt.nested) { //nolint:gosec // a count of levels
			t.Errorf("%.120q is %d deep through its aliases and %d as it is written (alias: %v), with %d levels left open", document, depth, learnt.nested, learnt.aliases, learnt.inside)
		}
	}
	if deepest == 0 || aliased < 100 || plain < 500 {
		t.Fatalf("the corpus has %d documents with an alias and %d without, the deepest %d deep", aliased, plain, deepest)
	}
	t.Logf("the deepest document of the corpus is %d deep, the bound is %d; %d documents have an alias and %d none", deepest, MaxDepth, aliased, plain)
}

// With the bound in force, decoding the deepest document it accepts grows the
// goroutine stack by a few megabytes, not the tens or hundreds a chain past
// the bound would, and a chain of 400,000 links — a stack of hundreds of
// megabytes were it decoded — is refused with a stack of a few kilobytes and
// in time linear in it, the depth added up without a call for each link.
// Measured on a fresh goroutine, as the survey's stack is; skipped under the
// race detector, which keeps no such stack.
//
// A level of the bound is a sequence or a mapping, and what the library calls
// itself for on the way to one is the most where each is a mapping merged
// into the next through an alias: 9,996 of those, hidden from the library's
// own count of aliases in a mapping with a key written twice, are nested to
// the bound and decoded by the library in a stack grown to 16 MB and no
// further, and one more is refused for its depth in a shallow one.
func TestAYAMLDocumentAcceptedAtTheDepthBoundIsDecodedInABoundedStack(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector keeps a stack of its own")
	}
	accepted := "p: &x " + yamlFlowSeq(9999) + "\nq: *x\n"
	if got := yamlDepthOf(parsedYAML(t, accepted)); got != MaxDepth {
		t.Fatalf("the accepted document is %d deep, want the bound of %d", got, MaxDepth)
	}
	var err error
	grown := yamlStackOf(func() { _, err = decodeYAML([]byte(accepted)) })
	if err != nil {
		t.Fatalf("the accepted document is not decoded: %v", err)
	}
	if grown > 8<<20 {
		t.Errorf("decoding the deepest accepted document grew the stack by %d kB, want under 8 MB", grown>>10)
	}

	big := yamlAliasChain("seq", 400000)
	var refused error
	grown = yamlStackOf(func() { _, refused = decodeYAML([]byte(big)) })
	if !isYAMLTooDeep(refused) {
		t.Fatalf("the 400,000-link chain is refused with %v, want the depth", refused)
	}
	if grown > 1<<20 {
		t.Errorf("refusing the 400,000-link chain grew the stack by %d kB, want under 1 MB", grown>>10)
	}

	merges := func(links int) string {
		var doc strings.Builder
		fmt.Fprintf(&doc, "pad: [%s1]\nx:\n  d: 1\n  d: 2\n  c:\n  - &a0 {k: x}\n", strings.Repeat("1,", links/20))
		for i := 1; i <= links; i++ {
			fmt.Fprintf(&doc, "  - &a%d {<<: *a%d}\n", i, i-1)
		}
		fmt.Fprintf(&doc, "y: *a%d\n", links)
		return doc.String()
	}
	costly := merges(9996)
	if got := yamlDepthOf(parsedYAML(t, costly)); got != MaxDepth {
		t.Fatalf("the chain of merges is %d deep, want the bound of %d", got, MaxDepth)
	}
	grown = yamlStackOf(func() { _, err = decodeYAML([]byte(costly)) })
	if err == nil || isYAMLTooDeep(err) || !strings.Contains(err.Error(), `mapping key "d" already defined`) {
		t.Fatalf("the chain of merges at the bound is decoded with %v, want the library's refusal of the key written twice", err)
	}
	if grown > 17<<20 {
		t.Errorf("decoding the chain of merges at the bound grew the stack by %d kB, want no more than 16 MB", grown>>10)
	}
	grown = yamlStackOf(func() { _, refused = decodeYAML([]byte(merges(9997))) })
	if !isYAMLTooDeep(refused) {
		t.Fatalf("the chain of merges one over the bound is refused with %v, want the depth", refused)
	}
	if grown > 1<<20 {
		t.Errorf("refusing the chain of merges one over the bound grew the stack by %d kB, want under 1 MB", grown>>10)
	}
}

// yamlHiddenChain is a chain of anchors the library decodes in a stack as
// deep as the chain is long: they are written in a mapping with a key written
// twice, which the library reads no further, each a sequence of an alias of
// the one before, so that nothing expands them but the one alias of the last,
// outside; and beside enough plain items that the chain is no more of the
// document than the library lets aliases be.
func yamlHiddenChain(links int) string {
	var doc strings.Builder
	doc.WriteString("pad: [")
	for range links / 20 {
		doc.WriteString("1,")
	}
	doc.WriteString("1]\n")
	doc.WriteString(yamlAnchorsInAMappingWrittenTwice(links))
	return doc.String()
}

// The stack the bound is there for is one the library took: a chain of
// 20,000 anchors, 419 kB of body, each expanded inside the one after it by
// the alias of the last, is decoded by the library in a call a link, a
// stack of some 16 MB, and one of 100,000 in 67 MB; a few hundred thousand,
// within a response of the size the default limit allows, are hundreds of
// megabytes, and past a gigabyte the process ends, which no recover catches.
// The document is now refused for its depth before the library is given it,
// in a stack that does not grow with the chain.
func TestAChainOfAliasesTheLibraryDecodedInADeepStackIsRefusedInAShallowOne(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector changes how much stack a call takes")
	}
	body := []byte(yamlHiddenChain(20000))
	var root yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(body)).Decode(&root); err != nil {
		t.Fatal(err)
	}
	library := yamlStackOf(func() {
		var v any
		_ = root.Decode(&v)
	})
	if library < 4<<20 {
		t.Fatalf("the library decodes the chain in a stack of %d kB: the document no longer shows what the bound is for", library>>10)
	}
	var err error
	now := yamlStackOf(func() { _, err = decodeYAML(body) })
	if !isYAMLTooDeep(err) {
		t.Fatalf("the chain is decoded with %v, want it refused for its depth", err)
	}
	if now > 1<<20 {
		t.Errorf("refusing the chain took a stack of %d kB, want under 1 MB; the library's took %d kB", now>>10, library>>10)
	}
}
