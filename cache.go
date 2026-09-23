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

// cacheEntry is one collector result held until it expires. It is fresh
// until freshUntil, answering repeats of its probe without a trip to the
// target, and then stale until expires, kept only to stand in for a trip that
// fails (cache.stale_if_error).
type cacheEntry struct {
	collector  string
	fetched    time.Time
	freshUntil time.Time
	expires    time.Time
	set        MetricSet
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

// Get returns a private copy of the cached metric set, and when it was
// fetched from the target, when a fresh entry exists for key. An entry past
// its freshness is a miss; one past its stale window too is removed.
func (c *responseCache) Get(key string, now time.Time) (MetricSet, time.Time, bool) {
	return c.lookup(key, now, false)
}

// GetStale is Get for a trip that failed: it also returns an entry past its
// freshness, as long as it is within the collector's stale_if_error.
func (c *responseCache) GetStale(key string, now time.Time) (MetricSet, time.Time, bool) {
	return c.lookup(key, now, true)
}

func (c *responseCache) lookup(key string, now time.Time, stale bool) (MetricSet, time.Time, bool) {
	if key == "" {
		return MetricSet{}, time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return MetricSet{}, time.Time{}, false
	}
	if !entry.expires.After(now) {
		delete(c.entries, key)
		return MetricSet{}, time.Time{}, false
	}
	if !stale && !entry.freshUntil.After(now) {
		return MetricSet{}, time.Time{}, false
	}
	return cloneMetricSet(entry.set), entry.fetched, true
}

// Put stores a private copy of set under key, fresh for ttl and then kept
// stale for staleIfError more. When both are zero the collector does not
// cache and nothing is stored.
func (c *responseCache) Put(key, collector string, set MetricSet, ttl, staleIfError time.Duration, maxEntries int, now time.Time) {
	if key == "" || ttl < 0 || staleIfError < 0 || ttl+staleIfError <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	freshUntil := now.Add(ttl)
	c.entries[key] = &cacheEntry{collector: collector, fetched: now, freshUntil: freshUntil, expires: freshUntil.Add(staleIfError), set: cloneMetricSet(set)}
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

// Stats sweeps expired entries and reports the entries held per collector,
// stale ones included.
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
