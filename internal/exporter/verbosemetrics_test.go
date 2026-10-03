package exporter

import (
	"reflect"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// With verbose self-metrics, the exporter publishes a histogram of trips to
// the target per collector, and the state of each collector's Python workers
// (verbosemetrics.go). Without verbose, neither exists: see verboseOnlyNames.

func TestScrapeDurationHistogramBuckets(t *testing.T) {
	d := &durationHistogram{}
	for _, elapsed := range []time.Duration{3 * time.Millisecond, 5 * time.Millisecond, 70 * time.Millisecond, 2 * time.Second, 90 * time.Second} {
		d.observe(elapsed)
	}
	h := d.histogram()
	cumulative := map[float64]uint64{}
	for _, b := range h.Buckets {
		cumulative[b.UpperBound] = b.CumulativeCount
	}
	// A value on a boundary lands in that bucket; one above the last bound
	// only in +Inf, which is the count.
	for bound, want := range map[float64]uint64{0.005: 2, 0.05: 2, 0.1: 3, 1: 3, 2.5: 4, 60: 4} {
		if cumulative[bound] != want {
			t.Errorf("le=%v: %d, want %d", bound, cumulative[bound], want)
		}
	}
	if h.Count != 5 || h.Sum < 92.07 || h.Sum > 92.08 {
		t.Fatalf("count=%d sum=%v", h.Count, h.Sum)
	}
	if empty := (&durationHistogram{}).histogram(); empty.Count != 0 || len(empty.Buckets) != len(scrapeDurationBuckets) {
		t.Fatalf("an unscraped collector: %+v", empty)
	}
}

// The histogram a collector's statistics hold counts as the one kept by the
// collector's name did, copied here: for trips of every length, the same
// buckets, sum and count, and the same empty histogram before the first.
func TestTheHistogramOfTheStatisticsCountsAsTheOneKeptByNameDid(t *testing.T) {
	type byName struct {
		counts []uint64
		sum    float64
		count  uint64
	}
	kept := map[string]*byName{}
	observe := func(collector string, elapsed time.Duration) {
		seconds := elapsed.Seconds()
		h := kept[collector]
		if h == nil {
			h = &byName{counts: make([]uint64, len(scrapeDurationBuckets))}
			kept[collector] = h
		}
		h.sum += seconds
		h.count++
		for i, bound := range scrapeDurationBuckets {
			if seconds <= bound {
				h.counts[i]++
				return
			}
		}
	}
	histogram := func(collector string) *model.Histogram {
		out := &model.Histogram{}
		h := kept[collector]
		var cumulative uint64
		for i, bound := range scrapeDurationBuckets {
			if h != nil {
				cumulative += h.counts[i]
			}
			out.Buckets = append(out.Buckets, model.Bucket{UpperBound: bound, CumulativeCount: cumulative})
		}
		if h != nil {
			out.Sum, out.Count = h.sum, h.count
		}
		return out
	}
	stats := newServerStats(time.Now())
	if got, want := stats.tripHistogram(), histogram("c"); !reflect.DeepEqual(got, want) {
		t.Fatalf("before the first trip: %+v, want %+v", got, want)
	}
	// Trips from a microsecond to two minutes, on the bounds and between.
	elapsed := time.Microsecond
	for i := range 400 {
		if i%7 == 0 {
			elapsed = time.Duration(scrapeDurationBuckets[i%len(scrapeDurationBuckets)] * float64(time.Second))
		}
		observe("c", elapsed)
		stats.durations.observe(elapsed)
		if got, want := stats.tripHistogram(), histogram("c"); !reflect.DeepEqual(got, want) {
			t.Fatalf("after %d trips, the last of %v: %+v, want %+v", i+1, elapsed, got, want)
		}
		elapsed = elapsed*21/16 + time.Microsecond
		if elapsed > 2*time.Minute {
			elapsed = 3 * time.Microsecond
		}
	}
	if got := stats.tripHistogram(); got.Count != 400 || got.Buckets[0].CumulativeCount == 0 || got.Buckets[len(got.Buckets)-1].CumulativeCount == got.Count {
		t.Fatalf("the trips do not cover the buckets and what is beyond them: %+v", got)
	}
}
