package fetch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every request key must belong to at least one type. A field added to
// RequestConfig without deciding which types accept it would be rejected for
// all of them; this makes that decision impossible to forget.
func TestEveryRequestKeyBelongsToAType(t *testing.T) {
	claimed := map[string]bool{"type": true}
	targetClaimed := map[string]bool{}
	for _, rt := range RequestTypes {
		for _, key := range rt.Fields {
			claimed[key] = true
		}
		for _, key := range rt.TargetFields {
			targetClaimed[key] = true
		}
		if rt.Validate == nil || rt.Fetch == nil {
			t.Errorf("request type %q is missing Validate or Fetch", rt.Name)
		}
	}
	for _, key := range yamlKeys(reflect.TypeOf(model.RequestConfig{})) {
		if !claimed[key] {
			t.Errorf("request.%s is accepted by no request type", key)
		}
	}
	for _, key := range yamlKeys(reflect.TypeOf(model.TargetRequestConfig{})) {
		if !targetClaimed[key] {
			t.Errorf("a static target's request.%s is accepted by no request type", key)
		}
	}
}

func yamlKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if key != "" && key != "-" {
			keys = append(keys, key)
		}
	}
	return keys
}
