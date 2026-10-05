package transform

import (
	"maps"
	"slices"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// raceDetector says the tests were built with the race detector, under
// which nothing can be said of how much a piece of code allocates: the
// detector allocates for what it watches, and sync.Pool, which the regexp
// and XPath engines keep their working memory in, hands back only some of
// what it is given.
const raceDetector = alloctest.RaceDetector

// pairTaken says whether a test that holds every item of one list against
// every item of another takes the pair of the items at i and j: every pair,
// and under the race detector, where each pair costs several times as much,
// one in n of them, those of every n-th diagonal. Each item of either list
// is then still held against a part of the other, and none is left out
// where the other list has n items or more.
func pairTaken(i, j, n int) bool {
	return !raceDetector || (i+j)%n == 0
}

// places numbers the keys of a map in their order, for a test that ranges
// over the map, in whatever order that is, and takes a part of it by
// pairTaken: the same part at every run.
func places[V any](of map[string]V) map[string]int {
	at := make(map[string]int, len(of))
	for i, key := range slices.Sorted(maps.Keys(of)) {
		at[key] = i
	}
	return at
}
