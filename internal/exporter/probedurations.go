package exporter

import (
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// http_exporter_probe_duration_seconds is a histogram per collector, always
// published, of how long the collector's probes and static target scrapes
// took, each measured as http_exporter_scrape_duration_seconds measures it:
// from the probe's arrival, or the scrape's start, to its answer being
// ready. The gauge only holds the last of them, so a slow probe between two
// scrapes of the self-metrics is lost; the histogram keeps every one.
//
// It is labelled collector and kind: probe for /probe, static for the
// static target scrapes, since the two answer different callers on
// different schedules, and a quantile across both would describe neither.
// Every collector has its probe series from the start; a collector has its
// static series while a static target of the file in force names it, so an
// exporter without static targets has none. It is never labelled by
// target: a probe names its target, which would make a series of every
// address anyone asked for.
//
// Unlike the verbose trip histogram (verbosemetrics.go), which observes
// trips to the target alone, it observes every probe, those answered from
// the cache or by sharing another probe's trip included: it says how long
// the exporter took to answer, which is what a scrape_timeout is set
// against, where the trip histogram describes the target. Both have the
// same buckets, exponential buckets and exemplars (durationHistogram).
const (
	probeDurationName = "http_exporter_probe_duration_seconds"
	probeDurationHelp = "Duration of this collector's probes (kind probe) and static target scrapes (kind static), to when the answer was ready, in seconds; cache hits and shared probes included."
)

// observeDuration records elapsed in the histogram of stats that pick
// chooses, and makes it its bucket's exemplar once the trace of the probe or the
// scrape, link, is exported (nil for none).
func observeDuration(stats *serverStats, pick func(*serverStats) *durationHistogram, elapsed time.Duration, link *traceLink) {
	if stats == nil {
		return
	}
	at := time.Now()
	stats.mu.Lock()
	bucket := pick(stats).observe(elapsed)
	stats.mu.Unlock()
	if link == nil {
		return
	}
	link.whenExported(func(trace, span string) {
		e := &model.Exemplar{TraceID: trace, SpanID: span, Value: elapsed.Seconds(), Time: at}
		stats.mu.Lock()
		pick(stats).exemplify(bucket, e)
		stats.mu.Unlock()
	})
}

func probeDurations(s *serverStats) *durationHistogram  { return &s.probeDurations }
func staticDurations(s *serverStats) *durationHistogram { return &s.staticDurations }

// observeProbe records the duration of a probe of the collector whose
// statistics are stats, link being its trace.
func observeProbe(stats *serverStats, elapsed time.Duration, link *traceLink) {
	observeDuration(stats, probeDurations, elapsed, link)
}

// observeStaticScrape records the duration of a static target scrape.
func observeStaticScrape(stats *serverStats, elapsed time.Duration, link *traceLink) {
	observeDuration(stats, staticDurations, elapsed, link)
}

// durationsOf is the histogram of stats that pick chooses, read under its
// lock.
func durationsOf(stats *serverStats, pick func(*serverStats) *durationHistogram) *model.Histogram {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	return pick(stats).histogram()
}

// probeDurationMetrics builds the family for the collectors named, in
// order, whose statistics are stats: each collector's probe series, and its
// static series when a static target in force names it. Both count since
// the collector's statistics do (selfcreated.go).
func (s *Server) probeDurationMetrics(names []string, stats map[string]*serverStats) []model.Metric {
	static := map[string]bool{}
	for _, target := range s.manager.StaticTargets() {
		static[target.Collector] = true
	}
	out := make([]model.Metric, 0, len(names)+len(static))
	for _, name := range names {
		collector := stats[name]
		created := createdMillis(collector.snapshot().created)
		out = append(out, model.Metric{
			Name: probeDurationName, Help: probeDurationHelp, Type: model.HistogramMetricType,
			Labels: map[string]string{"collector": name, "kind": "probe"}, Histogram: durationsOf(collector, probeDurations), Created: created,
		})
		if static[name] {
			out = append(out, model.Metric{
				Name: probeDurationName, Help: probeDurationHelp, Type: model.HistogramMetricType,
				Labels: map[string]string{"collector": name, "kind": "static"}, Histogram: durationsOf(collector, staticDurations), Created: created,
			})
		}
	}
	return out
}
