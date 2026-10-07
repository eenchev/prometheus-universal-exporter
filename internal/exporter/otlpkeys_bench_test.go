package exporter

// Benchmarks of what an OTLP export does for each of its points beside
// encoding them: the queue a probe's or a static target's points wait in, a
// map of each resource's series, which is emptied in order for the export,
// and the start time of each cumulative point, kept by series. Each makes
// the key of every point (otlpSeriesKey), so they are what a change to the
// keys is measured with (docs/DEVELOPMENT.md):
//
//	go test -run '^$' -bench 'OTLP' -benchtime 2s ./internal/exporter/
//
// n is the number of series, each a counter with three labels.

import (
	"fmt"
	"log/slog"
	"strconv"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// otlpBenchSet is n counters of one family, told apart by their labels.
func otlpBenchSet(n int) model.MetricSet {
	at := int64(1_700_000_000_000)
	set := model.MetricSet{Metrics: make([]model.Metric, n)}
	for i := range set.Metrics {
		set.Metrics[i] = model.Metric{
			Name: "item_requests_total", Type: model.CounterMetricType, Value: float64(i), Timestamp: &at,
			Labels: map[string]string{"id": "item-" + strconv.Itoa(i), "region": "eu-west-" + strconv.Itoa(i%3), "kind": "disk"},
		}
	}
	return set
}

func BenchmarkOTLPPoints(b *testing.B) {
	otlp := otlpConfig("http://collector.invalid/v1/metrics")
	identity := defaultResourceIdentity(otlp)
	for _, n := range []int{100, 5000} {
		set := otlpBenchSet(n)
		// Queued by a probe and emptied for an export.
		b.Run(fmt.Sprintf("queue/n=%d", n), func(b *testing.B) {
			server := NewServer(config.NewManager(&model.Config{OTLP: otlp}, "", slog.Default()), "python3", slog.Default())
			b.ReportAllocs()
			for b.Loop() {
				server.queueOTLPResource(set, identity, scrapeTime{})
				if drained := server.drainOTLP(); len(drained) != 1 || len(drained[0].Set.Metrics) != n {
					b.Fatalf("drained %d resources", len(drained))
				}
			}
		})
		// Made the points of an export, each with its start time.
		b.Run(fmt.Sprintf("start/n=%d", n), func(b *testing.B) {
			starts := newOTLPStartTimes()
			b.ReportAllocs()
			for b.Loop() {
				if metrics := otlpMetrics(set, "1", starts.forResource(identity.key())); len(metrics) != 1 || len(metrics[0].Sum.DataPoints) != n {
					b.Fatalf("%d metrics", len(metrics))
				}
			}
		})
	}
}
