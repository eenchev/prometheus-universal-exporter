//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe whose budget runs out inside a metric rule fails as a probe that
// ran out of time does, whatever the rule's error mode: 502, in the transform
// stage, naming the budget and the rule it was at. Under ignore and log the
// rule was skipped instead, so the probe answered 200 with the series of the
// rules after it, as if that were all the target had, and the partial answer
// was cached. Nothing is cached, so the next probe goes back to the target,
// and nothing is counted against the rule, which did not fail.
//
// The budget is a second, which the rule that never ends uses up: the
// target has to have answered within it for the deadline to pass inside the
// rule, and at 300ms a machine busy enough fails the probe at the fetch.
// How long the probe took is not measured: the budget it names is what
// ended it.
func TestADeadlineInsideARuleFailsTheProbe(t *testing.T) {
	testutil.CaptureLogs(t)
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	t.Cleanup(target.Close)
	for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeLog, model.ErrorModeFail} {
		t.Run(mode, func(t *testing.T) {
			hits.Store(0)
			c := model.Collector{
				Name: "slow_rule_" + mode, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
				Cache: model.CacheConfig{TTL: model.Duration(time.Minute)},
				Metrics: []model.MetricRule{
					{Name: "slow", Type: model.GaugeMetricType, Expression: "last(range(1e12))", ErrorMode: mode},
					{Name: "fast", Type: model.GaugeMetricType, Expression: ".a", ErrorMode: mode},
				},
			}
			server, _ := newCacheTestServer(t, c)
			server.SetTimeoutOffset(100 * time.Millisecond)
			query := "collector=" + c.Name + "&target=" + url.QueryEscape(target.URL)
			for probe := 1; probe <= 2; probe++ {
				response := probeWithScrapeTimeout(t, server, query, "1.1")
				if response.Code != http.StatusBadGateway {
					t.Fatalf("probe %d: status=%d body=%s", probe, response.Code, response.Body)
				}
				for _, want := range []string{"collector " + c.Name + " transform failed", `the transform was stopped at metric "slow": context deadline exceeded`, "ran out of its 1s budget"} {
					if !strings.Contains(response.Body.String(), want) {
						t.Fatalf("probe %d: body %q lacks %q", probe, response.Body, want)
					}
				}
				if strings.Contains(response.Body.String(), "fast") || strings.Contains(response.Body.String(), `"stage":"metric"`) {
					t.Fatalf("probe %d: answered with the other rule's series, or as the rule's own failure: %s", probe, response.Body)
				}
				// The first probe's answer was not kept for the second.
				if got := hits.Load(); got != int64(probe) {
					t.Fatalf("after probe %d the target was asked %d times", probe, got)
				}
			}
			exposition := selfMetrics(t, server)
			for series, want := range map[string]float64{
				`http_exporter_rule_failures_total{collector="` + c.Name + `",metric="slow"}`: 0,
				`http_exporter_rule_failures_total{collector="` + c.Name + `",metric="fast"}`: 0,
				`http_exporter_transform_errors_total{collector="` + c.Name + `"}`:            2,
				`http_exporter_missing_keys_total{collector="` + c.Name + `"}`:                0,
			} {
				if got := seriesValue(t, exposition, series); got != want {
					t.Errorf("%s = %v, want %v", series, got, want)
				}
			}
		})
	}
}
