package alloctest

// UnlessRaced returns plain, or raced when the tests were built with the
// race detector. It is how a test sizes what it works on. Under the
// detector, code runs several times slower, and `make test` and CI run every
// test twice with it, so a test over tens of thousands of generated
// documents, or a body of megabytes, costs minutes there that it does not
// cost a plain run. The plain run keeps the size that makes the test
// thorough; the run under the detector, which is there to find data races
// and order-dependent tests, takes a smaller size of the same kind.
//
// The two are sizes of one thing (a count of generated cases, the length of
// a body, the rounds of a loop), never what a test asserts: a test checks
// everything under the detector that it checks without it.
func UnlessRaced[T any](plain, raced T) T {
	if RaceDetector {
		return raced
	}
	return plain
}
