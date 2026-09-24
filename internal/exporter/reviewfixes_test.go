package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Where one list of a probe's parameters ends and the next begins is part of
// its cache key, so two different probes never share a key.
func TestCacheKeysKeepListsApart(t *testing.T) {
	c := testutil.Collector("keys", "text")
	a := probeCacheKey(&c, "http://h", url.Values{"method": {"GET"}, "path": {"/admin"}}, nil)
	b := probeCacheKey(&c, "http://h", url.Values{"method": {"GET", "path", "/admin"}}, nil)
	if a == "" || a == b {
		t.Fatal("two different probes share a cache key")
	}
	h1 := probeCacheKey(&c, "http://h", nil, http.Header{"X-A": {"1", "X-B"}})
	h2 := probeCacheKey(&c, "http://h", nil, http.Header{"X-A": {"1"}, "X-B": {}})
	if h1 == h2 {
		t.Fatal("header values run into the next header")
	}
	// A header name in any case is the same header.
	if probeCacheKey(&c, "http://h", nil, http.Header{"x-a": {"1"}}) != probeCacheKey(&c, "http://h", nil, http.Header{"X-A": {"1"}}) {
		t.Fatal("a header's case changes the key")
	}
}

// A probe whose caller went away is not the target's failure: nothing is
// logged, and no stale answer is counted or queued for OTLP.
func TestAnAbandonedProbeIsNotAFailure(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	var block atomic.Bool
	var arrived atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		if block.Load() {
			<-r.Context().Done()
			return
		}
		_, _ = fmt.Fprint(w, "value=42\n")
	}))
	defer target.Close()
	c := testutil.Collector("abandoned", "text")
	c.Cache = model.CacheConfig{TTL: model.Duration(time.Millisecond), StaleIfError: model.Duration(time.Hour)}
	off := false
	c.Coalesce = &off
	server := flightServer(t, c)
	if got := probeOnce(t, server, probePath("abandoned", target.URL, ""), nil); got.Code != http.StatusOK {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	time.Sleep(5 * time.Millisecond)
	block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	outcome := probeAsync(ctx, server, probePath("abandoned", target.URL, ""), nil)
	for deadline := time.Now().Add(5 * time.Second); arrived.Load() < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-outcome
	metrics := selfMetrics(t, server)
	if !strings.Contains(metrics, `http_exporter_cache_stale_served_total{collector="abandoned"} 0`) {
		t.Fatalf("an abandoned probe was answered stale:\n%s", metrics)
	}
	if strings.Contains(logs.String(), "probe failed") || strings.Contains(logs.String(), "last successful result") {
		t.Fatalf("an abandoned probe was logged as a failure:\n%s", logs)
	}
}

// The process start time is the same at every scrape.
func TestTheProcessStartTimeIsStable(t *testing.T) {
	first, ok := processStartSeconds()
	if !ok {
		t.Skip("no /proc")
	}
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		if again, _ := processStartSeconds(); again != first {
			t.Fatalf("start time moved from %f to %f", first, again)
		}
	}
	if now := float64(time.Now().Unix()); first > now+1 || first < now-7*24*3600 {
		t.Fatalf("start time %f is not near now, %f", first, now)
	}
}

// A static target that scrapes on time again ends its run of skipped scrapes
// in the failure log, so a later skip is not reported as one long failure.
func TestAStaticTargetBackOnScheduleIsRecovered(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "value=1\n")
	}))
	defer target.Close()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Second), Targets: []model.StaticTarget{{Name: "t", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = slog.Default()
	key := failureKey("text", "static target t", "schedule")
	server.failures.failed(server.logger, slog.LevelWarn, key, "static target scrape skipped", "schedule", errStillRunning)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.StaticScrapeLoop(ctx)
		close(done)
	}()
	remembered := func() bool {
		server.failures.mu.Lock()
		defer server.failures.mu.Unlock()
		_, ok := server.failures.entries[key]
		return ok
	}
	for deadline := time.Now().Add(5 * time.Second); remembered() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if remembered() {
		t.Fatal("the skipped scrapes are still remembered")
	}
	// The loop has stopped, so the log is read without a writer.
	if !strings.Contains(logs.String(), "static target scrapes on schedule again") {
		t.Fatalf("no recovery logged:\n%s", logs)
	}
}
