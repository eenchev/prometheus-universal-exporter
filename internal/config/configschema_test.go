package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every key the configuration reads is in the schema, at every depth, and the
// schema has no key the configuration does not read.
func TestConfigSchemaCoversEveryKey(t *testing.T) {
	var walk func(t reflect.Type, schema map[string]any, path string)
	walk = func(typ reflect.Type, schema map[string]any, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			switch typ.Kind() {
			case reflect.Pointer:
				typ = typ.Elem()
			case reflect.Slice:
				typ, schema = typ.Elem(), schema["items"].(map[string]any)
			case reflect.Map:
				typ = typ.Elem()
				if sub, ok := schema["additionalProperties"].(map[string]any); ok {
					schema = sub
				}
			}
		}
		if typ.Kind() != reflect.Struct || typ == durationType {
			return
		}
		properties := schema["properties"].(map[string]any)
		seen := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			key, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
			if key == "" || key == "-" {
				continue
			}
			seen[key] = true
			sub, ok := properties[key].(map[string]any)
			if !ok {
				t.Errorf("%s.%s is read from the configuration but missing from the schema", path, key)
				continue
			}
			walk(typ.Field(i).Type, sub, path+"."+key)
		}
		for key := range properties {
			if !seen[key] {
				t.Errorf("%s.%s is in the schema but not read from the configuration", path, key)
			}
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s allows unknown keys, but the configuration rejects them", path)
		}
	}
	walk(reflect.TypeOf(model.Config{}), configSchema(), "")
}

// The request types offered are the ones this build carries.
func TestConfigSchemaListsTheBuiltRequestTypes(t *testing.T) {
	collector := configSchema()["properties"].(map[string]any)["collectors"].(map[string]any)["items"].(map[string]any)
	requestType := collector["properties"].(map[string]any)["request"].(map[string]any)["properties"].(map[string]any)["type"].(map[string]any)
	if !reflect.DeepEqual(requestType["enum"], fetch.BuiltRequestTypes()) {
		t.Fatalf("enum=%v, built=%v", requestType["enum"], fetch.BuiltRequestTypes())
	}
}
