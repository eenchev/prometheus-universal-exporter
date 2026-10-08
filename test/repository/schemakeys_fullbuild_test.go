//go:build !select_request_types

package repository

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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

// sizeKeys are the keys of a schema that take a size, found by what a size
// is to a schema: a whole number, or text of the pattern of a size.
func sizeKeys(schema map[string]any) []string {
	var keys []string
	var walk func(node map[string]any, path string)
	walk = func(node map[string]any, path string) {
		if node["pattern"] == model.ByteSizePattern {
			keys = append(keys, path)
		}
		properties, _ := node["properties"].(map[string]any)
		for key, sub := range properties {
			walk(sub.(map[string]any), strings.TrimPrefix(path+"."+key, "."))
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(items, path+"[]")
		}
	}
	walk(schema, "")
	sort.Strings(keys)
	return keys
}

// Every key of the schemas that takes a size is put through the ways a
// number of bytes is written (sizeForms): its row says so, and a size key
// that is added fails here until its row does. The configuration and a
// collector file have five, and the target file none.
func TestEverySizeKeyOfTheSchemasIsPutThroughTheWaysASizeIsWritten(t *testing.T) {
	marked := map[string]bool{}
	for _, key := range slices.Concat(httpSchemaKeys(), grpcSchemaKeys(), graphiteSchemaKeys(), localfileSchemaKeys()) {
		if key.size {
			if key.file != inConfiguration {
				t.Errorf("%s is marked as a size in a file that has none", key.name())
			}
			marked[key.key] = true
		}
	}
	sizes := sizeKeys(loadSchema(t))
	for _, key := range sizes {
		if !marked[key] {
			t.Errorf("%s takes a size and no row of schemakeys_*_test.go says size: true of it", key)
		}
	}
	if len(sizes) != 5 || len(marked) != len(sizes) {
		t.Errorf("the configuration's schema has the size keys %v, and the rows mark %d: want the five sizes, each marked", sizes, len(marked))
	}
	if inFile := sizeKeys(loadSchemaFile(t, collectorFileSchemaFile)); !slices.Equal(inFile, sizes) {
		t.Errorf("a collector file's schema has the size keys %v, and the configuration's %v", inFile, sizes)
	}
	if inTargets := sizeKeys(loadSchemaFile(t, staticTargetsSchemaFile)); len(inTargets) != 0 {
		t.Errorf("the target file's schema has the size keys %v, and no row puts them through the ways a size is written", inTargets)
	}
}
