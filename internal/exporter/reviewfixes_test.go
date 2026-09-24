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

// On the static targets endpoint a result's age is how old its data is when
// the endpoint is read, not how old it was when the scrape made it.
func TestAStaticResultAgesUntilTheNextScrape(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "value=1\n")
	}))
	defer target.Close()
	c := testutil.Collector("aged", "text")
	c.Cache = model.CacheConfig{TTL: model.Duration(time.Minute), StaleIfError: model.Duration(time.Hour)}
	cfg := &model.Config{Collectors: []model.Collector{c}}
	file := &model.StaticTargetFile{Interval: model.Duration(10 * time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "aged", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(t.Context(), 10*time.Second)
	ageOf := func(body string) float64 {
		for _, line := range strings.Split(body, "\n") {
			if value, found := strings.CutPrefix(line, `http_exporter_result_age_seconds{static_target="t"} `); found {
				var age float64
				_, _ = fmt.Sscan(value, &age)
				return age
			}
		}
		t.Fatalf("no age:\n%s", body)
		return 0
	}
	if age := ageOf(getStaticTargets(t, server, "/static-targets")); age >= 1 {
		t.Fatalf("just scraped, the age is %v", age)
	}
	// Ninety seconds later, with no scrape between, the data is ninety
	// seconds old.
	server.staticMu.Lock()
	stored := server.staticResults["t"]
	server.staticFetched["t"] = server.staticFetched["t"].Add(-90 * time.Second)
	server.staticMu.Unlock()
	if age := ageOf(getStaticTargets(t, server, "/static-targets")); age < 90 || age > 95 {
		t.Fatalf("age %v, want about 90", age)
	}
	// The stored result is left as the scrape made it.
	for _, m := range stored.Metrics {
		if m.Name == resultAgeMetric && m.Value != 0 {
			t.Fatalf("the stored result was changed: %v", m.Value)
		}
	}
	// A collector without stale_if_error has no age to keep.
	plain := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("aged", "text")}}, file)
	plain.logger = testutil.QuietLogger(t)
	plain.scrapeStaticTargets(t.Context(), 10*time.Second)
	if _, aged := plain.staticFetched["t"]; aged || strings.Contains(getStaticTargets(t, plain, "/static-targets"), resultAgeMetric) {
		t.Fatal("a collector without stale_if_error has an age")
	}
}

// Where a static target's own sections end is part of its cache key: a
// target value that reads like a section name cannot make two different
// targets share a key.
func TestTargetOwnSectionsAreCounted(t *testing.T) {
	a := targetOwnRequest(&model.StaticTarget{Request: model.TargetRequestConfig{
		Targets: []string{"a", "metadata", "k", "v"},
	}})
	b := targetOwnRequest(&model.StaticTarget{Request: model.TargetRequestConfig{
		Targets:  []string{"a"},
		Metadata: map[string]string{"k": "v"},
	}})
	if strings.Join(a, "\x00") == strings.Join(b, "\x00") {
		t.Fatalf("two different targets share their own sections: %q", a)
	}
	c := testutil.Collector("keys", "text")
	if probeCacheKey(&c, "http://h", nil, nil, a...) == probeCacheKey(&c, "http://h", nil, nil, b...) {
		t.Fatal("two different targets share a cache key")
	}
}

// A probe whose target the collector's allowed_targets or denied_targets
// refuses is answered 403, not sent, and counted; a static target so refused
// fails in the target_policy stage.
func TestARefusedTargetIsAnswered403(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprint(w, "value=1\n")
	}))
	defer target.Close()
	c := testutil.Collector("guarded", "text")
	c.Request.DeniedTargets = []string{"127.0.0.0/8"}
	c.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	server := flightServer(t, c)
	got := probeOnce(t, server, probePath("guarded", target.URL, ""), nil)
	if got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "request.denied_targets") {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	if hits.Load() != 0 {
		t.Fatal("the refused target was contacted")
	}
	if !strings.Contains(selfMetrics(t, server), `http_exporter_targets_refused_total{collector="guarded"} 1`) {
		t.Fatalf("not counted:\n%s", selfMetrics(t, server))
	}

	cfg := &model.Config{Collectors: []model.Collector{c}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "guarded", Target: target.URL}}}
	static := newStaticServer(t, cfg, file)
	static.logger = testutil.QuietLogger(t)
	static.scrapeStaticTargets(t.Context(), 10*time.Second)
	if body := getStaticTargets(t, static, "/static-targets"); !strings.Contains(body, `http_exporter_target_up{collector="guarded",static_target="t",target="`+target.URL+`"} 0`) {
		t.Fatalf("a refused static target is up:\n%s", body)
	}
}

// A status request.accept_status names is decoded like a 2xx, and jq rules
// read the status and headers as $status and $headers; a status it does not
// name still fails the probe.
func TestAnAcceptedStatusIsDecoded(t *testing.T) {
	status := http.StatusServiceUnavailable
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Mode", "maintenance")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{"queued": 7}`)
	}))
	defer target.Close()
	c := testutil.Collector("accepting", "json")
	c.Request.AcceptStatus = []string{"2xx", "503"}
	c.Transform.Type = "jq"
	c.Metrics = []model.MetricRule{
		{Name: "queued", Type: model.GaugeMetricType, Expression: ".queued"},
		{Name: "http_status", Type: model.GaugeMetricType, Expression: "$status", Labels: []model.LabelRule{{Name: "mode", Expression: `$headers["x-mode"]`}}},
	}
	server := flightServer(t, c)
	got := probeOnce(t, server, probePath("accepting", target.URL, ""), nil)
	if got.Code != http.StatusOK {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	for _, want := range []string{"queued 7\n", `http_status{mode="maintenance"} 503` + "\n"} {
		if !strings.Contains(got.Body.String(), want) {
			t.Fatalf("no %q in:\n%s", want, got.Body)
		}
	}
	status = http.StatusInternalServerError
	if got := probeOnce(t, server, probePath("accepting", target.URL, "&x=1"), nil); got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), "received HTTP status 500") {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
}
