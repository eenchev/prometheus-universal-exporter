//go:build !select_request_types || request_type_http

package exporter

// Benchmarks of reading every collector's statistics, which each scrape of
// the self-metrics does (collectorStats in selfmetrics.go), for 50, 500 and
// 2,000 collectors, and how much of it is done with the statistics lock
// held, which every probe takes to find its collector's statistics
// (docs/DEVELOPMENT.md):
//
//	go test -run '^$' -bench 'CollectorStats/steady' ./internal/exporter/
//	go test -run '^$' -bench 'CollectorStats/reloaded' -benchtime 20x ./internal/exporter/
//
// steady is a read that finds every collector's statistics there, as every
// read but the first after a reload does, and reloaded the first read after a
// reload that replaced every collector by another, which makes the
// statistics of each with the lock held. The reload itself is not timed, and
// takes far longer than the read: hence the fixed number of rounds.

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// timeStatsRead has the reads of every collector's statistics made until the
// benchmark ends add the time they hold the statistics lock to the duration
// returned, nothing else asking for the lock here.
func timeStatsRead(b *testing.B) *time.Duration {
	b.Helper()
	var taken time.Time
	locked := new(time.Duration)
	hook := func(held bool) {
		if held {
			taken = time.Now()
			return
		}
		*locked += time.Since(taken)
	}
	statsReadHook.Store(&hook)
	b.Cleanup(func() { statsReadHook.Store(nil) })
	return locked
}

// BenchmarkCollectorStats is one read of every collector's statistics.
func BenchmarkCollectorStats(b *testing.B) {
	for _, n := range []int{50, 500, 2000} {
		b.Run(fmt.Sprintf("steady/n=%d", n), func(b *testing.B) {
			server := NewServer(followBenchManager(b, testutil.CollectorsDocument(followBenchNames(0, n)...), false), "python3", slog.New(slog.DiscardHandler))
			locked := timeStatsRead(b)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				server.collectorStats()
			}
			b.ReportMetric(float64(locked.Nanoseconds())/float64(b.N), "locked-ns/op")
		})
	}
	for _, n := range []int{50, 500, 2000} {
		b.Run(fmt.Sprintf("reloaded/n=%d", n), func(b *testing.B) {
			managers := [2]*config.Manager{
				followBenchManager(b, testutil.CollectorsDocument(followBenchNames(0, n)...), false),
				followBenchManager(b, testutil.CollectorsDocument(followBenchNames(n, n)...), false),
			}
			server := NewServer(managers[0], "python3", slog.New(slog.DiscardHandler))
			locked := timeStatsRead(b)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				// The reload removes every collector, and with it its
				// statistics, and adds as many: the read that follows
				// makes those of each.
				b.StopTimer()
				server.manager = managers[(i+1)%2]
				server.reconcile()
				b.StartTimer()
				server.collectorStats()
			}
			b.ReportMetric(float64(locked.Nanoseconds())/float64(b.N), "locked-ns/op")
		})
	}
}
