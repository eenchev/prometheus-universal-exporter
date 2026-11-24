package config

import (
	"fmt"
	"math"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkHistogramRule checks a rule's histogram block. where names the rule,
// whose type is already defaulted. The block makes a histogram of the values
// the rule reads. So it belongs to a rule of type histogram whose transform
// reads values. It does not belong to a prometheus rule, which passes on the
// target's histograms with their own buckets, or to a python rule, whose
// script makes its series itself.
//
// The buckets are the upper bounds of the histogram's buckets. Each is
// written once, in increasing order, as the exposition writes them. A bound
// out of order or written twice is a mistake that sorting would hide. +Inf
// is every histogram's last bucket and the exporter adds it, so neither it
// nor any other non-finite bound should be written.
//
// A label named le is refused too. A histogram's buckets carry that label,
// and a series that set its own would fail every scrape, as a histogram read
// from a target with one does (model.MetricSet.Validate).
func checkHistogramRule(x *model.Collector, r *model.MetricRule, where string) error {
	h := r.Histogram
	if h == nil {
		return nil
	}
	switch {
	case x.Transform.Type == "prometheus":
		return fmt.Errorf("%s sets histogram, which a prometheus rule does not take: it passes on the histograms the target exposes, with their own buckets; take the block out", where)
	case x.Transform.Type == "python":
		return fmt.Errorf("%s sets histogram, which a python rule does not take: the script makes each of its series and gives it its type itself; take the block out", where)
	case r.Type != model.HistogramMetricType:
		return fmt.Errorf("%s sets histogram, but its type is %s; the block makes a histogram of the values the rule reads, so set type: histogram, or take the block out", where, r.Type)
	case len(h.Buckets) == 0:
		return fmt.Errorf("%s histogram has no buckets; list the upper bounds of its buckets in increasing order, as in buckets: [0.1, 0.5, 1], and the bucket +Inf is added", where)
	case len(h.Buckets) > model.MaxHistogramBuckets:
		return fmt.Errorf("%s histogram has %d buckets, more than the most, %d; every bucket is a sample of every histogram on every scrape, so choose fewer bounds", where, len(h.Buckets), model.MaxHistogramBuckets)
	}
	for i, bound := range h.Buckets {
		switch {
		case math.IsInf(bound, 1):
			return fmt.Errorf("%s histogram bucket %d is +Inf, which every histogram has as its last bucket; leave it out", where, i+1)
		case math.IsNaN(bound) || math.IsInf(bound, 0):
			return fmt.Errorf("%s histogram bucket %d is %v, not a finite number", where, i+1, bound)
		case i > 0 && bound <= h.Buckets[i-1]:
			return fmt.Errorf("%s histogram bucket %d, %v, is not greater than the one before it, %v; list the upper bounds in increasing order, each once", where, i+1, bound, h.Buckets[i-1])
		}
	}
	for _, label := range r.Labels {
		if label.Name == "le" {
			return fmt.Errorf("%s has a label le, which a histogram's buckets carry; name the label something else", where)
		}
	}
	return nil
}
