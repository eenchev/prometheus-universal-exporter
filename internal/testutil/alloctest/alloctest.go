// Package alloctest measures what a function allocates, for the tests that
// bound it or compare it with what another function allocates. It is
// imported only by tests, and imports nothing of the module, so that the
// tests of every package can use it.
//
// What the runtime counts is the allocations of the whole process. While a
// test measures a function, goroutines the tests before it left behind are
// still running - a test server closing its connections, a timer, a worker
// of the Python pool, the handler of a log - and so is the collector, which
// starts its workers at the first collection of the process and empties
// sync.Pool at every one; what they allocate meanwhile is counted with the
// function's own. One count taken with testing.AllocsPerRun is therefore
// right on a quiet machine and too high now and then on a busy one, under
// the race detector and in a shuffled run above all.
//
// The others can only add to the count, never take from it. So a measurement
// here is made several times and the least of them is the answer: the
// function's own cost, or nearer to it than any one of them.
package alloctest

import "runtime"

// attempts is how many times a measurement is made, unless one before the
// last is already within the bound its caller gave.
const attempts = 5

// Allocations is what one call of f costs: how many allocations, as
// testing.AllocsPerRun counts them - f is called once uncounted and then
// runs times, on one processor, and the count is divided by runs as a whole
// number - and how many bytes they are. Each is the least of five such
// measurements, before each of which the collector runs and the other
// goroutines are let run.
func Allocations(runs int, f func()) (perRun float64, bytesPerRun uint64) {
	mallocs, bytes := least(runs, f, func(uint64, uint64) bool { return false })
	return float64(mallocs), bytes
}

// Once is one such measurement and not the least of five: what one call of f
// costs and whatever the rest of the process allocated meanwhile, so never
// less than the cost and now and then more. It is for a cost a test only
// measures another against by a fraction of it - a refusal that must cost
// less than half of what making every series does - which a little more
// makes no difference to, and which takes too long to be measured five
// times.
func Once(runs int, f func()) (perRun float64, bytesPerRun uint64) {
	mallocs, bytes := least(runs, f, func(uint64, uint64) bool { return true })
	return float64(mallocs), bytes
}

// AllocsAtMost is the allocations of one call of f, as Allocations counts
// them, for a test that wants no more than most of them: the first
// measurement within the bound is the answer, so a quiet machine pays for
// one, and the least of five is where none is.
func AllocsAtMost(runs int, most float64, f func()) float64 {
	mallocs, _ := least(runs, f, func(mallocs, _ uint64) bool { return float64(mallocs) <= most })
	return float64(mallocs)
}

// BytesAtMost is the bytes one call of f allocates, as Allocations counts
// them, for a test that wants no more than most of them: the first
// measurement within the bound is the answer, and the least of five where
// none is.
func BytesAtMost(runs int, most uint64, f func()) uint64 {
	_, bytes := least(runs, f, func(_, bytes uint64) bool { return bytes <= most })
	return bytes
}

// least measures runs calls of f up to attempts times and returns the least
// allocations and the least bytes of a call that it saw, stopping at the
// first measurement after which they are enough.
//
// Before each measurement the collector runs, so that a collection the
// process was about to make, its first with the workers it starts, is not
// made within the count; the other goroutines are let run; and f is called
// once, which fills again the pools the collection emptied.
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
