package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func selfMetrics(t *testing.T, server *Server) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/self-metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("self-metrics status=%d", recorder.Code)
	}
	return recorder.Body.String()
}

// expireCachedEntries rewinds every cached entry so a test can observe expiry
// without waiting on the wall clock.
func expireCachedEntries(server *Server) {
	server.cache.mu.Lock()
	defer server.cache.mu.Unlock()
	for _, entry := range server.cache.entries {
		entry.expires = time.Now().Add(-time.Second)
	}
}

func countingTarget(body func(r *http.Request) string) (*httptest.Server, *atomic.Int64) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body(r)))
	}))
	return target, &requests
}

func TestCollectorCacheServesRepeatedProbeFromMemory(t *testing.T) {
	var serial atomic.Int64
	target, requests := countingTarget(func(*http.Request) string {
		if serial.Add(1) == 1 {
			return "value=42\n"
		}
		return "value=7\n"
	})
	defer target.Close()
	server, _ := newCacheTestServer(t, cachingCollector("cached", time.Minute))

	first := probeOnce(t, server, "/probe?target="+target.URL+"&collector=cached", nil)
	second := probeOnce(t, server, "/probe?target="+target.URL+"&collector=cached", nil)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("status first=%d second=%d", first.Code, second.Code)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("target requests=%d, want 1", got)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("cached body differs:\nfirst=%q\nsecond=%q", first.Body.String(), second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "demo_value 42") {
		t.Fatalf("body=%q", second.Body.String())
	}
	exposition := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_cache_hits_total{collector="cached"} 1`,
		`http_exporter_cache_misses_total{collector="cached"} 1`,
		`http_exporter_cache_entries{collector="cached"} 1`,
		`http_exporter_scrapes_total{collector="cached"} 2`,
	} {
		if !strings.Contains(exposition, want) {
			t.Fatalf("self-metrics missing %q:\n%s", want, exposition)
		}
	}
}

func TestCollectorCacheIsDisabledByDefault(t *testing.T) {
	target, requests := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	server, _ := newCacheTestServer(t, testutil.Collector("uncached", "text"))

	for i := 0; i < 2; i++ {
		if response := probeOnce(t, server, "/probe?target="+target.URL+"&collector=uncached", nil); response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("target requests=%d, want 2", got)
	}
	exposition := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_cache_hits_total{collector="uncached"} 0`,
		`http_exporter_cache_misses_total{collector="uncached"} 0`,
		`http_exporter_cache_entries{collector="uncached"} 0`,
	} {
		if !strings.Contains(exposition, want) {
			t.Fatalf("self-metrics missing %q:\n%s", want, exposition)
		}
	}
}

func TestCollectorCacheExpiresAndRefetches(t *testing.T) {
	var serial atomic.Int64
	target, requests := countingTarget(func(*http.Request) string {
		if serial.Add(1) == 1 {
			return "value=1\n"
		}
		return "value=2\n"
	})
	defer target.Close()
	server, _ := newCacheTestServer(t, cachingCollector("expiring", 50*time.Millisecond))

	first := probeOnce(t, server, "/probe?target="+target.URL+"&collector=expiring", nil)
	if !strings.Contains(first.Body.String(), "demo_value 1") {
		t.Fatalf("first body=%q", first.Body.String())
	}
	expireCachedEntries(server)
	second := probeOnce(t, server, "/probe?target="+target.URL+"&collector=expiring", nil)
	if !strings.Contains(second.Body.String(), "demo_value 2") {
		t.Fatalf("second body=%q", second.Body.String())
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("target requests=%d, want 2", got)
	}
	if entries := server.cache.Stats(time.Now()); entries["expiring"] != 1 {
		t.Fatalf("live cache entries=%d, want 1", entries["expiring"])
	}
}

func TestCollectorCacheKeyRequiresIdenticalRequest(t *testing.T) {
	collector := cachingCollector("keyed", time.Minute)
	collector.Request.ForwardAuthorization = true
	collector.Request.ForwardHeaders = []string{"X-Tenant"}
	cfg := &model.Config{Collectors: []model.Collector{collector}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	c := &cfg.Collectors[0]

	baseQuery := url.Values{
		"target":               {"http://target.invalid"},
		"collector":            {"keyed"},
		"method":               {"POST"},
		"path":                 {"/api/status"},
		"body":                 {"payload"},
		"timeout":              {"2s"},
		"insecure_skip_verify": {"false"},
		"follow_redirects":     {"true"},
		"enable_http2":         {"false"},
		"retry_attempts":       {"1"},
		"retry_backoff":        {"1s"},
		"header_X-Tenant":      {"team-a"},
	}
	baseHeader := http.Header{"Authorization": {"Bearer token-a"}, "X-Tenant": {"team-a"}}
	base := probeCacheKey(c, "http://target.invalid", baseQuery, baseHeader)
	if base == "" {
		t.Fatal("probeCacheKey() returned an empty key")
	}
	if again := probeCacheKey(c, "http://target.invalid", baseQuery, baseHeader); again != base {
		t.Fatal("probeCacheKey() is not stable for an identical request")
	}

	withQuery := func(mutate func(url.Values)) url.Values {
		out := url.Values{}
		for key, values := range baseQuery {
			out[key] = append([]string(nil), values...)
		}
		mutate(out)
		return out
	}
	withHeader := func(mutate func(http.Header)) http.Header {
		out := http.Header{}
		for name, values := range baseHeader {
			out[name] = append([]string(nil), values...)
		}
		mutate(out)
		return out
	}

	changed := cachingCollector("keyed", time.Minute)
	changed.Metrics[0].Name = "other_value"
	changedConfig := &model.Config{Collectors: []model.Collector{changed}}
	if err := config.Validate(changedConfig); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		collector *model.Collector
		target    string
		query     url.Values
		header    http.Header
	}{
		{name: "different target", collector: c, target: "http://other.invalid", query: baseQuery, header: baseHeader},
		{name: "different collector definition", collector: &changedConfig.Collectors[0], target: "http://target.invalid", query: baseQuery, header: baseHeader},
		{name: "different method", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("method", "GET") }), header: baseHeader},
		{name: "different path", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("path", "/other") }), header: baseHeader},
		{name: "different body", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("body", "other") }), header: baseHeader},
		{name: "different timeout", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("timeout", "3s") }), header: baseHeader},
		{name: "different tls override", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("insecure_skip_verify", "true") }), header: baseHeader},
		{name: "missing tls override", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Del("insecure_skip_verify") }), header: baseHeader},
		{name: "different follow redirects", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("follow_redirects", "false") }), header: baseHeader},
		{name: "missing follow redirects", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Del("follow_redirects") }), header: baseHeader},
		{name: "different http2 setting", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("enable_http2", "true") }), header: baseHeader},
		{name: "missing http2 setting", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Del("enable_http2") }), header: baseHeader},
		{name: "different retry attempts", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("retry_attempts", "2") }), header: baseHeader},
		{name: "different retry backoff", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("retry_backoff", "2s") }), header: baseHeader},
		{name: "additional parameter", collector: c, target: "http://target.invalid", query: withQuery(func(v url.Values) { v.Set("extra", "1") }), header: baseHeader},
		{name: "different forwarded header", collector: c, target: "http://target.invalid", query: baseQuery, header: withHeader(func(h http.Header) { h.Set("X-Tenant", "team-b") })},
		{name: "missing forwarded header", collector: c, target: "http://target.invalid", query: baseQuery, header: withHeader(func(h http.Header) { h.Del("X-Tenant") })},
		{name: "different credential", collector: c, target: "http://target.invalid", query: baseQuery, header: withHeader(func(h http.Header) { h.Set("Authorization", "Bearer token-b") })},
		{name: "missing credential", collector: c, target: "http://target.invalid", query: baseQuery, header: withHeader(func(h http.Header) { h.Del("Authorization") })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := probeCacheKey(test.collector, test.target, test.query, test.header); got == base {
				t.Fatal("probeCacheKey() matched the baseline request; a cached result would be shared")
			}
		})
	}
}

func TestCollectorCacheIsNotSharedAcrossCredentials(t *testing.T) {
	target, requests := countingTarget(func(r *http.Request) string {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			return "value=42\n"
		case "Bearer token-b":
			return "value=7\n"
		default:
			return "value=0\n"
		}
	})
	defer target.Close()
	collector := cachingCollector("credentialed", time.Minute)
	collector.Request.ForwardAuthorization = true
	server, _ := newCacheTestServer(t, collector)
	probe := "/probe?target=" + target.URL + "&collector=credentialed"

	authorized := probeOnce(t, server, probe, http.Header{"Authorization": {"Bearer token-a"}})
	if !strings.Contains(authorized.Body.String(), "demo_value 42") {
		t.Fatalf("authorized body=%q", authorized.Body.String())
	}
	other := probeOnce(t, server, probe, http.Header{"Authorization": {"Bearer token-b"}})
	if !strings.Contains(other.Body.String(), "demo_value 7") {
		t.Fatalf("second credential read another credential's cached result: %q", other.Body.String())
	}
	anonymous := probeOnce(t, server, probe, nil)
	if !strings.Contains(anonymous.Body.String(), "demo_value 0") {
		t.Fatalf("unauthenticated probe read a cached authenticated result: %q", anonymous.Body.String())
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("target requests=%d, want 3", got)
	}
	repeated := probeOnce(t, server, probe, http.Header{"Authorization": {"Bearer token-a"}})
	if !strings.Contains(repeated.Body.String(), "demo_value 42") || requests.Load() != 3 {
		t.Fatalf("identical authorized probe was not cached: body=%q requests=%d", repeated.Body.String(), requests.Load())
	}
}

func TestCollectorCacheIsInvalidatedByConfigurationReload(t *testing.T) {
	target, requests := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	server, _ := newCacheTestServer(t, cachingCollector("reloaded", time.Minute))
	probe := "/probe?target=" + target.URL + "&collector=reloaded"

	if response := probeOnce(t, server, probe, nil); !strings.Contains(response.Body.String(), "demo_value 42") {
		t.Fatalf("body=%q", response.Body.String())
	}
	updated := cachingCollector("reloaded", time.Minute)
	updated.Metrics[0].Name = "renamed_value"
	reloaded := &model.Config{Collectors: []model.Collector{updated}}
	if err := config.Validate(reloaded); err != nil {
		t.Fatal(err)
	}
	installConfig(server, reloaded)

	response := probeOnce(t, server, probe, nil)
	if !strings.Contains(response.Body.String(), "renamed_value 42") {
		t.Fatalf("reloaded configuration served a stale cached result: %q", response.Body.String())
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("target requests=%d, want 2", got)
	}
}

func TestCollectorCacheDoesNotStoreFailedProbes(t *testing.T) {
	var serial atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if serial.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server, _ := newCacheTestServer(t, cachingCollector("failing", time.Minute))
	probe := "/probe?target=" + target.URL + "&collector=failing"

	if failed := probeOnce(t, server, probe, nil); failed.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", failed.Code, failed.Body.String())
	}
	if entries := server.cache.Stats(time.Now()); entries["failing"] != 0 {
		t.Fatalf("failed probe was cached: entries=%d", entries["failing"])
	}
	recovered := probeOnce(t, server, probe, nil)
	if recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), "demo_value 42") {
		t.Fatalf("status=%d body=%q", recovered.Code, recovered.Body.String())
	}
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

func TestCachedMetricSetIsCopiedOnStoreAndRead(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	timestamp := int64(5)
	original := model.MetricSet{Metrics: []model.Metric{{
		Name:      "demo_value",
		Type:      model.HistogramMetricType,
		Labels:    map[string]string{"zone": "a"},
		Timestamp: &timestamp,
		Histogram: &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 2}}, Sum: 3, Count: 2},
	}}}
	cache.Put("key", "collector", original, time.Minute, 0, 10, now)
	original.Metrics[0].Labels["zone"] = "mutated-source"

	first, _, ok := cache.Get("key", now)
	if !ok {
		t.Fatal("entry was not cached")
	}
	if first.Metrics[0].Labels["zone"] != "a" {
		t.Fatalf("cache stored an aliased label map: %v", first.Metrics[0].Labels)
	}
	first.Metrics[0].Labels["zone"] = "mutated-read"
	first.Metrics[0].Histogram.Buckets[0].CumulativeCount = 99
	*first.Metrics[0].Timestamp = 99

	second, _, _ := cache.Get("key", now)
	if second.Metrics[0].Labels["zone"] != "a" || second.Metrics[0].Histogram.Buckets[0].CumulativeCount != 2 || *second.Metrics[0].Timestamp != 5 {
		t.Fatalf("cached entry was mutated by a reader: %+v", second.Metrics[0])
	}
}

func TestCacheConfigurationParsingAndValidation(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	document := "collectors:\n" +
		"  - name: cached\n" +
		"    request:\n" +
		"      type: http\n" +
		"    cache:\n" +
		"      ttl: 90s\n" +
		"    transform:\n" +
		"      type: regex\n" +
		"    limits:\n" +
		"      max_cache_entries: 5\n" +
		"    metrics:\n" +
		"      - name: demo_value\n" +
		"        expression: 'value=(\\d+)'\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.Collectors[0].Cache.TTL) != 90*time.Second {
		t.Fatalf("cache=%v, want 90s", time.Duration(cfg.Collectors[0].Cache.TTL))
	}
	if cfg.Collectors[0].Limits.MaxCacheEntries != 5 {
		t.Fatalf("max_cache_entries=%d, want 5", cfg.Collectors[0].Limits.MaxCacheEntries)
	}

	defaults := &model.Config{Collectors: []model.Collector{testutil.Collector("defaulted", "text")}}
	if err := config.Validate(defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.Collectors[0].Cache.TTL != 0 {
		t.Fatalf("cache=%v, want disabled by default", time.Duration(defaults.Collectors[0].Cache.TTL))
	}
	if defaults.Collectors[0].Limits.MaxCacheEntries != 1000 {
		t.Fatalf("max_cache_entries=%d, want 1000", defaults.Collectors[0].Limits.MaxCacheEntries)
	}

	negative := &model.Config{Collectors: []model.Collector{cachingCollector("negative", -time.Second)}}
	if err := config.Validate(negative); err == nil || !strings.Contains(err.Error(), "cache.ttl must not be negative") {
		t.Fatalf("Validate() error=%v, want a negative cache error", err)
	}
}

func TestCollectorCacheServesConcurrentProbesSafely(t *testing.T) {
	target, requests := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	server, _ := newCacheTestServer(t, cachingCollector("concurrent", time.Minute))
	probe := "/probe?target=" + target.URL + "&collector=concurrent"

	if response := probeOnce(t, server, probe, nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodGet, probe, nil)
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "demo_value 42") {
				t.Errorf("concurrent probe status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		}()
	}
	wg.Wait()
	if got := requests.Load(); got != 1 {
		t.Fatalf("target requests=%d, want 1", got)
	}
	if entries := server.cache.Stats(time.Now()); entries["concurrent"] != 1 {
		t.Fatalf("cache entries=%d, want 1", entries["concurrent"])
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

func loadCacheConfig(t *testing.T, cache string) (*model.Config, error) {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	document := "collectors:\n  - name: cached\n    request:\n      type: http\n" + cache +
		"    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
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
	if err != nil || cfg.Collectors[0].Cache.TTL != 0 || !model.UsesCache(&cfg.Collectors[0]) {
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

func staleCollector(ttl, stale time.Duration) model.Collector {
	c := testutil.Collector("flaky", "text")
	c.Cache = model.CacheConfig{TTL: model.Duration(ttl), StaleIfError: model.Duration(stale)}
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
	cfg := &model.Config{Collectors: []model.Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "t", Collector: "flaky", Target: target.URL}}}
	server := newScheduledServer(t, cfg, file)

	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	_ = server.drainOTLP()
	flaky.mode.Store("status")
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	var all model.MetricSet
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

// The per-collector index always holds exactly the keys of the entries, and a
// collector's max_cache_entries leaves the others' entries alone.
func TestTheCacheIndexFollowsItsEntries(t *testing.T) {
	cache := newResponseCache()
	now := time.Now()
	set := model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1}}}
	check := func(when string) {
		t.Helper()
		indexed := 0
		for collector, keys := range cache.byCollector {
			if len(keys) == 0 {
				t.Errorf("%s: %s has an empty index", when, collector)
			}
			for key := range keys {
				indexed++
				if entry := cache.entries[key]; entry == nil || entry.collector != collector {
					t.Errorf("%s: %s indexes %s, which it does not hold", when, collector, key)
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
