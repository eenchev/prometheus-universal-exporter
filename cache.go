package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// cacheEntry is one collector result held until it expires.
type cacheEntry struct {
	collector string
	expires   time.Time
	set       MetricSet
}

// responseCache is the process-local, in-memory probe cache. Entries are keyed
// by a fingerprint covering the collector definition and every element of the
// incoming probe request, so a cached result can only ever be served to a
// byte-for-byte identical request. Nothing is written to disk and nothing is
// shared between exporter processes.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

func newResponseCache() *responseCache {
	return &responseCache{entries: map[string]*cacheEntry{}}
}

// Get returns a private copy of the cached metric set when a live entry exists
// for key. Expired entries are removed on lookup and reported as a miss.
func (c *responseCache) Get(key string, now time.Time) (MetricSet, bool) {
	if key == "" {
		return MetricSet{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return MetricSet{}, false
	}
	if !entry.expires.After(now) {
		delete(c.entries, key)
		return MetricSet{}, false
	}
	return cloneMetricSet(entry.set), true
}

// Put stores a private copy of set under key for the given time to live. A
// non-positive ttl disables caching for the collector and stores nothing.
func (c *responseCache) Put(key, collector string, set MetricSet, ttl time.Duration, maxEntries int, now time.Time) {
	if key == "" || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &cacheEntry{collector: collector, expires: now.Add(ttl), set: cloneMetricSet(set)}
	c.evictLocked(collector, maxEntries, now)
}

// evictLocked keeps a collector within its configured entry budget. Expired
// entries are dropped first; if the collector is still over budget the entries
// closest to expiry are removed.
func (c *responseCache) evictLocked(collector string, maxEntries int, now time.Time) {
	if maxEntries <= 0 {
		return
	}
	var live []string
	for key, entry := range c.entries {
		if entry.collector != collector {
			continue
		}
		if !entry.expires.After(now) {
			delete(c.entries, key)
			continue
		}
		live = append(live, key)
	}
	if len(live) <= maxEntries {
		return
	}
	sort.Slice(live, func(i, j int) bool {
		first, second := c.entries[live[i]], c.entries[live[j]]
		if first.expires.Equal(second.expires) {
			return live[i] < live[j]
		}
		return first.expires.Before(second.expires)
	})
	for _, key := range live[:len(live)-maxEntries] {
		delete(c.entries, key)
	}
}

// Stats sweeps expired entries and reports the live entry count per collector.
func (c *responseCache) Stats(now time.Time) map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := make(map[string]int, len(c.entries))
	for key, entry := range c.entries {
		if !entry.expires.After(now) {
			delete(c.entries, key)
			continue
		}
		counts[entry.collector]++
	}
	return counts
}

// probeCacheKey fingerprints a probe request. The key covers the collector
// definition, the target, every query parameter of the probe request including
// per-scrape overrides, and every header forwarded to the target including a
// forwarded Authorization value. A request that omits a credential, a header,
// or a TLS override therefore produces a different key and can never read an
// entry populated by a request that supplied one. An empty result means the
// request must not be cached.
func probeCacheKey(c *Collector, target string, query url.Values, forwarded http.Header) string {
	fingerprint := collectorFingerprint(c)
	if fingerprint == "" {
		return ""
	}
	digest := sha256.New()
	write := func(values ...string) {
		for _, value := range values {
			fmt.Fprintf(digest, "%d:", len(value))
			_, _ = io.WriteString(digest, value)
			_, _ = digest.Write([]byte{0})
		}
	}
	write("collector", c.Name, "definition", fingerprint, "target", target)
	write("query")
	queryKeys := make([]string, 0, len(query))
	for key := range query {
		queryKeys = append(queryKeys, key)
	}
	sort.Strings(queryKeys)
	for _, key := range queryKeys {
		write(key)
		write(query[key]...)
	}
	write("headers")
	headerNames := make([]string, 0, len(forwarded))
	for name := range forwarded {
		headerNames = append(headerNames, http.CanonicalHeaderKey(name))
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		write(name)
		write(forwarded[name]...)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// collectorFingerprint hashes the effective collector definition so that a
// configuration reload retires every entry cached under the previous
// definition. An empty result disables caching for the collector.
func collectorFingerprint(c *Collector) string {
	encoded, err := yaml.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func cloneMetricSet(in MetricSet) MetricSet {
	out := MetricSet{Metrics: make([]Metric, 0, len(in.Metrics))}
	for _, metric := range in.Metrics {
		out.Metrics = append(out.Metrics, cloneMetric(metric))
	}
	return out
}

func cloneMetric(in Metric) Metric {
	out := in
	if in.Labels != nil {
		out.Labels = cloneLabels(in.Labels)
	}
	if in.Timestamp != nil {
		timestamp := *in.Timestamp
		out.Timestamp = &timestamp
	}
	if in.Histogram != nil {
		histogram := *in.Histogram
		histogram.Buckets = append([]Bucket(nil), in.Histogram.Buckets...)
		out.Histogram = &histogram
	}
	if in.Summary != nil {
		summary := *in.Summary
		summary.Quantiles = append([]Quantile(nil), in.Summary.Quantiles...)
		out.Summary = &summary
	}
	return out
}
