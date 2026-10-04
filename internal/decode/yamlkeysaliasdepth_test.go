package decode

import (
	"bytes"
	"fmt"
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

// yamlDepthOf is how deep the library's decoding recurses through a parsed
// document and its aliases.
func yamlDepthOf(root *yaml.Node) uint64 {
	l := yamlLearnt{large: yamlLargeMapping}
	l.learn(root, true)
	return l.deepest(root)
}

// isYAMLTooDeep reports whether an error is the refusal of a document nested
// through its aliases deeper than it may be written.
func isYAMLTooDeep(err error) bool {
	return err != nil && strings.Contains(err.Error(), "deeper than a YAML document may be written")
}

// A YAML document nested through its aliases deeper than the parser lets one
// be written is refused before anything decodes it, on every way the document
// is decoded, the library's own included. A single alias of a deeply nested
// anchor is a chain the library decodes: at the bound it is decoded into what
// the library makes of it, and one level over the bound the library still
// decodes it, deep stack and all, but the exporter refuses it for its depth.
// The chain runs through a sequence, through a mapping and through a merge
// key, whose value the library decodes through too.
func TestAYAMLDocumentNestedThroughItsAliasesDeeperThanItMayBeWrittenIsRefused(t *testing.T) {
	// A single alias of a deeply nested anchor: the library decodes it (its
	// aliases expand to no more than twice its size), and so does the exporter
	// up to the bound.
	for _, nest := range []func(int) string{yamlFlowSeq, yamlFlowMap} {
		// 9996 levels of nesting under the alias come to a depth of exactly the
		// bound; one level more is over it.
		at := parsedYAML(t, "p: &x "+nest(9996)+"\nq: *x\n")
		if got := yamlDepthOf(at); got != yamlDepthLimit {
			t.Fatalf("the accepted document is %d deep, want the bound of %d", got, yamlDepthLimit)
		}
		library, ours := yamlByTheLibraryAlone(at), yamlWith(at, yamlLargeMapping, nil)
		if !library.same(ours) {
			t.Errorf("at the bound the document decodes to %s, and the library makes %s of it", ours.text(), library.text())
		}

		over := parsedYAML(t, "p: &x "+nest(9998)+"\nq: *x\n")
		if got := yamlDepthOf(over); got <= yamlDepthLimit {
			t.Fatalf("the document over the bound is %d deep, want more than %d", got, yamlDepthLimit)
		}
		if library := yamlByTheLibraryAlone(over); library.err != nil || library.failed != nil {
			t.Errorf("the library does not decode the document over the bound, it makes %s of it", library.text())
		}
		if ours := yamlWith(over, yamlLargeMapping, nil); !isYAMLTooDeep(ours.err) {
			t.Errorf("the document over the bound is refused with %s, want the depth", ours.text())
		}
	}

	// A merge key's value is a node the library decodes through, so a merge of
	// a deeply nested mapping is counted in the depth and refused over the
	// bound, where the library decodes it.
	merge := parsedYAML(t, "p: &x "+yamlFlowMap(9998)+"\nq:\n  <<: *x\n")
	if got := yamlDepthOf(merge); got <= yamlDepthLimit {
		t.Fatalf("the merge of a nested mapping is %d deep, want more than %d", got, yamlDepthLimit)
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
		// 4998 links come to a depth of exactly the bound, 4999 to one over it.
		at := parsedYAML(t, yamlAliasChain(kind, 4998))
		if got := yamlDepthOf(at); got != yamlDepthLimit {
			t.Fatalf("the %s chain at the bound is %d deep, want %d", kind, got, yamlDepthLimit)
		}
		library, ours := yamlByTheLibraryAlone(at), yamlWith(at, yamlLargeMapping, nil)
		if isYAMLTooDeep(ours.err) {
			t.Errorf("the %s chain at the bound is refused for its depth, want the library's %s", kind, library.text())
		}
		if !library.same(ours) {
			t.Errorf("the %s chain at the bound is %s, and the library makes %s of it", kind, ours.text(), library.text())
		}

		over := parsedYAML(t, yamlAliasChain(kind, 4999))
		if got := yamlDepthOf(over); got <= yamlDepthLimit {
			t.Fatalf("the %s chain over the bound is %d deep, want more than %d", kind, got, yamlDepthLimit)
		}
		if ours := yamlWith(over, yamlLargeMapping, nil); !isYAMLTooDeep(ours.err) {
			t.Errorf("the %s chain over the bound is refused with %s, want the depth", kind, ours.text())
		}
	}
}

// A deeply nested YAML document without aliases is nested no deeper than the
// parser let it be written, so it is not looked through for its depth and is
// decoded as it always was: 9,990 levels of sequences, which the library
// decodes, the exporter decodes the same.
func TestADeeplyNestedYAMLDocumentWithoutAliasesIsDecodedAsBefore(t *testing.T) {
	root := parsedYAML(t, strings.Repeat("- ", 9990)+"x\n")
	learnt := yamlLearnt{large: yamlLargeMapping}
	learnt.learn(root, true)
	if learnt.aliases {
		t.Fatal("a document of sequences has an alias")
	}
	library, ours := yamlByTheLibraryAlone(root), yamlWith(root, yamlLargeMapping, nil)
	if isYAMLTooDeep(ours.err) {
		t.Fatalf("a document of 9990 sequences and no alias is refused for its depth: %v", ours.err)
	}
	if !library.same(ours) {
		t.Errorf("the nested document decodes to %s, and the library makes %s of it", ours.text(), library.text())
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
	if got := yamlDepthOf(root); got > yamlDepthLimit {
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
// of 10,000, so the bound changes none of them.
func TestTheDepthBoundRefusesNoDocumentOfTheDifferentialCorpus(t *testing.T) {
	documents := append(append(yamlFixtureDocuments(t), hostileYAMLDocuments()...), yamlWrittenDocuments()...)
	var deepest uint64
	for _, document := range documents {
		root, _ := yamlDocumentOf([]byte(document))
		if root == nil {
			continue
		}
		learnt := yamlLearnt{large: yamlLargeMapping}
		learnt.learn(root, true)
		if !learnt.aliases {
			continue
		}
		depth := learnt.deepest(root)
		if depth > deepest {
			deepest = depth
		}
		if depth > yamlDepthLimit {
			t.Errorf("%.120q is %d deep, which the bound of %d refuses", document, depth, yamlDepthLimit)
		}
	}
	if deepest == 0 {
		t.Fatal("no document of the corpus has an alias to look through the depth of")
	}
	t.Logf("the deepest document of the corpus is %d deep, the bound is %d", deepest, yamlDepthLimit)
}

// With the bound in force, decoding the deepest document it accepts grows the
// goroutine stack by a few megabytes, not the tens or hundreds a chain past
// the bound would, and a chain of 400,000 links — a stack of hundreds of
// megabytes were it decoded — is refused with a stack of a few kilobytes and
// in time linear in it, the depth added up without a call for each link.
// Measured on a fresh goroutine, as the survey's stack is; skipped under the
// race detector, which keeps no such stack.
func TestAYAMLDocumentAcceptedAtTheDepthBoundIsDecodedInABoundedStack(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector keeps a stack of its own")
	}
	accepted := "p: &x " + yamlFlowSeq(9996) + "\nq: *x\n"
	if got := yamlDepthOf(parsedYAML(t, accepted)); got != yamlDepthLimit {
		t.Fatalf("the accepted document is %d deep, want the bound of %d", got, yamlDepthLimit)
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
