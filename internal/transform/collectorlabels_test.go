package transform

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// transform.labels, remove_labels and rename_labels apply to the metrics of
// every transform (applyCollectorLabels).

func runCollectorLabels(t *testing.T, c model.Collector, body string) *model.MetricSet {
	t.Helper()
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// A jq collector gets the collector-wide labels as a prometheus passthrough
// does: added, removed and renamed, on every metric.
func TestCollectorLabelsApplyToEveryTransform(t *testing.T) {
	c := model.Collector{
		Name: "jq", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq", Labels: map[string]string{"env": "prod", "team": "core"}, RemoveLabels: []string{"team"}, RenameLabels: map[string]string{"host": "instance"}},
		Metrics: []model.MetricRule{
			{Name: "a", Type: model.GaugeMetricType, Expression: ".a", Labels: []model.LabelRule{{Name: "host", Expression: ".host"}}},
			{Name: "b", Type: model.GaugeMetricType, Expression: ".b"},
		},
	}
	set := runCollectorLabels(t, c, `{"a":1,"b":2,"host":"web01"}`)
	got := map[string]string{}
	for _, m := range set.Metrics {
		got[m.Name] = fmt.Sprint(m.Labels)
	}
	if got["a"] != "map[env:prod instance:web01]" || got["b"] != "map[env:prod]" {
		t.Fatalf("labels %v", got)
	}
}

// Renames are made at once from the labels as they were, so they never chain,
// whatever order the map is walked in.
func TestLabelRenamesDoNotDependOnOrder(t *testing.T) {
	c := model.Collector{
		Name: "passthrough", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "prometheus"},
		Transform: model.TransformConfig{Type: "prometheus", RenameLabels: map[string]string{"a": "b", "b": "c"}},
	}
	for range 200 {
		set := runCollectorLabels(t, c, "v{a=\"A\",b=\"B\"} 1\n")
		if got := fmt.Sprint(set.Metrics[0].Labels); got != "map[b:A c:B]" {
			t.Fatalf("labels %s, want map[b:A c:B]", got)
		}
	}
}

// The output never shares a label map with the decoded input.
func TestCollectorLabelsLeaveTheInputAlone(t *testing.T) {
	in := model.MetricSet{Metrics: []model.Metric{{Name: "v", Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"a": "A"}}}}
	out := &model.MetricSet{Metrics: append([]model.Metric(nil), in.Metrics...)}
	applyCollectorLabels(out, model.TransformConfig{Labels: map[string]string{"env": "prod"}, RenameLabels: map[string]string{"a": "b"}})
	if fmt.Sprint(in.Metrics[0].Labels) != "map[a:A]" || fmt.Sprint(out.Metrics[0].Labels) != "map[b:A env:prod]" {
		t.Fatalf("in %v, out %v", in.Metrics[0].Labels, out.Metrics[0].Labels)
	}
}
