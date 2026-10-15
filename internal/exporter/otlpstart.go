package exporter

import (
	"container/list"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An OTLP cumulative point — a counter's sum, a histogram, a summary — carries
// the time its series started counting, which a backend uses to tell a reset
// from a series that only grew. The exporter reads counters from targets and
// cannot know when a target began counting, so it does what the
// OpenTelemetry Collector's Prometheus receiver does: a series starts when the
// exporter first exports it, and starts again at a point whose count went
// down, which only a reset does. A series not exported for otlpStartForget is
// forgotten, and starts again when it comes back.
//
// A series is seen here when an export has a point of it, also when the
// point is then left out for another kind of its name written later
// (otlpMetricsOf): the count is one the series had, so it starts where it
// was first seen, and a count that fell in a point left out is the reset
// it is. Two writers of one series under a resource, two probes that
// answer with one counter, are one series: the lower count of one after
// the other's is a reset, and the higher after it is none.
//
// The exporter's own counters, histograms and summaries are not among them:
// it knows when each began to count, and their points carry that time
// (selfcreated.go, otlpMetrics).
//
// The series remembered are bounded as well, by otlpStartSeriesPerPoint
// times otlp.max_pending_points: series whose labels keep changing — a
// request ID, a timestamp — would otherwise each be kept for an hour, without
// limit. One export carries at most max_pending_points points, so the bound
// leaves room for every series being exported and for some churn; past it,
// the series seen least recently is forgotten first, and starts again if it
// comes back. And each is remembered by a key of at most otlpKeyLongest
// bytes, a longer one by its digest (otlpKeyOf), whatever its labels hold:
// a probe's target, with otlp.probe_attributes, may be 8 KiB.

const (
	otlpStartForget = time.Hour
	// otlpStartSeriesPerPoint times otlp.max_pending_points is how many
	// series' start times are remembered.
	otlpStartSeriesPerPoint = 2
)

type otlpStartTimes struct {
	mu     sync.Mutex
	series map[string]*list.Element
	// recent holds the series' *otlpStart, the most recently seen first, so
	// the ones to forget, unseen for longest, are at the back.
	recent *list.List
	// max bounds len(series); 0 is otlpStartMaxSeries of the default
	// otlp.max_pending_points.
	max int
	now func() time.Time
}

type otlpStart struct {
	key   string
	start string
	last  float64
	seen  time.Time
}

func newOTLPStartTimes() *otlpStartTimes {
	return &otlpStartTimes{series: map[string]*list.Element{}, recent: list.New(), now: time.Now}
}

// otlpStartMaxSeries is how many series' start times are remembered with
// otlp.max_pending_points set to maxPending, 0 for its default.
func otlpStartMaxSeries(maxPending int) int {
	if maxPending <= 0 {
		maxPending = model.DefaultOTLPMaxPendingPoints
	}
	return otlpStartSeriesPerPoint * maxPending
}

// bound sets how many series' start times are remembered, for
// otlp.max_pending_points set to maxPending; a lower bound than before takes
// effect at the next point.
func (t *otlpStartTimes) bound(maxPending int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.max = otlpStartMaxSeries(maxPending)
}

// forResource is the start time of each cumulative point of one resource,
// whose key resource is. A series is remembered under the resource's key,
// after its length as a part of a key is written (appendKeyPart), and then
// its own (otlpSeriesKey): where the one ends and the other begins is not
// read from what either holds, so no series of one resource has the start
// time of a series of another. The key is remembered as itself or by its
// digest (otlpKeyOf), and is made where the point's start time is found,
// so a series already seen is found without an allocation.
func (t *otlpStartTimes) forResource(resource string) func(m model.Metric, at string) string {
	of := string(appendKeyPart(nil, resource))
	return func(m model.Metric, at string) string {
		var room [otlpKeyRoom]byte
		var digest [sha256.Size]byte
		return t.start(otlpKeyOf(appendOTLPKey(room[:0], of, m.Labels, m.Name, string(m.Type)), &digest), cumulativeCount(m), at)
	}
}

// start is when the series key began counting, given a point of count at
// time at: the time of its first point, or of the first after a reset. key
// is the series' key as it is remembered (otlpKeyOf).
func (t *otlpStartTimes) start(key []byte, count float64, at string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// The series unseen for longest are at the back: forget those unseen
	// for otlpStartForget.
	for back := t.recent.Back(); back != nil && now.Sub(back.Value.(*otlpStart).seen) > otlpStartForget; back = t.recent.Back() {
		t.forgetLocked(back)
	}
	var s *otlpStart
	if element := t.series[string(key)]; element != nil {
		s = element.Value.(*otlpStart)
		t.recent.MoveToFront(element)
		if count < s.last {
			s.start = at
		}
	} else {
		s = &otlpStart{key: string(key), start: at}
		t.series[s.key] = t.recent.PushFront(s)
		limit := t.max
		if limit <= 0 {
			limit = otlpStartMaxSeries(0)
		}
		for len(t.series) > limit {
			t.forgetLocked(t.recent.Back())
		}
	}
	s.last, s.seen = count, now
	return s.start
}

func (t *otlpStartTimes) forgetLocked(element *list.Element) {
	t.recent.Remove(element)
	delete(t.series, element.Value.(*otlpStart).key)
}

// cumulativeCount is what a cumulative series counts: a histogram's or a
// summary's observations, a counter's value.
func cumulativeCount(m model.Metric) float64 {
	switch {
	case m.Histogram != nil:
		return float64(m.Histogram.Count)
	case m.Summary != nil:
		return float64(m.Summary.Count)
	}
	return m.Value
}
