//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// slowTarget answers after delay, so a scrape or probe of it takes at least
// that long.
func slowTarget(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(target.Close)
	return target
}

// Both load counters are in the default self-metrics, without verbose mode,
// at 0 before anything was scraped or probed, and without a label: one
// series each for the whole exporter.
func TestLoadCountersAreDefaultSelfMetrics(t *testing.T) {
	server := flightServer(t, testutil.Collector("text", "text"))
	if server.manager.Get().Web.SelfMetrics.Verbose {
		t.Fatal("the test server is in verbose mode")
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		"# TYPE http_exporter_static_targets_scrape_seconds_total counter\nhttp_exporter_static_targets_scrape_seconds_total 0\n",
		"# TYPE http_exporter_probes_seconds_total counter\nhttp_exporter_probes_seconds_total 0\n",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("no %q in:\n%s", want, metrics)
		}
	}
}

// A static target scrape adds the time it took, the same time its
// collector's http_exporter_scrape_duration_seconds reports, to
// http_exporter_static_targets_scrape_seconds_total, and nothing to the
// probes' counter; a second scrape adds its own.
func TestStaticTargetScrapeSecondsAddUp(t *testing.T) {
	const delay = 20 * time.Millisecond
	target := slowTarget(t, delay)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "slow", Collector: "text", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)

	server.scrapeStaticTargets(context.Background(), 0)
	metrics := selfMetrics(t, server)
	first := seriesValue(t, metrics, `http_exporter_scrape_duration_seconds{collector="text"}`)
	if got := seriesValue(t, metrics, "http_exporter_static_targets_scrape_seconds_total"); got != first || got < delay.Seconds() {
		t.Fatalf("static target seconds=%v after one scrape of %v", got, first)
	}
	if got := seriesValue(t, metrics, "http_exporter_probes_seconds_total"); got != 0 {
		t.Fatalf("probe seconds=%v with no probe", got)
	}

	server.scrapeStaticTargets(context.Background(), 0)
	metrics = selfMetrics(t, server)
	second := seriesValue(t, metrics, `http_exporter_scrape_duration_seconds{collector="text"}`)
	want := (time.Duration(first*1e9+0.5) + time.Duration(second*1e9+0.5)).Seconds()
	if got := seriesValue(t, metrics, "http_exporter_static_targets_scrape_seconds_total"); got != want {
		t.Fatalf("static target seconds=%v after scrapes of %v and %v", got, first, second)
	}
}

// A probe adds the time it took to http_exporter_probes_seconds_total, and
// nothing to the static targets' counter; one answered from the cache adds
// its own, shorter time.
func TestProbeSecondsAddUp(t *testing.T) {
	const delay = 20 * time.Millisecond
	target := slowTarget(t, delay)
	c := testutil.Collector("text", "text")
	c.Cache.TTL = model.Duration(time.Minute)
	server := flightServer(t, c)

	if got := probeOnce(t, server, probePath("text", target.URL, ""), nil); got.Code != http.StatusOK {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	metrics := selfMetrics(t, server)
	first := seriesValue(t, metrics, `http_exporter_scrape_duration_seconds{collector="text"}`)
	if got := seriesValue(t, metrics, "http_exporter_probes_seconds_total"); got != first || got < delay.Seconds() {
		t.Fatalf("probe seconds=%v after one probe of %v", got, first)
	}
	if got := seriesValue(t, metrics, "http_exporter_static_targets_scrape_seconds_total"); got != 0 {
		t.Fatalf("static target seconds=%v with no static target", got)
	}

	if got := probeOnce(t, server, probePath("text", target.URL, ""), nil); got.Code != http.StatusOK {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	metrics = selfMetrics(t, server)
	if hits := seriesValue(t, metrics, `http_exporter_cache_hits_total{collector="text"}`); hits != 1 {
		t.Fatalf("cache hits=%v", hits)
	}
	second := seriesValue(t, metrics, `http_exporter_scrape_duration_seconds{collector="text"}`)
	want := (time.Duration(first*1e9+0.5) + time.Duration(second*1e9+0.5)).Seconds()
	if got := seriesValue(t, metrics, "http_exporter_probes_seconds_total"); got != want {
		t.Fatalf("probe seconds=%v after probes of %v and %v", got, first, second)
	}
}
