package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// configSchemaID is where the published schema lives, for editors to fetch.
const configSchemaID = "https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/config.schema.json"

// configSchema describes the configuration file as JSON Schema (draft
// 2020-12), for editors: with the yaml-language-server modeline at the top of
// a configuration, VS Code and other editors complete keys, show descriptions
// and flag unknown keys and bad values as you type.
//
// It is generated from the Config struct by reflection, so a key added to the
// configuration is in the schema without anyone remembering to add it, and
// configSchemaRules adds what the struct cannot say: allowed values,
// patterns, required keys and descriptions. config.schema.json in the
// repository is this function's output for a default build, printed by
// --config.schema; a test fails when the two differ.
//
// The schema describes the canonical spelling. The exporter is more lenient
// in places — it accepts request.type in any case, and the deprecated "warn"
// error policy — and startup validation remains the authority on what is
// valid: it also checks what a schema cannot, such as that expressions
// compile. The request types listed are the ones this binary was built with.
func configSchema() map[string]any {
	schema := schemaFor(reflect.TypeOf(Config{}), "")
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = configSchemaID
	schema["title"] = "prometheus-universal-exporter configuration"
	return schema
}

// collectorFileSchemaID is where the published collector file schema lives.
const collectorFileSchemaID = "https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/collector-file.schema.json"

// collectorFileSchema describes a collector file (collectorfiles.go): a
// collectors list, required and non-empty, and no other key. The collectors
// are described exactly as in the configuration schema, from the same rules.
func collectorFileSchema() map[string]any {
	schema := schemaFor(reflect.TypeOf(collectorFile{}), "")
	delete(schema, "anyOf")
	schema["required"] = []string{collectorFileKey}
	collectors := schema["properties"].(map[string]any)[collectorFileKey].(map[string]any)
	collectors["minItems"] = 1
	schema["description"] = "A collector file of the exporter, listed under collector_files in the configuration. It holds collectors and nothing else. See docs/CONFIGURATION.md#collector-files."
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = collectorFileSchemaID
	schema["title"] = "prometheus-universal-exporter collector file"
	return schema
}

// configSchemaJSON renders the schema with sorted keys and a trailing newline,
// so the committed file and a fresh render compare byte for byte.
func configSchemaJSON() ([]byte, error) {
	return renderSchema(configSchema())
}

// collectorFileSchemaJSON renders the collector file schema the same way.
func collectorFileSchemaJSON() ([]byte, error) {
	return renderSchema(collectorFileSchema())
}

func renderSchema(schema map[string]any) ([]byte, error) {
	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

var (
	durationType   = reflect.TypeOf(Duration(0))
	metricTypeType = reflect.TypeOf(MetricType(""))
)

// durationPattern is what time.ParseDuration accepts: a sequence of decimal
// numbers with units, optionally signed, or a bare 0.
const durationPattern = `^[-+]?(0|([0-9]*(\.[0-9]*)?(ns|us|µs|μs|ms|s|m|h))+)$`

func schemaFor(t reflect.Type, path string) map[string]any {
	var schema map[string]any
	switch t {
	case durationType:
		schema = map[string]any{"type": "string", "pattern": durationPattern, "description": "A duration such as 500ms, 30s or 5m."}
	case metricTypeType:
		schema = map[string]any{"type": "string", "enum": []string{string(GaugeMetricType), string(CounterMetricType), string(HistogramMetricType), string(SummaryMetricType), string(UntypedMetricType)}}
	default:
		switch t.Kind() {
		case reflect.Pointer:
			return schemaFor(t.Elem(), path)
		case reflect.Struct:
			properties := map[string]any{}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
				if key == "" || key == "-" || !field.IsExported() {
					continue
				}
				properties[key] = schemaFor(field.Type, joinSchemaPath(path, key))
			}
			schema = map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
		case reflect.Slice:
			schema = map[string]any{"type": "array", "items": schemaFor(t.Elem(), path+"[]")}
		case reflect.Map:
			schema = map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem(), path+".*")}
		case reflect.Bool:
			schema = map[string]any{"type": "boolean"}
		case reflect.Int, reflect.Int64:
			schema = map[string]any{"type": "integer", "minimum": 0}
		case reflect.String:
			// YAML reads an unquoted 1 or true as a number or a boolean, and the
			// exporter takes it as the string it spells, so a plain string
			// accepts them too. A key with allowed values or a pattern is
			// string-only.
			schema = map[string]any{"type": []string{"string", "number", "boolean"}}
		}
	}
	if rule, ok := configSchemaRules()[path]; ok {
		for key, value := range rule {
			schema[key] = value
		}
		if _, restricted := rule["enum"]; restricted {
			schema["type"] = "string"
		}
		if _, restricted := rule["pattern"]; restricted {
			schema["type"] = "string"
		}
	}
	return schema
}

func joinSchemaPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// configSchemaRules adds, by path, what the struct cannot say. A path is the
// chain of keys, with [] for a list item and .* for any map value.
func configSchemaRules() map[string]map[string]any {
	errorPolicy := map[string]any{"enum": []string{ErrorPolicyFail, ErrorPolicyLog, ErrorPolicyIgnore, errorPolicyWarn}, "description": "fail stops the probe, log carries on and logs why, ignore carries on quietly. Defaults to fail. warn is a deprecated spelling of log."}
	libraries := map[string]any{"enum": sortedCopy(keysOf(pythonLibraries)), "description": "A bundled Python library the script uses. Declared libraries are imported when the interpreter starts."}
	return map[string]map[string]any{
		"": {
			"anyOf": []any{
				map[string]any{"required": []string{"collectors"}, "properties": map[string]any{"collectors": map[string]any{"minItems": 1}}},
				map[string]any{"required": []string{"collector_files"}, "properties": map[string]any{"collector_files": map[string]any{"minItems": 1}}},
			},
			"description": "The exporter configuration. See docs/CONFIGURATION.md. Collectors are defined under collectors, in the files collector_files lists, or both.",
		},
		"collectors":        {"description": "The collectors. A probe names one with its collector parameter. A name must be unique across this list and every collector file."},
		"collector_files":   {"description": "Further files of collectors, as paths or glob patterns such as collectors.d/*.yaml, relative to this file. A collector file holds a collectors list and nothing else. See docs/CONFIGURATION.md#collector-files."},
		"collector_files[]": {"type": "string", "minLength": 1},
		"collectors[]": {
			"required":    []string{"name", "request"},
			"description": "How to reach a kind of target and turn its response into metrics.",
		},
		"collectors[].name":            {"pattern": `^[a-zA-Z_][a-zA-Z0-9_]*$`, "description": "Unique name, used as the collector parameter of /probe."},
		"collectors[].metrics_prefix":  {"pattern": metricsPrefixRE.String(), "description": "Joined with _ to the front of every metric the collector exports, such as grafana for grafana_statuspage_status. Letters and digits, in parts joined by single underscores."},
		"collectors[].cache":           {"description": "Answer a repeat of the same probe from memory for this long. Omit or 0s to disable."},
		"collectors[].coalesce":        {"description": "Share one request to the target among identical probes that arrive while it is in flight. Defaults to true."},
		"collectors[].request":         requestSchemaRule(),
		"collectors[].request.type":    {"enum": builtRequestTypes(), "description": "Required. How the collector reaches its data."},
		"collectors[].request.method":  {"enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "get", "post", "put", "patch", "delete", "head"}, "description": "HTTP method. Defaults to GET."},
		"collectors[].request.path":    {"description": "http: joined onto the target URL. localfile: the file, relative to request.root and joined after the target. May contain {{param_name}} or {{param_name:default}} path parameters, filled by param_<name> probe parameters."},
		"collectors[].request.root":    {"description": "localfile, required: the absolute directory the collector may read files under. No read reaches outside it, through .. or a symbolic link. See docs/LOCALFILE.md."},
		"collectors[].request.max_age": {"description": "localfile: refuse a file last modified longer ago than this, so a writer that has stopped fails the scrape instead of exporting its last values forever."},
		"collectors[].response.format": {
			"enum":        []string{"auto", "json", "yaml", "xml", "csv", "html", "prometheus", "text"},
			"description": "How to decode the response. Defaults to auto, which the transform or the Content-Type decides.",
		},
		"collectors[].decoder.type": {"enum": []string{"auto", "json", "yaml", "xml", "csv", "html", "prometheus", "text"}},
		"collectors[].transform.type": {
			"enum":        []string{"none", "jq", "yq", "xpath", "css", "csv", "regex", "python", "prometheus"},
			"description": "How metrics are extracted from the decoded response.",
		},
		"collectors[].transform.libraries[]":             libraries,
		"collectors[].transform.required_libs[]":         libraries,
		"collectors[].transform.pre_script":              {"description": "Python run before the transform. It receives data and must leave its result in data."},
		"collectors[].transform.script":                  {"description": "Python for the python transform. It emits metrics with metric(...)."},
		"collectors[].error_handling.on_http_error":      errorPolicy,
		"collectors[].error_handling.on_decode_error":    errorPolicy,
		"collectors[].error_handling.on_transform_error": errorPolicy,
		"collectors[].metrics[].name":                    {"pattern": `^[a-zA-Z_:][a-zA-Z0-9_:]*$`, "description": "The metric name, before metrics_prefix."},
		"collectors[].metrics[].items":                   {"description": "jq and yq only: selects the things the metric is about. The expression and labels are then evaluated once per item, with the item as . and the whole document as $root."},
		"collectors[].metrics[].expression":              {"description": "Where the value comes from, in the transform's language: jq, a regex, a CSS selector, an XPath expression, a CSV column or a source metric pattern."},
		"collectors[].metrics[].error_mode": {
			"enum":        []string{ErrorModeFail, ErrorModeLog, ErrorModeIgnore, errorPolicyWarn},
			"description": "What happens when this metric cannot be extracted. Defaults to log. warn is a deprecated spelling of log.",
		},
		"collectors[].metrics[].required":          {"description": "When false, a missing value is skipped without an error. Defaults to true."},
		"collectors[].metrics[].labels[].name":     {"pattern": `^[a-zA-Z_][a-zA-Z0-9_]*$`},
		"collectors[].metrics[].labels[].type":     {"enum": []string{"string", "expression"}, "description": "string for a literal value, expression for a value from the response."},
		"collectors[].metrics[].labels[].truncate": {"description": "Cut a value longer than limits.max_label_value_length to fit, ending in …, instead of failing the scrape."},
		"collectors[].limits.script_timeout":       {"description": "How long a Python script may run. Starting the interpreter is not counted. Defaults to 100ms."},
		"otlp.endpoint":                            {"description": "OTLP/HTTP metrics endpoint, such as http://otel-collector:4318/v1/metrics."},
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

// requestSchemaRule requires type, and for each built request type with
// required keys of its own, those keys when the type is chosen.
func requestSchemaRule() map[string]any {
	rule := map[string]any{"required": []string{"type"}, "description": "How the collector reaches its data. See docs/REQUESTS.md, and docs/LOCALFILE.md for localfile."}
	var conditions []any
	for _, name := range builtRequestTypes() {
		if name == RequestTypeLocalFile {
			conditions = append(conditions, map[string]any{
				"if":   map[string]any{"properties": map[string]any{"type": map[string]any{"const": name}}, "required": []string{"type"}},
				"then": map[string]any{"required": []string{"root"}},
			})
		}
	}
	if len(conditions) > 0 {
		rule["allOf"] = conditions
	}
	return rule
}
