package transform

import (
	"context"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A prometheus rule that scales a series drops what the target said of the
// value it wrote: its exemplar, its unit and an info's or a stateset's type.
// A rule that only renames keeps them.
func TestAScaledSeriesDropsItsExemplarAndUnit(t *testing.T) {
	scale := 1000.0
	c := &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "prometheus"}, Metrics: []model.MetricRule{
		{Name: "wait_ms_total", Expression: "^wait_seconds_total$", Scale: &scale},
		{Name: "wait_total", Expression: "^wait_seconds_total$"},
	}}
	in := model.MetricSet{Metrics: []model.Metric{{Name: "wait_seconds_total", Type: model.CounterMetricType, Value: 3,
		Exemplar: &model.Exemplar{Value: 1}, OpenMetrics: &model.OpenMetricsFamily{Unit: "seconds"}}}}
	set, _, err := applyPrometheusTransform(context.Background(), in, c, c.Transform, c.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 2 {
		t.Fatalf("%d series", len(set.Metrics))
	}
	if scaled := set.Metrics[0]; scaled.Value != 3000 || scaled.Exemplar != nil || scaled.OpenMetrics != nil {
		t.Errorf("the scaled series %+v", scaled)
	}
	if renamed := set.Metrics[1]; renamed.Exemplar == nil || renamed.Unit() != "seconds" {
		t.Errorf("the renamed series %+v", renamed)
	}
}
