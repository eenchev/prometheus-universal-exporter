//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A trip whose every probe left while its transform ran is cancelled there,
// and the transform ends with the cancellation as its error. That is not a
// failure of the target or of the collector's rules: nobody waited for the
// answer. So it is counted neither as a failed transform nor against the
// rule it was at, as a trip cancelled in any other stage is not counted as
// that stage's failure; the answer it had decoded stays counted.
func TestATripAbandonedInItsTransformIsNotAFailedTransform(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	t.Cleanup(target.Close)
	c := model.Collector{
		Name: "abandoned", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		// The rule runs until its trip is cancelled.
		Metrics: []model.MetricRule{{Name: "slow", Type: model.GaugeMetricType, Expression: "last(range(1e15))", ErrorMode: model.ErrorModeFail}},
	}
	server := verboseServer(t, true, c)
	server.logger = testutil.QuietLogger(t)
	count := func(series string) float64 {
		return metricValue(t, selfMetrics(t, server), series+`{collector="abandoned"}`)
	}

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	probe := probeAsync(ctx, server, probePath("abandoned", target.URL, ""), nil)
	testutil.WaitFor(t, "the trip to reach its transform", func() bool { return count("http_exporter_decode_success_total") == 1 })
	leave()
	<-probe
	// The trip ends on its own once the rule sees the cancellation,
	// and has counted all it counts when its duration is observed.
	testutil.WaitFor(t, "the cancelled trip to end", func() bool {
		return strings.Contains(selfMetrics(t, server), `http_exporter_collector_scrape_duration_seconds_count{collector="abandoned"} 1`+"\n")
	})

	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_transform_errors_total{collector="abandoned"}`:            0,
		`http_exporter_missing_keys_total{collector="abandoned"}`:                0,
		`http_exporter_scrape_success_total{collector="abandoned"}`:              0,
		`http_exporter_decode_success_total{collector="abandoned"}`:              1,
		`http_exporter_metrics_emitted_total{collector="abandoned"}`:             0,
		`http_exporter_rule_failures_total{collector="abandoned",metric="slow"}`: 0,
	} {
		if got := metricValue(t, exposition, series); got != want {
			t.Errorf("after a trip abandoned in its transform, %s is %v, want %v", series, got, want)
		}
	}
}
