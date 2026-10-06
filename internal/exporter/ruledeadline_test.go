//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
// The rule never ends, so the budget is what ends the probe, and the deadline
// passes inside the rule once the target has answered within the budget.
// How long that takes is the machine's to say: the budget starts at 100ms,
// and a round in which it ran out while the target was still asked, which
// the probe's answer says, the http stage failing for the budget, is made
// again with twice the budget and a new exporter, up to half a minute. A
// slow machine makes the test slower, where one budget, a second, fails it
// when the target takes longer. How long the probe took is not measured: the
// budget it names is what ended it.
func TestADeadlineInsideARuleFailsTheProbe(t *testing.T) {
	testutil.CaptureLogs(t)
	// The target counts what it is asked by path, and each round asks a path
	// of its own: a request of a round whose budget ended first can reach
	// the target when the next round has begun, and is not that round's.
	var mu sync.Mutex
	hits := map[string]int{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	t.Cleanup(target.Close)
	const offset = 100 * time.Millisecond
	// round probes twice with the budget, and reports whether the target
	// answered within it both times: only then is the deadline one that
	// passed inside the rule, and only then does the round show anything.
	round := func(t *testing.T, mode string, budget time.Duration) (inTime bool) {
		path := "/" + mode + "/" + budget.String()
		c := model.Collector{
			Name: "slow_rule_" + mode, Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
			Cache: model.CacheConfig{TTL: model.Duration(time.Minute)},
			Metrics: []model.MetricRule{
				{Name: "slow", Type: model.GaugeMetricType, Expression: "last(range(1e12))", ErrorMode: mode},
				{Name: "fast", Type: model.GaugeMetricType, Expression: ".a", ErrorMode: mode},
			},
		}
		server, _ := newCacheTestServer(t, c)
		server.SetTimeoutOffset(offset)
		query := "collector=" + c.Name + "&target=" + url.QueryEscape(target.URL+path)
		scrapeTimeout := strconv.FormatFloat((budget + offset).Seconds(), 'f', -1, 64)
		ranOut := "ran out of its " + probeBudget(http.Header{scrapeTimeoutHeader: {scrapeTimeout}}, offset).String() + " budget"
		for probe := 1; probe <= 2; probe++ {
			response := probeWithScrapeTimeout(t, server, query, scrapeTimeout)
			body := response.Body.String()
			if response.Code == http.StatusBadGateway && strings.Contains(body, "collector "+c.Name+" http failed") && strings.Contains(body, ranOut) {
				return false
			}
			if response.Code != http.StatusBadGateway {
				t.Fatalf("probe %d: status=%d body=%s", probe, response.Code, response.Body)
			}
			for _, want := range []string{"collector " + c.Name + " transform failed", `the transform was stopped at metric "slow": context deadline exceeded`, ranOut} {
				if !strings.Contains(body, want) {
					t.Fatalf("probe %d: body %q lacks %q", probe, response.Body, want)
				}
			}
			if strings.Contains(body, "fast") || strings.Contains(body, `"stage":"metric"`) {
				t.Fatalf("probe %d: answered with the other rule's series, or as the rule's own failure: %s", probe, response.Body)
			}
			// The first probe's answer was not kept for the second.
			mu.Lock()
			got := hits[path]
			mu.Unlock()
			if got != probe {
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
		return true
	}
	for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeLog, model.ErrorModeFail} {
		t.Run(mode, func(t *testing.T) {
			for budget := 100 * time.Millisecond; !round(t, mode, budget); budget *= 2 {
				if 2*budget > 30*time.Second {
					t.Fatalf("the target did not answer within a budget of %s", budget)
				}
			}
		})
	}
}
