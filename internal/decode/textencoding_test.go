package decode

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestSanitizeUTF8(t *testing.T) {
	shared := map[string]string{"a": "ok\xff", "b": "fine"}
	set := &model.MetricSet{Metrics: []model.Metric{
		{Name: "x", Help: "h\xfe", Labels: shared},
		{Name: "y", Labels: shared},
		{Name: "z", Labels: map[string]string{"c": "clean"}},
	}}
	changed, first := model.SanitizeUTF8(set)
	if changed != 3 || first != "x" {
		t.Fatalf("changed=%d first=%q", changed, first)
	}
	if set.Metrics[0].Help != "h�" || set.Metrics[0].Labels["a"] != "ok�" || set.Metrics[1].Labels["a"] != "ok�" {
		t.Fatalf("%+v", set.Metrics)
	}
	if shared["a"] != "ok\xff" {
		t.Fatal("a label map shared with the transform was changed in place")
	}
}
