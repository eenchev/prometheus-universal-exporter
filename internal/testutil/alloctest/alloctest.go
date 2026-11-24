// Package alloctest measures what a function allocates, for the tests that
// bound it or compare it with what another function allocates. It is
// imported only by tests and imports nothing from the module, so the tests
// of every package can use it.
//
// The runtime counts the allocations of the whole process. While a test
// measures a function, goroutines that earlier tests left behind are still
// running (a test server closing its connections, a timer, a worker of the
// Python pool, a log handler), and so is the garbage collector, which starts
// its workers at the process's first collection and empties sync.Pool at
// every one. What they allocate meanwhile is counted with the function's
// own. A single count taken with testing.AllocsPerRun is therefore right on
// a quiet machine and occasionally too high on a busy one, especially under
// the race detector and in a shuffled run.
//
// The other goroutines can only add to the count, never subtract from it. A
// measurement here is therefore made several times, and the lowest result is
// the answer: the function's own cost, or closer to it than any single
// measurement.
//
// The race detector changes what is allocated and how long a test takes, so
// the package also says whether the tests were built with it (RaceDetector)
// and gives a test the smaller size to work on there (UnlessRaced).
package alloctest

import "runtime"

// attempts is how many times a measurement is made, unless one before the
// last is already within the bound its caller gave.
const attempts = 5

// Allocations is what one call of f costs: how many allocations, as
// testing.AllocsPerRun counts them (f is called once uncounted and then runs
// times, on one processor, and the count is divided by runs as a whole
// number), and how many bytes they take. Each is the lowest of five such
// measurements, before each of which the collector runs and the other
// goroutines are allowed to run.
func Allocations(runs int, f func()) (perRun float64, bytesPerRun uint64) {
	mallocs, bytes := least(runs, f, func(uint64, uint64) bool { return false })
	return float64(mallocs), bytes
}

// Once is a single such measurement instead of the lowest of five: what one
// call of f costs plus whatever the rest of the process allocated meanwhile,
// so never less than the cost, and occasionally more. It is for a cost that
// a test only compares another against by a fraction of it (a refusal that
// must cost less than half of what making every series does), which a
// little extra does not affect, and which takes too long to measure five
// times.
func Once(runs int, f func()) (perRun float64, bytesPerRun uint64) {
	mallocs, bytes := least(runs, f, func(uint64, uint64) bool { return true })
	return float64(mallocs), bytes
}

// AllocsAtMost is the allocations of one call of f, as Allocations counts
// them, for a test that wants no more than most. The first measurement
// within the bound is the answer, so a quiet machine pays for one
// measurement; where none is within it, the answer is the lowest of five.
func AllocsAtMost(runs int, most float64, f func()) float64 {
	mallocs, _ := least(runs, f, func(mallocs, _ uint64) bool { return float64(mallocs) <= most })
	return float64(mallocs)
}

// BytesAtMost is the bytes that one call of f allocates, as Allocations
// counts them, for a test that wants no more than most. The first
// measurement within the bound is the answer; where none is within it, the
// answer is the lowest of five.
func BytesAtMost(runs int, most uint64, f func()) uint64 {
	_, bytes := least(runs, f, func(_, bytes uint64) bool { return bytes <= most })
	return bytes
}

// least measures runs calls of f up to attempts times, and returns the
// lowest allocations and the lowest bytes per call that it saw, stopping at
// the first measurement after which they are enough.
//
// Before each measurement the collector runs, so that a collection the
// process was about to make (its first, with the workers it starts) is not
// made within the count. The other goroutines are then allowed to run, and f
// is called once, which refills the pools that the collection emptied.
func least(runs int, f func(), enough func(mallocs, bytes uint64) bool) (mallocs, bytes uint64) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	mallocs, bytes = ^uint64(0), ^uint64(0)
	var before, after runtime.MemStats
	for range attempts {
		runtime.GC()
		runtime.Gosched()
		f()
		runtime.ReadMemStats(&before)
		counted := uint64(0)
		for range runs {
			f()
			counted++
		}
		runtime.ReadMemStats(&after)
		mallocs = min(mallocs, (after.Mallocs-before.Mallocs)/counted)
		bytes = min(bytes, (after.TotalAlloc-before.TotalAlloc)/counted)
		if enough(mallocs, bytes) {
			break
		}
	}
	return mallocs, bytes
}
