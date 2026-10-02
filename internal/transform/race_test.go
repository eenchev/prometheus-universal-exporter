//go:build race

package transform

// raceDetector says the tests were built with the race detector, under
// which nothing can be said of how much a piece of code allocates: the
// detector allocates for what it watches, and sync.Pool, which the regexp
// and XPath engines keep their working memory in, hands back only some of
// what it is given.
const raceDetector = true
