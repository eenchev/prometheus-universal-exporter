package model

import (
	"math"
	"slices"
	"testing"
)

// The bucket of a value is OpenTelemetry's mapping function: the i for which
// base^i < v <= base^(i+1). The vectors at scale 1 are the specification's
// reference implementation's (the logarithm mapping's tests), those at 0 and
// below the exponent mapping's, and the powers of two the bounds every scale
// shares, which belong to the bucket below them.
func TestExponentialIndexVectors(t *testing.T) {
	for _, c := range []struct {
		scale int32
		value float64
		want  int32
	}{
		// scale 1: base √2
		{1, 15, 7}, {1, 9, 6}, {1, 7, 5}, {1, 5, 4}, {1, 3, 3}, {1, 2.5, 2},
		{1, 1.5, 1}, {1, 1.2, 0}, {1, 1, -1}, {1, 0.75, -1}, {1, 0.55, -2}, {1, 0.45, -3},
		{1, 1.41, 0}, {1, 1.42, 1}, {1, 2, 1}, {1, 4, 3},
		// scale 0: base 2
		{0, 1, -1}, {0, 1.5, 0}, {0, 2, 0}, {0, 3, 1}, {0, 4, 1}, {0, 4.0001, 2}, {0, 0.5, -2}, {0, 0.75, -1},
		// scale -1: base 4
		{-1, 1, -1}, {-1, 2, 0}, {-1, 4, 0}, {-1, 5, 1}, {-1, 16, 1}, {-1, 17, 2},
		// scale -2: base 16
		{-2, 16, 0}, {-2, 17, 1}, {-2, 256, 1},
		// the finest scale: every power of two is a bound, 2^20 buckets apart
		{20, 1, -1}, {20, 2, 1<<20 - 1}, {20, 0.5, -1<<20 - 1}, {20, math.MaxFloat64, 1024<<20 - 1},
		{20, math.SmallestNonzeroFloat64, -1074<<20 - 1},
		// scale 3, Prometheus's schema 3, bounds 2^(k/8): 1.1 is above
		// 2^(1/8) = 1.0905 and not above 2^(2/8) = 1.1892
		{3, 1.1, 1}, {3, 1.09, 0}, {3, 1.19, 2}, {3, 0.005, -62}, {3, 60, 47},
	} {
		if got := ExponentialIndex(c.value, c.scale); got != c.want {
			t.Errorf("ExponentialIndex(%v, %d) = %d, want %d", c.value, c.scale, got, c.want)
		}
	}
	// Each index holds its value: base^i < v <= base^(i+1).
	for _, scale := range []int32{-3, 0, 2, 3, 8, 12} {
		base := math.Exp2(math.Exp2(-float64(scale)))
		for _, v := range []float64{0.001, 0.0123, 0.3, 0.999, 1.0001, 7.77, 59.9, 1234.5} {
			i := ExponentialIndex(v, scale)
			if low, high := math.Pow(base, float64(i)), math.Pow(base, float64(i+1)); !(low < v && v <= high*(1+1e-12)) {
				t.Errorf("scale %d: %v in bucket %d, (%v, %v]", scale, v, i, low, high)
			}
		}
	}
}

// A recorder holds at most its number of buckets: values further apart lower
// the scale, merging buckets in pairs, and the counts are kept whole.
func TestExponentialObserveDownscales(t *testing.T) {
	h := &ExponentialHistogram{Scale: 0}
	for _, v := range []float64{1, 2, 4, 8} {
		h.Observe(v, 4)
	}
	// indices -1, 0, 1, 2 fit four buckets at scale 0
	if h.Scale != 0 || h.Offset != -1 || !slices.Equal(h.Counts, []uint64{1, 1, 1, 1}) {
		t.Fatalf("got scale %d offset %d %v", h.Scale, h.Offset, h.Counts)
	}
	// 16 is index 3: five buckets, so scale -1, where -1, 0, 1, 2, 3 are -1, 0, 0, 1, 1
	h.Observe(16, 4)
	if h.Scale != -1 || h.Offset != -1 || !slices.Equal(h.Counts, []uint64{1, 2, 2}) {
		t.Fatalf("got scale %d offset %d %v", h.Scale, h.Offset, h.Counts)
	}
	// A value below the buckets grows them downwards.
	h.Observe(0.2, 4)
	if h.Offset != -2 || !slices.Equal(h.Counts, []uint64{1, 1, 2, 2}) {
		t.Fatalf("got scale %d offset %d %v", h.Scale, h.Offset, h.Counts)
	}
	h.Observe(0, 4)
	if h.ZeroCount != 1 || h.Min != 0 || h.Max != 16 {
		t.Fatalf("zero %d min %v max %v", h.ZeroCount, h.Min, h.Max)
	}

	// From the finest scale, as a recorder starts: durations from 5 ms to a
	// minute settle where 160 buckets hold them.
	h = NewExponentialHistogram()
	var total uint64
	for v := 0.005; v <= 60; v *= 1.01 {
		h.Observe(v, ExponentialMaxSize)
		total++
	}
	if len(h.Counts) > ExponentialMaxSize || h.Scale != 3 {
		t.Fatalf("scale %d with %d buckets", h.Scale, len(h.Counts))
	}
	var counted uint64
	for _, c := range h.Counts {
		counted += c
	}
	if counted != total {
		t.Fatalf("counted %d of %d", counted, total)
	}
	if h.Offset != ExponentialIndex(0.005, 3) {
		t.Fatalf("offset %d", h.Offset)
	}
}

// Downscaling merges 2^by buckets, rounding indices towards minus infinity,
// negative ones included, and AtMostScale leaves the histogram as it is.
func TestExponentialDownscaleMerges(t *testing.T) {
	h := &ExponentialHistogram{Scale: 4, Offset: -3, Counts: []uint64{1, 2, 3, 4, 5, 6}}
	low := h.AtMostScale(2)
	// -3, -2, -1, 0, 1, 2 >> 2: -1, -1, -1, 0, 0, 0
	if low.Scale != 2 || low.Offset != -1 || !slices.Equal(low.Counts, []uint64{6, 15}) {
		t.Fatalf("got scale %d offset %d %v", low.Scale, low.Offset, low.Counts)
	}
	if h.Scale != 4 || !slices.Equal(h.Counts, []uint64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("the original changed: %v", h.Counts)
	}
	if same := h.AtMostScale(8); same.Scale != 4 || !slices.Equal(same.Counts, h.Counts) {
		t.Fatalf("raised the scale: %d", same.Scale)
	}
}
