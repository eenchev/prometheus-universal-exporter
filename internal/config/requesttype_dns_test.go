//go:build !select_request_types || request_type_dns

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A dns collector's answer is a JSON document the exporter makes: its
// decoder is json, left out or written, another decoder is refused, and so
// is a transform that does not read JSON.
func TestDNSDecoderAndTransform(t *testing.T) {
	base := func() model.Collector {
		return model.Collector{
			Name:      "resolve",
			Request:   model.RequestConfig{Type: fetch.RequestTypeDNS},
			Transform: model.TransformConfig{Type: "jq"},
			Metrics:   []model.MetricRule{{Name: "v", Expression: ".count"}},
		}
	}
	for want, edit := range map[string]func(*model.Collector){
		"decodes with xml, but a dns collector's answer is JSON":                             func(c *model.Collector) { c.Decoder.Type = "xml" },
		"transforms with regex, which cannot read the JSON a dns collector is answered with": func(c *model.Collector) { c.Transform.Type = "regex" },
	} {
		c := base()
		edit(&c)
		if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	for _, edit := range []func(*model.Collector){
		func(*model.Collector) {},
		func(c *model.Collector) { c.Decoder.Type = "json" },
		func(c *model.Collector) { c.Transform.Type = "yq" },
		func(c *model.Collector) {
			c.Transform = model.TransformConfig{Type: "python", Script: "pass"}
			c.Metrics = nil
		},
	} {
		c := base()
		edit(&c)
		cfg := &model.Config{Collectors: []model.Collector{c}}
		if err := Validate(cfg); err != nil {
			t.Error(err)
			continue
		}
		if got := cfg.Collectors[0].Decoder.Type; got != "json" {
			t.Errorf("decoder %q, want json", got)
		}
		for _, warning := range cfg.Warnings {
			if strings.Contains(warning, "decoder") {
				t.Errorf("warned: %s", warning)
			}
		}
	}
}
