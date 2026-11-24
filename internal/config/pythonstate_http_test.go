//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// transform.state keeps a dict between the runs of a collector's Python
// code, so it is taken with a python transform and with a pre_script, and
// refused on a collector that runs no script, which it would do nothing
// for.
func TestTransformStateNeedsAPythonScript(t *testing.T) {
	collector := func(edit func(*model.Collector)) *model.Config {
		c := model.Collector{
			Name:      "app",
			Request:   model.RequestConfig{Type: "http"},
			Transform: model.TransformConfig{Type: "jq", State: true},
			Metrics:   []model.MetricRule{{Name: "v", Expression: ".v"}},
		}
		edit(&c)
		return &model.Config{Collectors: []model.Collector{c}}
	}
	if err := Validate(collector(func(*model.Collector) {})); err == nil || !strings.Contains(err.Error(), `collector "app" sets transform.state`) {
		t.Fatalf("err=%v", err)
	}
	if err := Validate(collector(func(c *model.Collector) { c.Transform.PreScript = "pass" })); err != nil {
		t.Fatal(err)
	}
	if err := Validate(collector(func(c *model.Collector) {
		c.Transform = model.TransformConfig{Type: "python", Script: "pass", State: true}
		c.Metrics = nil
	})); err != nil {
		t.Fatal(err)
	}
}
