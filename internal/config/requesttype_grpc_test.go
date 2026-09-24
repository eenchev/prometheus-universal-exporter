//go:build !select_request_types || request_type_grpc

package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const grpcConfig = `collectors:
  - name: queue_stats
    request:
      type: grpc
      rpc: acme.queue.v1.QueueService/GetStats
      message: '{"queue": {{param_queue|json}}, "include_shards": true}'
      metadata:
        x-tenant: "{{param_tenant:default}}"
      descriptors: PROTOSET_OR_REFLECTION
      retry:
        attempts: 2
        codes: [unavailable, aborted]
    transform:
      type: jq
    metrics:
      - name: queue_depth
        items: .shards[]
        expression: .depth
        labels:
          - name: shard
            expression: .id
  - name: health
    request:
      type: grpc
      rpc: grpc.health.v1.Health/Check
    transform:
      type: jq
    metrics:
      - name: serving
        expression: 'if .status == "SERVING" then 1 else 0 end'
`

// grpc is a request type like the others, whatever builds carry it.
func TestGRPCIsAKnownRequestType(t *testing.T) {
	cfg, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "reflection", 1)))
	if err != nil {
		t.Fatal(err)
	}
	queue, health := cfg.Collectors[0], cfg.Collectors[1]
	// A grpc answer is JSON, so decoder auto is json, with no warning.
	if queue.Decoder.Type != "json" || health.Decoder.Type != "json" || len(cfg.Warnings) != 0 {
		t.Fatalf("decoders %s %s, warnings %v", queue.Decoder.Type, health.Decoder.Type, cfg.Warnings)
	}
	if strings.Join(queue.Request.Retry.Codes, ",") != "UNAVAILABLE,ABORTED" {
		t.Fatalf("codes %v", queue.Request.Retry.Codes)
	}
	// Validating again, as a reload of a loaded configuration does, changes
	// nothing.
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	params := fetch.RequestParams(&queue)
	if len(params) != 2 || params[0].Name != "param_queue" || !params[0].Required || params[1].Default != "default" {
		t.Fatalf("params %+v", params)
	}
}

// With a descriptor set, a method or message mistake fails the load, as
// --dry-run reports it.
func TestGRPCProtosetMistakesFailTheLoad(t *testing.T) {
	dir := t.TempDir()
	set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), false)
	config := strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset\n      protoset_file: "+set, 1)
	if _, err := Load(testutil.WriteIn(t, dir, "good.yaml", config)); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string][2]string{
		"no such method":  {"QueueService/GetStats", "QueueService/Stats"},
		"streaming":       {"QueueService/GetStats", "QueueService/Watch"},
		"a wrong message": {`{{param_queue|json}}, "include_shards": true`, `{{param_queue:orders|json}}, "include_shards": "yes"`},
	} {
		_, err := Load(testutil.WriteIn(t, dir, name+".yaml", strings.Replace(config, edit[0], edit[1], 1)))
		if err == nil || !strings.Contains(err.Error(), `collector "queue_stats"`) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A grpc collector's answer is JSON: another decoder, or a transform that
// does not read JSON, is refused.
func TestGRPCDecoderAndTransform(t *testing.T) {
	base := func() model.Collector {
		return model.Collector{
			Name:      "health",
			Request:   model.RequestConfig{Type: fetch.RequestTypeGRPC, RPC: "grpc.health.v1.Health/Check"},
			Transform: model.TransformConfig{Type: "jq"},
			Metrics:   []model.MetricRule{{Name: "v", Expression: ".status"}},
		}
	}
	for want, edit := range map[string]func(*model.Collector){
		"decodes with xml, but a grpc collector's answer is JSON": func(c *model.Collector) { c.Decoder.Type = "xml" },
		"transforms with regex, which cannot read the JSON":       func(c *model.Collector) { c.Transform.Type = "regex" },
	} {
		c := base()
		edit(&c)
		if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	for _, edit := range []func(*model.Collector){
		func(c *model.Collector) { c.Decoder.Type = "json" },
		func(c *model.Collector) { c.Transform.Type = "yq" },
		func(c *model.Collector) {
			c.Transform = model.TransformConfig{Type: "python", Script: "pass"}
			c.Metrics = nil
		},
	} {
		c := base()
		edit(&c)
		if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
			t.Error(err)
		}
	}
}

// A static target of a grpc collector brings its own message, metadata and
// retry codes, written out: no placeholders. Its message replaces the
// collector's, placeholders and all, so params has nothing to fill.
func TestGRPCStaticTargets(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(testutil.WriteIn(t, dir, "config.yaml", strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset\n      protoset_file: "+grpctest.WriteProtoset(t, filepath.Join(dir, "q.pb"), false), 1)))
	if err != nil {
		t.Fatal(err)
	}
	load := func(target string) error {
		f, err := LoadStaticTargets(testutil.WriteIn(t, t.TempDir(), "targets.yaml", "interval: 1m\ntargets:\n"+target))
		if err != nil {
			return err
		}
		if err := ValidateStaticTargets(f); err != nil {
			return err
		}
		return ValidateStaticTargetsAgainst(f, cfg)
	}
	good := `  - collector: queue_stats
    target: grpcs://queue.internal:9090
    request:
      message: '{"queue": "orders"}'
      metadata:
        x-tenant: acme
      retry:
        codes: [UNAVAILABLE]
`
	if err := load(good); err != nil {
		t.Fatal(err)
	}
	for want, target := range map[string]string{
		"request.message cannot use {{param_...}} placeholders":    strings.Replace(good, `"orders"}`, `"{{param_queue}}"}`, 1),
		"request.metadata x-tenant cannot use {{param_...}}":       strings.Replace(good, "x-tenant: acme", "x-tenant: '{{param_t}}'", 1),
		`unknown field "queues"`:                                   strings.Replace(good, `{"queue":`, `{"queues":`, 1),
		"has the scheme http://":                                   strings.Replace(good, "grpcs://queue.internal:9090", "http://queue.internal:9090/x", 1),
		"is not host:port":                                         strings.Replace(good, "grpcs://queue.internal:9090", "queue.internal", 1),
		`lists "SOON", which is not a gRPC status code`:            strings.Replace(good, "[UNAVAILABLE]", "[SOON]", 1),
		"sets request.path, which does not apply to collector":     good + "      path: /x\n",
		"sets request.headers, which does not apply to collector":  good + "      headers:\n        X-A: b\n",
		"sets request.retry.non_idempotent, which does not apply ": strings.Replace(good, "codes: [UNAVAILABLE]", "non_idempotent: true", 1),
	} {
		if err := load(target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}
