package decode

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlAsTheLibrary holds a document, decoded with a mapping large past so
// many keys, against what the library alone makes of it, which it is to be
// to the letter, and returns that. The library is handed no mapping of more
// keys than counts as large.
func yamlAsTheLibrary(t *testing.T, document string, large int) yamlOutcome {
	t.Helper()
	root := parsedYAML(t, document)
	library := yamlByTheLibraryAlone(root)
	got := yamlWith(root, large, func(handed *yaml.Node) {
		if pairs := largestYAMLMapping(handed, map[*yaml.Node]bool{}); pairs > max(large, 1) {
			t.Fatalf("%.300q, large past %d keys: the library is handed a mapping of %d", document, large, pairs)
		}
	})
	if library.failed != nil || !got.same(library) {
		t.Fatalf("%.2000q, large past %d keys, is\n%.600s\nand by the library alone\n%.600s", document, large, got.text(), library.text())
	}
	return got
}

// The items of a sequence beside a large mapping are handed to the library
// 128 at a time, not one by one: 20,000 numbers, one-key mappings, lists of
// three and aliases of a text, each list ended by a mapping of 129 keys, are
// handed in 157 parts and the mapping's two, and decode into what the
// library makes of the document, in no more allocations than a tenth over
// the library's own — a call of the library for each number cost three times
// its allocations — and in no more memory than a tenth over it, but for the
// numbers, which are so small that the list they are gathered in is half as
// much again. A mapping of 129 keys in the middle of the list, and one every
// hundred items, are decoded by the walk between the parts.
//
// And a large mapping beside a list the walk does not decode, 5000 items of
// small mappings under a key, costs the document nothing more where it has
// no alias: nothing is kept of the sequences and mappings the library is
// handed, which was a third more memory.
func TestTheItemsOfAYAMLSequenceBesideALargeMappingAreHandedToTheLibraryTogether(t *testing.T) {
	items := 20000
	if raceDetector {
		items = 4000
	}
	large := "{" + manyYAMLPairs(129, "", ", ") + "}"
	for name, tc := range map[string]struct {
		body  string
		hands int
		// memory is how much of the library's own memory decoding may take.
		memory float64
	}{
		"numbers":           {strings.Repeat("- 1\n", items) + "- " + large + "\n", (items+127)/128 + 2, 1.75},
		"one-key mappings":  {strings.Repeat("- {a: 1}\n", items) + "- " + large + "\n", (items+127)/128 + 2, 1.1},
		"lists of three":    {strings.Repeat("- [1, 2, 3]\n", items) + "- " + large + "\n", (items+127)/128 + 2, 1.1},
		"aliases of a text": {"- &s text\n" + strings.Repeat("- *s\n", items) + "- " + large + "\n", (items+1+127)/128 + 2, 1.75},
		"a large mapping in the middle": {
			strings.Repeat("- 1\n", items/2) + "- " + large + "\n" + strings.Repeat("- 1\n", items/2), 2*((items/2+127)/128) + 2, 1.75,
		},
		"a large mapping every hundred": {strings.Repeat(strings.Repeat("- 1\n", 100)+"- "+large+"\n", items/100), items / 100 * 3, 1.75},
		"small mappings under a key":    {typicalYAML(items/4) + "index: " + large + "\n", 4, 1.05},
	} {
		root := parsedYAML(t, tc.body)
		if way := yamlWayOf(root, yamlLargeMapping); way != yamlByParts {
			t.Errorf("%s: the document is decoded the way %d", name, way)
			continue
		}
		hands, most := 0, 0
		got := yamlWith(root, yamlLargeMapping, func(handed *yaml.Node) {
			hands++
			if handed.Kind == yaml.SequenceNode {
				most = max(most, len(handed.Content))
			}
		})
		library := yamlByTheLibraryAlone(root)
		if !got.same(library) || got.err != nil {
			t.Errorf("%s: the document is\n%.300s\nand by the library alone\n%.300s", name, got.text(), library.text())
		}
		if hands > tc.hands || most > yamlLargeMapping {
			t.Errorf("%s: the library was handed %d parts, the longest a sequence of %d items, want at most %d parts of %d", name, hands, most, tc.hands, yamlLargeMapping)
		}
		if raceDetector {
			continue
		}
		alone := testing.AllocsPerRun(2, func() { yamlByTheLibraryAlone(root) })
		now := testing.AllocsPerRun(2, func() { yamlWith(root, yamlLargeMapping, nil) })
		if now > 1.1*alone {
			t.Errorf("%s: decoding the document allocates %.0f times, and the library alone %.0f: more than a tenth over it", name, now, alone)
		}
		memoryAlone := allocatedBy(func() { yamlByTheLibraryAlone(root) })
		memory := allocatedBy(func() { yamlWith(root, yamlLargeMapping, nil) })
		if float64(memory) > tc.memory*float64(memoryAlone) {
			t.Errorf("%s: decoding the document allocates %d bytes, and the library alone %d: more than %.2f times as much", name, memory, memoryAlone, tc.memory)
		}
	}
}

// yamlSequenceItems are the items of a sequence that the library is handed
// with their neighbours where no mapping is large past few keys: numbers,
// texts, null written three ways and not written, values with a tag that
// fits and with one of the document's own, a date, small mappings and
// lists, a set and an ordered map, aliases of a text, a mapping and a list,
// a mapping with a merge, and one with a key that is no key, which is a
// problem listed at the end.
var yamlSequenceItems = []string{
	"1", "text", "~", "null", "", `""`, "!!str 5", `!!int "5"`, "!thing x", "!!binary aGVsbG8=", "2024-06-01", "1.5", "true",
	"{a: 1}", "{a: 1, b: [2, 3]}", "[1, [2, 3]]", "[]", "{}", "!!set {a, b}", "!!omap [a: 1]", "!!str [a, b]", "!!null [a]",
	"*s", "*m", "*l", "{c: 3, <<: *m}", "{? !!str [a] : 1, b: 2}", "&again [1, 2]", "*s", "1", "2", "text",
}

// yamlSequenceItemsOfTheWalk are items the walk decodes itself: a mapping
// past 128 keys, an alias of it, a list and a mapping that hold it, and a
// list with a tag.
var yamlSequenceItemsOfTheWalk = []string{"*big", "LARGE", "[1, *big]", "{a: *big}", "[LARGE, 2]", "!!str [*big]", "*big", "LARGE"}

// yamlSequenceItemsRefused are items the document is refused for: a value
// that does not fit its tag, and an anchor that holds itself, alone and
// around a large mapping.
var yamlSequenceItemsRefused = []string{"!!int foo", "&q [1, *q]", "&q [*big, [*q]]", `!!binary "@@@"`}

// A sequence the walk decodes is what the library makes of it, wherever in
// it the items are that the walk decodes itself: sequences of one to six
// items, each either the walk's or the library's in each of the ways that
// can be — the library's drawn from numbers, texts, nulls, tagged values,
// small mappings and lists, aliases, a merge and a key that is listed as a
// problem; the walk's from a large mapping, an alias of it and collections
// that hold it — now and then with an item the document is refused for, so
// that the problems and the refusal come in the library's order; and
// sequences of 126 to 130, 255 to 258 and 385 items with an item of the
// walk's at each place a part of 128 begins or ends at, at two such places,
// and at none. Each is decoded with a mapping large past one, two, four and
// 128 keys, which is how many items a part has.
func TestAYAMLSequenceOfItemsOfTheWalkAndOfTheLibraryIsWhatTheLibraryMakesOfIt(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 129)) //nolint:gosec // documents for a test
	large := "{" + manyYAMLPairs(129, "", ", ") + "}"
	head := "s: &s text\nm: &m {a: 1, b: 2}\nl: &l [1, 2]\nbig: &big " + large + "\nseq:\n"
	item := func(of []string) string {
		return "- " + strings.ReplaceAll(of[random.IntN(len(of))], "LARGE", large) + "\n"
	}
	var documents []string
	fills := 4
	if raceDetector {
		fills = 1
	}
	for length := 1; length <= 6; length++ {
		for ofTheWalk := range 1 << length {
			for range fills {
				var doc strings.Builder
				doc.WriteString(head)
				for at := range length {
					switch {
					case random.IntN(40) == 0:
						doc.WriteString(item(yamlSequenceItemsRefused))
					case ofTheWalk&(1<<at) != 0:
						doc.WriteString(item(yamlSequenceItemsOfTheWalk))
					default:
						doc.WriteString(item(yamlSequenceItems))
					}
				}
				documents = append(documents, doc.String()+"after: 1\n")
			}
		}
	}
	short := len(documents)
	for _, length := range []int{126, 127, 128, 129, 130, 255, 256, 257, 258, 385} {
		places := []int{-1}
		for at := range length {
			if in := at % yamlLargeMapping; in <= 2 || in >= yamlLargeMapping-2 || at == length-1 {
				places = append(places, at)
			}
		}
		for i, first := range places {
			second := places[(i*7+3)%len(places)]
			if raceDetector && i%8 != 0 {
				continue
			}
			for _, two := range []bool{false, true} {
				var doc strings.Builder
				doc.WriteString(head)
				for at := range length {
					switch {
					case at == first || two && at == second:
						doc.WriteString(item(yamlSequenceItemsOfTheWalk))
					case at%9 == 0:
						doc.WriteString(item(yamlSequenceItems))
					default:
						fmt.Fprintf(&doc, "- %d\n", at)
					}
				}
				documents = append(documents, doc.String())
			}
		}
	}
	decoded, refused, problems := 0, 0, 0
	for i, document := range documents {
		bounds := []int{1, 2, 4, yamlLargeMapping}
		switch {
		case i >= short:
			bounds = []int{yamlLargeMapping}
		case raceDetector:
			bounds = []int{2, yamlLargeMapping}
		}
		for _, large := range bounds {
			switch got := yamlAsTheLibrary(t, document, large); {
			case got.err == nil:
				decoded++
			case onlyProblems(got.err):
				problems++
			default:
				refused++
			}
		}
	}
	t.Logf("%d documents: %d decoded, %d with problems, %d refused", len(documents), decoded, problems, refused)
	if !raceDetector && (decoded < 1500 || problems < 100 || refused < 100) {
		t.Errorf("%d documents were decoded, %d had problems listed and %d were refused: the documents do not cover it", decoded, problems, refused)
	}
}

// onlyProblems reports whether an error is the library's list of problems.
func onlyProblems(err error) bool {
	var problems *yaml.TypeError
	return errors.As(err, &problems)
}

// yamlNumbers is a sequence of so many numbers in flow style.
func yamlNumbers(count int) string {
	return "[" + strings.TrimSuffix(strings.Repeat("1, ", count), ", ") + "]"
}

// Aliases beside a large mapping are counted for the parts the library is
// handed as the library counts them itself, so a document whose aliases are
// well under the share the library refuses is decoded whatever part of it
// they are in. The library refuses what it is handed once it has decoded
// more than 1000 nodes, more than 100 of them for an alias, and those are
// more than 99% of all; a part is cut before that, counting the node made
// to hold the part, which the library decodes too. Beside 600 numbers
// that no alias stands for:
//
//   - a mapping of 129 keys and one to seven more whose values are aliases
//     of a list of 150 to 1001 numbers, which a part of two to seven of
//     them, or one alone, is over the bound with, and 130 more of a list of
//     150, 300 and 500;
//   - the same aliases as the items of a list ended by a large mapping;
//   - the alias alone in a mapping and in a list that is no part of a large
//     mapping, where the mapping or the list, its key and the node made for
//     the pair come to 1001 nodes at 995 to 997 numbers;
//   - the alias as a key of a mapping of 129 keys: of a list, which is
//     refused as no key (`invalid map key`) and not for its aliases, where
//     the key and the mapping made for it come to 1001 nodes at 998 numbers;
//     and of a list tagged as text, which is listed as a problem, among the
//     mapping's own keys and in a mapping merged into it.
//
// Each is what the library alone makes of the whole document.
func TestAliasesInThePartsOfALargeYAMLMappingAreCountedAsTheLibraryCountsThem(t *testing.T) {
	plain := "real: " + yamlNumbers(600) + "\n"
	large := manyYAMLPairs(129, "", ", ")
	sizes := []int{150, 199, 200, 300, 332, 333, 499, 500}
	for size := 994; size <= 1001; size++ {
		sizes = append(sizes, size)
	}
	if raceDetector {
		sizes = []int{997}
	}
	decoded, problems, noKeys := 0, 0, 0
	for _, size := range sizes {
		list := "d: &d " + yamlNumbers(size) + "\n"
		text := "d: &d !!str " + yamlNumbers(size) + "\n"
		documents := []string{
			plain + list + "big: {" + large + "}\nm: {k: *d}\n",
			plain + list + "big: {" + large + "}\nm: [*d]\n",
			plain + list + "big: {" + large + "}\nm: {j: 1, k: [*d]}\n",
			plain + list + "m: {k: *d, " + large + "}\n",
			plain + list + "m: {k: {j: *d}, " + large + "}\n",
			plain + list + "m: {? *d : 1, " + large + "}\n",
			plain + list + "m: {" + large + "? *d : 1}\n",
			plain + text + "m: {? *d : 1, " + large + "}\n",
			plain + text + "m: {" + large + "<<: {? *d : 1, z: 26}}\n",
			plain + text + "m: {" + large + "<<: [{y: 25}, {z: 26, ? *d : 1}]}\n",
		}
		counts := []int{1, 2, 3, 4, 5, 7}
		if size == 150 || size == 300 || size == 500 {
			counts = append(counts, 130)
		}
		for _, aliases := range counts {
			var values, items strings.Builder
			for i := range aliases {
				fmt.Fprintf(&values, "a%d: *d, ", i)
				items.WriteString("*d, ")
			}
			documents = append(documents,
				plain+list+"m: {"+large+values.String()+"}\n",
				plain+list+"m: {"+values.String()+large+"}\n",
				plain+list+"m: ["+items.String()+"{"+large+"}]\n",
				plain+list+"m: [{"+large+"}, "+items.String()+"]\n",
			)
		}
		for _, document := range documents {
			switch got := yamlAsTheLibrary(t, document, yamlLargeMapping); {
			case got.err == nil:
				decoded++
			case onlyProblems(got.err) && strings.Contains(got.err.Error(), "cannot unmarshal !!str `` into string"):
				problems++
			case strings.HasPrefix(got.err.Error(), "yaml: invalid map key: []interface {}{1, "):
				noKeys++
			default:
				t.Fatalf("%.300q is refused: %.300v", document, got.err)
			}
		}
	}
	if !raceDetector && (decoded < 200 || problems < 20 || noKeys < 10) {
		t.Errorf("%d documents were decoded, %d had the key listed as a problem and %d were refused for it: the documents do not cover it", decoded, problems, noKeys)
	}
}

// The keys of a mapping with a merge are counted once more, as the library
// decodes them once more to know which keys the merge may not replace:
// after a mapping of 129 keys with a merge, which the walk decodes, a list
// of aliases of a thousand numbers is refused for its aliases from the
// 154th on, and after one of 127 keys with a merge beside a large one,
// which the library is handed whole, from the 182nd, as the library alone
// refuses them, which is one alias further than without those keys counted.
func TestTheKeysOfAYAMLMappingWithAMergeAreCountedOnceMore(t *testing.T) {
	for name, tc := range map[string]struct {
		mappings    string
		refusedFrom int
	}{
		"the walk's":   {"m: {<<: {z: 26}, " + manyYAMLPairs(129, "", ", ") + "}\n", 154},
		"handed whole": {"m: {<<: {z: 26}, " + manyYAMLPairs(127, "", ", ") + "}\nbig: {" + manyYAMLPairs(129, "", ", ") + "}\n", 182},
	} {
		from, to := tc.refusedFrom-3, tc.refusedFrom+2
		if raceDetector {
			from, to = tc.refusedFrom-1, tc.refusedFrom
		}
		for aliases := from; aliases <= to; aliases++ {
			document := "d: &d " + yamlNumbers(999) + "\n" + tc.mappings + "l: [" + strings.Repeat("*d, ", aliases) + "]\n"
			got := yamlAsTheLibrary(t, document, yamlLargeMapping)
			switch {
			case got.err == nil && aliases >= tc.refusedFrom:
				t.Errorf("%s: %d aliases are decoded, want them refused from %d on", name, aliases, tc.refusedFrom)
			case got.err != nil && (aliases < tc.refusedFrom || got.err.Error() != "yaml: document contains excessive aliasing"):
				t.Errorf("%s: %d aliases are refused with %v, want them refused for the aliases from %d on", name, aliases, got.err, tc.refusedFrom)
			}
		}
	}
}

// What an item of a sequence fails with comes before the refusal of a later
// item's aliases, as it did when each item was handed to the library alone.
// A list ended by a mapping of 129 keys holds a sequence of 997 numbers and
// one of 900, some two hundred aliases of the first, a number, and an alias
// of the second, which the number is handed to the library with: with as
// many aliases, 209, as are decoded without that last one and refused with
// it, and a value that does not fit its tag in place of the number, the
// document is refused for the value, as the library alone refuses it, and
// not for the aliases.
func TestAnItemOfAYAMLSequenceFailsBeforeALaterItemsAliasesAreRefused(t *testing.T) {
	document := func(aliases int, before, last string) string {
		return "- &t " + yamlNumbers(997) + "\n- &u " + yamlNumbers(900) + "\n" + strings.Repeat("- *t\n", aliases) + "- " + before + "\n" + last + "- {" + manyYAMLPairs(129, "", ", ") + "}\n"
	}
	from := 206
	if raceDetector {
		from = 209
	}
	for aliases := from; aliases <= 212; aliases++ {
		if yamlAsTheLibrary(t, document(aliases, "1", "- *u\n"), yamlLargeMapping).err == nil {
			continue
		}
		if got := yamlAsTheLibrary(t, document(aliases, "1", ""), yamlLargeMapping); got.err != nil {
			t.Fatalf("%d aliases are refused without the last: %v", aliases, got.err)
		}
		got := yamlAsTheLibrary(t, document(aliases, "!!int foo", "- *u\n"), yamlLargeMapping)
		if want := "yaml: cannot decode !!str `foo` as a !!int"; got.err == nil || got.err.Error() != want {
			t.Errorf("with a value that does not fit its tag after %d aliases the document is refused with %v, want %q", aliases, got.err, want)
		}
		return
	}
	t.Error("206 to 212 aliases are decoded: the documents do not cover a refusal")
}
