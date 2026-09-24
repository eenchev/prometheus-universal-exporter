package exporter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
	set        model.MetricSet
}

// responseCache is the process-local, in-memory probe cache. Entries are keyed
// by a fingerprint covering the collector definition and every element of the
// incoming probe request, so a cached result can only ever be served to a
// byte-for-byte identical request. Nothing is written to disk and nothing is
// shared between exporter processes.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	// byCollector indexes the keys of entries by collector, so keeping one
	// collector within its max_cache_entries looks at its entries alone.
	// Every change to entries goes through putLocked or removeLocked, which
	// keep the two in step.
	byCollector map[string]map[string]struct{}
}

func newResponseCache() *responseCache {
	return &responseCache{entries: map[string]*cacheEntry{}, byCollector: map[string]map[string]struct{}{}}
}

// putLocked stores entry under key, replacing any entry there.
func (c *responseCache) putLocked(key string, entry *cacheEntry) {
	c.removeLocked(key)
	c.entries[key] = entry
	keys := c.byCollector[entry.collector]
	if keys == nil {
		keys = map[string]struct{}{}
		c.byCollector[entry.collector] = keys
	}
	keys[key] = struct{}{}
}

// removeLocked removes the entry under key, if there is one.
func (c *responseCache) removeLocked(key string) {
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	delete(c.entries, key)
	keys := c.byCollector[entry.collector]
	delete(keys, key)
	if len(keys) == 0 {
		delete(c.byCollector, entry.collector)
	}
}

// Get returns a private copy of the cached metric set, and when it was
// fetched from the target, when a fresh entry exists for key. An entry past
// its freshness is a miss; one past its stale window too is removed.
func (c *responseCache) Get(key string, now time.Time) (model.MetricSet, time.Time, bool) {
	return c.lookup(key, now, false)
}

// GetStale is Get for a trip that failed: it also returns an entry past its
// freshness, as long as it is within the collector's stale_if_error.
func (c *responseCache) GetStale(key string, now time.Time) (model.MetricSet, time.Time, bool) {
	return c.lookup(key, now, true)
}

func (c *responseCache) lookup(key string, now time.Time, stale bool) (model.MetricSet, time.Time, bool) {
	if key == "" {
		return model.MetricSet{}, time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return model.MetricSet{}, time.Time{}, false
	}
	if !entry.expires.After(now) {
		c.removeLocked(key)
		return model.MetricSet{}, time.Time{}, false
	}
	if !stale && !entry.freshUntil.After(now) {
		return model.MetricSet{}, time.Time{}, false
	}
	return model.CloneMetricSet(entry.set), entry.fetched, true
}

// Put stores a private copy of set under key, fresh for ttl and then kept
// stale for StaleIfError more. When both are zero the collector does not
// cache and nothing is stored.
func (c *responseCache) Put(key, collector string, set model.MetricSet, ttl, staleIfError time.Duration, maxEntries int, now time.Time) {
	if key == "" || ttl < 0 || staleIfError < 0 || ttl+staleIfError <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	freshUntil := now.Add(ttl)
	c.putLocked(key, &cacheEntry{collector: collector, fetched: now, freshUntil: freshUntil, expires: freshUntil.Add(staleIfError), set: model.CloneMetricSet(set)})
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
	for key := range c.byCollector[collector] {
		if !c.entries[key].expires.After(now) {
			c.removeLocked(key)
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
		c.removeLocked(key)
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
			c.removeLocked(key)
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
//
// own is what a static target sets that no probe parameter can: a graphite
// collector's targets (statictarget.go). It is written in a
// section of its own after the headers, which a probe never writes, so a probe
// cannot be given a key that reads a result made with them.
func probeCacheKey(c *model.Collector, target string, query url.Values, forwarded http.Header, own ...string) string {
	return probeCacheKeyWith(collectorFingerprint(c), c, target, query, forwarded, own...)
}

// probeCacheKey is the key of a probe of c in cfg, with the collector's
// fingerprint remembered for as long as cfg is the configuration
// (fingerprint.go).
func (s *Server) probeCacheKey(cfg *model.Config, c *model.Collector, target string, query url.Values, forwarded http.Header, own ...string) string {
	return probeCacheKeyWith(s.fingerprints.fingerprint(cfg, c), c, target, query, forwarded, own...)
}

// probeCacheKeyWith is probeCacheKey for a collector whose fingerprint is
// already known.
func probeCacheKeyWith(fingerprint string, c *model.Collector, target string, query url.Values, forwarded http.Header, own ...string) string {
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
	if len(own) > 0 {
		write("own", strconv.Itoa(len(own)))
		write(own...)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// collectorFingerprint hashes the effective collector definition so that a
// configuration reload retires every entry cached under the previous
// definition. An empty result disables caching for the collector.
func collectorFingerprint(c *model.Collector) string {
	encoded, err := yaml.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// A collector's cache has two windows:
//
//	cache:
//	  ttl: 30s             # a result answers repeats of its probe for 30s
//	  stale_if_error: 5m   # and, for 5m more, a probe whose trip fails
//
// Within ttl a result is fresh: a repeat of its probe is answered from memory
// without a trip to the target. After it, the result is stale and is kept for
// stale_if_error longer, only to stand in for a trip that fails — the target
// is down, answers an error status, times out, or its response cannot be
// decoded, transformed or validated, or the collector is at its
// max_concurrent_probes. The probe is then answered 200 with the last
// successful result instead of an error, as RFC 5861's stale-if-error does for
// HTTP caches. ttl may be 0: every probe then goes to the target, and the last
// good result is kept only as a fallback.
//
// An answer served stale must not pass for a fresh one, so while
// stale_if_error is set every answer of the collector carries two series of
// its own, which no rule of the collector may produce:
//
//	http_exporter_result_stale        1 when the answer is a stale result
//	                                  standing in for a failed trip, else 0
//	http_exporter_result_age_seconds  how long ago the answered result was
//	                                  fetched from the target: 0 for a trip
//	                                  just made, the entry's age for a
//	                                  cached answer, fresh or stale
//
// The failure is still logged and counted as it would have been, and the
// collector's http_exporter_cache_stale_served_total counts the stale
// answers. A stale answer does not count as a success.

// The series every answer of a collector with stale_if_error carries.
const (
	resultStaleMetric = "http_exporter_result_stale"
	resultAgeMetric   = "http_exporter_result_age_seconds"
)

var resultFreshnessHelp = map[string]string{
	resultStaleMetric: "1 when this answer is the last successful result, served because the trip to the target failed (cache.stale_if_error), else 0.",
	resultAgeMetric:   "Seconds since the answered result was fetched from the target: 0 for a trip just made, the cache entry's age otherwise.",
}

// withFreshness adds the freshness series to an answer of a collector with
// stale_if_error. fetched is when the result came from the target. It refuses
// a result that already has one of them, which the collector's rules made.
func withFreshness(set model.MetricSet, c *model.Collector, stale bool, fetched, now time.Time) (model.MetricSet, error) {
	if model.StaleIfError(c) <= 0 {
		return set, nil
	}
	if err := checkFreshnessNames(set); err != nil {
		return set, err
	}
	staleValue := 0.0
	if stale {
		staleValue = 1
	}
	age := now.Sub(fetched).Seconds()
	if age < 0 {
		age = 0
	}
	set.Metrics = append(set.Metrics,
		model.Metric{Name: resultStaleMetric, Help: resultFreshnessHelp[resultStaleMetric], Type: model.GaugeMetricType, Value: staleValue},
		model.Metric{Name: resultAgeMetric, Help: resultFreshnessHelp[resultAgeMetric], Type: model.GaugeMetricType, Value: age},
	)
	return set, nil
}

// checkFreshnessNames refuses a result that has a series of the names the
// freshness series use.
func checkFreshnessNames(set model.MetricSet) error {
	var clash []string
	seen := map[string]bool{}
	for _, m := range set.Metrics {
		if _, reserved := resultFreshnessHelp[m.Name]; reserved && !seen[m.Name] {
			seen[m.Name] = true
			clash = append(clash, m.Name)
		}
	}
	if len(clash) == 0 {
		return nil
	}
	sort.Strings(clash)
	return fmt.Errorf("the metric name %s is reserved for the series a collector with cache.stale_if_error adds itself", strings.Join(clash, " and "))
}
