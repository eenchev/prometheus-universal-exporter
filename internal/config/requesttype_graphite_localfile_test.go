//go:build !select_request_types || (request_type_graphite && request_type_localfile)

package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func graphiteTestCollector() model.Collector {
	return model.Collector{
		Name:      "graphite",
		Request:   model.RequestConfig{Type: fetch.RequestTypeGraphite, Targets: []string{"a.b"}},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "v", Items: ".series[]", Expression: ".value"}},
	}
}

func TestGraphiteDecoderSettings(t *testing.T) {
	for name, test := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"a bad value":        {func(c *model.Collector) { c.Response.Graphite.Value = "median" }, `response.graphite.value is "median"; want one of last, max, min, avg, sum`},
		"a negative max_age": {func(c *model.Collector) { c.Response.Graphite.MaxAge = model.Duration(-time.Second) }, "response.graphite.max_age must not be negative"},
		"bad invalid_lines":  {func(c *model.Collector) { c.Response.Graphite.InvalidLines = "drop" }, `response.graphite.invalid_lines is "drop"; want fail or skip`},
		"another decoder":    {func(c *model.Collector) { c.Decoder.Type = "json"; c.Response.Graphite.Value = "max" }, "sets response.graphite, which applies to the graphite decoder, but its decoder is json"},
		"a regex transform": {func(c *model.Collector) {
			c.Transform.Type = "regex"
			c.Decoder.Type = "graphite"
			c.Metrics = []model.MetricRule{{Name: "v", Expression: "(.*)"}}
		}, "decodes Graphite series, which a regex transform cannot read; use jq, yq or python"},
		"a prometheus transform": {func(c *model.Collector) { c.Transform.Type = "prometheus"; c.Metrics = nil }, "which a prometheus transform cannot read"},
	} {
		t.Run(name, func(t *testing.T) {
			c := graphiteTestCollector()
			test.edit(&c)
			if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
	for name, edit := range map[string]func(*model.Collector){
		"value in any case": func(c *model.Collector) {
			c.Response.Graphite.Value = " MAX "
			c.Response.Graphite.InvalidLines = "Skip"
		},
		"a python transform": func(c *model.Collector) {
			c.Transform = model.TransformConfig{Type: "python", Script: "pass"}
			c.Metrics = nil
		},
		"a yq transform": func(c *model.Collector) { c.Transform.Type = "yq" },
		"the answer as json": func(c *model.Collector) {
			c.Decoder.Type = "json"
			c.Metrics[0].Items = ".[]"
			c.Metrics[0].Expression = ".datapoints[-1][0]"
		},
		"a localfile, auto": func(c *model.Collector) {
			c.Request = model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: t.TempDir()}
			c.Response.Graphite.Value = "sum"
		},
		"a graphite decoder on a localfile": func(c *model.Collector) {
			c.Request = model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: t.TempDir()}
			c.Decoder.Type = "graphite"
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := graphiteTestCollector()
			edit(&c)
			cfg := &model.Config{Collectors: []model.Collector{c}}
			if err := Validate(cfg); err != nil {
				t.Fatal(err)
			}
			if g := cfg.Collectors[0].Response.Graphite; name == "value in any case" && (g.Value != "max" || g.InvalidLines != "skip") {
				t.Fatalf("settings %+v", g)
			}
		})
	}
}
