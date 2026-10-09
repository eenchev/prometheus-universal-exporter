//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe of a cached collector that max_concurrent_probes or
// --probe.max-concurrent turns away is counted as rejected and not as a
// cache miss: the misses are the probes that went to the target, one each,
// while each limit turned one away.
func TestAProbeTurnedAwayByAConcurrencyLimitIsNoCacheMiss(t *testing.T) {
	held := newHeldTarget(t)
	c := testutil.Collector("single", "text")
	c.MaxConcurrentProbes = 1
	c.Cache.TTL = model.Duration(time.Minute)
	other := testutil.Collector("capped", "text")
	other.Cache.TTL = model.Duration(time.Minute)
	server := verboseServer(t, false, c, other)
	busy := make(chan int, 1)
	go func() {
		busy <- probeOnce(t, server, "/probe?collector=single&target="+url.QueryEscape(held.server.URL+"/a"), nil).Code
	}()
	testutil.WaitFor(t, "the collector's slot to be taken", func() bool { return server.trips.count("single") == 1 })
	if code := probeOnce(t, server, "/probe?collector=single&target="+url.QueryEscape(held.server.URL+"/b"), nil).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("the probe over max_concurrent_probes was answered %d, want 503", code)
	}
	server.SetMaxConcurrent(1)
	if code := probeOnce(t, server, "/probe?collector=capped&target="+url.QueryEscape(held.server.URL+"/c"), nil).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("the probe over --probe.max-concurrent was answered %d, want 503", code)
	}
	held.open()
	if code := <-busy; code != http.StatusOK {
		t.Fatalf("the probe within the limits was answered %d", code)
	}
	server.SetMaxConcurrent(0)
	if code := probeOnce(t, server, "/probe?collector=capped&target="+url.QueryEscape(held.server.URL+"/c"), nil).Code; code != http.StatusOK {
		t.Fatalf("the probe after the limits was answered %d", code)
	}
	metrics := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_cache_misses_total{collector="single"}`:                   1,
		`http_exporter_probes_rejected_total{collector="single"}`:                1,
		`http_exporter_cache_misses_total{collector="capped"}`:                   1,
		`http_exporter_probes_rejected_exporter_limit_total{collector="capped"}`: 1,
	} {
		if got := seriesValue(t, metrics, series); got != want {
			t.Errorf("%s is %v, want %v", series, got, want)
		}
	}
	if held.requests() != 2 {
		t.Fatalf("%d trips went to the target, want 2", held.requests())
	}
}

// A static target scrape of a cached collector that finds no slot within its
// budget is counted as rejected and not as a cache miss; the scrape that has
// its slot and goes to the target is the one miss.
func TestAStaticTargetScrapeThatFindsNoSlotIsNoCacheMiss(t *testing.T) {
	held := newHeldTarget(t)
	scheduled := textTarget(t, "value=42\n")
	c := testutil.Collector("shared_limit", "text")
	c.MaxConcurrentProbes = 1
	c.Cache.TTL = model.Duration(time.Minute)
	cfg := &model.Config{Collectors: []model.Collector{c}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "scheduled", Collector: "shared_limit", Target: scheduled.URL}}}
	server := newStaticServer(t, cfg, file)
	busy := make(chan int, 1)
	go func() {
		busy <- probeOnce(t, server, "/probe?collector=shared_limit&target="+url.QueryEscape(held.server.URL), nil).Code
	}()
	testutil.WaitFor(t, "the probe to take the slot", func() bool { return server.trips.count("shared_limit") == 1 })
	server.scrapeStaticTargets(context.Background(), 50*time.Millisecond)
	metrics := selfMetrics(t, server)
	if rejected, misses := seriesValue(t, metrics, `http_exporter_probes_rejected_total{collector="shared_limit"}`), seriesValue(t, metrics, `http_exporter_cache_misses_total{collector="shared_limit"}`); rejected != 1 || misses != 1 {
		t.Fatalf("with the slot held: rejected %v, misses %v; want 1 and the probe's 1", rejected, misses)
	}
	held.open()
	<-busy
	testutil.WaitFor(t, "the slot to be freed", func() bool { return server.trips.count("shared_limit") == 0 })
	server.scrapeStaticTargets(context.Background(), time.Minute)
	if misses := seriesValue(t, selfMetrics(t, server), `http_exporter_cache_misses_total{collector="shared_limit"}`); misses != 2 {
		t.Fatalf("after the scrape that went to the target: misses %v, want 2", misses)
	}
}
