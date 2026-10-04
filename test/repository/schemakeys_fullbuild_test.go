//go:build !select_request_types

package repository

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

// constrainedKeys are the keys of a schema that take text and are held to
// allowed values, a pattern or a length, and the mappings whose keys are,
// each as a row of the tables names it: [] for an entry of a list, {} for a
// key of a mapping.
func constrainedKeys(schema map[string]any) []string {
	var keys []string
	constrained := func(node map[string]any) bool {
		for _, keyword := range []string{"enum", "pattern", "minLength", "maxLength", "not", "const"} {
			if _, ok := node[keyword]; ok {
				return true
			}
		}
		return false
	}
	var walk func(node map[string]any, path string)
	walk = func(node map[string]any, path string) {
		if path != "" && slices.Contains(toStrings(node["type"]), "string") && constrained(node) {
			keys = append(keys, path)
		}
		if names, ok := node["propertyNames"].(map[string]any); ok && constrained(names) {
			keys = append(keys, path+"{}")
		}
		properties, _ := node["properties"].(map[string]any)
		for key, sub := range properties {
			if path == "" {
				walk(sub.(map[string]any), key)
			} else {
				walk(sub.(map[string]any), path+"."+key)
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(items, path+"[]")
		}
		if values, ok := node["additionalProperties"].(map[string]any); ok {
			walk(values, path+".*")
		}
	}
	walk(schema, "")
	sort.Strings(keys)
	return keys
}

// The tables that hold the schemas and the exporter to one verdict on a key
// left out, written "", written well and written badly are complete: every
// key the configuration's schema, a collector file's and the target file's
// hold to allowed values, a pattern or a length is a row of one of them, in
// the file of the request type it belongs to. A key that comes to be held
// to such a rule fails here until a row says what both do with it written
// "". Every row's key is a key of its schema, and some fifty are held.
func TestEveryConstrainedKeyOfTheSchemasIsInATable(t *testing.T) {
	rows := map[keyFile]map[string]bool{inConfiguration: {}, inTargetFile: {}}
	for _, key := range slices.Concat(httpSchemaKeys(), grpcSchemaKeys(), graphiteSchemaKeys(), localfileSchemaKeys()) {
		rows[key.file][key.key] = true
	}
	held := 0
	for file, schema := range map[keyFile]map[string]any{inConfiguration: loadSchema(t), inTargetFile: loadSchemaFile(t, staticTargetsSchemaFile)} {
		for key := range rows[file] {
			schemaNode(t, schema, key)
		}
		for _, key := range constrainedKeys(schema) {
			held++
			if !rows[file][key] {
				t.Errorf("%s is held to values, a pattern or a length by its schema and is in no table of schemakeys_*_test.go; add a row that says what the schema and the exporter do with it left out, written \"\", written well and written badly", key)
			}
		}
	}
	if got, want := constrainedKeys(loadSchemaFile(t, collectorFileSchemaFile)), slices.DeleteFunc(constrainedKeys(loadSchema(t)), func(key string) bool { return !strings.HasPrefix(key, "collectors[]") }); !slices.Equal(got, want) {
		t.Errorf("a collector file's schema holds %v, and the configuration's holds of its collectors %v", got, want)
	}
	if held < 50 {
		t.Errorf("only %d keys are held to a rule by the schemas", held)
	}
	t.Logf("%d keys are held to values, a pattern or a length", held)
}
