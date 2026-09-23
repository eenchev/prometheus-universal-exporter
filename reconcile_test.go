package main

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Per-collector state after a reload (reconcile.go), the probe's missing
// parameters, and the exporter's own HTTP server timeouts (lifecycle.go).

func reloadTo(t *testing.T, manager *ConfigManager, collectors ...Collector) {
	t.Helper()
	cfg := &Config{Collectors: collectors, Web: manager.Get().Web}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	manager.current.Store(cfg)
}

// A removed collector's series stop, with everything kept about it; one added
// again under the name starts from zero.
func TestRemovedCollectorsStopBeingExposed(t *testing.T) {
	target, _ := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	kept, gone := cachingCollector("kept", time.Minute), cachingCollector("gone", time.Minute)
	cfg := &Config{Collectors: []Collector{kept, gone}, Web: WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	manager := NewConfigManager(cfg, "", quietLogger(t))
	server := NewServer(manager, "python3", quietLogger(t))
	for _, name := range []string{"kept", "gone"} {
		probeOnce(t, server, "/probe?collector="+name+"&target="+target.URL, nil)
	}
	before := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_scrapes_total{collector="gone"} 1`, `http_exporter_cache_entries{collector="gone"} 1`, `http_exporter_collector_scrape_duration_seconds_count{collector="gone"} 1`, `collector="gone",http_method="GET"`} {
		if !strings.Contains(before, want) {
			t.Fatalf("before the reload, missing %s", want)
		}
	}

	reloadTo(t, manager, kept)
	after := selfMetrics(t, server)
	if strings.Contains(after, `collector="gone"`) {
		t.Fatalf("a removed collector is still exposed:\n%s", after)
	}
	if got := seriesValue(t, after, `http_exporter_scrapes_total{collector="kept"}`); got != 1 {
		t.Fatalf("a kept collector's counter is %v, want 1", got)
	}
	if n := server.cache.Stats(time.Now())["gone"]; n != 0 {
		t.Fatalf("%d cached results of the removed collector are kept", n)
	}

	reloadTo(t, manager, kept, gone)
	again := selfMetrics(t, server)
	if got := seriesValue(t, again, `http_exporter_scrapes_total{collector="gone"}`); got != 0 {
		t.Fatalf("a collector added again starts at %v, want 0", got)
	}
}

// A changed collector keeps its counters; its cached results, which it could
// never serve again, are dropped at once.
func TestChangedCollectorsDropTheirCache(t *testing.T) {
	target, requests := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	server, manager := newCacheTestServer(t, cachingCollector("changing", time.Minute))
	server.logger = quietLogger(t)
	probeOnce(t, server, "/probe?collector=changing&target="+target.URL, nil)
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_cache_entries{collector="changing"}`); got != 1 {
		t.Fatalf("cache entries %v, want 1", got)
	}
	changed := cachingCollector("changing", time.Minute)
	changed.Metrics[0].Name = "renamed_value"
	reloadTo(t, manager, changed)
	exposition := selfMetrics(t, server)
	if got := seriesValue(t, exposition, `http_exporter_cache_entries{collector="changing"}`); got != 0 {
		t.Fatalf("a changed collector still counts %v cache entries of its old definition", got)
	}
	if got := seriesValue(t, exposition, `http_exporter_scrapes_total{collector="changing"}`); got != 1 {
		t.Fatalf("a changed collector's counter is %v, want 1", got)
	}
	r := probeOnce(t, server, "/probe?collector=changing&target="+target.URL, nil)
	if !strings.Contains(r.Body.String(), "renamed_value 42") || requests.Load() != 2 {
		t.Fatalf("after the change: %q, %d requests", r.Body.String(), requests.Load())
	}
}

// An unchanged reload keeps everything.
func TestAnUnchangedReloadKeepsTheCache(t *testing.T) {
	target, requests := countingTarget(func(*http.Request) string { return "value=42\n" })
	defer target.Close()
	server, manager := newCacheTestServer(t, cachingCollector("same", time.Minute))
	server.logger = quietLogger(t)
	probeOnce(t, server, "/probe?collector=same&target="+target.URL, nil)
	reloadTo(t, manager, cachingCollector("same", time.Minute))
	probeOnce(t, server, "/probe?collector=same&target="+target.URL, nil)
	if requests.Load() != 1 {
		t.Fatalf("an unchanged reload dropped the cache: %d requests", requests.Load())
	}
}

func TestProbeNamesTheMissingParameter(t *testing.T) {
	server := verboseServer(t, false, testCollector("web", "text"))
	for query, want := range map[string]string{
		"/probe?target=http://a.example": "the collector parameter is required: /probe?collector=<name>&target=<target>",
		"/probe":                         "the collector parameter is required",
		"/probe?collector=web":           `the target parameter is required for collector "web", whose request.type is http`,
		"/probe?collector=nope":          `unknown collector "nope"`,
	} {
		r := probeOnce(t, server, query, nil)
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), want) {
			t.Errorf("%s: %d %q, want %q", query, r.Code, r.Body.String(), want)
		}
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	s := newHTTPServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != 10*time.Second || s.ReadTimeout != 30*time.Second || s.IdleTimeout != 2*time.Minute || s.WriteTimeout != 0 {
		t.Fatalf("timeouts: header %s, read %s, idle %s, write %s", s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout, s.WriteTimeout)
	}
}

// An idle keep-alive connection is closed by the exporter.
func TestIdleConnectionsAreClosed(t *testing.T) {
	previous := httpIdleTimeout
	httpIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { httpIdleTimeout = previous })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var served atomic.Int64
	s := newHTTPServer(listener.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	go func() { _ = s.Serve(listener) }()
	t.Cleanup(func() { _ = s.Close() })
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = reader.ReadByte()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the idle connection was not closed: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the idle connection was closed after %s", took)
	}
}

// failureLogFor is a failure log with a clock the test moves.
func failureLogFor(t *testing.T) (*failureLog, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	f := newFailureLog()
	f.now = func() time.Time { return now }
	return f, &now
}

func TestRepeatedFailuresAreLoggedSparingly(t *testing.T) {
	f, now := failureLogFor(t)
	logs := captureLogs(t)
	logger := slog.Default()
	key := failureKey("web", "http://a", "")
	boom := errors.New("connection refused")
	// A failure a minute: at 12:00 in full, 12:01-12:04 only at debug level,
	// 12:05 again with the count.
	for i := 0; i < 6; i++ {
		f.failed(logger, slog.LevelError, key, "probe failed", "http", boom, "collector", "web")
		*now = now.Add(time.Minute)
	}
	if got := strings.Count(logs.String(), `"msg":"probe failed"`); got != 2 {
		t.Fatalf("6 failures over 5 minutes logged %d lines, want the first and one repeat:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"repeated":5`) || !strings.Contains(logs.String(), `"failing_since":"2026-09-01T12:00:00Z"`) || !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("the repeat line lacks its count or start:\n%s", logs.String())
	}
	// Another error is a new failure, logged at once.
	f.failed(logger, slog.LevelError, key, "probe failed", "http", errors.New("timeout"), "collector", "web")
	if got := strings.Count(logs.String(), `"msg":"probe failed"`); got != 3 {
		t.Fatalf("a different error was not logged: %d lines", got)
	}
	*now = now.Add(90 * time.Second)
	f.recovered(logger, key, "probe recovered", "collector", "web")
	if !strings.Contains(logs.String(), `"msg":"probe recovered","collector":"web","stage":"http","failed_for":"1m30s","failures":1`) {
		t.Fatalf("recovery not logged as expected:\n%s", logs.String())
	}
	before := logs.Len()
	f.recovered(logger, key, "probe recovered")
	if logs.Len() != before {
		t.Fatal("a success without a failure was logged")
	}
}

// At debug level every repeat is still visible.
func TestRepeatsAreLoggedAtDebugLevel(t *testing.T) {
	f, _ := failureLogFor(t)
	var out strings.Builder
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for i := 0; i < 3; i++ {
		f.failed(logger, slog.LevelWarn, "k", "probe stage failed; continuing", "decode", errors.New("bad"))
	}
	if strings.Count(out.String(), `"repeat":true`) != 2 || strings.Count(out.String(), `"level":"WARN"`) != 1 {
		t.Fatalf("debug output:\n%s", out.String())
	}
}

// What is remembered is bounded; failures not seen for an hour make room.
func TestTheFailureLogIsBounded(t *testing.T) {
	f, now := failureLogFor(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for i := 0; i < failureLogMaxEntries; i++ {
		f.failed(logger, slog.LevelError, failureKey("c", "t", string(rune(i))), "m", "s", nil)
	}
	f.failed(logger, slog.LevelError, "extra", "m", "s", nil)
	if _, ok := f.entries["extra"]; ok || len(f.entries) != failureLogMaxEntries {
		t.Fatalf("remembered %d entries past the bound", len(f.entries))
	}
	*now = now.Add(failureLogForget + time.Minute)
	f.failed(logger, slog.LevelError, "extra", "m", "s", nil)
	if _, ok := f.entries["extra"]; !ok || len(f.entries) != 1 {
		t.Fatalf("stale entries were not forgotten: %d", len(f.entries))
	}
	f.forgetCollectors(map[string]bool{"extra": true})
	if len(f.entries) != 0 {
		t.Fatal("a removed collector's failures are still remembered")
	}
}

// Through probes: a target down for many probes logs once, and its recovery.
func TestAFailingTargetLogsOnceAndItsRecovery(t *testing.T) {
	var up atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	defer target.Close()
	server := verboseServer(t, false, testCollector("web", "text"))
	logs := captureLogs(t)
	server.logger = slog.Default()
	for i := 0; i < 4; i++ {
		if r := probeOnce(t, server, "/probe?collector=web&target="+target.URL, nil); r.Code != http.StatusBadGateway {
			t.Fatalf("probe %d: %d", i, r.Code)
		}
	}
	if got := strings.Count(logs.String(), `"msg":"probe failed"`); got != 1 {
		t.Fatalf("4 identical failures logged %d lines:\n%s", got, logs.String())
	}
	up.Store(true)
	probeOnce(t, server, "/probe?collector=web&target="+target.URL, nil)
	if !strings.Contains(logs.String(), `"msg":"probe recovered"`) || !strings.Contains(logs.String(), `"failures":4`) || !strings.Contains(logs.String(), `"stage":"http_status"`) {
		t.Fatalf("recovery not logged:\n%s", logs.String())
	}
	// The self-metrics count every failure regardless.
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_scrapes_total{collector="web"}`); got != 5 {
		t.Fatalf("scrapes %v", got)
	}
}

// A scheduled target that keeps failing logs once, and its recovery.
func TestAFailingScheduledTargetLogsOnce(t *testing.T) {
	var up atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	defer target.Close()
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	server := newScheduledServer(t, cfg, &TargetFile{Targets: []ScheduledTarget{{Name: "api", Collector: "text", Target: target.URL}}})
	logs := captureLogs(t)
	server.logger = slog.Default()
	for i := 0; i < 3; i++ {
		server.scrapeScheduledTargets(t.Context(), 5*time.Second)
	}
	if got := strings.Count(logs.String(), `"msg":"scheduled target scrape failed"`); got != 1 {
		t.Fatalf("3 identical failures logged %d lines:\n%s", got, logs.String())
	}
	up.Store(true)
	server.scrapeScheduledTargets(t.Context(), 5*time.Second)
	if !strings.Contains(logs.String(), `"msg":"scheduled target recovered","target":"api"`) || !strings.Contains(logs.String(), `"failures":3`) {
		t.Fatalf("recovery not logged:\n%s", logs.String())
	}
}
