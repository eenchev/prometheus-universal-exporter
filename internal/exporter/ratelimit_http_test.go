//go:build !select_request_types || request_type_http

package exporter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe that request.rate_limit makes wait past its deadline fails at
// once, saying why, and the collector counts it in
// http_exporter_rate_limit_refused_total, by collector alone; a collector
// without a rate limit has the families at 0.
func TestAProbeRefusedByTheRateLimitIsCounted(t *testing.T) {
	fetch.CloseConnections()
	t.Cleanup(fetch.CloseConnections)
	for _, family := range []string{"http_exporter_rate_limit_waits_total", "http_exporter_rate_limit_refused_total"} {
		if only := requestTypeFamilies[family]; !slices.Equal(only, []string{"http", "graphite"}) {
			t.Errorf("%s is of %v, want http and graphite", family, only)
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "value=1") }))
	t.Cleanup(target.Close)
	burst := 1
	limited := testutil.Collector("legacy", "text")
	limited.Request.RateLimit = &model.RateLimitConfig{RequestsPerSecond: 0.1, Burst: &burst}
	server := modeServer(t, limited, testutil.Collector("plain", "text"))
	header := http.Header{"X-Prometheus-Scrape-Timeout-Seconds": {"2"}}
	probeURL := "/probe?collector=legacy&target=" + url.QueryEscape(target.URL)
	if r := probeOnce(t, server, probeURL, header); r.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	r := probeOnce(t, server, probeURL, header)
	if r.Code == http.StatusOK || !strings.Contains(r.Body.String(), "rate limit of 0.1 requests per second for "+target.URL+" would delay the request past its deadline") {
		t.Fatalf("answered %d: %s; want the rate limit's refusal", r.Code, r.Body)
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_rate_limit_refused_total{collector="legacy"} 1`,
		`http_exporter_rate_limit_waits_total{collector="legacy"} 0`,
		`http_exporter_rate_limit_refused_total{collector="plain"} 0`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics:\n%s", want, metrics)
		}
	}
}
