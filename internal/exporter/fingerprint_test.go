package exporter

import (
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func fingerprintTestConfig() *model.Config {
	return &model.Config{Collectors: []model.Collector{cachingCollector("first", time.Minute), cachingCollector("second", time.Minute)}}
}

// The remembered fingerprint is the one collectorFingerprint works out, for
// every collector of the configuration.
func TestFingerprintMemoMatchesTheDefinition(t *testing.T) {
	memo := &fingerprintMemo{}
	cfg := fingerprintTestConfig()
	for i := range cfg.Collectors {
		c := &cfg.Collectors[i]
		want := collectorFingerprint(c)
		if want == "" {
			t.Fatal("collectorFingerprint() is empty")
		}
		for range 2 {
			if got := memo.fingerprint(cfg, c); got != want {
				t.Fatalf("%s: memo=%s, want %s", c.Name, got, want)
			}
		}
	}
	if memo.fingerprint(cfg, &cfg.Collectors[0]) == memo.fingerprint(cfg, &cfg.Collectors[1]) {
		t.Fatal("two collectors share a fingerprint")
	}
}

// A fingerprint is worked out once per configuration: the memo answers from
// memory while the configuration stands, and afresh for the next one.
func TestFingerprintMemoFollowsTheConfiguration(t *testing.T) {
	memo := &fingerprintMemo{}
	cfg := fingerprintTestConfig()
	first := memo.fingerprint(cfg, &cfg.Collectors[0])
	// The memo holds what it worked out: a value planted in it is what it
	// answers, which shows it did not encode the collector again.
	memo.current.Load().values[0] = "remembered"
	if got := memo.fingerprint(cfg, &cfg.Collectors[0]); got != "remembered" {
		t.Fatalf("the memo worked the fingerprint out again: %s", got)
	}

	reloaded := fingerprintTestConfig()
	reloaded.Collectors[0].Request.Path = "/changed"
	changed := memo.fingerprint(reloaded, &reloaded.Collectors[0])
	if changed == first || changed != collectorFingerprint(&reloaded.Collectors[0]) {
		t.Fatalf("a new configuration must be fingerprinted afresh: %s", changed)
	}
	if memo.current.Load().config != reloaded {
		t.Fatal("the memo should follow the newest configuration")
	}
	// A probe still holding the previous configuration gets its answer.
	if got := memo.fingerprint(cfg, &cfg.Collectors[1]); got != collectorFingerprint(&cfg.Collectors[1]) {
		t.Fatalf("previous configuration: %s", got)
	}
}

// A collector that is not one of the configuration's own — a copy, or no
// configuration at all — is fingerprinted directly and never remembered.
func TestFingerprintMemoComputesCollectorsOutsideTheConfiguration(t *testing.T) {
	memo := &fingerprintMemo{}
	cfg := fingerprintTestConfig()
	copied := cfg.Collectors[0]
	copied.Request.Path = "/elsewhere"
	if got := memo.fingerprint(cfg, &copied); got != collectorFingerprint(&copied) {
		t.Fatalf("copy: %s", got)
	}
	if got := memo.fingerprint(nil, &copied); got != collectorFingerprint(&copied) {
		t.Fatalf("no configuration: %s", got)
	}
	if memo.current.Load() != nil {
		t.Fatal("a collector outside the configuration must not start a generation")
	}
}

func TestFingerprintMemoIsSafeForConcurrentProbes(t *testing.T) {
	memo := &fingerprintMemo{}
	configs := []*model.Config{fingerprintTestConfig(), fingerprintTestConfig()}
	configs[1].Collectors[1].Request.Path = "/v2"
	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				cfg := configs[(worker+i)%2]
				c := &cfg.Collectors[i%2]
				if got := memo.fingerprint(cfg, c); got != collectorFingerprint(c) {
					t.Errorf("fingerprint of %s = %s", c.Name, got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The key a probe uses is the one probeCacheKey gives, so remembering the
// fingerprint changes nothing a cached result is filed under.
func TestServerCacheKeyMatchesProbeCacheKey(t *testing.T) {
	server, manager := newCacheTestServer(t, cachingCollector("first", time.Minute))
	cfg := manager.Get()
	c := &cfg.Collectors[0]
	query := url.Values{"collector": {"first"}, "target": {"http://target.invalid"}}
	header := http.Header{"X-Tenant": {"a"}}
	want := probeCacheKey(c, "http://target.invalid", query, header)
	for range 2 {
		if got := server.probeCacheKey(cfg, c, "http://target.invalid", query, header); got != want {
			t.Fatalf("server key=%s, want %s", got, want)
		}
	}
}

// BenchmarkProbeCacheKey compares a probe's cache key with the fingerprint
// worked out every time and remembered.
func BenchmarkProbeCacheKey(b *testing.B) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("demo", "text")}}
	cfg.Collectors[0].Cache.TTL = model.Duration(time.Minute)
	c := &cfg.Collectors[0]
	query := url.Values{"collector": {"demo"}, "target": {"http://target.invalid"}}
	b.Run("encoded", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = probeCacheKey(c, "http://target.invalid", query, nil)
		}
	})
	b.Run("remembered", func(b *testing.B) {
		b.ReportAllocs()
		server := &Server{fingerprints: &fingerprintMemo{}}
		for b.Loop() {
			_ = server.probeCacheKey(cfg, c, "http://target.invalid", query, nil)
		}
	})
}
