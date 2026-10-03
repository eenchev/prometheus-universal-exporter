//go:build !select_request_types

package repository

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
)

// configs/collector-file.schema.json is committed, current (its flag printing it is
// checked in cli_test.go), and
// describes collectors exactly as the configuration schema does.
func TestCommittedCollectorFileSchemaIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(collectorFileSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := config.CollectorFileSchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, generated) {
		t.Fatalf("%s is out of date; regenerate the schemas with: make schemas", collectorFileSchemaFile)
	}
	var parsed map[string]any
	if err := json.Unmarshal(generated, &parsed); err != nil {
		t.Fatal(err)
	}
	configSchema := parsedSchema(t, config.SchemaJSON)
	fileItems := parsed["properties"].(map[string]any)["collectors"].(map[string]any)["items"]
	configItems := configSchema["properties"].(map[string]any)["collectors"].(map[string]any)["items"]
	if !reflect.DeepEqual(fileItems, configItems) {
		t.Fatal("the collector file schema describes collectors differently from the configuration schema")
	}
}

// The committed schema is exactly what the code generates, so it cannot drift
// from the configuration it describes. Regenerate it, with the others, with:
//
//	make schemas
func TestCommittedConfigSchemaIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(configSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := config.SchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, generated) {
		t.Fatalf("%s is out of date; regenerate the schemas with: make schemas", configSchemaFile)
	}
}
