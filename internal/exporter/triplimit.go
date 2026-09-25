package exporter

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// Static target scrapes waiting for a slot wait in line. A freed slot is
// handed to the first waiter it can serve — its collector under its own limit
// and the process under --probe.max-concurrent — rather than every waiter
// being woken to race for it: with many targets waiting, that would wake them
// all on every release, and a waiter whose collector is still full could
// keep one whose collector is not from ever being woken.

// tripLimiter counts the trips in progress per collector and in all.
type tripLimiter struct {
	mu       sync.Mutex
	inFlight map[string]int
	// total is the trips in progress of every collector; ceiling, when
	// positive, bounds it.
	total, ceiling int
	// waiting is the static target scrapes waiting for a slot, in the order
	// they began to wait.
	waiting []*tripWaiter
}

// tripWaiter is a scrape waiting for a slot. granted is closed once a slot
// was taken for it.
type tripWaiter struct {
	collector string
	limit     int
	granted   chan struct{}
}

// tripLimitError is a trip refused for a full limit: its collector's
// max_concurrent_probes, or --probe.max-concurrent when byExporter.
type tripLimitError struct {
	message    string
	byExporter bool
	err        error
}

func (e *tripLimitError) Error() string {
	if e.err == nil {
		return e.message
	}
	return fmt.Sprintf("%s, and none finished in time: %v", e.message, e.err)
}

func (e *tripLimitError) Unwrap() error { return e.err }

func newTripLimiter() *tripLimiter {
	return &tripLimiter{inFlight: map[string]int{}}
}

// setMax sets the process-wide limit; 0 leaves it unbounded.
func (l *tripLimiter) setMax(ceiling int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ceiling = ceiling
	l.grantLocked()
}

// fullLocked says why no slot is free for collector, nil when one is.
func (l *tripLimiter) fullLocked(collector string, limit int) *tripLimitError {
	if l.inFlight[collector] >= limit {
		return &tripLimitError{message: fmt.Sprintf("collector %s already has %d trips to its targets in progress, its max_concurrent_probes", collector, limit)}
	}
	if l.ceiling > 0 && l.total >= l.ceiling {
		return &tripLimitError{message: fmt.Sprintf("the exporter already has %d trips to targets in progress, its --probe.max-concurrent", l.ceiling), byExporter: true}
	}
	return nil
}

func (l *tripLimiter) takeLocked(collector string) {
	l.inFlight[collector]++
	l.total++
}

// tryAcquire takes a slot for a collector if one is free, or says why none
// is.
func (l *tripLimiter) tryAcquire(collector string, limit int) *tripLimitError {
	l.mu.Lock()
	defer l.mu.Unlock()
	if full := l.fullLocked(collector, limit); full != nil {
		return full
	}
	l.takeLocked(collector)
	return nil
}

// acquire waits in line for a slot until ctx ends.
func (l *tripLimiter) acquire(ctx context.Context, collector string, limit int) error {
	l.mu.Lock()
	full := l.fullLocked(collector, limit)
	if full == nil {
		l.takeLocked(collector)
		l.mu.Unlock()
		return nil
	}
	w := &tripWaiter{collector: collector, limit: limit, granted: make(chan struct{})}
	l.waiting = append(l.waiting, w)
	l.mu.Unlock()
	select {
	case <-w.granted:
		return nil
	case <-ctx.Done():
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-w.granted:
		// Granted as the context ended: the slot is given back.
		l.releaseLocked(collector)
	default:
		l.waiting = slices.DeleteFunc(l.waiting, func(x *tripWaiter) bool { return x == w })
	}
	full = l.fullLocked(collector, limit)
	if full == nil {
		full = &tripLimitError{message: fmt.Sprintf("collector %s waited for a trip slot", collector)}
	}
	full.err = ctx.Err()
	return full
}

// grantLocked hands free slots to the waiters, in line, that they can serve.
func (l *tripLimiter) grantLocked() {
	kept := l.waiting[:0]
	for _, w := range l.waiting {
		if l.fullLocked(w.collector, w.limit) == nil {
			l.takeLocked(w.collector)
			close(w.granted)
			continue
		}
		kept = append(kept, w)
	}
	clear(l.waiting[len(kept):])
	l.waiting = kept
}

// release frees a collector's slot, for the first waiter it can serve.
func (l *tripLimiter) release(collector string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseLocked(collector)
}

func (l *tripLimiter) releaseLocked(collector string) {
	l.inFlight[collector]--
	l.total--
	if l.inFlight[collector] <= 0 {
		delete(l.inFlight, collector)
	}
	l.grantLocked()
}

// count reports a collector's trips in progress.
func (l *tripLimiter) count(collector string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight[collector]
}

// waitingCount reports the static target scrapes waiting in line for a
// slot.
func (l *tripLimiter) waitingCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.waiting)
}

// totals reports the trips in progress of every collector and the
// process-wide limit.
func (l *tripLimiter) totals() (inFlight, ceiling int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, l.ceiling
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

// countRejection counts a trip refused for a full limit under the limit
// that refused it.
func countRejection(x *serverStats, err error) {
	var full *tripLimitError
	if errors.As(err, &full) && full.byExporter {
		x.rejectedByExporter++
		return
	}
	x.rejected++
}

// tripTotalMetrics are the exporter-wide trip series: every collector's
// trips in progress, which --probe.max-concurrent bounds, and that limit, 0
// for none.
func (s *Server) tripTotalMetrics() []model.Metric {
	inFlight, ceiling := s.trips.totals()
	return []model.Metric{
		{Name: "http_exporter_trips_in_flight", Help: exporterMetricHelp["http_exporter_trips_in_flight"], Type: model.GaugeMetricType, Value: float64(inFlight)},
		{Name: "http_exporter_trips_max_concurrent", Help: exporterMetricHelp["http_exporter_trips_max_concurrent"], Type: model.GaugeMetricType, Value: float64(ceiling)},
	}
}
