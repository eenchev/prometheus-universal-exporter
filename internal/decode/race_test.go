//go:build race

package decode

// raceDetector says the tests were built with the race detector, under
// which a test that decodes tens of thousands of documents takes many times
// as long, and decodes fewer, and nothing can be said of how much a piece
// of code allocates.
const raceDetector = true
