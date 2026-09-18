package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func cachingCollector(name string, ttl time.Duration) Collector {
	c := testCollector(name, "text")
	c.Cache = Duration(ttl)
	return c
}

func newCacheTestServer(t *testing.T, collectors ...Collector) (*Server, *ConfigManager) {
	t.Helper()
	cfg := &Config{Collectors: collectors}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, "", slog.Default())
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
	server, _ := newCacheTestServer(t, testCollector("uncached", "text"))

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
	cfg := &Config{Collectors: []Collector{collector}}
	if err := cfg.Validate(); err != nil {
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
	changedConfig := &Config{Collectors: []Collector{changed}}
	if err := changedConfig.Validate(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		collector *Collector
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
	server, manager := newCacheTestServer(t, cachingCollector("reloaded", time.Minute))
	probe := "/probe?target=" + target.URL + "&collector=reloaded"

	if response := probeOnce(t, server, probe, nil); !strings.Contains(response.Body.String(), "demo_value 42") {
		t.Fatalf("body=%q", response.Body.String())
	}
	updated := cachingCollector("reloaded", time.Minute)
	updated.Metrics[0].Name = "renamed_value"
	reloaded := &Config{Collectors: []Collector{updated}}
	if err := reloaded.Validate(); err != nil {
		t.Fatal(err)
	}
	manager.current.Store(reloaded)

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
	set := MetricSet{Metrics: []Metric{{Name: "demo_value", Type: GaugeMetricType, Value: 1}}}

	cache.Put("first", "collector", set, time.Minute, 2, now)
	cache.Put("second", "collector", set, 2*time.Minute, 2, now)
	if _, ok := cache.Get("first", now.Add(time.Second)); !ok {
		t.Fatal("first entry should still be live")
	}
	if _, ok := cache.Get("first", now.Add(2*time.Minute)); ok {
		t.Fatal("expired entry was served")
	}

	cache = newResponseCache()
	cache.Put("first", "collector", set, time.Minute, 2, now)
	cache.Put("second", "collector", set, 2*time.Minute, 2, now)
	cache.Put("third", "collector", set, 3*time.Minute, 2, now)
	cache.Put("other", "different", set, time.Minute, 2, now)
	if _, ok := cache.Get("first", now); ok {
		t.Fatal("entry closest to expiry was not evicted")
	}
	for _, key := range []string{"second", "third", "other"} {
		if _, ok := cache.Get(key, now); !ok {
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
	original := MetricSet{Metrics: []Metric{{
		Name:      "demo_value",
		Type:      HistogramMetricType,
		Labels:    map[string]string{"zone": "a"},
		Timestamp: &timestamp,
		Histogram: &Histogram{Buckets: []Bucket{{UpperBound: 1, CumulativeCount: 2}}, Sum: 3, Count: 2},
	}}}
	cache.Put("key", "collector", original, time.Minute, 10, now)
	original.Metrics[0].Labels["zone"] = "mutated-source"

	first, ok := cache.Get("key", now)
	if !ok {
		t.Fatal("entry was not cached")
	}
	if first.Metrics[0].Labels["zone"] != "a" {
		t.Fatalf("cache stored an aliased label map: %v", first.Metrics[0].Labels)
	}
	first.Metrics[0].Labels["zone"] = "mutated-read"
	first.Metrics[0].Histogram.Buckets[0].CumulativeCount = 99
	*first.Metrics[0].Timestamp = 99

	second, _ := cache.Get("key", now)
	if second.Metrics[0].Labels["zone"] != "a" || second.Metrics[0].Histogram.Buckets[0].CumulativeCount != 2 || *second.Metrics[0].Timestamp != 5 {
		t.Fatalf("cached entry was mutated by a reader: %+v", second.Metrics[0])
	}
}

func TestCacheConfigurationParsingAndValidation(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	document := "collectors:\n" +
		"  - name: cached\n" +
		"    cache: 90s\n" +
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
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.Collectors[0].Cache) != 90*time.Second {
		t.Fatalf("cache=%v, want 90s", time.Duration(cfg.Collectors[0].Cache))
	}
	if cfg.Collectors[0].Limits.MaxCacheEntries != 5 {
		t.Fatalf("max_cache_entries=%d, want 5", cfg.Collectors[0].Limits.MaxCacheEntries)
	}

	defaults := &Config{Collectors: []Collector{testCollector("defaulted", "text")}}
	if err := defaults.Validate(); err != nil {
		t.Fatal(err)
	}
	if defaults.Collectors[0].Cache != 0 {
		t.Fatalf("cache=%v, want disabled by default", time.Duration(defaults.Collectors[0].Cache))
	}
	if defaults.Collectors[0].Limits.MaxCacheEntries != 1000 {
		t.Fatalf("max_cache_entries=%d, want 1000", defaults.Collectors[0].Limits.MaxCacheEntries)
	}

	negative := &Config{Collectors: []Collector{cachingCollector("negative", -time.Second)}}
	if err := negative.Validate(); err == nil || !strings.Contains(err.Error(), "cache must not be negative") {
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
