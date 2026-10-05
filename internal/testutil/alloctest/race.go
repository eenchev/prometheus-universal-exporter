//go:build race

package alloctest

// RaceDetector says the tests were built with the race detector. Under it
// the detector allocates for what it watches and sync.Pool hands back only
// some of what it is given, so a bound on allocations that holds without it
// may not hold with it, and a test that decodes tens of thousands of
// documents takes many times as long.
const RaceDetector = true
