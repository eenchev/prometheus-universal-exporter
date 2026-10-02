package model

import (
	"maps"
	"testing"

	"gopkg.in/yaml.v3"
)

// DecodedEntries gives each key of a mapping the value the decoder gives it:
// the mapping's own over a merged one, wherever the merge key stands; the
// first of a list of merges; a merged mapping's own over what it merges in
// itself. It is held to the decoder here, document by document, since a
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
			if entry.Key.ShortTag() == nullTag {
				continue
			}
			walked[entry.Key.Value] = entry.Value.Value
			if entry.Value.ShortTag() == nullTag {
				walked[entry.Key.Value] = ""
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
