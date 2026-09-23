package main

import (
	"context"
	"fmt"
	"sync"
)

// A collector usually stands for one backend, or one kind of backend, and each
// probe of it is a request that backend has to answer. Coalescing
// (probeflight.go) already makes identical probes share one request, but
// probes of different targets or with different parameters each make their
// own, so a burst of them — many targets of one collector scraped at the same
// moment, a slow backend making them pile up — would reach the backend with no
// bound at all. max_concurrent_probes bounds the trips a collector makes at
// once: its requests or file reads with the decoding and transforms after
// them.
//
// A probe over the limit is answered 503 Service Unavailable at once, rather
// than queued behind the others: Prometheus has a scrape timeout, and an
// answer saying why beats one that never comes. A scheduled target over the
// limit waits for a free slot within its scrape budget instead, since nothing
// is waiting on it. Probes answered from the cache or by sharing another
// probe's request make no trip and take no slot.

// DefaultMaxConcurrentProbes is the limit of a collector that sets none.
const DefaultMaxConcurrentProbes = 32

// maxConcurrentProbes is a collector's limit.
func maxConcurrentProbes(c *Collector) int {
	if c.MaxConcurrentProbes > 0 {
		return c.MaxConcurrentProbes
	}
	return DefaultMaxConcurrentProbes
}

// tripLimiter counts the trips in progress per collector.
type tripLimiter struct {
	mu       sync.Mutex
	inFlight map[string]int
	// freed is closed, and replaced, whenever a slot is released, which is
	// what a waiting scheduled scrape waits on.
	freed chan struct{}
}

func newTripLimiter() *tripLimiter {
	return &tripLimiter{inFlight: map[string]int{}, freed: make(chan struct{})}
}

// tryAcquire takes a slot for a collector if one is free.
func (l *tripLimiter) tryAcquire(collector string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[collector] >= limit {
		return false
	}
	l.inFlight[collector]++
	return true
}

// acquire waits for a slot until ctx ends.
func (l *tripLimiter) acquire(ctx context.Context, collector string, limit int) error {
	for {
		l.mu.Lock()
		if l.inFlight[collector] < limit {
			l.inFlight[collector]++
			l.mu.Unlock()
			return nil
		}
		freed := l.freed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("collector %q already has %d trips to its targets in progress (max_concurrent_probes) and none finished in time: %w", collector, limit, ctx.Err())
		case <-freed:
		}
	}
}

// release frees a collector's slot.
func (l *tripLimiter) release(collector string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight[collector]--
	if l.inFlight[collector] <= 0 {
		delete(l.inFlight, collector)
	}
	close(l.freed)
	l.freed = make(chan struct{})
}

// count reports a collector's trips in progress.
func (l *tripLimiter) count(collector string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight[collector]
}
