package alloctest

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// kept holds what the measured functions of these tests allocate, so that
// each of their allocations is one of the heap.
var kept *[64]byte

// neighbour starts a goroutine as the tests before an allocation test leave
// them behind - a test server closing its connections, a worker of the
// Python pool - and returns a function that gives it the processor and waits
// until it gives it back, as a function that reads from a connection does,
// or one that allocates enough to start a collection. The first quiet times
// it is given the processor the goroutine does nothing, each of the busy
// times after those it allocates each times, and then it has finished what
// it was doing and allocates nothing.
func neighbour(t *testing.T, quiet, busy, each int) (yield func()) {
	t.Helper()
	wake, done := make(chan struct{}), make(chan struct{})
	go func() {
		held := make([]*[64]byte, each)
		for range wake {
			if quiet--; quiet < 0 && quiet >= -busy {
				for i := range held {
					held[i] = new([64]byte)
				}
			}
			done <- struct{}{}
		}
	}()
	t.Cleanup(func() { close(wake) })
	return func() {
		wake <- struct{}{}
		<-done
	}
}

// What a function allocates is counted as testing.AllocsPerRun counts it,
// with the bytes: three allocations of 64 bytes are three and 192 bytes, a
// megabyte made at once is one allocation of a megabyte, and a function that
// allocates on every other call allocates, as a whole number, nothing. A
// measurement with a bound gives the same.
func TestWhatAFunctionAllocatesIsCountedForOneCall(t *testing.T) {
	var large []byte
	calls := 0
	for name, tc := range map[string]struct {
		f     func()
		count float64
		bytes uint64
	}{
		"three small allocations": {func() { kept, kept, kept = new([64]byte), new([64]byte), new([64]byte) }, 3, 192},
		"a megabyte at once":      {func() { large = make([]byte, 1<<20) }, 1, 1 << 20},
		"one on every other call": {func() {
			if calls++; calls%2 == 0 {
				kept = new([64]byte)
			}
		}, 0, 32},
		"none": {func() { calls++ }, 0, 0},
	} {
		if single := testing.AllocsPerRun(10, tc.f); single != tc.count {
			t.Errorf("%s: testing.AllocsPerRun counts %v allocations, want %v: the test compares with nothing", name, single, tc.count)
		}
		if count, bytes := Allocations(10, tc.f); count != tc.count || bytes != tc.bytes {
			t.Errorf("%s: %v allocations of %d bytes, want %v of %d", name, count, bytes, tc.count, tc.bytes)
		}
		if count, bytes := Once(10, tc.f); count != tc.count || bytes != tc.bytes {
			t.Errorf("%s: measured once, %v allocations of %d bytes, want %v of %d", name, count, bytes, tc.count, tc.bytes)
		}
		if count := AllocsAtMost(10, tc.count, tc.f); count != tc.count {
			t.Errorf("%s: %v allocations where at most %v are wanted, want %v", name, count, tc.count, tc.count)
		}
		if bytes := BytesAtMost(10, tc.bytes, tc.f); bytes != tc.bytes {
			t.Errorf("%s: %d bytes where at most %d are wanted, want %d", name, bytes, tc.bytes, tc.bytes)
		}
	}
	_ = large
}

// What the other goroutines of the process allocate while a function is
// measured is not counted as the function's: a function of one allocation
// that gives up the processor, beside a goroutine that allocates a hundred
// times each time it is given it until it has finished, is counted by
// testing.AllocsPerRun as 101 allocations a call, which is the failure CI
// saw of a bound of 4, and by Allocations as the one allocation of 64 bytes
// it makes, once the goroutine has finished, which it has not during the
// first four measurements of five, and as much where the goroutine is busy
// during all but the first. Measured once it is 101, as Once says of itself.
func TestWhatOtherGoroutinesAllocateMeanwhileIsNotCounted(t *testing.T) {
	const runs, each = 10, 100
	measured := func(quiet, busy int) func() {
		yield := neighbour(t, quiet, busy, each)
		return func() {
			kept = new([64]byte)
			yield()
		}
	}
	// A measurement is runs calls and one before them.
	const measurement = runs + 1
	if single := testing.AllocsPerRun(runs, measured(0, measurement)); single != 1+each {
		t.Errorf("testing.AllocsPerRun counts %v allocations beside a busy goroutine, want the %d of the function and the goroutine: the test shows nothing", single, 1+each)
	}
	if count, bytes := Once(runs, measured(0, measurement)); count != 1+each || bytes != 64*(1+each) {
		t.Errorf("measured once beside a busy goroutine: %v allocations of %d bytes, want the %d of the function and the goroutine", count, bytes, 1+each)
	}
	if count, bytes := Allocations(runs, measured(0, 4*measurement)); count != 1 || bytes != 64 {
		t.Errorf("beside a goroutine busy for four measurements: %v allocations of %d bytes, want the function's own, 1 of 64", count, bytes)
	}
	if count, bytes := Allocations(runs, measured(measurement, 4*measurement)); count != 1 || bytes != 64 {
		t.Errorf("beside a goroutine busy after the first measurement: %v allocations of %d bytes, want the function's own, 1 of 64", count, bytes)
	}
	if count := AllocsAtMost(runs, 1, measured(0, 4*measurement)); count != 1 {
		t.Errorf("beside a goroutine busy for four measurements: %v allocations where at most 1 is wanted, want the function's own", count)
	}
	if bytes := BytesAtMost(runs, 64, measured(0, 4*measurement)); bytes != 64 {
		t.Errorf("beside a goroutine busy for four measurements: %d bytes where at most 64 are wanted, want the function's own", bytes)
	}
	// Beside one that never finishes, the least that was seen is all that
	// can be said.
	if count, bytes := Allocations(runs, measured(0, attempts*measurement)); count != 1+each || bytes != 64*(1+each) {
		t.Errorf("beside a goroutine busy throughout: %v allocations of %d bytes, want the %d of both", count, bytes, 1+each)
	}
}

// A measurement with a bound stops at the first that is within it, so a
// quiet machine pays for one, as it paid testing.AllocsPerRun: the calls
// counted and one before them. Where a goroutine was busy during the first
// two it is the third, and where the function costs more than the bound it
// is all five, as it is for a measurement without a bound; and Once is one.
func TestAMeasurementWithinItsBoundIsTheLast(t *testing.T) {
	const runs = 10
	calls := 0
	one := func() {
		calls++
		kept = new([64]byte)
	}
	for name, tc := range map[string]struct {
		measure func(f func())
		f       func()
		want    int
	}{
		"allocations within the bound":         {func(f func()) { AllocsAtMost(runs, 1, f) }, one, runs + 1},
		"bytes within the bound":               {func(f func()) { BytesAtMost(runs, 64, f) }, one, runs + 1},
		"measured once":                        {func(f func()) { Once(runs, f) }, one, runs + 1},
		"allocations over the bound":           {func(f func()) { AllocsAtMost(runs, 0, f) }, one, attempts * (runs + 1)},
		"bytes over the bound":                 {func(f func()) { BytesAtMost(runs, 63, f) }, one, attempts * (runs + 1)},
		"no bound":                             {func(f func()) { Allocations(runs, f) }, one, attempts * (runs + 1)},
		"allocations, a neighbour busy at two": {func(f func()) { AllocsAtMost(runs, 1, f) }, nil, 3 * (runs + 1)},
		"bytes, a neighbour busy at two":       {func(f func()) { BytesAtMost(runs, 64, f) }, nil, 3 * (runs + 1)},
	} {
		calls = 0
		f := tc.f
		if f == nil {
			yield := neighbour(t, 0, 2*(runs+1), 100)
			f = func() {
				one()
				yield()
			}
		}
		tc.measure(f)
		if calls != tc.want {
			t.Errorf("%s: the function was called %d times, want %d", name, calls, tc.want)
		}
	}
}

// Before each measurement the collector runs, so that a collection the
// process was about to make - its first above all, which starts the
// collector's workers, a few allocations that were counted as those of the
// first test of a process to allocate a few megabytes - is made before the
// count and not within it: five collections for a measurement without a
// bound, one for one that is within its bound at once.
func TestTheCollectorRunsBeforeEachMeasurement(t *testing.T) {
	collections := func(measure func()) int64 {
		var before, after debug.GCStats
		debug.ReadGCStats(&before)
		measure()
		debug.ReadGCStats(&after)
		return after.NumGC - before.NumGC
	}
	// The collector is kept from running of itself meanwhile.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	f := func() { kept = new([64]byte) }
	if got := collections(func() { Allocations(10, f) }); got != attempts {
		t.Errorf("%d collections during a measurement without a bound, want %d, one before each", got, attempts)
	}
	if got := collections(func() { AllocsAtMost(10, 1, f) }); got != 1 {
		t.Errorf("%d collections during a measurement within its bound, want 1", got)
	}
}

// The count is made on one processor, as testing.AllocsPerRun makes it, and
// the processors the process had are given back afterwards, also when the
// measured function ends its test.
func TestAMeasurementIsMadeOnOneProcessor(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	during := 0
	Allocations(1, func() { during = runtime.GOMAXPROCS(0) })
	if after := runtime.GOMAXPROCS(0); during != 1 || after != 2 {
		t.Errorf("the function ran on %d processors and the process has %d afterwards, want 1 and 2", during, after)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		AllocsAtMost(1, 0, runtime.Goexit)
	}()
	<-ended
	if after := runtime.GOMAXPROCS(0); after != 2 {
		t.Errorf("the process has %d processors after a measured function ended its goroutine, want 2", after)
	}
}

// RaceDetector says what the build says of itself: that the tests were built
// with -race, or were not.
func TestRaceDetectorSaysHowTheTestsWereBuilt(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("the test binary does not say how it was built")
	}
	race := false
	for _, setting := range info.Settings {
		if setting.Key == "-race" {
			race = setting.Value == "true"
		}
	}
	if RaceDetector != race {
		t.Errorf("RaceDetector is %v in a build with -race=%v", RaceDetector, race)
	}
}
