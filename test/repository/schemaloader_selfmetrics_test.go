//go:build !select_request_types || request_type_http

package repository

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// web.self_metrics holds switches. Every key of the block,
// created_timestamps among them, is accepted by the schema and the exporter
// alike as true and as false, and refused by both as anything else: a number,
// a word, text that reads true, a list. A key the block does not have is
// refused by both too. The keys are read from the block's type, so one added
// to it is held to the same.
func TestSchemaAndExporterAgreeOnSelfMetrics(t *testing.T) {
	schema := loadSchema(t)
	block := reflect.TypeOf(model.SelfMetricsConfig{})
	keys := map[string]bool{}
	for i := 0; i < block.NumField(); i++ {
		key, _, _ := strings.Cut(block.Field(i).Tag.Get("yaml"), ",")
		if block.Field(i).Type.Kind() != reflect.Bool {
			t.Fatalf("web.self_metrics.%s is no switch; say here what it takes", key)
		}
		keys[key] = true
		for written, accepted := range map[string]bool{
			"true": true, "false": true,
			"1": false, "0": false, "sometimes": false, `"true"`: false, "[true]": false, "{}": false,
		} {
			agree(t, schema, "web.self_metrics."+key+": "+written, "web: {self_metrics: {"+key+": "+written+"}}\n"+testutil.MinimalConfig, accepted)
		}
	}
	if !keys["created_timestamps"] || !keys["verbose"] || !keys["resource_metrics_enabled"] {
		t.Fatalf("the keys tried are %v", keys)
	}
	agree(t, schema, "every switch on", "web: {self_metrics: {verbose: true, resource_metrics_enabled: true, created_timestamps: true}}\n"+testutil.MinimalConfig, true)
	agree(t, schema, "a key the block does not have", "web: {self_metrics: {created_timestamp: true}}\n"+testutil.MinimalConfig, false)
}
