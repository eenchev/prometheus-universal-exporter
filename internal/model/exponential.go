package model

import "math"

// An exponential histogram, OpenTelemetry's name for what Prometheus calls a
// native histogram, has no bounds of its own: its buckets are the powers of a
// base, 2^(2^-scale), so that at scale 0 they are (0.5, 1], (1, 2], (2, 4],
// …, at scale 1 each of those halves on the logarithmic scale, and so on.
// Bucket i holds the values in (base^i, base^(i+1)]. A recorder starts at
// the finest scale and lowers it, merging neighbouring buckets in pairs,
// whenever an observation would need more than a bounded number of
// buckets, so the histogram's resolution adapts to the range of values it
// actually sees, in bounded memory.
//
// Prometheus's native histograms are the same buckets: its schema is the
// scale, and its bucket i is (base^(i-1), base^i], one index above
// OpenTelemetry's. A histogram recorded here is written as either: OTLP's
// ExponentialHistogram and, in Prometheus's protobuf exposition, a native
// histogram (exporter/protobuf.go).

// The bounds of a recorder, those of OpenTelemetry's SDKs by default: it
// starts at scale 20, the finest the specification defines, and holds at
// most 160 buckets, lowering the scale to stay within them. A histogram of
// durations from 5 ms to a minute settles at scale 3, a bucket per 9% of the
// value.
const (
	ExponentialMaxScale int32 = 20
	ExponentialMaxSize  int   = 160
)

// ExponentialHistogram is the exponential buckets of a histogram of values
// that are not negative, the exporter's own durations. It is kept beside a
// Histogram's classic buckets, whose Sum and Count it shares: Count is
// ZeroCount and the counts of Counts added up.
//
// Counts[k] is the count of bucket Offset+k at Scale; ZeroCount counts the
// observations of 0. Min and Max are the least and the greatest value
// observed, 0 while nothing has been.
type ExponentialHistogram struct {
	Scale     int32
	ZeroCount uint64
	Offset    int32
	Counts    []uint64
	Min       float64
	Max       float64
	// observed says Min and Max hold a value.
	observed bool
}

// NewExponentialHistogram is an empty histogram at ExponentialMaxScale.
func NewExponentialHistogram() *ExponentialHistogram {
	return &ExponentialHistogram{Scale: ExponentialMaxScale}
}

// ExponentialIndex is the bucket of a positive, finite value v at a scale:
// the i for which base^i < v <= base^(i+1), base being 2^(2^-scale). It is
// the mapping function of OpenTelemetry's data model: a value's binary
// exponent alone at a scale of 0 or below, and its logarithm above; an exact
// power of two, which falls on a bound, is placed by its exponent, since a
// logarithm rounded either way could put it in the bucket above.
func ExponentialIndex(v float64, scale int32) int32 {
	frac, exp := math.Frexp(v)
	// v = frac × 2^exp with frac in [0.5, 1): v = 1.f × 2^(exp-1).
	exponent := int32(exp - 1) //nolint:gosec // G115: a double's exponent, -1074 to 1024
	powerOfTwo := frac == 0.5
	if scale <= 0 {
		if powerOfTwo {
			exponent--
		}
		return exponent >> -scale
	}
	if powerOfTwo {
		return exponent<<scale - 1
	}
	return int32(math.Ceil(math.Log(v)*math.Ldexp(math.Log2E, int(scale)))) - 1
}

// Observe adds a value, keeping at most maxSize buckets: when v falls further
// from the buckets held than they allow, the scale is lowered first, by as
// little as brings every bucket, v's included, within maxSize. A value of 0
// or below, which no power of the base is, counts in ZeroCount; NaN is not
// observed.
func (h *ExponentialHistogram) Observe(v float64, maxSize int) {
	if math.IsNaN(v) {
		return
	}
	if !h.observed || v < h.Min {
		h.Min = v
	}
	if !h.observed || v > h.Max {
		h.Max = v
	}
	h.observed = true
	if v <= 0 {
		h.ZeroCount++
		return
	}
	if math.IsInf(v, 1) {
		v = math.MaxFloat64
	}
	index := ExponentialIndex(v, h.Scale)
	if len(h.Counts) == 0 {
		h.Offset = index
		h.Counts = append(h.Counts, 1)
		return
	}
	low, high := min(h.Offset, index), max(h.last(), index)
	by := int32(0)
	for int64(high>>by)-int64(low>>by)+1 > int64(maxSize) {
		by++
	}
	if by > 0 {
		h.Downscale(by)
		index >>= by
	}
	switch last := h.last(); {
	case index < h.Offset:
		grown := make([]uint64, int(last-index)+1)
		copy(grown[h.Offset-index:], h.Counts)
		h.Counts, h.Offset = grown, index
	case index > last:
		h.Counts = append(h.Counts, make([]uint64, index-last)...)
	}
	h.Counts[index-h.Offset]++
}

// last is the index of the last bucket held.
func (h *ExponentialHistogram) last() int32 {
	return h.Offset + int32(len(h.Counts)) - 1 //nolint:gosec // G115: at most a few hundred buckets
}

// Downscale lowers the scale by by, merging each 2^by neighbouring buckets
// into the one bucket of the lower scale that holds them all: bucket i
// becomes bucket i >> by, the shift rounding towards minus infinity, as the
// bounds of the lower scale are every 2^by'th bound of the higher.
func (h *ExponentialHistogram) Downscale(by int32) {
	if by <= 0 {
		return
	}
	h.Scale -= by
	if len(h.Counts) == 0 {
		return
	}
	offset := h.Offset >> by
	merged := make([]uint64, int(h.last()>>by-offset)+1)
	for k, count := range h.Counts {
		merged[(h.Offset+int32(k))>>by-offset] += count //nolint:gosec // G115: as in last
	}
	h.Offset, h.Counts = offset, merged
}

// AtMostScale is a copy of the histogram at scale, or at its own scale when
// that is lower already.
func (h *ExponentialHistogram) AtMostScale(scale int32) *ExponentialHistogram {
	out := *h
	out.Counts = append([]uint64(nil), h.Counts...)
	out.Downscale(h.Scale - scale)
	return &out
}
