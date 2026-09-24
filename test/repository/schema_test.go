package repository

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// collector-file.schema.json is committed, current (its flag printing it is
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
		t.Fatalf("%s is out of date; regenerate it with: go run . --config.collector-file-schema > %s", collectorFileSchemaFile, collectorFileSchemaFile)
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

func TestCollectorFileSchemaAcceptsCollectorsOnly(t *testing.T) {
	schema := loadSchemaFile(t, collectorFileSchemaFile)
	valid := testutil.CollectorsDocument("demo")
	for name, tc := range map[string]struct {
		document string
		valid    bool
	}{
		"collectors":        {valid, true},
		"another key":       {"web: {}\n" + valid, false},
		"collector_files":   {"collector_files: [x.yaml]\n" + valid, false},
		"no collectors":     {"collectors: []\n", false},
		"nothing":           {"{}\n", false},
		"invalid collector": {strings.Replace(valid, "name: demo", "name: bad-name", 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			var doc any
			if err := yaml.Unmarshal([]byte(tc.document), &doc); err != nil {
				t.Fatal(err)
			}
			errs := validateAgainstSchema(schema, normalizeYAML(doc))
			if tc.valid != (len(errs) == 0) {
				t.Fatalf("valid=%v, errors=%v", tc.valid, errs)
			}
		})
	}
	// The configuration schema accepts a configuration made of collector
	// files alone.
	var doc any
	if err := yaml.Unmarshal([]byte("collector_files: ['collectors.d/*.yaml']\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validateAgainstSchema(loadSchema(t), normalizeYAML(doc)); len(errs) > 0 {
		t.Fatal(errs)
	}
}

// config.schema.json is the JSON Schema of the configuration file, generated
// from the Config struct (config/configschema.go) and printed by --config.schema.

const (
	configSchemaFile        = "config.schema.json"
	collectorFileSchemaFile = "collector-file.schema.json"
)

// The committed schema is exactly what the code generates, so it cannot drift
// from the configuration it describes. Regenerate it with:
//
//	go run . --config.schema > config.schema.json
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
		t.Fatalf("%s is out of date; regenerate it with: go run . --config.schema > %s", configSchemaFile, configSchemaFile)
	}
}

// Every shipped configuration is valid against the schema, so an editor
// pointed at it shows no false errors on the examples.
func TestShippedConfigurationsMatchTheSchema(t *testing.T) {
	schema := loadSchema(t)
	files := []string{"config.example.yaml", "config.otlp.example.yaml"}
	testdata, _ := filepath.Glob("testdata/config.*.yaml")
	files = append(files, testdata...)
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			if errs := validateAgainstSchema(schema, readYAMLDocument(t, file)); len(errs) > 0 {
				t.Fatalf("%s does not match the schema:\n%s", file, strings.Join(errs, "\n"))
			}
		})
	}
}

// So is the configuration the Helm chart ships by default.
func TestChartDefaultConfigurationMatchesTheSchema(t *testing.T) {
	raw, err := os.ReadFile(chartDir + "values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	document, ok := values.Config.Data["config.yaml"]
	if !ok {
		t.Fatal("the chart ships no config.yaml")
	}
	var doc any
	if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validateAgainstSchema(loadSchema(t), normalizeYAML(doc)); len(errs) > 0 {
		t.Fatalf("the chart's default configuration does not match the schema:\n%s", strings.Join(errs, "\n"))
	}
}

// The schema rejects what startup rejects, where a schema can tell.
func TestConfigSchemaRejectsInvalidConfigurations(t *testing.T) {
	schema := loadSchema(t)
	base := `collectors:
  - name: demo
    request:
      type: http
    transform:
      type: jq
    metrics:
      - name: value
        expression: .value
`
	tests := map[string]string{
		"unknown top-level key":   "colectors: []\n" + base,
		"no collectors":           "collectors: []\n",
		"nothing at all":          "web: {}\n",
		"empty collector_files":   "collector_files: []\n",
		"empty collector file":    "collector_files: ['']\n",
		"collector_files string":  "collector_files: collectors.d/*.yaml\n",
		"localfile without root":  strings.Replace(base, "type: http", "type: localfile", 1),
		"unknown collector key":   strings.Replace(base, "    request:", "    requst: {}\n    request:", 1),
		"missing request.type":    strings.Replace(base, "      type: http\n", "      path: /x\n", 1),
		"unknown request.type":    strings.Replace(base, "type: http", "type: gopher", 1),
		"unknown transform":       strings.Replace(base, "type: jq", "type: jsonpath", 1),
		"bad metric name":         strings.Replace(base, "name: value", "name: bad-name", 1),
		"bad metrics_prefix":      strings.Replace(base, "  - name: demo\n", "  - name: demo\n    metrics_prefix: grafana_\n", 1),
		"bad duration":            strings.Replace(base, "  - name: demo\n", "  - name: demo\n    cache:\n      ttl: five minutes\n", 1),
		"bad error policy":        strings.Replace(base, "  - name: demo\n", "  - name: demo\n    error_handling:\n      on_fetch_error: panic\n", 1),
		"label with a type":       base + "        labels:\n          - name: l\n            type: string\n            value: x\n",
		"label value and expr":    base + "        labels:\n          - name: l\n            value: x\n            expression: .l\n",
		"label without either":    base + "        labels:\n          - name: l\n",
		"label without a name":    base + "        labels:\n          - value: x\n",
		"negative limit":          strings.Replace(base, "  - name: demo\n", "  - name: demo\n    limits:\n      max_metrics: -1\n", 1),
		"unsupported library":     strings.Replace(base, "      type: jq\n", "      type: jq\n      libraries: [requests]\n", 1),
		"string for a list":       strings.Replace(base, "  - name: demo\n", "  - name: demo\n    request_list: x\n", 1),
		"boolean for a structure": strings.Replace(base, "    request:\n      type: http\n", "    request: true\n", 1),
	}
	var doc any
	if err := yaml.Unmarshal([]byte(base), &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validateAgainstSchema(schema, normalizeYAML(doc)); len(errs) > 0 {
		t.Fatalf("the base document should be valid: %v", errs)
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			var doc any
			if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
				t.Fatal(err)
			}
			if errs := validateAgainstSchema(schema, normalizeYAML(doc)); len(errs) == 0 {
				t.Fatalf("accepted:\n%s", document)
			}
		})
	}
}

// The examples point editors at the published schema.
func TestExamplesReferenceTheSchema(t *testing.T) {
	modeline := "# yaml-language-server: $schema=" + parsedSchema(t, config.SchemaJSON)["$id"].(string)
	for _, file := range []string{"config.example.yaml", "config.otlp.example.yaml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(raw), modeline+"\n") {
			t.Errorf("%s does not start with %q", file, modeline)
		}
	}
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	return loadSchemaFile(t, configSchemaFile)
}

func loadSchemaFile(t *testing.T, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func readYAMLDocument(t *testing.T, file string) any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return normalizeYAML(doc)
}

// normalizeYAML turns yaml.v3's map[string]any and ints into the shapes a
// JSON decoder produces, which is what a schema validator sees.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, value := range x {
			out[k] = normalizeYAML(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = normalizeYAML(value)
		}
		return out
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return v
}

// validateAgainstSchema checks the part of JSON Schema the generated schema
// uses: type, properties, additionalProperties, required, items, minItems,
// enum, pattern and minimum. It is enough to test the schema without a
// third-party validator.
func validateAgainstSchema(schema map[string]any, value any) []string {
	var errs []string
	var check func(schema map[string]any, value any, path string)
	check = func(schema map[string]any, value any, path string) {
		if types, ok := schema["type"]; ok && !schemaTypeMatches(types, value) {
			errs = append(errs, fmt.Sprintf("%s: %T is not %v", path, value, types))
			return
		}
		if enum, ok := schema["enum"].([]any); ok {
			found := false
			for _, allowed := range enum {
				if allowed == value {
					found = true
				}
			}
			if !found {
				errs = append(errs, fmt.Sprintf("%s: %v is not one of %v", path, value, enum))
			}
		}
		if pattern, ok := schema["pattern"].(string); ok {
			if s, ok := value.(string); ok && !regexp.MustCompile(pattern).MatchString(s) {
				errs = append(errs, fmt.Sprintf("%s: %q does not match %s", path, s, pattern))
			}
		}
		if minLength, ok := schema["minLength"].(float64); ok {
			if s, ok := value.(string); ok && float64(len(s)) < minLength {
				errs = append(errs, fmt.Sprintf("%s: %q is shorter than %v", path, s, minLength))
			}
		}
		if constant, ok := schema["const"]; ok && constant != value {
			errs = append(errs, fmt.Sprintf("%s: %v is not %v", path, value, constant))
		}
		if allOf, ok := schema["allOf"].([]any); ok {
			for _, part := range allOf {
				errs = append(errs, validateAgainstSchema(part.(map[string]any), value)...)
			}
		}
		if condition, ok := schema["if"].(map[string]any); ok && len(validateAgainstSchema(condition, value)) == 0 {
			if then, ok := schema["then"].(map[string]any); ok {
				errs = append(errs, validateAgainstSchema(then, value)...)
			}
		}
		if anyOf, ok := schema["anyOf"].([]any); ok {
			matched := false
			for _, alternative := range anyOf {
				if len(validateAgainstSchema(alternative.(map[string]any), value)) == 0 {
					matched = true
				}
			}
			if !matched {
				errs = append(errs, path+": matches none of anyOf")
			}
		}
		if oneOf, ok := schema["oneOf"].([]any); ok {
			matched := 0
			for _, alternative := range oneOf {
				if len(validateAgainstSchema(alternative.(map[string]any), value)) == 0 {
					matched++
				}
			}
			if matched != 1 {
				errs = append(errs, fmt.Sprintf("%s: matches %d of oneOf, want exactly one", path, matched))
			}
		}
		if minimum, ok := schema["minimum"].(float64); ok {
			if n, ok := value.(float64); ok && n < minimum {
				errs = append(errs, fmt.Sprintf("%s: %v is below %v", path, n, minimum))
			}
		}
		switch x := value.(type) {
		case map[string]any:
			properties, _ := schema["properties"].(map[string]any)
			for _, key := range toStrings(schema["required"]) {
				if _, ok := x[key]; !ok {
					errs = append(errs, fmt.Sprintf("%s: %s is required", path, key))
				}
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if sub, ok := properties[key].(map[string]any); ok {
					check(sub, x[key], path+"."+key)
					continue
				}
				switch additional := schema["additionalProperties"].(type) {
				case bool:
					if !additional {
						errs = append(errs, fmt.Sprintf("%s: unknown key %q", path, key))
					}
				case map[string]any:
					check(additional, x[key], path+"."+key)
				}
			}
		case []any:
			if minItems, ok := schema["minItems"].(float64); ok && float64(len(x)) < minItems {
				errs = append(errs, fmt.Sprintf("%s: %d items, want at least %v", path, len(x), minItems))
			}
			if items, ok := schema["items"].(map[string]any); ok {
				for i, item := range x {
					check(items, item, fmt.Sprintf("%s[%d]", path, i))
				}
			}
		}
	}
	check(schema, value, "")
	return errs
}

func schemaTypeMatches(types, value any) bool {
	for _, name := range toStrings(types) {
		switch name {
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "number":
			if _, ok := value.(float64); ok {
				return true
			}
		case "integer":
			if n, ok := value.(float64); ok && n == float64(int64(n)) {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		}
	}
	return value == nil
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case []string:
		return x
	}
	return nil
}

// parsedSchema is a schema as render prints it, parsed.
func parsedSchema(t *testing.T, render func() ([]byte, error)) map[string]any {
	t.Helper()
	raw, err := render()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}
