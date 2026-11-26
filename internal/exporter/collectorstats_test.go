//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The first read of every collector's statistics after a reload makes the
// statistics of a collector it brought in with the statistics lock
// released, and takes the lock only to find and to keep them. A probe that
// made the collector's statistics in between keeps them: the read returns
// those, not the ones it made, so what the probe counts is shown.
func TestTheStatisticsOfANewCollectorAreMadeWithoutTheLock(t *testing.T) {
	first := followBenchManager(t, testutil.CollectorsDocument(followBenchNames(0, 3)...), false)
	second := followBenchManager(t, testutil.CollectorsDocument(followBenchNames(3, 3)...), false)
	server := NewServer(first, "python3", slog.New(slog.DiscardHandler))
	server.collectorStats()
	server.manager = second
	followed := server.reconcile()
	name := followed.config.Collectors[0].Name
	var made *serverStats
	held, released := 0, 0
	hook := func(lock bool) {
		if lock {
			held++
			return
		}
		released++
		if released == 1 {
			// Between the read's two turns of the lock, a probe of the
			// collector makes its statistics.
			made = server.statsSince(followed.generation, name)
		}
	}
	statsReadHook.Store(&hook)
	t.Cleanup(func() { statsReadHook.Store(nil) })
	_, _, stats := server.collectorStats()
	if held != 2 || released != 2 {
		t.Errorf("the lock was taken %d times and released %d, want twice: to find the statistics and to keep those made", held, released)
	}
	if made == nil || stats[name] != made || made.retired.Load() {
		t.Errorf("the read returned %p for %s, the probe made %p", stats[name], name, made)
	}
	for _, c := range followed.config.Collectors[1:] {
		if x := stats[c.Name]; x == nil || x.retired.Load() || server.statsSince(followed.generation, c.Name) != x {
			t.Errorf("%s: the read's statistics %p are not those kept", c.Name, x)
		}
	}
	statsReadHook.Store(nil)
	held, released = 0, 0
	statsReadHook.Store(&hook)
	server.collectorStats()
	if held != 1 {
		t.Errorf("a read that finds every collector's statistics took the lock %d times, want once", held)
	}
}
