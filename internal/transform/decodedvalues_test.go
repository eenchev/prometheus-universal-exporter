package transform

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What JSON and YAML decode to is something jq handles whole: an integer ID
// of any length keeps every digit, a YAML timestamp stays the text it was
// written as, and an integer past int64 is compared and written as a number.

func valueCollector(decoder string, rules ...model.MetricRule) model.Collector {
	return model.Collector{
		Name: "values", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: decoder},
		Transform: model.TransformConfig{Type: map[string]string{"json": "jq", "yaml": "yq"}[decoder]}, Metrics: rules,
	}
}

func TestLongJSONIntegersKeepEveryDigit(t *testing.T) {
	c := valueCollector("json", model.MetricRule{Name: "item", Type: model.GaugeMetricType, Items: ".items[]", Expression: ".v",
		Labels: []model.LabelRule{{Name: "id", Expression: ".id"}}})
	set := runCollectorLabels(t, c, `{"items":[{"id":12345678901234567891,"v":1},{"id":12345678901234567892,"v":2},{"id":42,"v":3},{"id":1.5,"v":4}]}`)
	got := map[string]float64{}
	for _, m := range set.Metrics {
		got[m.Labels["id"]] = m.Value
	}
	for id, want := range map[string]float64{"12345678901234567891": 1, "12345678901234567892": 2, "42": 3, "1.5": 4} {
		if got[id] != want {
			t.Errorf("id %s = %v, want %v (all: %v)", id, got[id], want, got)
		}
	}
}

func TestYAMLTimestampsAndLargeIntegersReachJQ(t *testing.T) {
	c := valueCollector("yaml",
		model.MetricRule{Name: "updated", Type: model.GaugeMetricType, Expression: `if .updated > "2024" then 1 else 0 end`,
			Labels: []model.LabelRule{{Name: "at", Expression: ".updated"}}},
		model.MetricRule{Name: "big", Type: model.GaugeMetricType, Expression: `.counter`,
			Labels: []model.LabelRule{{Name: "text", Expression: ".counter|tostring"}}},
		model.MetricRule{Name: "positive", Type: model.GaugeMetricType, Expression: `[to_entries[]|select(.value|type=="number" and . > 0)]|length`},
	)
	set := runCollectorLabels(t, c, "updated: 2024-06-01\ncounter: 18446744073709551615\n")
	byName := map[string]model.Metric{}
	for _, m := range set.Metrics {
		byName[m.Name] = m
	}
	if m := byName["updated"]; m.Value != 1 || m.Labels["at"] != "2024-06-01" {
		t.Errorf("updated: %+v", m)
	}
	if m := byName["big"]; m.Labels["text"] != "18446744073709551615" || m.Value != 18446744073709551615 {
		t.Errorf("big: %+v", m)
	}
	if m := byName["positive"]; m.Value != 1 {
		t.Errorf("the uint64 was dropped by a comparison: %+v", m)
	}
}
