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

// The entries of rate_limits are shared by every collector: a probe of a
// second collector to a host the first used up the turns of is refused,
// counted by entry in http_exporter_host_rate_limit_refused_total. A reload
// that leaves the entry as it was keeps its bucket, one that changes it
// starts it from a full one, and one that removes it ends its series.
func TestHostRateLimitsAreSharedAndFollowReloads(t *testing.T) {
	fetch.CloseConnections()
	t.Cleanup(fetch.CloseConnections)
	target := textTarget(t, "value=1\n")
	document := func(rate string) string {
		doc := testutil.CollectorsDocument("first", "second")
		if rate != "" {
			doc += "rate_limits:\n  - name: api\n    hosts: [\"127.0.0.1\"]\n    rate_limit:\n      requests_per_second: " + rate + "\n"
		}
		return doc
	}
	r := newReloadable(t, document("0.1"), "")
	header := http.Header{"X-Prometheus-Scrape-Timeout-Seconds": {"2"}}
	probeBy := func(collector string) *httptest.ResponseRecorder {
		return probeOnce(t, r.server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), header)
	}
	refusal := `rate limit of 0.1 requests per second of rate_limits entry "api" for 127.0.0.1 would delay the request past its deadline`
	if got := probeBy("first"); got.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", got.Code, got.Body)
	}
	if got := probeBy("second"); got.Code == http.StatusOK || !strings.Contains(got.Body.String(), refusal) {
		t.Fatalf("the second collector's probe answered %d: %s; want the entry's refusal", got.Code, got.Body)
	}
	r.reloadTo(document("0.1"))
	if got := probeBy("first"); got.Code == http.StatusOK || !strings.Contains(got.Body.String(), refusal) {
		t.Fatalf("after a reload that left the entry as it was, answered %d: %s; want its bucket kept", got.Code, got.Body)
	}
	metrics := selfMetrics(t, r.server)
	for _, want := range []string{
		`http_exporter_host_rate_limit_refused_total{rule="api"} 2`,
		`http_exporter_host_rate_limit_waits_total{rule="api"} 0`,
		`http_exporter_rate_limit_refused_total{collector="second"} 0`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics:\n%s", want, metrics)
		}
	}
	r.reloadTo(document("0.2"))
	if got := probeBy("second"); got.Code != http.StatusOK {
		t.Fatalf("after a reload that changed the entry, answered %d: %s; want it sent from a full bucket", got.Code, got.Body)
	}
	if metrics := selfMetrics(t, r.server); !strings.Contains(metrics, `http_exporter_host_rate_limit_refused_total{rule="api"} 2`+"\n") {
		t.Errorf("the changed entry's counts were not kept:\n%s", metrics)
	}
	r.reloadTo(document(""))
	if got := probeBy("second"); got.Code != http.StatusOK {
		t.Fatalf("after a reload that removed the entry, answered %d: %s", got.Code, got.Body)
	}
	if metrics := selfMetrics(t, r.server); strings.Contains(metrics, "http_exporter_host_rate_limit") {
		t.Errorf("the removed entry still has series:\n%s", metrics)
	}
}
