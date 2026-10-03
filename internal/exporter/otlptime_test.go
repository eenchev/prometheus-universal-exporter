//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A data point exported over OTLP carries the time its value came from the
// target, also when a cached or stale result is answered with (scrapeTime,
// otlp.go).

// pendingTimes drains the pending OTLP points and returns when each is as of,
// by name.
func pendingTimes(t *testing.T, server *Server) map[string]time.Time {
	t.Helper()
	times := map[string]time.Time{}
	for _, resource := range server.drainOTLP() {
		for _, metric := range resource.Set.Metrics {
			if metric.Timestamp == nil {
				t.Fatalf("the pending point %s has no time", metric.Name)
			}
			times[metric.Name] = time.UnixMilli(*metric.Timestamp)
		}
	}
	return times
}

// entryFetched is when the server's one cache entry came from the target, to
// the millisecond an OTLP point keeps.
func entryFetched(t *testing.T, server *Server) time.Time {
	t.Helper()
	server.cache.mu.Lock()
	defer server.cache.mu.Unlock()
	if len(server.cache.entries) != 1 {
		t.Fatalf("%d cache entries, want 1", len(server.cache.entries))
	}
	for _, entry := range server.cache.entries {
		return time.UnixMilli(entry.fetched.UnixMilli())
	}
	return time.Time{}
}

// A probe's answer is exported as of the trip that made it: the trip just
// made, and, for an answer from the cache, fresh or stale, the earlier trip
// the entry is of, however long ago. The freshness series, which are the
// exporter's own and say how old the result is as it is answered, are as of
// the answer.
func TestOTLPPointsOfACachedAnswerCarryTheTimeOfTheScrape(t *testing.T) {
	testutil.CaptureLogs(t)
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(1)
	cfg := &model.Config{Collectors: []model.Collector{staleCollector(time.Minute, time.Hour)}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	server := newStaticServer(t, cfg, nil)
	probe := "/probe?collector=flaky&target=" + url.QueryEscape(target.URL)
	answer := func(what string, want int) map[string]time.Time {
		t.Helper()
		if r := probeOnce(t, server, probe, nil); r.Code != want {
			t.Fatalf("%s answered %d: %s", what, r.Code, r.Body)
		}
		return pendingTimes(t, server)
	}

	start := time.UnixMilli(time.Now().UnixMilli())
	fresh := answer("the first probe", http.StatusOK)
	scraped := entryFetched(t, server)
	if !fresh["demo_value"].Equal(scraped) || scraped.Before(start) {
		t.Fatalf("the trip's point is as of %s, its cache entry of %s, the probe began at %s", fresh["demo_value"], scraped, start)
	}

	// Ten seconds later the cache answers.
	ageEntries(server, 10*time.Second)
	scraped = entryFetched(t, server)
	hit := answer("the cache hit", http.StatusOK)
	if flaky.calls.Load() != 1 {
		t.Fatalf("the second probe went to the target")
	}
	if !hit["demo_value"].Equal(scraped) {
		t.Errorf("a cache hit's point is as of %s, want the scrape %s, 10s before the hit", hit["demo_value"], scraped)
	}
	for _, own := range []string{resultStaleMetric, resultAgeMetric} {
		if hit[own].Before(start) {
			t.Errorf("%s of a cache hit is as of %s, want the time of the answer, after %s", own, hit[own], start)
		}
	}

	// So does a trip that finds the entry as it starts, filled by a probe
	// that finished while it waited (probeTrip).
	var key string
	for k := range server.cache.entries {
		key = k
	}
	collector := &server.manager.Get().Collectors[0]
	if result := server.probeTrip(t.Context(), upstreamProbe{collector: collector, target: target.URL, logTarget: target.URL, rec: server.recorderFor(server.statsFor("flaky"), "flaky", "", ""), cacheKey: key}); !result.ok || flaky.calls.Load() != 1 {
		t.Fatalf("the trip was not answered from the cache: %+v", result)
	}
	if at := pendingTimes(t, server)["demo_value"]; !at.Equal(scraped) {
		t.Errorf("the point of a cache hit at the start of a trip is as of %s, want the scrape %s", at, scraped)
	}

	// Past cache.ttl the target fails, and the stale result answers.
	ageEntries(server, 5*time.Minute)
	scraped = entryFetched(t, server)
	flaky.mode.Store("status")
	stale := answer("the stale answer", http.StatusOK)
	if !stale["demo_value"].Equal(scraped) {
		t.Errorf("a stale answer's point is as of %s, want the scrape %s, 5m10s before it", stale["demo_value"], scraped)
	}
	for _, own := range []string{resultStaleMetric, resultAgeMetric} {
		if stale[own].Before(start) {
			t.Errorf("%s of a stale answer is as of %s, want the time of the answer, after %s", own, stale[own], start)
		}
	}
}

// A static target exported over OTLP is as of its scrape too: a scrape the
// collector's cache answers exports the result as of the trip that made it,
// and the target's health metrics, which are about this scrape, as of now.
func TestOTLPPointsOfAStaticTargetsCachedScrapeCarryTheTimeOfTheTrip(t *testing.T) {
	testutil.CaptureLogs(t)
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(1)
	for _, staleIfError := range []time.Duration{0, time.Hour} {
		flaky.calls.Store(0)
		cfg := &model.Config{Collectors: []model.Collector{staleCollector(time.Minute, staleIfError)}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
		server := newStaticServer(t, cfg, &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
			{ExportViaOTLP: true, Name: "one", Collector: "flaky", Target: target.URL, Labels: map[string]string{"site": "a"}},
		}})
		start := time.UnixMilli(time.Now().UnixMilli())
		server.scrapeStaticTargets(t.Context(), 0)
		fresh, scraped := pendingTimes(t, server), entryFetched(t, server)
		if !fresh["demo_value"].Equal(scraped) || scraped.Before(start) {
			t.Fatalf("stale_if_error %s: the trip's point is as of %s, its cache entry of %s, the scrape began at %s", staleIfError, fresh["demo_value"], scraped, start)
		}

		ageEntries(server, 10*time.Second)
		scraped = entryFetched(t, server)
		server.scrapeStaticTargets(t.Context(), 0)
		hit := pendingTimes(t, server)
		if flaky.calls.Load() != 1 {
			t.Fatalf("stale_if_error %s: the second scrape went to the target", staleIfError)
		}
		if !hit["demo_value"].Equal(scraped) {
			t.Errorf("stale_if_error %s: a cached scrape's point is as of %s, want the trip %s, 10s before it", staleIfError, hit["demo_value"], scraped)
		}
		own := []string{"http_exporter_target_up", "http_exporter_target_scrape_duration_seconds", "http_exporter_target_last_success_timestamp_seconds"}
		if staleIfError > 0 {
			own = append(own, resultStaleMetric, resultAgeMetric)
		}
		for _, name := range own {
			if at, exported := hit[name]; !exported || at.Before(start) {
				t.Errorf("stale_if_error %s: %s of a cached scrape is as of %s (exported: %v), want the time of the scrape, after %s", staleIfError, name, at, exported, start)
			}
		}
	}
}

// A trip reports the moment its result came from the target as the very time
// its cache entry has, so the result is exported as of one moment by the
// probe that made the trip and by every probe the cache answers afterwards,
// rather than as of two a few microseconds apart, the later first.
func TestATripIsAsOfTheTimeItsCacheEntryHas(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=1\n")
	server, _ := newCacheTestServer(t, staleCollector(time.Minute, 0))
	c := &server.manager.Get().Collectors[0]
	trip := server.collect(t.Context(), collectJob{collector: c, target: target.URL, display: target.URL, cacheKey: "key", rec: server.recorderFor(server.statsFor("flaky"), "flaky", "", "")})
	if trip.failed() {
		t.Fatalf("the trip failed in %s: %v", trip.stage, trip.err)
	}
	_, fetched, cached := server.cache.Get("key", time.Now())
	if !cached || trip.fetched.IsZero() || !trip.fetched.Equal(fetched) {
		t.Fatalf("the trip is as of %s, its cache entry (%v) of %s", trip.fetched, cached, fetched)
	}
}

// A point that brings a time of its own from the target keeps it, and a set
// queued without a scrape time is as of when it is queued.
func TestOTLPPointsKeepATimeOfTheirOwn(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	server := newStaticServer(t, cfg, nil)
	own, earlier := int64(1_700_000_000_000), time.Now().Add(-time.Hour)
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "stamped", Type: model.GaugeMetricType, Value: 1, Timestamp: &own},
		{Name: "plain", Type: model.GaugeMetricType, Value: 2},
	}}
	server.queueProbeOTLP(set, &cfg.Collectors[0], "http://t", earlier)
	times := pendingTimes(t, server)
	if !times["stamped"].Equal(time.UnixMilli(own)) || !times["plain"].Equal(time.UnixMilli(earlier.UnixMilli())) {
		t.Fatalf("points as of %v, want the target's own time and the scrape's", times)
	}
	before := time.UnixMilli(time.Now().UnixMilli())
	server.queueOTLP(set)
	if times := pendingTimes(t, server); !times["stamped"].Equal(time.UnixMilli(own)) || times["plain"].Before(before) {
		t.Fatalf("points as of %v, want the target's own time and now", times)
	}
}
