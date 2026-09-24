package exporter

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter keeps state per collector: its self-metric counters, the
// verbose per-request series and scrape-time histogram, cached results and
// what the failure log remembers. A reload can remove a collector, or change
// its definition, and that state has to follow:
//
//   - A removed collector's self-metric series stop being exposed and exported,
//     so Prometheus marks them stale instead of showing a collector that no
//     longer exists with frozen values; everything kept about it is dropped,
//     and a collector later added again under the name starts from zero.
//   - A changed collector keeps its counters, which describe the collector by
//     name, but its cached results are dropped: their keys carry the old
//     definition, so they could never be served again, and would only hold
//     memory and count in http_exporter_cache_entries until they expired.
//
// Rather than asking every way a configuration can change to notify the
// server, the server compares the configuration it last saw with the current
// one whenever it is about to use its per-collector state, which costs a
// pointer comparison while nothing has changed.

// reconcile brings the per-collector state in line with the configuration.
func (s *Server) reconcile() {
	cfg := s.manager.Get()
	previous := s.seenConfig.Swap(cfg)
	if previous == nil || previous == cfg {
		return
	}
	current := make(map[string]string, len(cfg.Collectors))
	for i := range cfg.Collectors {
		current[cfg.Collectors[i].Name] = s.fingerprints.fingerprint(cfg, &cfg.Collectors[i])
	}
	removed, stale := map[string]bool{}, map[string]bool{}
	for i := range previous.Collectors {
		c := &previous.Collectors[i]
		fingerprint, kept := current[c.Name]
		switch {
		case !kept:
			removed[c.Name] = true
			stale[c.Name] = true
		case fingerprint != collectorFingerprint(c):
			stale[c.Name] = true
		}
	}
	if len(stale) == 0 {
		return
	}
	if len(removed) > 0 {
		s.statsMu.Lock()
		for name := range removed {
			delete(s.stats, name)
		}
		s.statsMu.Unlock()
		s.requests.forgetCollectors(removed)
		s.durations.forgetCollectors(removed)
		s.failures.forgetCollectors(removed)
	}
	dropped := s.cache.dropCollectors(stale)
	s.logger.Debug("per-collector state follows the reloaded configuration", "removed", model.SortedKeys(removed), "changed", len(stale)-len(removed), "cache_entries_dropped", dropped)
}

// dropCollectors removes the cached results of the named collectors, and
// reports how many.
func (c *responseCache) dropCollectors(names map[string]bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for name := range names {
		for key := range c.byCollector[name] {
			c.removeLocked(key)
			dropped++
		}
	}
	return dropped
}

// forgetCollectors drops the per-request series of the named collectors.
func (t *requestTracker) forgetCollectors(names map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.stats {
		if names[key.Collector] {
			delete(t.stats, key)
		}
	}
	t.capReached = len(t.stats) >= VerboseRequestSeriesLimit
}

// forgetCollectors drops the scrape-time histograms of the named collectors.
func (d *scrapeDurations) forgetCollectors(names map[string]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name := range names {
		delete(d.collectors, name)
	}
}
