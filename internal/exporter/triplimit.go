package exporter

import (
	"context"
	"fmt"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
// answer saying why beats one that never comes. A static target over the
// limit waits for a free slot within its scrape budget instead, since nothing
// is waiting on it. Probes answered from the cache or by sharing another
// probe's request make no trip and take no slot.

// DefaultMaxConcurrentProbes is the limit of a collector that sets none.
const DefaultMaxConcurrentProbes = 32

// maxConcurrentProbes is a collector's limit.
func maxConcurrentProbes(c *model.Collector) int {
	if c.MaxConcurrentProbes > 0 {
		return c.MaxConcurrentProbes
	}
	return DefaultMaxConcurrentProbes
}

// --probe.max-concurrent bounds the trips of every collector together, since
// each holds a response body, its decoded form and its series in memory
// while it runs: without it, many collectors each within their own limit
// could still hold more than the process has. A probe over it is answered 503
// too, and a static target scrape waits for a slot, as with a collector's
// own limit. 0 leaves the process unbounded.

// tripLimiter counts the trips in progress per collector and in all.
type tripLimiter struct {
	mu       sync.Mutex
	inFlight map[string]int
	// total is the trips in progress of every collector; ceiling, when
	// positive, bounds it.
	total, ceiling int
	// freed is closed, and replaced, whenever a slot is released, which is
	// what a waiting static target scrape waits on.
	freed chan struct{}
}

func newTripLimiter() *tripLimiter {
	return &tripLimiter{inFlight: map[string]int{}, freed: make(chan struct{})}
}

// setMax sets the process-wide limit; 0 leaves it unbounded.
func (l *tripLimiter) setMax(ceiling int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ceiling = ceiling
}

// fullLocked says why no slot is free for collector, or "" when one is.
func (l *tripLimiter) fullLocked(collector string, limit int) string {
	if l.inFlight[collector] >= limit {
		return fmt.Sprintf("collector %s already has %d trips to its targets in progress, its max_concurrent_probes", collector, limit)
	}
	if l.ceiling > 0 && l.total >= l.ceiling {
		return fmt.Sprintf("the exporter already has %d trips to targets in progress, its --probe.max-concurrent", l.ceiling)
	}
	return ""
}

func (l *tripLimiter) takeLocked(collector string) {
	l.inFlight[collector]++
	l.total++
}

// tryAcquire takes a slot for a collector if one is free, or says why none
// is.
func (l *tripLimiter) tryAcquire(collector string, limit int) (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if full := l.fullLocked(collector, limit); full != "" {
		return false, full
	}
	l.takeLocked(collector)
	return true, ""
}

// acquire waits for a slot until ctx ends.
func (l *tripLimiter) acquire(ctx context.Context, collector string, limit int) error {
	for {
		l.mu.Lock()
		full := l.fullLocked(collector, limit)
		if full == "" {
			l.takeLocked(collector)
			l.mu.Unlock()
			return nil
		}
		freed := l.freed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s, and none finished in time: %w", full, ctx.Err())
		case <-freed:
		}
	}
}

// release frees a collector's slot.
func (l *tripLimiter) release(collector string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight[collector]--
	l.total--
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

// ValidateMaxConcurrent refuses a negative --probe.max-concurrent.
func ValidateMaxConcurrent(limit int) error {
	if limit < 0 {
		return fmt.Errorf("--probe.max-concurrent must not be negative, got %d", limit)
	}
	return nil
}

// SetMaxConcurrent bounds the trips of all collectors together; 0 leaves
// them unbounded.
func (s *Server) SetMaxConcurrent(limit int) { s.trips.setMax(limit) }
