package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

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

// CacheConfig is a collector's cache.
type CacheConfig struct {
	// TTL is how long a result answers repeats of its probe.
	TTL Duration `yaml:"ttl"`
	// StaleIfError is how long after TTL a result stands in for a trip that
	// fails.
	StaleIfError Duration `yaml:"stale_if_error"`
}

var cacheConfigKeys = []string{"ttl", "stale_if_error"}

// UnmarshalYAML refuses the keys it does not know, which a custom decoder
// would otherwise let through, and explains the one-value form.
func (c *CacheConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return fmt.Errorf("line %d: cache is a mapping: write cache: {ttl: %s} to answer repeats of a probe for %s, and add stale_if_error to answer with the last good result when the target fails", n.Line, n.Value, n.Value)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: cache must be a mapping with ttl and stale_if_error", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if key := n.Content[i].Value; !contains(cacheConfigKeys, key) {
			return fmt.Errorf("line %d: cache has the unknown key %q; it takes %s", n.Content[i].Line, key, strings.Join(cacheConfigKeys, " and "))
		}
	}
	type plain CacheConfig
	return n.Decode((*plain)(c))
}

// validateCache checks a collector's cache when the configuration loads.
func validateCache(c *Collector) error {
	if c.Cache.TTL < 0 {
		return fmt.Errorf("collector %q cache.ttl must not be negative", c.Name)
	}
	if c.Cache.StaleIfError < 0 {
		return fmt.Errorf("collector %q cache.stale_if_error must not be negative", c.Name)
	}
	return nil
}

// cacheTTL and staleIfError are a collector's two windows.
func cacheTTL(c *Collector) time.Duration     { return time.Duration(c.Cache.TTL) }
func staleIfError(c *Collector) time.Duration { return time.Duration(c.Cache.StaleIfError) }

// usesCache reports whether the collector keeps results at all.
func usesCache(c *Collector) bool { return cacheTTL(c) > 0 || staleIfError(c) > 0 }

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
func withFreshness(set MetricSet, c *Collector, stale bool, fetched, now time.Time) (MetricSet, error) {
	if staleIfError(c) <= 0 {
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
		Metric{Name: resultStaleMetric, Help: resultFreshnessHelp[resultStaleMetric], Type: GaugeMetricType, Value: staleValue},
		Metric{Name: resultAgeMetric, Help: resultFreshnessHelp[resultAgeMetric], Type: GaugeMetricType, Value: age},
	)
	return set, nil
}

// checkFreshnessNames refuses a result that has a series of the names the
// freshness series use.
func checkFreshnessNames(set MetricSet) error {
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
