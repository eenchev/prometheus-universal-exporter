package exporter

import (
	"context"
	"strings"
	"testing"
	"time"
)

// max_concurrent_probes bounds a collector's trips to its targets
// (triplimit.go).

func TestTripLimiterAcquireHonoursItsContext(t *testing.T) {
	limiter := newTripLimiter()
	if full := limiter.tryAcquire("c", 1); full != nil {
		t.Fatal(full)
	}
	if limiter.tryAcquire("c", 1) == nil {
		t.Fatal("the limit of one was not enforced")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.acquire(ctx, "c", 1); err == nil || !strings.Contains(err.Error(), "max_concurrent_probes") {
		t.Fatalf("err=%v", err)
	}
	if len(limiter.waiting) != 0 {
		t.Fatal("a waiter that gave up is still in line")
	}
	limiter.release("c")
	if err := limiter.acquire(context.Background(), "c", 1); err != nil {
		t.Fatal(err)
	}
	if full := limiter.tryAcquire("d", 1); limiter.count("c") != 1 || full != nil {
		t.Fatal("collectors do not have limits of their own")
	}
}

// --probe.max-concurrent bounds the trips of all collectors together, on top
// of each collector's own limit, and says which limit refused a trip.
func TestTripLimiterProcessWideLimit(t *testing.T) {
	limiter := newTripLimiter()
	limiter.setMax(2)
	for _, c := range []string{"a", "b"} {
		if full := limiter.tryAcquire(c, 10); full != nil {
			t.Fatal(full)
		}
	}
	full := limiter.tryAcquire("c", 10)
	if full == nil || !full.byExporter || !strings.Contains(full.Error(), "--probe.max-concurrent") {
		t.Fatalf("a third trip was let through: %v", full)
	}
	if own := limiter.tryAcquire("a", 1); own == nil || own.byExporter {
		t.Fatalf("a collector's own limit is reported as the exporter's: %v", own)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.acquire(ctx, "c", 10); err == nil || !strings.Contains(err.Error(), "--probe.max-concurrent") {
		t.Fatalf("err=%v", err)
	}
	limiter.release("a")
	if full := limiter.tryAcquire("c", 10); full != nil {
		t.Fatal(full)
	}
	if inFlight, ceiling := limiter.totals(); inFlight != 2 || ceiling != 2 {
		t.Fatalf("totals %d/%d", inFlight, ceiling)
	}
	if ValidateMaxConcurrent(-1) == nil || ValidateMaxConcurrent(0) != nil {
		t.Fatal("validation")
	}
}

// A freed slot goes to the first waiter it can serve, not to every waiter:
// one waiting behind its own full collector does not keep another collector's
// waiter from being served, and waiters of one collector are served in
// line.
func TestTripLimiterHandsSlotsToWaitersInLine(t *testing.T) {
	limiter := newTripLimiter()
	limiter.setMax(2)
	_ = limiter.tryAcquire("a", 1)
	_ = limiter.tryAcquire("b", 5)
	served := make(chan string, 3)
	wait := func(name, collector string, limit int) {
		go func() {
			if err := limiter.acquire(context.Background(), collector, limit); err == nil {
				served <- name
			}
		}()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			limiter.mu.Lock()
			n := len(limiter.waiting)
			queued := n > 0 && limiter.waiting[n-1].collector == collector
			limiter.mu.Unlock()
			if queued {
				return
			}
		}
	}
	wait("a-waiter", "a", 1)
	wait("c-first", "c", 5)
	wait("c-second", "c", 5)
	for deadline := time.Now().Add(5 * time.Second); limiter.waitingCount() != 3 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if n := limiter.waitingCount(); n != 3 {
		t.Fatalf("%d waiting, want 3", n)
	}
	// b's slot frees the process limit, which a-waiter cannot use: its
	// collector is still full. The first c waiter gets it.
	limiter.release("b")
	if got := <-served; got != "c-first" {
		t.Fatalf("%s was served first", got)
	}
	limiter.release("a")
	if got := <-served; got != "a-waiter" {
		t.Fatalf("%s was served, want a-waiter", got)
	}
	select {
	case got := <-served:
		t.Fatalf("%s was served with no slot free", got)
	case <-time.After(20 * time.Millisecond):
	}
}
