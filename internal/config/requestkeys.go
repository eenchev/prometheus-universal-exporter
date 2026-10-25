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
		var schema map[string]any
		if err := json.Unmarshal([]byte(entry.Schema), &schema); err != nil {
			panic(fmt.Sprintf("internal/fetch/requestkeys_catalogue.go: request.%s: %v", entry.Key, err))
		}
		properties[entry.Key] = schema
	}
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
