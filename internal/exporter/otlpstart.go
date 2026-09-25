package exporter

import (
	"container/list"
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
// The series remembered are bounded as well, by otlpStartSeriesPerPoint
// times otlp.max_pending_points: series whose labels keep changing — a
// request ID, a timestamp — would otherwise each be kept for an hour, without
// limit. One export carries at most max_pending_points points, so the bound
// leaves room for every series being exported and for some churn; past it,
// the series seen least recently is forgotten first, and starts again if it
// comes back.

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

// forResource is the start time of each cumulative point of one resource.
func (t *otlpStartTimes) forResource(resource string) func(m model.Metric, at string) string {
	return func(m model.Metric, at string) string {
		return t.start(resource+"\x00"+otlpMetricKey(m), cumulativeCount(m), at)
	}
}

// start is when the series key began counting, given a point of count at
// time at: the time of its first point, or of the first after a reset.
func (t *otlpStartTimes) start(key string, count float64, at string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// The series unseen for longest are at the back: forget those unseen
	// for otlpStartForget.
	for back := t.recent.Back(); back != nil && now.Sub(back.Value.(*otlpStart).seen) > otlpStartForget; back = t.recent.Back() {
		t.forgetLocked(back)
	}
	var s *otlpStart
	if element := t.series[key]; element != nil {
		s = element.Value.(*otlpStart)
		t.recent.MoveToFront(element)
		if count < s.last {
			s.start = at
		}
	} else {
		s = &otlpStart{key: key, start: at}
		t.series[key] = t.recent.PushFront(s)
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
