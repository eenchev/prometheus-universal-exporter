package exporter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A gauge read from an OpenMetrics info or stateset family is a cumulative,
// non-monotonic sum over OTLP, whatever its values, without a start time,
// which a value that goes up and down cannot be given; another gauge is a
// gauge. The debug page names the kind and says why.
func TestOTLPExportsAnInfoAndAStateSetAsNonMonotonicSums(t *testing.T) {
	info := &model.OpenMetricsFamily{Kind: model.GaugeKindInfo}
	stateset := &model.OpenMetricsFamily{Kind: model.GaugeKindStateSet}
	set := model.MetricSet{Metrics: []model.Metric{
		{Name: "build_info", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"version": "1.2"}, OpenMetrics: info},
		{Name: "mode", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"mode": "on"}, OpenMetrics: stateset},
		{Name: "mode", Type: model.GaugeMetricType, Value: 0, Labels: map[string]string{"mode": "off"}, OpenMetrics: stateset},
		{Name: "odd_info", Type: model.GaugeMetricType, Value: 2, OpenMetrics: info},
		{Name: "queue_depth", Type: model.GaugeMetricType, Value: 4},
	}}
	started := 0
	out := otlpMetrics(set, "7", func(model.Metric, string) string { started++; return "5" })
	if len(out) != 4 || started != 0 {
		t.Fatalf("%d metrics, %d start times asked for", len(out), started)
	}
	for i, name := range []string{"build_info", "mode", "odd_info"} {
		m := out[i]
		if m.Name != name || m.Sum == nil || m.Sum.IsMonotonic || m.Sum.AggregationTemporality != otlpCumulative || m.Sum.DataPoints[0].StartTimeUnixNano != "" {
			t.Errorf("%s: %+v", name, m)
		}
	}
	if points := out[1].Sum.DataPoints; len(points) != 2 || *points[0].AsDouble != 1 || *points[1].AsDouble != 0 {
		t.Errorf("the stateset's points %+v", points)
	}
	if out[3].Gauge == nil {
		t.Errorf("the gauge %+v", out[3])
	}
	body, err := json.Marshal(out[0])
	if err != nil || !strings.Contains(string(body), `"isMonotonic":false`) {
		t.Errorf("%s %v", body, err)
	}
	if kind := otlpKind(out[1]); kind != otlpKindUpDownSum || debugOTLPKind(kind) != "sum (cumulative, non-monotonic)" {
		t.Errorf("kind %d", kind)
	}
	notes := debugPointNotes(set.Metrics[1], set.Metrics[1], out[1], false, nil)
	if !strings.Contains(strings.Join(notes, "; "), "an OpenMetrics stateset: exported as a non-monotonic sum, without a start time") {
		t.Errorf("notes %q", notes)
	}
}
