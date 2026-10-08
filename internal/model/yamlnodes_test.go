package model

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// DecodedEntries gives each key of a mapping the value the decoder gives it:
// the mapping's own over a merged one, wherever the merge key stands; the
// first of a list of merges; a merged mapping's own over what it merges in
// itself; a key that is an alias as the key it names, which keeps out what
// that key would, where it kept out nothing. It is held to the decoder here,
// document by document, since a
// check that walks a mapping's values must look at the ones that are used:
// the walk once took every merged value for used, and refused a bad one in
// an anchor that the mapping overrode.
func TestDecodedEntriesAreTheValuesTheDecoderUses(t *testing.T) {
	for name, document := range map[string]string{
		"its own key after the merge":       "a: &a {k: 1, j: 1}\nm: {<<: *a, k: 0}\n",
		"its own key before the merge":      "a: &a {k: 1, j: 1}\nm: {k: 0, <<: *a}\n",
		"a list of merges":                  "a: &a {k: 1, j: 1}\nb: &b {k: 2, l: 2}\nm: {<<: [*a, *b]}\n",
		"the list the other way":            "a: &a {k: 1, j: 1}\nb: &b {k: 2, l: 2}\nm: {<<: [*b, *a]}\n",
		"a merge in a merged mapping":       "a: &a {k: 1, j: 1}\nb: &b {<<: *a, k: 2, l: 2}\nm: {<<: *b}\n",
		"a nested merge before a later one": "a: &a {k: 1, j: 1}\nb: &b {<<: *a, l: 2}\nc: &c {k: 3}\nm: {<<: [*b, *c]}\n",
		"a nested merge after an earlier":   "a: &a {k: 1, j: 1}\nb: &b {<<: *a, l: 2}\nc: &c {k: 3}\nm: {<<: [*c, *b]}\n",
		"a mapping written in the merge":    "m: {<<: {k: 1, j: 1}, k: 0}\n",
		"a quoted key of digits":            "a: &a {'1': 1}\nm: {'1': 0, <<: *a}\n",
		"keys YAML does not read as text":   "a: &a {1: 1, true: 1}\nm: {1: 0, true: 0, <<: *a}\n",
		"a null key merged in":              "a: &a {~: 1, k: 1}\nm: {<<: *a, k: 0, j: 5}\n",
		"its own null value":                "a: &a {k: 1}\nm: {k: ~, <<: *a}\n",
		"no merge at all":                   "m: {k: 1, j: 2}\n",
		"an alias key after the merge":      "x: &x k\na: &a {k: 1, j: 1}\nm: {<<: *a, *x : 0}\n",
		"an alias key of a merged mapping":  "x: &x k\na: &a {*x : 1, j: 1}\nb: &b {k: 2}\nm: {<<: [*a, *b]}\n",
		"an alias key overridden":           "x: &x k\na: &a {*x : 1, j: 1}\nm: {k: 0, <<: *a}\n",
		"an alias key of digits":            "x: &x 1\na: &a {1: 1}\nm: {*x : 0, <<: *a}\n",
		"an alias key of a quoted key":      "x: &x '1'\na: &a {1: 1}\nm: {*x : 0, <<: *a}\n",
	} {
		var decoded struct {
			M map[string]string `yaml:"m"`
		}
		var doc struct {
			M yaml.Node `yaml:"m"`
		}
		if err := yaml.Unmarshal([]byte(document), &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// A null key is dropped, and a later entry of a key replaces an
		// earlier, as in the decoder's map.
		walked := map[string]string{}
		for _, entry := range DecodedEntries(&doc.M) {
			key := resolveAlias(entry.Key)
			if key.ShortTag() == nullTag {
				continue
			}
			walked[key.Value] = entry.Value.Value
			if entry.Value.ShortTag() == nullTag {
				walked[key.Value] = ""
			}
		}
		if !maps.Equal(walked, decoded.M) {
			t.Errorf("%s: the entries give %v, the decoder %v", name, walked, decoded.M)
		}
	}
	// An overridden key is in no entry, and a null key in every one of its.
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("a: &a {k: 1, ~: 1}\nm: {<<: *a, k: 0, ~: 2}\n"), &doc); err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, entry := range DecodedEntries(doc.Content[0].Content[3]) {
		lines = append(lines, entry.Key.Line)
	}
	if len(lines) != 3 || lines[0] != 2 || lines[1] != 2 || lines[2] != 1 {
		t.Fatalf("the entries are at lines %v, want the mapping's k and ~ at 2 and the anchor's ~ at 1", lines)
	}
	if entries := DecodedEntries(nil); entries != nil {
		t.Fatalf("no mapping: %v", entries)
	}
}

// decodedEntriesAsItWas is DecodedEntries before a key that is an alias was
// read as the key it names, kept as the oracle of the mappings that have no
// such key.
func decodedEntriesAsItWas(n *yaml.Node) []MappingEntry {
	var entries []MappingEntry
	taken := map[string]bool{}
	visiting := map[*yaml.Node]bool{}
	var collect func(n *yaml.Node, merged bool)
	collect = func(n *yaml.Node, merged bool) {
		n = resolveAlias(n)
		if n == nil || n.Kind != yaml.MappingNode || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		var merge *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], resolveAlias(n.Content[i+1])
			if key.Kind == yaml.ScalarNode && key.Tag == mergeTag {
				merge = value
				continue
			}
			if tag := key.ShortTag(); key.Kind == yaml.ScalarNode && tag != nullTag {
				if merged && taken[key.Value] {
					continue
				}
				if merged || tag == stringTag {
					taken[key.Value] = true
				}
			}
			entries = append(entries, MappingEntry{key, value})
		}
		if merge != nil && merge.Kind == yaml.SequenceNode {
			for _, each := range merge.Content {
				collect(each, true)
			}
			return
		}
		collect(merge, true)
	}
	collect(n, false)
	return entries
}

// Over generated mappings without a key that is an alias — own keys, keys
// that are not text, null keys, values that are aliases, merges of one
// mapping, of a list and of mappings that merge in turn, merge keys before
// and after — DecodedEntries gives the entries it gave, node for node.
func TestDecodedEntriesWithoutAnAliasKeyAreTheEntriesTheyWere(t *testing.T) {
	random := rand.New(rand.NewPCG(117, 3))
	keys := []string{"k", "j", "'k'", "1", "'1'", "true", "~", "l"}
	mapping := func(merges []string) string {
		var parts []string
		for range random.IntN(4) {
			parts = append(parts, keys[random.IntN(len(keys))]+": "+[]string{"0", "~", "*v", "x"}[random.IntN(4)])
		}
		if len(merges) > 0 && random.IntN(4) > 0 {
			merge := "<<: " + merges[random.IntN(len(merges))]
			if random.IntN(2) == 0 {
				merge = "<<: [" + strings.Join(merges, ", ") + "]"
			}
			parts = slices.Insert(parts, random.IntN(len(parts)+1), merge)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	compared := 0
	for round := range 2000 {
		var document strings.Builder
		document.WriteString("v: &v 7\n")
		var anchors []string
		for i := range 3 {
			fmt.Fprintf(&document, "a%d: &a%d %s\n", i, i, mapping(anchors))
			anchors = append(anchors, fmt.Sprintf("*a%d", i))
		}
		fmt.Fprintf(&document, "m: %s\n", mapping(anchors))
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(document.String()), &doc); err != nil {
			continue
		}
		m := doc.Content[0].Content[len(doc.Content[0].Content)-1]
		if got, was := DecodedEntries(m), decodedEntriesAsItWas(m); !slices.Equal(got, was) {
			t.Fatalf("round %d, %s: the entries are %v, and were %v", round, document.String(), got, was)
		}
		compared++
	}
	if compared < 1500 {
		t.Fatalf("only %d of 2,000 generated documents were YAML", compared)
	}
}
