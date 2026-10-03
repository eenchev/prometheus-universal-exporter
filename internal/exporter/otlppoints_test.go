package exporter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A cumulative point starts when its series was first exported, and again
// after a reset, when its count went down; a gauge has no start.
func TestOTLPStartTimes(t *testing.T) {
	starts := newOTLPStartTimes()
	start := starts.forResource("r")
	counter := func(v float64) model.Metric {
		return model.Metric{Name: "c_total", Type: model.CounterMetricType, Value: v}
	}
	if got := start(counter(5), "100"); got != "100" {
		t.Fatalf("first: %s", got)
	}
	if got := start(counter(7), "200"); got != "100" {
		t.Fatalf("growing: %s", got)
	}
	if got := start(counter(2), "300"); got != "300" {
		t.Fatalf("after a reset: %s", got)
	}
	if got := starts.forResource("other")(counter(9), "400"); got != "400" {
		t.Fatalf("another resource: %s", got)
	}
	histogram := model.Metric{Name: "h", Type: model.HistogramMetricType, Histogram: &model.Histogram{Count: 3, Sum: 1}}
	if got := start(histogram, "500"); got != "500" {
		t.Fatalf("histogram: %s", got)
	}
	histogram.Histogram = &model.Histogram{Count: 4, Sum: 2}
	if got := start(histogram, "600"); got != "500" {
		t.Fatalf("histogram growing: %s", got)
	}
	// Forgotten after an hour unseen.
	starts.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if got := start(counter(8), "700"); got != "700" {
		t.Fatalf("after an hour unseen: %s", got)
	}
	raw, _ := json.Marshal(otlpMetrics(model.MetricSet{Metrics: []model.Metric{counter(1), {Name: "g", Type: model.GaugeMetricType, Value: 1}}}, "900", newOTLPStartTimes().forResource("r")))
	text := string(raw)
	if strings.Count(text, `"startTimeUnixNano":"900"`) != 1 {
		t.Fatalf("one start, the counter's: %s", text)
	}
}
