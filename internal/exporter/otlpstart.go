package exporter

import (
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

const (
	otlpStartForget     = time.Hour
	otlpStartSweepEvery = 10 * time.Minute
)

type otlpStartTimes struct {
	mu        sync.Mutex
	series    map[string]*otlpStart
	lastSweep time.Time
	now       func() time.Time
}

type otlpStart struct {
	start string
	last  float64
	seen  time.Time
}

func newOTLPStartTimes() *otlpStartTimes {
	return &otlpStartTimes{series: map[string]*otlpStart{}, now: time.Now}
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
	if now.Sub(t.lastSweep) >= otlpStartSweepEvery {
		t.lastSweep = now
		for k, s := range t.series {
			if now.Sub(s.seen) > otlpStartForget {
				delete(t.series, k)
			}
		}
	}
	s := t.series[key]
	if s == nil || count < s.last {
		s = &otlpStart{start: at}
		t.series[key] = s
	}
	s.last, s.seen = count, now
	return s.start
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
