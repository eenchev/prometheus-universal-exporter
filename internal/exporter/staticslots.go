package exporter

import (
	"context"
	"slices"
	"sync"
)

// The static target file's concurrency bounds the scrapes in progress of
// every collector together, and a scrape holds its slot until it ends, at
// its deadline at the latest. Handed out first come, first served, the
// slots would all go to whichever collector's targets are slow: a database
// that hangs, scraped every minute with a timeout of a minute, would hold
// every slot, and every other collector's targets would be skipped turn
// after turn. So the slots are shared between the collectors the file
// names:
//
//   - each collector may hold its share, concurrency divided by the number
//     of collectors the file names, at least one, whenever it asks;
//   - beyond its share it takes a slot only while that leaves one free for
//     every other collector holding none, so that a collector that comes
//     for a slot finds one at once, however the others' targets behave;
//   - a slot freed goes to the scrape waiting whose collector holds the
//     fewest, the first to wait of those, so a collector with many slots
//     does not take it from one with few.
//
// A file of a single collector, and one whose collectors are all busy
// within their shares, uses every slot, as before. A file of more
// collectors than slots gives each one slot at least.

// staticSlots are the static target file's slots.
type staticSlots struct {
	mu sync.Mutex
	// limit is the file's concurrency, and collectors the number of
	// collectors its targets name.
	limit, collectors int
	held              map[string]int
	total             int
	waiting           []*staticSlotWaiter
}

// staticSlotWaiter is a scrape waiting for a slot: granted is closed once
// it holds one.
type staticSlotWaiter struct {
	collector string
	granted   chan struct{}
}

func newStaticSlots() *staticSlots {
	return &staticSlots{held: map[string]int{}, limit: 1, collectors: 1}
}

// shape sets the concurrency and the number of collectors, as a reload
// leaves them; scrapes in progress keep their slots, and a larger limit
// hands slots to those waiting.
func (s *staticSlots) shape(limit, collectors int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit, s.collectors = max(limit, 1), max(collectors, 1)
	s.grantLocked()
}

// mayTakeLocked says whether collector may take a slot now.
func (s *staticSlots) mayTakeLocked(collector string) bool {
	free := s.limit - s.total
	if free <= 0 {
		return false
	}
	if s.held[collector] < max(1, s.limit/s.collectors) {
		return true
	}
	// Beyond its share: one slot stays free for each other collector the
	// file names that holds none.
	holding := 0
	for name, n := range s.held {
		if name != collector && n > 0 {
			holding++
		}
	}
	idle := max(0, s.collectors-1-holding)
	return free-1 >= idle
}

// acquire waits for a slot for a scrape of collector until ctx ends.
func (s *staticSlots) acquire(ctx context.Context, collector string) error {
	s.mu.Lock()
	if len(s.waiting) == 0 && s.mayTakeLocked(collector) {
		s.held[collector]++
		s.total++
		s.mu.Unlock()
		return nil
	}
	w := &staticSlotWaiter{collector: collector, granted: make(chan struct{})}
	s.waiting = append(s.waiting, w)
	// One waiting may take a slot that the queue ahead of it, all of
	// collectors over their shares, may not.
	s.grantLocked()
	s.mu.Unlock()
	select {
	case <-w.granted:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		if i := slices.Index(s.waiting, w); i >= 0 {
			s.waiting = slices.Delete(s.waiting, i, i+1)
			return ctx.Err()
		}
		// Granted as the context ended: the slot is given back.
		s.releaseLocked(collector)
		return ctx.Err()
	}
}

// release gives back a slot of collector.
func (s *staticSlots) release(collector string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(collector)
}

func (s *staticSlots) releaseLocked(collector string) {
	s.held[collector]--
	if s.held[collector] <= 0 {
		delete(s.held, collector)
	}
	s.total--
	s.grantLocked()
}

// grantLocked hands free slots to the scrapes waiting that may take them:
// each to the one whose collector holds the fewest, the first to wait of
// those, so that a collector over its share does not win a slot from one
// under it by having waited longer.
func (s *staticSlots) grantLocked() {
	for {
		best := -1
		for i, w := range s.waiting {
			if !s.mayTakeLocked(w.collector) {
				continue
			}
			if best < 0 || s.held[w.collector] < s.held[s.waiting[best].collector] {
				best = i
			}
		}
		if best < 0 {
			return
		}
		w := s.waiting[best]
		s.held[w.collector]++
		s.total++
		close(w.granted)
		s.waiting = slices.Delete(s.waiting, best, best+1)
	}
}

// heldBy is how many slots collector holds, for the tests.
func (s *staticSlots) heldBy(collector string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[collector]
}
