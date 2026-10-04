package decode

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// yamlBoundaryPairs are the pairs of a mapping that are no plain text key
// with a number: of each kind several, written as the pair of a mapping in
// block style, AT standing for the pair's place so that no two are one key,
// and LARGE for a mapping of 129 keys.
var yamlBoundaryPairs = []struct {
	kind  string
	pairs []string
}{
	{"a merge key", []string{"<<: *m", "<<: {key5: merged, extra: 2}", "<<: [*m, {key7: seven, more: 3}]", "<<: *big", "!!merge <<: *m", "<<: [*big, *m]", "<<: {<<: *m, key9: nine}"}},
	{"a null key", []string{"~: of null", "null: 1", `!!null "": [1, 2]`, "~:"}},
	{"a key that is no text", []string{"7AT: seven", "true: yes", "1.5: half", `!!int "5AT": five`, "0x1F: hex", "!!binary aGVsbG8=: bytes", "2024-06-01: a date"}},
	{"an alias key", []string{"*s : of a text", "*n : of a number", "*t : of a tagged text"}},
	{"an alias value", []string{"vAT: *s", "vAT: *m", "vAT: *l", "vAT: *n", "vAT: [*m, *s]"}},
	{"a value the walk decodes", []string{"vAT: *big", "vAT: LARGE", "vAT: [1, *big]", "vAT: {a: *big}", "vAT: [LARGE, *m]"}},
}

// A mapping at the bound the decoding counts by, 128 keys, is what the
// library makes of it whatever is at the places a part of 128 pairs begins
// and ends at: mappings of 127, 128, 129, 255, 256, 257 and 385 pairs, beside
// a mapping of 129 keys, with a merge key — of an alias, a mapping, a
// sequence of both, a mapping of 129 keys, tagged, and a merge within the
// merged — a null key, a key that is no text, an alias key, an alias value
// and a value the walk decodes, each as the first and the second pair of a
// part, as its last two and as the mapping's last, alone and with a pair of
// another kind at the next such place. Compared are the values with their
// types, the maps' among them, against the library alone, which is handed no
// mapping of more than 128 keys.
func TestAYAMLMappingAtTheBoundOfAPartIsWhatTheLibraryMakesOfIt(t *testing.T) {
	large := "{" + manyYAMLPairs(129, "", ", ") + "}"
	head := "s: &s text\nn: &n 7\nt: &t !!str 8\nm: &m {key5: of m, extra: 1}\nl: &l [1, 2]\nbig: &big " + large + "\nmain:\n"
	written, decoded, each := 0, map[string]int{}, map[string]int{}
	for _, size := range []int{127, 128, 129, 255, 256, 257, 385} {
		var places []int
		for at := range size {
			if in := at % yamlLargeMapping; in <= 1 || in >= yamlLargeMapping-2 || at == size-1 {
				places = append(places, at)
			}
		}
		for i, place := range places {
			if raceDetector && i%3 != 0 {
				continue
			}
			for kind, of := range yamlBoundaryPairs {
				// The same with a pair of another kind at the next place; two
				// merge keys would be a key written twice.
				other := yamlBoundaryPairs[(kind+1+(i+size)%(len(yamlBoundaryPairs)-1))%len(yamlBoundaryPairs)]
				next := places[(i+1)%len(places)]
				for _, two := range []bool{false, true} {
					if two && (next == place || raceDetector) {
						continue
					}
					var doc strings.Builder
					doc.WriteString(head)
					for at := range size {
						pair := fmt.Sprintf("key%d: %d", at, at)
						switch {
						case at == place:
							pair = of.pairs[(i+size)%len(of.pairs)]
							each[pair]++
						case two && at == next:
							pair = other.pairs[(i+size+kind)%len(other.pairs)]
							each[pair]++
						}
						pair = strings.ReplaceAll(strings.ReplaceAll(pair, "AT", strconv.Itoa(at)), "LARGE", large)
						fmt.Fprintf(&doc, "  %s\n", pair)
					}
					document := doc.String()
					if way := yamlWayOf(parsedYAML(t, document), yamlLargeMapping); way != yamlByParts {
						t.Fatalf("%d pairs, %s at %d: the document is decoded the way %d", size, of.kind, place, way)
					}
					got := yamlAsTheLibrary(t, document, yamlLargeMapping)
					if got.err != nil {
						t.Fatalf("%d pairs, %s at %d: the document is refused: %.300v", size, of.kind, place, got.err)
					}
					written++
					decoded[of.kind]++
				}
			}
		}
	}
	t.Logf("%d documents: %v", written, decoded)
	for _, of := range yamlBoundaryPairs {
		if decoded[of.kind] < 90 && !raceDetector {
			t.Errorf("%d documents have %s: the documents do not cover it", decoded[of.kind], of.kind)
		}
		for _, pair := range of.pairs {
			if each[pair] < 10 && !raceDetector {
				t.Errorf("%d documents have the pair %q: the documents do not cover it", each[pair], pair)
			}
		}
	}
}
