package decode

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// sameJSONWritingEveryPlace is sameJSON as it was: the place of every node
// written on the way down, whether or not the node differed.
func sameJSONWritingEveryPlace(old, got any, path string) string {
	switch x := old.(type) {
	case map[string]any:
		y, ok := got.(map[string]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
		for key, value := range x {
			other, has := y[key]
			if !has {
				return fmt.Sprintf("%s: no key %q", path, key)
			}
			if diff := sameJSONWritingEveryPlace(value, other, path+"."+key); diff != "" {
				return diff
			}
		}
	case []any:
		y, ok := got.([]any)
		if !ok || len(x) != len(y) || (x == nil) != (y == nil) {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
		for i := range x {
			if diff := sameJSONWritingEveryPlace(x[i], y[i], fmt.Sprintf("%s[%d]", path, i)); diff != "" {
				return diff
			}
		}
	case float64:
		if y, ok := got.(float64); !ok || math.Float64bits(x) != math.Float64bits(y) {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
	case *big.Int:
		if y, ok := got.(*big.Int); !ok || x.Cmp(y) != 0 {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
	case int, string, bool, nil:
		if old != got {
			return fmt.Sprintf("%s: %#v, was %#v", path, got, old)
		}
	default:
		return fmt.Sprintf("%s: the oracle made a %T", path, old)
	}
	return ""
}

// nestedJSON is a value nested depth deep, lists and mappings in turn, with
// leaf at the bottom.
func nestedJSON(depth int, leaf any) any {
	value := leaf
	for level := depth; level > 0; level-- {
		if level%2 == 0 {
			value = map[string]any{"a": value}
		} else {
			value = []any{value}
		}
	}
	return value
}

// Two values that are the same are found the same without a place being
// written: as deep as a response may nest, the comparison allocates
// nothing, where it wrote the place of every level on the way down, a text
// as long as the level is deep, and so fifty megabytes for the ten thousand
// levels. Under the race detector that was six seconds of the package's
// tests. The detector changes what is allocated, so the count is of a plain
// run; what is compared is the same in both.
func TestTheSameValuesAreComparedWithoutAPlaceBeingWritten(t *testing.T) {
	old, got := nestedJSON(MaxDepth, 1.5), nestedJSON(MaxDepth, 1.5)
	if diff := sameJSON(old, got, "$"); diff != "" {
		t.Fatalf("two values built alike differ: %.200s", diff)
	}
	if was := sameJSONWritingEveryPlace(old, got, "$"); was != "" {
		t.Fatalf("two values built alike differed to the comparison as it was: %.200s", was)
	}
	if alloctest.RaceDetector {
		return
	}
	const most = 4
	if allocations := alloctest.AllocsAtMost(3, most, func() { _ = sameJSON(old, got, "$") }); allocations > most {
		t.Errorf("comparing two values that are the same, nested %d deep, made %v allocations, want no more than %d: a place is written only where they differ", MaxDepth, allocations, most)
	}
	// What it was is measured beside it, so the bound is known to tell the
	// two apart: an allocation a level at the least.
	if was, _ := alloctest.Once(1, func() { _ = sameJSONWritingEveryPlace(old, got, "$") }); was < MaxDepth {
		t.Errorf("the comparison as it was made %v allocations for %d levels: the bound above no longer tells it from the one in use", was, MaxDepth)
	}
}

// jsonNodes counts the nodes of a decoded value, itself included.
func jsonNodes(value any) int {
	nodes := 1
	switch x := value.(type) {
	case map[string]any:
		for _, inner := range x {
			nodes += jsonNodes(inner)
		}
	case []any:
		for _, inner := range x {
			nodes += jsonNodes(inner)
		}
	}
	return nodes
}

// jsonWithOneChange is a copy of value in which the node numbered at, the
// nodes counted from the top and a mapping's keys taken in their order, is
// another: change says which other, by the kind of the node. A copy with
// nothing changed, where at is past the last node, is the value over.
func jsonWithOneChange(value any, at *int, change int) any {
	here := *at == 0
	*at--
	if here {
		switch x := value.(type) {
		case map[string]any:
			switch {
			case change%3 == 0 && len(x) > 0:
				// One key under another name: as many keys, and one missing.
				changed := maps.Clone(x)
				key := slices.Sorted(maps.Keys(x))[change/3%len(x)]
				delete(changed, key)
				changed[key+"\x00other"] = x[key]
				return changed
			case change%3 == 1:
				return append([]any{}, len(x))
			default:
				changed := maps.Clone(x)
				changed["\x00one more"] = nil
				return changed
			}
		case []any:
			if change%2 == 0 {
				return append(slices.Clone(x), nil)
			}
			return map[string]any{}
		case float64:
			if change%2 == 0 {
				return math.Float64frombits(math.Float64bits(x) ^ 1<<63)
			}
			// The next number: a large one is itself with one added.
			return math.Nextafter(x, math.Inf(1))
		case *big.Int:
			return new(big.Int).Add(x, big.NewInt(1))
		case string:
			return x + "x"
		case bool:
			return !x
		case int:
			return float64(x)
		default:
			return "was null"
		}
	}
	switch x := value.(type) {
	case map[string]any:
		changed := make(map[string]any, len(x))
		for _, key := range slices.Sorted(maps.Keys(x)) {
			changed[key] = jsonWithOneChange(x[key], at, change)
		}
		return changed
	case []any:
		changed := make([]any, len(x))
		for i, inner := range x {
			changed[i] = jsonWithOneChange(inner, at, change)
		}
		return changed
	}
	return value
}

// A difference is told where it was told and as it was: over generated
// documents, each with one node changed in turn — a number by its sign bit
// or to the next number, a text, a boolean, a null, a list by its length or its
// kind, a mapping by a key under another name, a key more or its kind — the
// comparison says what the one that wrote every place said, to the letter,
// and of a document beside a copy of itself both say nothing. One node is
// changed at a time, so that there is one difference to find, whatever
// order a mapping's keys are gone through in. Under the race detector it is
// a fifth of the documents.
func TestADifferenceIsToldWhereItWasTold(t *testing.T) {
	g := &jsonGenerator{random: rand.New(rand.NewPCG(2026, 1010))}
	documents := alloctest.UnlessRaced(1500, 300)
	compared, atTheTop, below, missing := 0, 0, 0, 0
	for decoded := 0; decoded < documents; {
		value, err := decodeJSON(g.document())
		if err != nil {
			continue
		}
		decoded++
		nodes := jsonNodes(value)
		for at := range min(nodes, 40) {
			// The nodes changed are spread over the document, a large one's
			// too, and what they are changed to goes through the kinds.
			node, change := at*nodes/min(nodes, 40), decoded+at
			changed := jsonWithOneChange(value, &node, change)
			was, is := sameJSONWritingEveryPlace(value, changed, "$"), sameJSON(value, changed, "$")
			compared++
			if was != is {
				t.Fatalf("document %d with node %d changed: the difference is told as\n%.300s\nand was told as\n%.300s", decoded, at, is, was)
			}
			if is == "" {
				t.Fatalf("document %d with node %d changed is found the same", decoded, at)
			}
			switch {
			case strings.Contains(is, ": no key "):
				missing++
			case strings.HasPrefix(is, "$: "):
				atTheTop++
			default:
				below++
			}
		}
		past := nodes
		same := jsonWithOneChange(value, &past, 0)
		if was, is := sameJSONWritingEveryPlace(value, same, "$"), sameJSON(value, same, "$"); was != "" || is != "" {
			t.Fatalf("document %d beside a copy of itself: %.300q, and it was %.300q", decoded, is, was)
		}
	}
	// The corpus is worth what it is told to be: differences found at the
	// top of a document, below it, and as a key that is not there.
	if compared < documents*3 || atTheTop < documents/2 || below < documents || missing < documents/50 {
		t.Errorf("%d documents were compared with %d changed copies: %d differences at the top, %d below it and %d of a key that is not there, want %d copies, %d, %d and %d at the least", documents, compared, atTheTop, below, missing, documents*3, documents/2, documents, documents/50)
	}
	t.Logf("%d documents, %d changed copies: %d differences at the top, %d below, %d of a missing key", documents, compared, atTheTop, below, missing)
}
