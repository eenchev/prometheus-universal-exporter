package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponseCacheFreshAndStaleWindows(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	set := MetricSet{Metrics: []Metric{{Name: "demo_value", Type: GaugeMetricType, Value: 1}}}
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

func loadCacheConfig(t *testing.T, cache string) (*Config, error) {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	document := "collectors:\n  - name: cached\n    request:\n      type: http\n" + cache +
		"    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestStaleIfErrorConfiguration(t *testing.T) {
	cfg, err := loadCacheConfig(t, "    cache:\n      ttl: 30s\n      stale_if_error: 5m\n")
	if err != nil {
		t.Fatal(err)
	}
	if c := cfg.Collectors[0].Cache; time.Duration(c.TTL) != 30*time.Second || time.Duration(c.StaleIfError) != 5*time.Minute {
		t.Fatalf("cache=%+v", c)
	}
	cfg, err = loadCacheConfig(t, "    cache:\n      stale_if_error: 1m\n")
	if err != nil || cfg.Collectors[0].Cache.TTL != 0 || !usesCache(&cfg.Collectors[0]) {
		t.Fatalf("stale_if_error without ttl: %v %+v", err, cfg)
	}
	for name, tc := range map[string]struct{ yaml, want string }{
		"one value":       {"    cache: 60s\n", "cache is a mapping: write cache: {ttl: 60s}"},
		"unknown key":     {"    cache:\n      stale: 5m\n", `cache has the unknown key "stale"`},
		"bad duration":    {"    cache:\n      stale_if_error: soon\n", "soon"},
		"negative ttl":    {"    cache:\n      ttl: -1s\n", "cache.ttl must not be negative"},
		"negative stale":  {"    cache:\n      stale_if_error: -1s\n", "cache.stale_if_error must not be negative"},
		"list, not a map": {"    cache: [30s]\n", "cache must be a mapping"},
	} {
		if _, err := loadCacheConfig(t, tc.yaml); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want it to contain %q", name, err, tc.want)
		}
	}
}

// flakyTarget answers value=<n> while healthy, and fails with its mode
// otherwise: "status" answers 503, "garbage" answers 200 without a value.
type flakyTarget struct {
	value atomic.Int64
	mode  atomic.Value
	calls atomic.Int64
}

func newFlakyTarget(t *testing.T) (*flakyTarget, *httptest.Server) {
	f := &flakyTarget{}
	f.mode.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.calls.Add(1)
		switch f.mode.Load().(string) {
		case "status":
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		case "garbage":
			_, _ = w.Write([]byte("<html>maintenance</html>"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=" + strconv.FormatInt(f.value.Load(), 10) + "\n"))
	}))
	t.Cleanup(server.Close)
	return f, server
}

func staleCollector(ttl, stale time.Duration) Collector {
	c := testCollector("flaky", "text")
	c.Cache = CacheConfig{TTL: Duration(ttl), StaleIfError: Duration(stale)}
	// A page without the value fails the probe rather than answering nothing.
	c.Metrics[0].ErrorMode = "fail"
	return c
}

// ageEntries moves every cache entry back in time by d.
func ageEntries(server *Server, d time.Duration) {
	server.cache.mu.Lock()
	defer server.cache.mu.Unlock()
	for _, e := range server.cache.entries {
		e.fetched = e.fetched.Add(-d)
		e.freshUntil = e.freshUntil.Add(-d)
		e.expires = e.expires.Add(-d)
	}
}

func TestStaleIfErrorAnswersAFailedTripWithTheLastGoodResult(t *testing.T) {
	for _, mode := range []string{"status", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			flaky, target := newFlakyTarget(t)
			flaky.value.Store(1)
			server, _ := newCacheTestServer(t, staleCollector(0, 5*time.Minute))
			probe := "/probe?collector=flaky&target=" + url.QueryEscape(target.URL)

			fresh := probeOnce(t, server, probe, nil)
			if fresh.Code != http.StatusOK || seriesValue(t, fresh.Body.String(), "demo_value") != 1 ||
				seriesValue(t, fresh.Body.String(), resultStaleMetric) != 0 || seriesValue(t, fresh.Body.String(), resultAgeMetric) != 0 {
				t.Fatalf("fresh answer: %d\n%s", fresh.Code, fresh.Body.String())
			}
			if !strings.Contains(fresh.Body.String(), "# TYPE "+resultStaleMetric+" gauge") {
				t.Errorf("the freshness series have no TYPE:\n%s", fresh.Body.String())
			}

			ageEntries(server, 90*time.Second)
			flaky.mode.Store(mode)
			stale := probeOnce(t, server, probe, nil)
			if stale.Code != http.StatusOK || seriesValue(t, stale.Body.String(), "demo_value") != 1 || seriesValue(t, stale.Body.String(), resultStaleMetric) != 1 {
				t.Fatalf("a failed trip within stale_if_error: %d\n%s", stale.Code, stale.Body.String())
			}
			if age := seriesValue(t, stale.Body.String(), resultAgeMetric); age < 90 || age > 95 {
				t.Errorf("age %v, want about 90", age)
			}
			exposition := selfMetrics(t, server)
			for _, want := range []string{
				`http_exporter_cache_stale_served_total{collector="flaky"} 1`,
				`http_exporter_scrape_success_total{collector="flaky"} 1`,
				`http_exporter_scrapes_total{collector="flaky"} 2`,
			} {
				if !strings.Contains(exposition, want) {
					t.Errorf("self-metrics missing %q", want)
				}
			}
			if flaky.calls.Load() != 2 {
				t.Errorf("the target was called %d times, want 2: ttl 0 goes to it every time", flaky.calls.Load())
			}

			// Recovered: fresh again, with the new value.
			flaky.mode.Store("")
			flaky.value.Store(2)
			again := probeOnce(t, server, probe, nil)
			if seriesValue(t, again.Body.String(), "demo_value") != 2 || seriesValue(t, again.Body.String(), resultStaleMetric) != 0 {
				t.Fatalf("after recovery:\n%s", again.Body.String())
			}

			// Past stale_if_error the failure is answered.
			ageEntries(server, 6*time.Minute)
			flaky.mode.Store(mode)
			if failed := probeOnce(t, server, probe, nil); failed.Code != http.StatusBadGateway {
				t.Fatalf("past stale_if_error: %d\n%s", failed.Code, failed.Body.String())
			}
		})
	}
}

func TestStaleIfErrorWithATTLAnswersFreshHitsWithTheirAge(t *testing.T) {
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(3)
	server, _ := newCacheTestServer(t, staleCollector(time.Minute, time.Minute))
	probe := "/probe?collector=flaky&target=" + url.QueryEscape(target.URL)
	_ = probeOnce(t, server, probe, nil)
	ageEntries(server, 20*time.Second)
	hit := probeOnce(t, server, probe, nil)
	if flaky.calls.Load() != 1 || seriesValue(t, hit.Body.String(), resultStaleMetric) != 0 || seriesValue(t, hit.Body.String(), resultAgeMetric) < 20 {
		t.Fatalf("a fresh cache hit (%d calls):\n%s", flaky.calls.Load(), hit.Body.String())
	}
	// Past the ttl the target is asked; when it fails, the entry stands in.
	ageEntries(server, time.Minute)
	flaky.mode.Store("status")
	stale := probeOnce(t, server, probe, nil)
	if flaky.calls.Load() != 2 || stale.Code != http.StatusOK || seriesValue(t, stale.Body.String(), resultStaleMetric) != 1 {
		t.Fatalf("past the ttl (%d calls): %d\n%s", flaky.calls.Load(), stale.Code, stale.Body.String())
	}
}

func TestWithoutStaleIfErrorNothingChanges(t *testing.T) {
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(1)
	server, _ := newCacheTestServer(t, staleCollector(time.Nanosecond, 0))
	probe := "/probe?collector=flaky&target=" + url.QueryEscape(target.URL)
	fresh := probeOnce(t, server, probe, nil)
	if strings.Contains(fresh.Body.String(), resultStaleMetric) || strings.Contains(fresh.Body.String(), resultAgeMetric) {
		t.Fatalf("freshness series without stale_if_error:\n%s", fresh.Body.String())
	}
	time.Sleep(time.Millisecond)
	flaky.mode.Store("status")
	if failed := probeOnce(t, server, probe, nil); failed.Code != http.StatusBadGateway {
		t.Fatalf("a failure without stale_if_error: %d", failed.Code)
	}
}

func TestStaleIfErrorReservesTheFreshnessNames(t *testing.T) {
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(1)
	c := staleCollector(0, time.Minute)
	c.Metrics[0].Name = resultStaleMetric
	server, _ := newCacheTestServer(t, c)
	r := probeOnce(t, server, "/probe?collector=flaky&target="+url.QueryEscape(target.URL), nil)
	if r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "reserved for the series a collector with cache.stale_if_error adds") {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	c.Cache.StaleIfError = 0
	server, _ = newCacheTestServer(t, c)
	if r := probeOnce(t, server, "/probe?collector=flaky&target="+url.QueryEscape(target.URL), nil); r.Code != http.StatusOK {
		t.Fatalf("without stale_if_error the name is free: %d %s", r.Code, r.Body.String())
	}
}

func TestScheduledScrapeExportsTheLastGoodResultMarkedStale(t *testing.T) {
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(5)
	collector := staleCollector(0, 5*time.Minute)
	cfg := &Config{Collectors: []Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "t", Collector: "flaky", Target: target.URL}}}
	server := newScheduledServer(t, cfg, file)

	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	_ = server.drainOTLP()
	flaky.mode.Store("status")
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	var all MetricSet
	for _, resource := range server.drainOTLP() {
		all.Metrics = append(all.Metrics, resource.Set.Metrics...)
	}
	if m := metricByName(all, "demo_value"); m == nil || m.Value != 5 {
		t.Fatalf("the last good result was not exported: %+v", all.Metrics)
	}
	if m := metricByName(all, resultStaleMetric); m == nil || m.Value != 1 {
		t.Fatalf("the export is not marked stale: %+v", all.Metrics)
	}
	if m := metricByName(all, "http_exporter_target_up"); m == nil || m.Value != 0 {
		t.Fatalf("target_up must still say the scrape failed: %+v", all.Metrics)
	}
	if !strings.Contains(selfMetrics(t, server), `http_exporter_cache_stale_served_total{collector="flaky"} 1`) {
		t.Error("the stale export was not counted")
	}
}
