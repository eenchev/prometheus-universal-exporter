//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// collectorStatsBefore is collectorStats as it was while it did all of its
// work with the statistics lock held, kept as the oracle of the tests below:
// it copied each collector to read its name, and made its map and its slice
// under the lock, without their size. between, which the former had not, is
// called where collectorStats calls statsReadHook before it takes the lock.
func collectorStatsBefore(s *Server, between func()) (names []string, values map[string]statsValues, stats map[string]*serverStats) {
	followed := s.reconcile()
	if between != nil {
		between()
	}
	s.statsMu.Lock()
	stats = make(map[string]*serverStats)
	for _, c := range followed.config.Collectors { //nolint:gocritic // the oracle copied each collector
		stats[c.Name] = s.statsSinceLocked(followed.generation, c.Name)
		names = append(names, c.Name)
	}
	s.statsMu.Unlock()
	sort.Strings(names)
	values = make(map[string]statsValues, len(names))
	for _, name := range names {
		values[name] = stats[name].snapshot()
	}
	return names, values, stats
}

// collectorStatsBetween is collectorStats with between called once the
// configuration is read and before the statistics lock is taken.
func collectorStatsBetween(s *Server, between func()) (names []string, values map[string]statsValues, stats map[string]*serverStats) {
	hook := func(held bool) {
		if held {
			between()
		}
	}
	statsReadHook.Store(&hook)
	defer statsReadHook.Store(nil)
	return s.collectorStats()
}

// statsRead is what a read of every collector's statistics returned.
type statsRead struct {
	names  []string
	values map[string]statsValues
	stats  map[string]*serverStats
}

// keptStatistics are the statistics server keeps, by collector.
func keptStatistics(server *Server) map[string]*serverStats {
	server.statsMu.Lock()
	defer server.statsMu.Unlock()
	return maps.Clone(server.stats)
}

// sameRead says how two reads of one server differ, one made after the
// other with nothing between them: the names, in their order and a slice or
// none alike, the counters, and the statistics, which are the very same.
func sameRead(got, want statsRead) error {
	if !reflect.DeepEqual(got.names, want.names) {
		return fmt.Errorf("the names are %#v, want %#v", got.names, want.names)
	}
	if !reflect.DeepEqual(got.values, want.values) {
		return fmt.Errorf("the counters are %v, want %v", got.values, want.values)
	}
	if got.stats == nil || !maps.Equal(got.stats, want.stats) {
		return fmt.Errorf("the statistics are %v, want the very same as %v", got.stats, want.stats)
	}
	return nil
}

// sameReadOfATwin says how the reads of two servers that were told the same
// differ, each made as the read took place on the other: the names, the
// counters but for when they began to count, which is when each server made
// them, and of each collector's statistics from when they are its own,
// whether they are retired and whether the server keeps them.
func sameReadOfATwin(got, want statsRead, gotKept, wantKept map[string]*serverStats) error {
	if !reflect.DeepEqual(got.names, want.names) {
		return fmt.Errorf("the names are %#v, want %#v", got.names, want.names)
	}
	if len(got.values) != len(want.values) || len(got.stats) != len(want.stats) || got.stats == nil || got.values == nil {
		return fmt.Errorf("%d counters and %d statistics, want %d and %d", len(got.values), len(got.stats), len(want.values), len(want.stats))
	}
	for name, wantStats := range want.stats {
		gotStats := got.stats[name]
		if gotStats == nil {
			return fmt.Errorf("no statistics of %s", name)
		}
		if gotStats.since != wantStats.since || gotStats.retired.Load() != wantStats.retired.Load() || (gotKept[name] == gotStats) != (wantKept[name] == wantStats) {
			return fmt.Errorf("the statistics of %s are since %d, retired %t, kept %t; want since %d, retired %t, kept %t", name,
				gotStats.since, gotStats.retired.Load(), gotKept[name] == gotStats, wantStats.since, wantStats.retired.Load(), wantKept[name] == wantStats)
		}
		gotValues, wantValues := got.values[name], want.values[name]
		if gotValues.created.IsZero() || gotValues != gotStats.snapshot() {
			return fmt.Errorf("the counters of %s are not those of its statistics", name)
		}
		gotValues.created, wantValues.created = time.Time{}, time.Time{}
		if gotValues != wantValues {
			return fmt.Errorf("the counters of %s are %+v, want %+v", name, gotValues, wantValues)
		}
	}
	return nil
}

// The read of every collector's statistics gives what it gave while it did
// all of its work under the statistics lock. Two servers are told the same,
// over generated sequences of reloads among collectors of twelve names, in
// every order: none of them, which the loader refuses and the server is not
// kept from, one, some, and once two hundred more (forty under the race
// detector), so that collectors are removed, added and brought back. After
// each reload one server is read as now and then as before, and the other as
// before and then as now: the second read of a server gives the names,
// sorted and a slice or none alike, the counters and the very statistics of
// the first, which the server keeps, so each makes the statistics of a
// collector first heard of as the other does and finds those the other
// made; and the two servers give the same, but for when their statistics
// were made. A read with a reload, or two, between its reading of the
// configuration and its taking of the lock gives, on both, the collectors it
// read: those a reload removed with statistics retired and not kept, though
// the second have brought them back, and the others with their own. The
// counters are counted in between, so that each name is seen to have its
// own. 40 sequences of eight reloads, and 8 under the race detector.
func TestTheStatisticsOfEveryCollectorAreReadAsTheyWereUnderTheLock(t *testing.T) {
	pool := make([]model.Collector, 12)
	for i := range pool {
		pool[i] = testutil.Collector(fmt.Sprintf("c%02d", (i*7)%len(pool)), "text")
	}
	many := make([]model.Collector, alloctest.UnlessRaced(200, 40))
	for i := range many {
		many[i] = testutil.Collector(fmt.Sprintf("m%03d", (i*77)%len(many)), "text")
	}
	random := rand.New(rand.NewPCG(20261006, 30)) //nolint:gosec // configurations for a test
	// generated is a configuration of some of the pool's collectors, in a
	// random order.
	generated := func() *model.Config {
		count := random.IntN(len(pool) + 1)
		switch random.IntN(6) {
		case 0:
			count = 0
		case 1:
			count = 1
		}
		chosen := make([]model.Collector, 0, count)
		for _, i := range random.Perm(len(pool))[:count] {
			chosen = append(chosen, pool[i])
		}
		return &model.Config{Collectors: chosen}
	}
	read := func(names []string, values map[string]statsValues, stats map[string]*serverStats) statsRead {
		return statsRead{names, values, stats}
	}
	sequences, emptied, single, made, found, retired, back := alloctest.UnlessRaced(40, 8), 0, 0, 0, 0, 0, 0
	for sequence := range sequences {
		first := generated()
		now, before := newTwin(t, first), newTwin(t, first)
		for step := range 8 {
			cfg := generated()
			if sequence == 0 && step == 3 {
				cfg = &model.Config{Collectors: slices.Concat(many, pool[:5])}
			}
			switch len(cfg.Collectors) {
			case 0:
				emptied++
			case 1:
				single++
			}
			at := fmt.Sprintf("sequence %d, step %d, %v", sequence, step, testutil.CollectorNames(cfg))
			if random.IntN(3) == 0 {
				// The reload comes between the read of the configuration and
				// the lock, and at times another before it: the read gives
				// the collectors of the configuration it read.
				through := []*model.Config{cfg}
				if random.IntN(2) == 0 {
					through = []*model.Config{generated(), cfg}
				}
				reload := func(server *Server) func() {
					return func() {
						for _, next := range through {
							installConfig(server, next)
							server.reconcile()
						}
					}
				}
				got := read(collectorStatsBetween(now, reload(now)))
				want := read(collectorStatsBefore(before, reload(before)))
				if err := sameReadOfATwin(got, want, keptStatistics(now), keptStatistics(before)); err != nil {
					t.Fatalf("%s, reloaded while the statistics were read: %v", at, err)
				}
				for name, stats := range got.stats {
					if stats.retired.Load() {
						retired++
						if slices.Contains(testutil.CollectorNames(cfg), name) {
							back++
						}
					}
				}
			} else {
				installConfig(now, cfg)
				installConfig(before, cfg)
			}
			formerly := keptStatistics(now)
			// One server is read as now first, the other as before first.
			nowFirst := read(now.collectorStats())
			nowSecond := read(collectorStatsBefore(now, nil))
			beforeFirst := read(collectorStatsBefore(before, nil))
			beforeSecond := read(before.collectorStats())
			if err := sameRead(nowSecond, nowFirst); err != nil {
				t.Fatalf("%s: read as before after a read as now: %v", at, err)
			}
			if err := sameRead(beforeSecond, beforeFirst); err != nil {
				t.Fatalf("%s: read as now after a read as before: %v", at, err)
			}
			if err := sameReadOfATwin(nowFirst, beforeFirst, keptStatistics(now), keptStatistics(before)); err != nil {
				t.Fatalf("%s: %v", at, err)
			}
			if kept := keptStatistics(now); !maps.Equal(kept, nowFirst.stats) || !sort.StringsAreSorted(nowFirst.names) || !slices.Equal(nowFirst.names, model.SortedKeys(kept)) {
				t.Fatalf("%s: the read gave the statistics %v by the names %v, and the server keeps %v", at, nowFirst.stats, nowFirst.names, kept)
			}
			for name, stats := range nowFirst.stats {
				if formerly[name] == stats {
					found++
				} else {
					made++
				}
			}
			// Each collector counts a number of its own, the same on both.
			for _, name := range nowFirst.names {
				count := random.Uint64N(1000)
				for _, stats := range []*serverStats{nowFirst.stats[name], beforeFirst.stats[name]} {
					stats.mu.Lock()
					stats.probes += count
					stats.lastBytes = int64(len(name)) + int64(step)
					stats.mu.Unlock()
				}
			}
		}
	}
	if emptied < sequences/2 || single < sequences/2 || made < 5*sequences || found < 5*sequences || retired < 3*sequences || back < sequences/4 {
		t.Errorf("%d configurations of no collector and %d of one, %d statistics made by a read and %d found, %d retired and %d of those of a collector brought back: the generator shows too little", emptied, single, made, found, retired, back)
	}
}

// newTwin is a server whose configuration in force is cfg.
func newTwin(t *testing.T, cfg *model.Config) *Server {
	t.Helper()
	server := verboseServer(t, false, testutil.Collector("first", "text"))
	server.logger = testutil.QuietLogger(t)
	installConfig(server, cfg)
	return server
}

// One read of the statistics of 500 collectors, each of which has its own,
// makes what it returns and no more: at most 515 allocations and 210,000
// bytes, where it makes 510 of 194,992 - the copy of each collector's
// counters, the two maps and the slices at their size, and the sort - and
// where it made 531 of 228,560 while the map of the statistics and the slice
// of the names grew as they were filled, with the statistics lock held. The
// read as it was is measured beside it, and is past both bounds.
func TestReadingTheStatisticsAllocatesNothingThatGrows(t *testing.T) {
	if alloctest.RaceDetector {
		// The race detector changes what is allocated.
		return
	}
	const collectors, mostAllocs, mostBytes = 500, 515, 210_000
	cfg := &model.Config{}
	for i := range collectors {
		cfg.Collectors = append(cfg.Collectors, testutil.Collector(fmt.Sprintf("c%d", i), "text"))
	}
	server := newTwin(t, cfg)
	if names, _, _ := server.collectorStats(); len(names) != collectors {
		t.Fatalf("%d collectors were read, want %d", len(names), collectors)
	}
	if allocs := alloctest.AllocsAtMost(10, mostAllocs, func() { server.collectorStats() }); allocs > mostAllocs {
		t.Errorf("a read of the statistics of %d collectors makes %v allocations, want at most %d", collectors, allocs, mostAllocs)
	}
	if bytes := alloctest.BytesAtMost(10, mostBytes, func() { server.collectorStats() }); bytes > mostBytes {
		t.Errorf("a read of the statistics of %d collectors allocates %d bytes, want at most %d", collectors, bytes, mostBytes)
	}
	allocs, bytes := alloctest.Allocations(10, func() { collectorStatsBefore(server, nil) })
	if allocs <= mostAllocs || bytes <= mostBytes {
		t.Errorf("the read as it was makes %v allocations of %d bytes: the bounds of %d and %d hold nothing", allocs, bytes, mostAllocs, mostBytes)
	}
}
