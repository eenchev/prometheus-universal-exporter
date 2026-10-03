package exporter

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func cachingCollector(name string, ttl time.Duration) model.Collector {
	c := testutil.Collector(name, "text")
	c.Cache.TTL = model.Duration(ttl)
	return c
}

func newCacheTestServer(t *testing.T, collectors ...model.Collector) (*Server, *config.Manager) {
	t.Helper()
	cfg := &model.Config{Collectors: collectors}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, "", slog.Default())
	return NewServer(manager, "python3", slog.Default()), manager
}

func probeOnce(t *testing.T, server *Server, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestResponseCacheExpiresAndEvictsBeyondLimit(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	set := model.MetricSet{Metrics: []model.Metric{{Name: "demo_value", Type: model.GaugeMetricType, Value: 1}}}

	cache.Put("first", "collector", set, time.Minute, 0, 2, now)
	cache.Put("second", "collector", set, 2*time.Minute, 0, 2, now)
	if _, _, ok := cache.Get("first", now.Add(time.Second)); !ok {
		t.Fatal("first entry should still be live")
	}
	if _, _, ok := cache.Get("first", now.Add(2*time.Minute)); ok {
		t.Fatal("expired entry was served")
	}

	cache = newResponseCache()
	cache.Put("first", "collector", set, time.Minute, 0, 2, now)
	cache.Put("second", "collector", set, 2*time.Minute, 0, 2, now)
	cache.Put("third", "collector", set, 3*time.Minute, 0, 2, now)
	cache.Put("other", "different", set, time.Minute, 0, 2, now)
	if _, _, ok := cache.Get("first", now); ok {
		t.Fatal("entry closest to expiry was not evicted")
	}
	for _, key := range []string{"second", "third", "other"} {
		if _, _, ok := cache.Get(key, now); !ok {
			t.Fatalf("entry %q was evicted unexpectedly", key)
		}
	}
	counts := cache.Stats(now)
	if counts["collector"] != 2 || counts["different"] != 1 {
		t.Fatalf("entry counts=%v", counts)
	}
	if counts := cache.Stats(now.Add(4 * time.Minute)); len(counts) != 0 {
		t.Fatalf("expired entries were not swept: %v", counts)
	}
}

// A stored set is the cache's own copy, which readers share rather than copy
// on every hit: appending to what a read returns, as the freshness series
// do, never reaches the entry or another read.
func TestCachedMetricSetIsCopiedOnStoreAndSharedOnRead(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	timestamp := int64(5)
	original := model.MetricSet{Metrics: make([]model.Metric, 1, 8)}
	original.Metrics[0] = model.Metric{
		Name:      "demo_value",
		Type:      model.HistogramMetricType,
		Labels:    map[string]string{"zone": "a"},
		Timestamp: &timestamp,
		Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 2}}, Sum: 3, Count: 2},
	}
	cache.Put("key", "collector", original, time.Minute, 0, 10, now)
	original.Metrics[0].Labels["zone"] = "mutated-source"
	timestamp = 99

	first, _, ok := cache.Get("key", now)
	if !ok {
		t.Fatal("entry was not cached")
	}
	if first.Metrics[0].Labels["zone"] != "a" || *first.Metrics[0].Timestamp != 5 {
		t.Fatalf("cache stored an aliased series: %+v", first.Metrics[0])
	}
	second, _, _ := cache.Get("key", now)
	a := append(first.Metrics, model.Metric{Name: "a"})
	b := append(second.Metrics, model.Metric{Name: "b"})
	if a[1].Name != "a" || b[1].Name != "b" {
		t.Fatal("two reads appended into one array")
	}
	third, _, _ := cache.Get("key", now)
	if len(third.Metrics) != 1 {
		t.Fatalf("an append reached the entry: %d series", len(third.Metrics))
	}
}

func TestResponseCacheFreshAndStaleWindows(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	set := model.MetricSet{Metrics: []model.Metric{{Name: "demo_value", Type: model.GaugeMetricType, Value: 1}}}
	cache.Put("key", "collector", set, time.Minute, 5*time.Minute, 10, now)
	if _, fetched, ok := cache.Get("key", now.Add(30*time.Second)); !ok || !fetched.Equal(now) {
		t.Fatalf("a fresh entry was not served: %v %v", ok, fetched)
	}
	if _, _, ok := cache.Get("key", now.Add(2*time.Minute)); ok {
		t.Fatal("Get served an entry past its ttl")
	}
	if got, fetched, ok := cache.GetStale("key", now.Add(2*time.Minute)); !ok || !fetched.Equal(now) || got.Metrics[0].Value != 1 {
		t.Fatalf("GetStale did not serve an entry within stale_if_error: %v", ok)
	}
	if n := cache.Stats(now.Add(2 * time.Minute))["collector"]; n != 1 {
		t.Fatalf("a stale entry is not counted: %d", n)
	}
	if _, _, ok := cache.GetStale("key", now.Add(7*time.Minute)); ok {
		t.Fatal("GetStale served an entry past stale_if_error")
	}
	if n := cache.Stats(now.Add(7 * time.Minute))["collector"]; n != 0 {
		t.Fatalf("an expired entry is still held: %d", n)
	}

	// ttl 0 keeps a result only as a fallback.
	cache.Put("fallback", "collector", set, 0, time.Minute, 10, now)
	if _, _, ok := cache.Get("fallback", now); ok {
		t.Fatal("a ttl of 0 answered from the cache")
	}
	if _, _, ok := cache.GetStale("fallback", now.Add(30*time.Second)); !ok {
		t.Fatal("a ttl of 0 with stale_if_error kept nothing to fall back on")
	}
	cache.Put("none", "collector", set, 0, 0, 10, now)
	if _, _, ok := cache.GetStale("none", now); ok {
		t.Fatal("a collector without a cache stored a result")
	}
}

// The per-collector index always holds exactly the keys of the entries, and a
// collector's max_cache_entries leaves the others' entries alone.
func TestTheCacheIndexFollowsItsEntries(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	set := model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1}}}
	check := func(when string) {
		t.Helper()
		indexed := 0
		for collector, entries := range cache.byCollector {
			if entries.Len() == 0 {
				t.Errorf("%s: %s has an empty index", when, collector)
			}
			for i, indexedEntry := range *entries {
				indexed++
				if entry := cache.entries[indexedEntry.key]; entry != indexedEntry || entry.collector != collector || entry.index != i {
					t.Errorf("%s: %s indexes %s, which it does not hold at %d", when, collector, indexedEntry.key, i)
				}
			}
		}
		if indexed != len(cache.entries) {
			t.Errorf("%s: %d keys indexed, %d entries", when, indexed, len(cache.entries))
		}
	}
	for i := range 20 {
		cache.Put(fmt.Sprint("a", i), "a", set, time.Minute, 0, 5, now.Add(time.Duration(i)*time.Second))
		cache.Put(fmt.Sprint("b", i), "b", set, time.Duration(i+1)*time.Second, 0, 0, now)
	}
	check("after the puts")
	if counts := cache.Stats(now); counts["a"] != 5 || counts["b"] != 20 {
		t.Fatalf("counts %v: a is capped at 5, b is not capped", counts)
	}
	cache.Put("a19", "a", set, time.Minute, 0, 5, now) // replaced in place
	check("after a replacement")
	if _, _, ok := cache.Get("b0", now.Add(2*time.Second)); ok {
		t.Fatal("an expired entry was served")
	}
	check("after an expiry")
	cache.Stats(now.Add(10 * time.Second))
	check("after a sweep")
	if dropped := cache.dropCollectors(map[string]bool{"a": true}); dropped != 5 {
		t.Fatalf("dropped %d, want 5", dropped)
	}
	check("after a drop")
	if _, ok := cache.byCollector["a"]; ok {
		t.Fatal("a dropped collector is still indexed")
	}
}

// Eviction removes the entries closest to expiry — their stale_if_error
// window included, not only their ttl — and of two expiring together the one
// with the smaller key; expired entries go before any live one.
func TestCacheEvictionOrder(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	set := model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1}}}
	// b's ttl is the shortest, but its stale window keeps it longest.
	cache.Put("a", "c", set, 2*time.Minute, 0, 3, now)
	cache.Put("b", "c", set, time.Second, 10*time.Minute, 3, now)
	cache.Put("d", "c", set, 3*time.Minute, 0, 3, now)
	cache.Put("e", "c", set, 3*time.Minute, 0, 3, now)
	held := func() string {
		keys := make([]string, 0, len(cache.entries))
		for _, entry := range *cache.byCollector["c"] {
			keys = append(keys, entry.key)
		}
		slices.Sort(keys)
		return strings.Join(keys, " ")
	}
	if got := held(); got != "b d e" {
		t.Fatalf("held %q, want a, closest to expiry, gone", got)
	}
	cache.Put("f", "c", set, 3*time.Minute, 0, 3, now)
	if got := held(); got != "b e f" {
		t.Fatalf("held %q, want d, the smaller of the keys expiring together, gone", got)
	}
	// Once e and f have expired, new entries take their places first.
	later := now.Add(4 * time.Minute)
	cache.Put("g", "c", set, time.Minute, 0, 3, later)
	cache.Put("h", "c", set, time.Minute, 0, 3, later)
	if got := held(); got != "b g h" {
		t.Fatalf("held %q, want the expired entries gone and b kept", got)
	}
}

// The eviction keeps exactly the live entries the sort over all of them kept,
// whatever the order and lifetimes of the puts.
func TestCacheEvictionMatchesSortingEveryEntry(t *testing.T) {
	cache := newResponseCache()
	reference := map[string]time.Time{} // live key -> expires
	start := time.Unix(1_700_000_000, 0)
	set := model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1}}}
	const maxEntries = 7
	seed := uint64(1)
	next := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	for i := range 5000 {
		now := start.Add(time.Duration(i) * time.Second)
		key := fmt.Sprint("k", next(40))
		ttl := time.Duration(next(30)) * time.Second
		stale := time.Duration(next(3)*next(20)) * time.Second
		if ttl+stale <= 0 {
			ttl = time.Second
		}
		cache.Put(key, "c", set, ttl, stale, maxEntries, now)

		// What the cache did before: every expired entry swept, then the
		// live ones sorted and those past the budget removed.
		reference[key] = now.Add(ttl + stale)
		var live []string
		for k, expires := range reference {
			if !expires.After(now) {
				delete(reference, k)
				continue
			}
			live = append(live, k)
		}
		if len(live) > maxEntries {
			slices.SortFunc(live, func(a, b string) int {
				if c := reference[a].Compare(reference[b]); c != 0 {
					return c
				}
				return strings.Compare(a, b)
			})
			for _, k := range live[:len(live)-maxEntries] {
				delete(reference, k)
			}
		}

		for k, entry := range cache.entries {
			if _, kept := reference[k]; !kept && entry.expires.After(now) {
				t.Fatalf("put %d: the cache holds %s, which the sort evicted", i, k)
			}
		}
		for k := range reference {
			if _, held := cache.entries[k]; !held {
				t.Fatalf("put %d: the cache evicted %s, which the sort kept", i, k)
			}
		}
	}
}

// A Put below a collector's max_cache_entries does no eviction work, and one
// at it finds the entry to evict without looking at, or sorting, the rest.
func BenchmarkCachePut(b *testing.B) {
	set := model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1}}}
	for _, entries := range []int{1000, 10000} {
		for _, full := range []bool{false, true} {
			name := fmt.Sprintf("entries=%d/under_cap", entries)
			if full {
				name = fmt.Sprintf("entries=%d/at_cap", entries)
			}
			b.Run(name, func(b *testing.B) {
				cache := newResponseCache()
				now := time.Now()
				for i := range entries {
					cache.Put(fmt.Sprint("k", i), "c", set, time.Hour, 0, entries, now.Add(time.Duration(i)*time.Millisecond))
				}
				limit := entries + 1
				if full {
					limit = entries
				}
				keys := make([]string, 1024)
				for i := range keys {
					keys[i] = fmt.Sprint("new", i)
				}
				b.ResetTimer()
				for i := range b.N {
					key := keys[i%len(keys)]
					if !full {
						// Stay one under the cap: replace a key already held.
						key = "k0"
					}
					cache.Put(key, "c", set, time.Hour, 0, limit, now.Add(time.Duration(entries+i)*time.Millisecond))
				}
			})
		}
	}
}
