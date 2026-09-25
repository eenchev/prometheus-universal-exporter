package exporter

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A trip the target refused as unauthorized, 401 or 403, is never answered
// with a stale result: the credential it was refused may have been revoked,
// and the last good result was given to another. Any other failure still
// is.
func TestARefusedCredentialGetsNoStaleResult(t *testing.T) {
	testutil.CaptureLogs(t)
	for _, mode := range []string{"unauthorized", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			flaky, target := newFlakyTarget(t)
			flaky.value.Store(1)
			server, _ := newCacheTestServer(t, staleCollector(0, 5*time.Minute))
			probe := "/probe?collector=flaky&target=" + url.QueryEscape(target.URL)
			if fresh := probeOnce(t, server, probe, nil); fresh.Code != http.StatusOK {
				t.Fatalf("fresh answer: %d %s", fresh.Code, fresh.Body)
			}
			flaky.mode.Store(mode)
			if refused := probeOnce(t, server, probe, nil); refused.Code != http.StatusBadGateway || strings.Contains(refused.Body.String(), "demo_value") {
				t.Fatalf("a %s trip answered %d:\n%s", mode, refused.Code, refused.Body)
			}
			if metrics := selfMetrics(t, server); !strings.Contains(metrics, `http_exporter_cache_stale_served_total{collector="flaky"} 0`) {
				t.Fatalf("a stale result was served:\n%s", metrics)
			}
			// The entry is still there for a failure that is not about the
			// credential.
			flaky.mode.Store("status")
			if stale := probeOnce(t, server, probe, nil); stale.Code != http.StatusOK || seriesValue(t, stale.Body.String(), resultStaleMetric) != 1 {
				t.Fatalf("a 503 was not answered stale: %d\n%s", stale.Code, stale.Body)
			}
		})
	}
}

// A static target's scrape refused as unauthorized publishes no stale
// result either; target up says it failed.
func TestAStaticTargetsRefusedCredentialGetsNoStaleResult(t *testing.T) {
	testutil.CaptureLogs(t)
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(7)
	c := staleCollector(time.Millisecond, time.Hour)
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "flaky", Target: target.URL}}}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{c}}, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	time.Sleep(5 * time.Millisecond)
	flaky.mode.Store("unauthorized")
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	body := getStaticTargets(t, server, "/static-targets")
	if strings.Contains(body, "demo_value") || !strings.Contains(body, `http_exporter_target_up{collector="flaky",static_target="t"`) {
		t.Fatalf("after a 401:\n%s", body)
	}
	flaky.mode.Store("status")
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	if body := getStaticTargets(t, server, "/static-targets"); !strings.Contains(body, `demo_value{static_target="t"} 7`) {
		t.Fatalf("after a 503 the stale result is not served:\n%s", body)
	}
}

// A label cut to limits.max_label_value_length with truncate: true stays
// within it when the target's text is not the UTF-8 it claims: the repair,
// which makes each invalid byte a three-byte U+FFFD, comes first.
func TestATruncatedLabelStaysWithinItsLimitAfterAUTF8Repair(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(append([]byte("v=1 note="), bytes.Repeat([]byte("\xe9a"), 30)...))
	}))
	t.Cleanup(target.Close)
	c := testutil.Collector("latin", "text")
	c.Metrics = []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: `v=(\d+) note=(.*)`,
		Labels: []model.LabelRule{{Name: "note", Expression: "2", Truncate: true}}}}
	c.Limits.MaxLabelValueLength = 20
	server := modeServer(t, c)
	r := probe(t, server, url.QueryEscape(target.URL), "latin")
	if r.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", r.Code, r.Body)
	}
	body := r.Body.String()
	start := strings.Index(body, `note="`) + len(`note="`)
	end := strings.Index(body[start:], `"`)
	if note := body[start : start+end]; len(note) > 20 || !strings.Contains(note, "�") {
		t.Fatalf("the label is %d bytes: %q", len(note), note)
	}
	if !strings.Contains(selfMetrics(t, server), `http_exporter_invalid_utf8_total{collector="latin"} 1`) {
		t.Fatalf("the repair was not counted:\n%s", selfMetrics(t, server))
	}
}
