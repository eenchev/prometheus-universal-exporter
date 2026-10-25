//go:build !select_request_types || request_type_kubernetes

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A kubernetes collector's answer is a JSON document the exporter makes: its
// decoder is json, left out or written, another decoder is refused, and so
// is a transform that does not read JSON.
func TestKubernetesDecoderAndTransform(t *testing.T) {
	base := func() model.Collector {
		return model.Collector{
			Name:      "kube",
			Request:   model.RequestConfig{Type: fetch.RequestTypeKubernetes, Own: fetch.KubernetesConfig{Resource: "pods"}},
			Transform: model.TransformConfig{Type: "jq"},
			Metrics:   []model.MetricRule{{Name: "v", Expression: ".count"}},
		}
	}
	for want, edit := range map[string]func(*model.Collector){
		"decodes with xml, but a kubernetes collector's answer is JSON":                             func(c *model.Collector) { c.Decoder.Type = "xml" },
		"transforms with regex, which cannot read the JSON a kubernetes collector is answered with": func(c *model.Collector) { c.Transform.Type = "regex" },
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

// A configuration whose kubernetes collector lists the core group's
// secrets is refused at load, naming the collector.
func TestAKubernetesCollectorOfSecretsIsRefusedAtLoad(t *testing.T) {
	c := model.Collector{
		Name:      "secrets",
		Request:   model.RequestConfig{Type: fetch.RequestTypeKubernetes, Own: fetch.KubernetesConfig{Resource: "secrets", AllNamespaces: true}},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "v", Expression: ".count"}},
	}
	if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), `collector "secrets" lists secrets, which a kubernetes collector may not`) {
		t.Errorf("err=%v", err)
	}
}

// A tls block written empty, tls: {}, is a tls block: without api_server it
// is refused as one with keys is, where it used to load unnoticed.
func TestAnEmptyTLSBlockWithoutAPIServerIsRefused(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: kube
    request:
      type: kubernetes
      resource: pods
      tls: {}
    transform: {type: jq}
    metrics: [{name: v, expression: .count}]
`))
	if err == nil || !strings.Contains(err.Error(), "sets request.tls or request.bearer_token_file without request.api_server") {
		t.Errorf("tls: {} without api_server: %v", err)
	}
}
