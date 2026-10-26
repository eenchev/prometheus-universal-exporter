package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// requestConfigType is a collector's request block, whose properties are
// its common part's keys and the own keys of every request type
// (ownRequestKeys).
var requestConfigType = reflect.TypeOf(model.RequestConfig{})

// ownRequestKeys adds to the properties of the request block at path the
// keys of every request type's own, as a collector of the type writes them
// beside the common part's (fetch.RequestType.Config): a built type's as
// its struct and its KeyRules say, and those of a type the build left out
// as the catalogue holds them (fetch.UnbuiltRequestKeys), so the schema
// describes every key of every type of the tree in every build.
func ownRequestKeys(properties map[string]any, path string, rules map[string]map[string]any) {
	for _, rt := range fetch.OwnKeyTypes() {
		for key, schema := range ownKeySchemas(rt, path, rules) {
			properties[key] = schema
		}
	}
	for _, entry := range fetch.UnbuiltRequestKeys() {
		var schema any
		if err := json.Unmarshal([]byte(entry.Schema), &schema); err != nil {
			panic(fmt.Sprintf("internal/fetch/requestkeys_catalogue.go: request.%s: %v", entry.Key, err))
		}
		properties[entry.Key] = asMade(schema)
	}
}

// asMade is a schema read back from JSON in the shapes schemaFor and the
// rules make it in, which the load reads it by (kindsOf): a list of texts
// as a []string, and a whole number as an int.
func asMade(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, sub := range value {
			value[key] = asMade(sub)
		}
		return value
	case []any:
		texts := make([]string, 0, len(value))
		for i, sub := range value {
			value[i] = asMade(sub)
			if text, ok := value[i].(string); ok {
				texts = append(texts, text)
			}
		}
		if len(texts) == len(value) && len(value) > 0 {
			return texts
		}
		return value
	case float64:
		if whole := int(value); float64(whole) == value {
			return whole
		}
	}
	return value
}

// ownKeySchemas are the schemas of the own keys of rt, by key, for the
// request block at path, with rt's KeyRules beside rules.
func ownKeySchemas(rt *fetch.RequestType, path string, rules map[string]map[string]any) map[string]any {
	if rt.KeyRules != nil {
		rules = maps.Clone(rules)
		for at, rule := range rt.KeyRules() {
			rules[joinSchemaPath(path, at)] = rule
		}
	}
	schemas := map[string]any{}
	own := reflect.TypeOf(rt.Config)
	for i := 0; i < own.NumField(); i++ {
		field := own.Field(i)
		key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if key == "" || key == "-" || !field.IsExported() {
			continue
		}
		schemas[key] = schemaFor(field.Type, joinSchemaPath(path, key), rules)
	}
	return schemas
}
