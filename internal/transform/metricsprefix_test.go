package transform

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// At scrape time, the prefixed name goes through the same limit check as
// every exported name.
func TestAPrefixedNameIsHeldToTheNameLengthLimit(t *testing.T) {
	set := &model.MetricSet{Metrics: []model.Metric{{Name: "script_value", Type: model.GaugeMetricType, Value: 1}}}
	applyMetricsPrefix(set, "acme")
	if set.Metrics[0].Name != "acme_script_value" {
		t.Fatalf("name=%q", set.Metrics[0].Name)
	}
	if err := set.Validate(model.Limits{MaxMetricNameLength: len("acme_script_value") - 1}); err == nil {
		t.Fatal("a prefixed name over the limit passed validation")
	}
}
