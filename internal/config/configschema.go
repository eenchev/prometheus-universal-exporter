package config

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// schemaBaseURL is where the published schemas live, for editors to fetch:
// the configs directory of the repository's main branch.
const schemaBaseURL = "https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/configs/"

// configSchemaID is where the published configuration schema lives.
const configSchemaID = schemaBaseURL + "config.schema.json"

// configSchema describes the configuration file as JSON Schema (draft
// 2020-12), for editors: with the yaml-language-server modeline at the top of
// a configuration, VS Code and other editors complete keys, show descriptions
// and flag unknown keys and bad values as you type.
//
// It is generated from the Config struct by reflection, so a key added to the
// configuration is in the schema without anyone remembering to add it, and
// configSchemaRules adds what the struct cannot say: allowed values,
// patterns, required keys and descriptions. configs/config.schema.json in the
// repository is this function's output for a default build, printed by
// --config.schema and written by make schemas; a test fails when the two
// differ.
//
// The schema describes the canonical spelling. The exporter is more lenient
// in places — it accepts request.type and error policies in any case — and
// startup validation remains the authority on what is
// valid: it also checks what a schema cannot, such as that expressions
// compile. The request types listed are the ones this binary was built with.
func configSchema() map[string]any {
	schema := schemaFor(reflect.TypeOf(model.Config{}), "", configSchemaRules())
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = configSchemaID
	allowExtensionKeys(schema)
	schema["title"] = "prometheus-universal-exporter configuration"
	return schema
}

// collectorFileSchemaID is where the published collector file schema lives.
const collectorFileSchemaID = schemaBaseURL + "collector-file.schema.json"

// collectorFileSchema describes a collector file (collectorfiles.go): a
// collectors list, required and non-empty, and no other key. The collectors
// are described exactly as in the configuration schema, from the same rules.
func collectorFileSchema() map[string]any {
	schema := schemaFor(reflect.TypeOf(collectorFile{}), "", configSchemaRules())
	delete(schema, "anyOf")
	schema["required"] = []string{collectorFileKey}
	collectors := schema["properties"].(map[string]any)[collectorFileKey].(map[string]any)
	collectors["minItems"] = 1
	schema["description"] = "A collector file of the exporter, listed under collector_files in the configuration. It holds collectors and nothing else. See docs/CONFIGURATION.md#collector-files."
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = collectorFileSchemaID
	allowExtensionKeys(schema)
	schema["title"] = "prometheus-universal-exporter collector file"
	return schema
}

// SchemaJSON renders the schema with sorted keys and a trailing newline,
// so the committed file and a fresh render compare byte for byte.
func SchemaJSON() ([]byte, error) {
	return renderSchema(configSchema())
}

// CollectorFileSchemaJSON renders the collector file schema the same way.
func CollectorFileSchemaJSON() ([]byte, error) {
	return renderSchema(collectorFileSchema())
}

// staticTargetsSchemaID is where the published static target file schema lives.
const staticTargetsSchemaID = schemaBaseURL + "static-targets.schema.json"

// staticTargetsSchema describes the static target file (--static-targets-file),
// generated from the StaticTargetFile struct as configSchema is from Config, with
// staticTargetsSchemaRules adding what the struct cannot say.
// configs/static-targets.schema.json is its output, printed by
// --static-targets-file-schema; a test fails when the two differ. As for the
// configuration, startup validation remains the authority: it also checks the
// targets against the collectors they name.
func staticTargetsSchema() map[string]any {
	schema := schemaFor(reflect.TypeOf(model.StaticTargetFile{}), "", staticTargetsSchemaRules())
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = staticTargetsSchemaID
	allowExtensionKeys(schema)
	schema["title"] = "prometheus-universal-exporter static target file"
	return schema
}

// StaticTargetsSchemaJSON renders the static target file schema the same way.
func StaticTargetsSchemaJSON() ([]byte, error) {
	return renderSchema(staticTargetsSchema())
}

// allowExtensionKeys lets a document's top level hold x- keys of its own, for
// YAML anchors (yamlerrors.go), whatever they contain.
func allowExtensionKeys(schema map[string]any) {
	schema["patternProperties"] = map[string]any{"^x-.+": map[string]any{"description": "The file's own, ignored by the exporter: a place for YAML anchors (&name) the rest of the file reuses with aliases (*name) and merge keys (<<: *name)."}}
}

func renderSchema(schema map[string]any) ([]byte, error) {
	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

var (
	durationType   = reflect.TypeOf(model.Duration(0))
	byteSizeType   = reflect.TypeOf(model.ByteSize(0))
	metricTypeType = reflect.TypeOf(model.MetricType(""))
)

// A duration is written as time.ParseDuration reads it: a sequence of decimal
// numbers with units, each number with a digit before or after its point, or
// a bare 0, and a sign before it. No key takes a negative duration, so
// durationPattern, a duration key's pattern, takes a - only before a zero,
// such as -0s, which is the zero it equals, and a + before any, as the
// exporter does: an editor then flags timeout: -5s, which the exporter
// refuses at the load. Whether a duration is long enough, the least interval
// say, is not a pattern's to tell, nor whether it is too long to be held,
// 2562048h, which the exporter refuses as no duration (documented with the
// schemas, docs/CONFIGURATION.md). signedDurationPattern is any duration, the
// negative ones too: what a block that is switched off holds, unchecked.
const (
	durationNumber        = `([0-9]+(\.[0-9]*)?|\.[0-9]+)`
	durationZero          = `(0+(\.0*)?|\.0+)`
	durationUnit          = `(ns|us|µs|μs|ms|s|m|h)`
	durationPattern       = `^(\+?(0|(` + durationNumber + durationUnit + `)+)|-(0|(` + durationZero + durationUnit + `)+))$`
	signedDurationPattern = `^[-+]?(0|(` + durationNumber + durationUnit + `)+)$`
)

// schemaFor describes t, found at path, with rules adding what the type
// cannot say (configSchemaRules, staticTargetsSchemaRules).
func schemaFor(t reflect.Type, path string, rules map[string]map[string]any) map[string]any {
	var schema map[string]any
	switch t {
	case durationType:
		schema = map[string]any{"type": "string", "pattern": durationPattern, "description": "A duration such as 500ms, 30s or 5m."}
	case byteSizeType:
		schema = map[string]any{"type": []string{"integer", "string"}, "minimum": 0, "pattern": model.ByteSizePattern, "description": "A size: a whole number of bytes, or a number with a unit such as 512KiB, 10MB or 1.5GiB. Under 2^63 bytes, which the exporter checks when the configuration loads."}
	case metricTypeType:
		schema = map[string]any{"type": "string", "enum": []string{string(model.GaugeMetricType), string(model.CounterMetricType), string(model.HistogramMetricType), string(model.SummaryMetricType), string(model.UntypedMetricType)}}
	default:
		switch t.Kind() {
		case reflect.Pointer:
			return schemaFor(t.Elem(), path, rules)
		case reflect.Struct:
			properties := map[string]any{}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
				if key == "" || key == "-" || !field.IsExported() {
					continue
				}
				properties[key] = schemaFor(field.Type, joinSchemaPath(path, key), rules)
			}
			schema = map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
		case reflect.Slice:
			schema = map[string]any{"type": "array", "items": schemaFor(t.Elem(), path+"[]", rules)}
		case reflect.Map:
			schema = map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem(), path+".*", rules)}
		case reflect.Bool:
			schema = map[string]any{"type": "boolean"}
		case reflect.Int, reflect.Int64:
			schema = map[string]any{"type": "integer", "minimum": 0}
		case reflect.Float64:
			schema = map[string]any{"type": "number"}
		case reflect.String:
			// YAML reads an unquoted 1 or true as a number or a boolean, and the
			// exporter takes it as the string it spells, so a plain string
			// accepts them too. A key with allowed values or a pattern is
			// string-only.
			schema = map[string]any{"type": []string{"string", "number", "boolean"}}
		}
	}
	if rule, ok := rules[path]; ok {
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

// enabledSwitchRule is the rule of a block that does nothing until its
// enabled key turns it on: every other key it takes requires enabled beside
// it, as the exporter does when it reads the block
// (model.decodeSwitchedBlock), so an editor flags credentials or an endpoint
// that nothing would use.
func enabledSwitchRule(t reflect.Type, description string) map[string]any {
	dependent := map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if key != "" && key != "-" && key != "enabled" {
			dependent[key] = []string{"enabled"}
		}
	}
	return map[string]any{"dependentRequired": dependent, "description": description}
}

func joinSchemaPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// configSchemaRules adds, by path, what the struct cannot say. A path is the
// chain of keys, with [] for a list item and .* for any map value.
func configSchemaRules() map[string]map[string]any {
	errorPolicy := map[string]any{"enum": []string{model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore}, "description": "fail stops the probe, log carries on and logs why, ignore carries on quietly. Defaults to fail."}
	libraries := map[string]any{"enum": model.SortedKeys(transform.PythonLibraries), "description": "A bundled Python library the script uses. Declared libraries are imported when the interpreter starts."}
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
			"required":    []string{"name", "request", "transform"},
			"description": "How to reach a kind of target and turn its response into metrics.",
		},
		"collectors[].name":                         {"pattern": `^[a-zA-Z_][a-zA-Z0-9_]*$`, "description": "Unique name, used as the collector parameter of /probe."},
		"collectors[].metrics_prefix":               {"pattern": transform.MetricsPrefixRE.String(), "description": "Joined with _ to the front of every metric the collector exports, such as grafana for grafana_statuspage_status. Letters and digits, in parts joined by single underscores."},
		"collectors[].cache":                        {"description": "The collector's response cache. See docs/CONFIGURATION.md#response-caching."},
		"collectors[].cache.ttl":                    {"description": "Answer a repeat of the same probe from memory for this long. Omit or 0s to always go to the target."},
		"collectors[].cache.stale_if_error":         {"description": "After ttl, keep a result this much longer to answer a probe whose trip to the target fails, marked by http_exporter_result_stale 1. Omit or 0s to answer the failure."},
		"collectors[].max_concurrent_probes":        {"description": "How many trips to its targets the collector makes at once; a probe over the limit is answered 503 at once, a static target waits. Omit or 0 for the default, 32."},
		"collectors[].coalesce":                     {"description": "Share one request to the target among identical probes that arrive while it is in flight. Defaults to true."},
		"collectors[].request":                      requestSchemaRule(),
		"collectors[].request.type":                 {"enum": fetch.BuiltRequestTypes(), "description": "Required. How the collector reaches its data."},
		"collectors[].request.method":               {"enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "get", "post", "put", "patch", "delete", "head"}, "description": "HTTP method. Defaults to GET."},
		"collectors[].request.body":                 {"description": "http: the request body. May contain {{param_name}} placeholders, written as |json, |number, |form, |xml or |raw: {{param_service|json}}. See docs/REQUESTS.md#in-the-body-headers-and-query."},
		"collectors[].request.path":                 {"description": "http and graphite: joined onto the target URL; it cannot hold ? or #, and query parameters go under request.query. localfile: the file, relative to request.root and joined after the target. May contain {{param_name}} or {{param_name:default}} path parameters, filled by param_<name> probe parameters."},
		"collectors[].request.root":                 {"description": "localfile, required: the absolute directory the collector may read files under. No read reaches outside it, through .. or a symbolic link. See docs/LOCALFILE.md."},
		"collectors[].request.files":                {"description": "localfile: read every file of the directory whose name matches one of these patterns (path.Match syntax, no /), each checked on its own and labelled file. Not with request.path. See docs/LOCALFILE.md#reading-a-directory."},
		"collectors[].request.max_files":            {"description": "localfile with request.files: the most files one scrape reads, in name order; the rest are skipped and logged. Defaults to 100."},
		"collectors[].request.max_total_bytes":      {"description": "localfile with request.files: the most one scrape reads across every file; a file that would go past it is refused. Defaults to 64 MiB."},
		"collectors[].request.max_age":              {"description": "localfile: refuse a file last modified longer ago than this, so a writer that has stopped fails the scrape instead of exporting its last values forever."},
		"collectors[].request.targets":              {"minItems": 1, "description": "graphite, required: the Graphite expressions asked of the render API, each sent as a target parameter, such as app.*.requests.count. May contain {{param_name}} placeholders, filled with letters, digits and _ - . : @ % + ~ only. See docs/GRAPHITE.md."},
		"collectors[].request.from":                 {"description": "graphite: the start of the render window, as Graphite writes a time: -15min, the default, -1h, or Unix seconds. The from probe parameter overrides it."},
		"collectors[].request.until":                {"description": "graphite: the end of the render window. Defaults to now. The until probe parameter overrides it."},
		"collectors[].request.retry.attempts":       {"maximum": fetch.MaxRetryAttempts, "description": "How many times a failed request is retried, from 0, the default, to 10."},
		"collectors[].request.retry.non_idempotent": {"description": "http and graphite: retry a request whose method is not idempotent, such as POST, which sending again may repeat. Unset, only GET, HEAD, OPTIONS, TRACE, PUT and DELETE requests are retried."},
		"collectors[].request.retry.codes":          {"items": map[string]any{"type": "string"}, "description": "grpc: the gRPC status codes that are retried, by name, such as [UNAVAILABLE, ABORTED]. Defaults to [UNAVAILABLE]; OK is refused."},
		"collectors[].request.rpc":                  {"pattern": `^/?([A-Za-z_][A-Za-z0-9_]*\.)*[A-Za-z_][A-Za-z0-9_]*/[A-Za-z_][A-Za-z0-9_]*$`, "description": "grpc, required: the method called, package.Service/Method, such as grpc.health.v1.Health/Check. See docs/GRPC.md."},
		"collectors[].request.message":              {"description": "grpc: the request message in the protobuf JSON mapping. Defaults to {}. May contain {{param_name}} placeholders, written as |json for a string, |number for a number, or |raw. The message probe parameter replaces it."},
		"collectors[].request.metadata":             {"propertyNames": map[string]any{"pattern": "^[0-9a-z_.-]+$"}, "description": "grpc: request metadata. Keys are lower-case; -bin keys and the ones gRPC reserves are refused. Values may contain {{param_name}} placeholders."},
		"collectors[].request.descriptors":          {"enum": []string{"reflection", "protoset", "proto"}, "description": "grpc, required but for grpc.health.v1.Health: where the message types come from. reflection asks the server's reflection service; protoset reads protoset_file; proto compiles proto_files."},
		"collectors[].request.protoset_file":        {"description": "grpc with descriptors: protoset: a FileDescriptorSet with its imports, as protoc --descriptor_set_out --include_imports or buf build -o writes it. Read again when it changes."},
		"collectors[].request.proto_files":          {"description": "grpc with descriptors: proto: the .proto files that define the service, compiled when the configuration loads and again when one changes."},
		"collectors[].request.proto_import_paths":   {"description": "grpc with descriptors: proto: the directories imports are resolved in, as protoc -I. Defaults to the directory of each file. The well-known types are built in."},
		"collectors[].response.graphite.value": {
			"enum":        model.GraphiteValues,
			"description": "graphite decoder: how a series' points become its value: last, the newest point, by default, or max, min, avg or sum.",
		},
		"collectors[].response.graphite.max_age": {"description": "graphite decoder: leave out a series whose newest point is older than this, so a series nobody writes any more is not exported with its last value."},
		"collectors[].response.graphite.invalid_lines": {
			"enum":        model.GraphiteInvalidLines,
			"description": "graphite decoder: what a carbon line that cannot be read does: fail the decode, the default, or skip, leaving it out, counted in http_exporter_decoder_lines_skipped_total and logged.",
		},
		// What checkCSVDelimiter refuses: more than one character, which a
		// schema counts as the exporter does, and the four that are one and
		// cannot separate fields.
		"collectors[].response.csv.delimiter": {
			"type":        "string",
			"maxLength":   1,
			"not":         map[string]any{"enum": []string{`"`, "\r", "\n", "\x00", "\uFFFD"}},
			"description": "csv decoder: the one character between fields, any but a double quote and a line break. Defaults to a comma. For a tab, write \"\\t\" in double quotes, where YAML reads \\t as the tab character.",
		},
		"collectors[].decoder.type": {
			"enum":        model.DecoderTypes,
			"description": "How to decode the response. Defaults to auto, which the transform or the Content-Type decides.",
		},
		"collectors[].transform.type": {
			"enum":        model.TransformTypes,
			"description": "How metrics are extracted from the decoded response. Required.",
		},
		"collectors[].transform":                         {"required": []string{"type"}},
		"collectors[].transform.libraries[]":             libraries,
		"collectors[].transform.required_libs[]":         libraries,
		"collectors[].transform.pre_script":              {"description": "Python run before the transform. It receives data and must leave its result in data."},
		"collectors[].transform.script":                  {"description": "Python for the python transform. It emits metrics with metric(...)."},
		"collectors[].error_handling.on_fetch_error":     errorPolicy,
		"collectors[].error_handling.on_decode_error":    errorPolicy,
		"collectors[].error_handling.on_transform_error": errorPolicy,
		"collectors[].metrics[].name":                    {"pattern": `^[a-zA-Z_:][a-zA-Z0-9_:]*$`, "description": "The metric name, before metrics_prefix."},
		"collectors[].metrics[].items":                   {"description": "jq, yq and css only: selects the things the metric is about, such as table rows. The expression and labels are then evaluated once per item: for jq and yq with the item as . and the whole document as $root, for css as selectors within the item."},
		"collectors[].metrics[].expression":              {"description": "Where the value comes from, in the transform's language: jq, a regex, a CSS selector, an XPath expression, a CSV column or a source metric pattern."},
		"collectors[].metrics[].error_mode": {
			"enum":        []string{model.ErrorModeFail, model.ErrorModeLog, model.ErrorModeIgnore},
			"description": "What happens when this metric cannot be extracted. Defaults to log.",
		},
		"collectors[].metrics[].required":      {"description": "When false, a missing value is skipped without an error. Defaults to true."},
		"collectors[].metrics[].labels[].name": {"pattern": `^[a-zA-Z_][a-zA-Z0-9_]*$`},
		"collectors[].metrics[].labels[]": {
			"required":    []string{"name"},
			"oneOf":       []any{map[string]any{"required": []string{"value"}}, map[string]any{"required": []string{"expression"}}},
			"description": "Set value for a static label, or expression to read it from the response.",
		},
		"collectors[].metrics[].labels[].value":      {"minLength": 1, "description": "A static label value, exported as written."},
		"collectors[].metrics[].labels[].expression": {"minLength": 1, "description": "Reads the label from the response, in the transform's language, like the metric's expression."},
		"collectors[].metrics[].labels[].truncate":   {"description": "Cut a value longer than limits.max_label_value_length to fit, ending in …, instead of failing the scrape."},
		"collectors[].metrics[].labels[].required":   {"description": "expression labels only: a series the expression gives no value, or an empty one, fails the metric under its error_mode instead of being exported without the label. Defaults to false."},
		"collectors[].request.accept_codes":          {"items": map[string]any{"type": "string"}, "description": "grpc: the gRPC status codes other than OK whose calls are answers rather than failures, by name, such as [NOT_FOUND]. Such a call is not retried; its rules see an empty object, the code as $status and the status message as $headers[\"grpc-message\"]."},
		"collectors[].request.accept_status":         {"items": map[string]any{"type": []string{"integer", "string"}, "minimum": 100, "maximum": 599, "pattern": "^[1-5][xX][xX]$|^[1-5][0-9][0-9]$"}, "description": "The HTTP statuses whose answers are decoded, such as [200, 503] or [\"2xx\", 503]; every 2xx when left out. Any other status fails the scrape in the http_status stage. An accepted status is not retried. http and graphite."},
		"collectors[].request.allowed_targets":       {"description": "Hosts, globs such as *.example.com, IP addresses and CIDR networks the collector's requests may reach: a target is allowed when its host matches by name, or every address it resolves to is in an allowed network. Checked before the request, on every redirect and on every connection. Written in ASCII, as a target's host is: an internationalised name is listed, and requested, in its xn-- form. http, graphite and grpc."},
		"collectors[].request.denied_targets":        {"description": "Hosts, globs, IP addresses and CIDR networks the collector's requests may not reach: a target matching by name, or resolving to any address in a denied network, is refused with 403. Wins over allowed_targets. Written in ASCII, as allowed_targets is."},
		"collectors[].metrics[].labels[].value_map":  {"description": "Turns the value the label's expression gives into another, such as {\"1\": running}; \"*\" maps any value it does not list, and a value mapped to \"\" leaves the label off. Without a match and without \"*\", the value is kept."},
		"collectors[].metrics[].value_map":           {"description": "Turns the text the expression gives into the value, such as {up: 1, down: 0}; \"*\" maps any value it does not list, numbers included. Without a match and without \"*\", the value is read as a number. Not for the prometheus and python transforms."},
		"collectors[].metrics[].scale":               {"description": "Multiplies the value, mapped or read as a number, such as 0.001 for milliseconds to seconds. Finite and not 0. Not for the python transform; for prometheus, plain samples only."},
		"collectors[].metrics[].time_format":         {"description": "Reads the text the expression gives as a time; the value is that time in Unix seconds. rfc3339, rfc1123, or a layout: the reference time Mon Jan 2 15:04:05 MST 2006 written as the text writes its times, such as \"2006-01-02 15:04:05\", with a year, a month and a day. scale applies after. Not with value_map, and not for the prometheus and python transforms."},
		"collectors[].metrics[].time_zone":           {"type": "string", "description": "With time_format: the zone a text that names none of its own is read in, as an IANA name such as Europe/Sofia. Defaults to UTC."},
		"collectors[].metrics[]":                     metricRuleSchemaRule(),
		"collectors[].limits.max_script_memory":      {"description": "The most memory, as address space, each of the collector's Python workers may use, the interpreter and its libraries included, such as 256MiB. A script that needs more fails with a MemoryError. At least 32MiB; 0, the default, leaves it unbounded. Enforced on Linux."},
		"collectors[].limits.script_timeout":         {"description": "How long a Python script may run. Starting the interpreter is not counted. Defaults to 100ms."},
		"otlp":                                       otlpSchemaRule(),
		"otlp.enabled":                               {"description": "Turn the export on. false keeps the block's settings without using them."},
		"web.basic_auth":                             enabledSwitchRule(reflect.TypeOf(model.ExporterBasicAuth{}), "Basic authentication on the exporter's own endpoints. Off until enabled is true; a block that sets any other key must say enabled, true or false."),
		"web.basic_auth.enabled":                     {"description": "Require the credentials. false keeps the block's settings without using them."},
		"otlp.endpoint":                              {"description": "OTLP/HTTP metrics endpoint, such as http://otel-collector:4318/v1/metrics."},
		"web.basic_auth.username_file":               {"description": "Read the username from this file instead of username, such as a mounted Secret. Read again when it changes."},
		"web.basic_auth.password_file":               {"description": "Read the password from this file instead of password, such as a mounted Secret. Read again when it changes."},
		"collectors[].name_escaping":                 {"enum": []string{transform.NameEscapingFail, transform.NameEscapingUnderscores, transform.NameEscapingValues}, "description": "What to do with a metric or label name that is not a classic Prometheus name, such as http.server.duration: fail the scrape (the default), replace what a classic name may not have with underscores, or use Prometheus's reversible values encoding (U__…). See docs/CONFIGURATION.md#utf-8-names."},
		"collectors[].response.charset":              {"description": "The encoding of the response when the target does not declare it or declares it wrongly, and of local files: a WHATWG name such as windows-1252, iso-8859-2, windows-1251 or shift_jis. See docs/CONFIGURATION.md#character-encodings."},
		"otlp.max_pending_points":                    {"description": "The most data points kept waiting for export while the endpoint fails; past it the oldest are dropped and counted. Defaults to 100000."},
		"otlp.unready_after_failures":                {"description": "Answer /ready with 503 after this many failed exports in a row, until one gets through. 0, the default, never does: an exporter whose exports fail still answers probes."},
		"otlp.probe_attributes":                      {"description": "Add collector and target attributes to the points a probe queues, so probes of different targets or collectors answering the same series are exported apart. Off, the default, the later probe's point replaces the earlier's."},
		"collectors[].request.tls.server_name":       {"description": "The name the target's certificate is checked against, and sent as SNI, when the target is addressed by something else, such as an IP address. Unset, the target's host."},
		"otlp.compression":                           {"enum": []string{model.OTLPCompressionGzip, model.OTLPCompressionNone}, "description": "Compression of the export requests. Defaults to gzip."},
		// A disabled block may hold any duration, so the two keys take a
		// signed one, and otlpSchemaRule refuses a negative one of a block
		// that is switched on.
		"otlp.timeout": {"pattern": signedDurationPattern, "description": "How long one export attempt may take. Defaults to 5s. Also bounds the last export at shutdown."},
		// The least interval is checked when the configuration loads
		// (validateOTLP): a duration is text to a schema, which cannot tell
		// 500ms from 5s, and a disabled block may hold any.
		"otlp.interval": {"pattern": signedDurationPattern, "description": "How often the points waiting are exported. At least 1s, which the exporter checks when the configuration loads; defaults to 30s."},
		// What a followed redirect carries (fetch/redirecttrust.go). An entry
		// of the list is what parseTrustedHost takes, as far as a pattern can
		// say it: "*", a name or a glob of one, with a final dot or without,
		// that is digits and dots alone or has a letter, a hyphen or an
		// underscore in it, or what is written as an IPv6 address is; in
		// brackets or not, with spaces around it. An address of that shape
		// that is none is the exporter's to refuse.
		"collectors[].request.follow_redirects":         {"description": "http and graphite: follow redirect statuses instead of answering with the redirect itself. A redirect that leaves the origin the request was made to — its scheme, host and port — is sent only Accept, Accept-Language and User-Agent, with the values the first request had, so a token must never be put in those three, and never the request body, and is refused over https when the collector has a tls.cert_file, unless its host is in redirect_trusted_hosts. Defaults to false. See docs/REQUESTS.md#what-a-followed-redirect-carries."},
		"collectors[].request.redirect_trusted_hosts":   {"description": "http and graphite: the hosts, besides the origin the request is made to, that a followed redirect may carry the collector's headers, credentials and request body to, and present its TLS client certificate to: host names, globs such as *.example.com, whose star matches dots too, and single IP addresses, each trusted on every port; \"*\" trusts every host. Matched against the host the redirect names as it is written there, which is never looked up: a final dot is part of the name, and an internationalised name is listed, and matched, in its xn-- form. No probe parameter sets it."},
		"collectors[].request.redirect_trusted_hosts[]": {"type": "string", "pattern": fetch.TrustedHostPattern},
		// An entry of allowed_targets or denied_targets is ASCII, which a
		// pattern can say; that it is a name, a glob, an address or a network
		// is the exporter's to check (fetch/targetpolicy.go). The pattern is
		// of what an entry may not hold, under not, so that an entry YAML
		// reads as a number is still taken, as the exporter takes it.
		"collectors[].request.allowed_targets[]": {"not": map[string]any{"type": "string", "pattern": fetch.OutsideASCIIPattern}},
		"collectors[].request.denied_targets[]":  {"not": map[string]any{"type": "string", "pattern": fetch.OutsideASCIIPattern}},
	}
}

// otlpSchemaRule is the rule of the otlp block: one that enabled switches on
// (enabledSwitchRule), whose timeout and interval, once it is on, are not
// negative, as the exporter checks them then (validateOTLP). Switched off,
// the block is kept unchecked by both, and the keys' own pattern takes any
// duration.
func otlpSchemaRule() map[string]any {
	rule := enabledSwitchRule(reflect.TypeOf(model.OTLPConfig{}), "OTLP export of probe results, self-metrics and static targets with export_via_otlp. Off until enabled is true; a block that sets any other key must say enabled, true or false. See docs/OTLP.md.")
	notNegative := map[string]any{"pattern": durationPattern}
	rule["if"] = map[string]any{"properties": map[string]any{"enabled": map[string]any{"const": true}}, "required": []string{"enabled"}}
	rule["then"] = map[string]any{"properties": map[string]any{"timeout": notNegative, "interval": notNegative}}
	return rule
}

// metricRuleSchemaRule is what the exporter refuses of a metric rule's
// time_format and time_zone that a schema can tell (transform.CheckMetricRule):
// time_zone without time_format, and time_format beside value_map, each of
// which turns the text into the value. Either key written "" is the key left
// out, to the exporter and so here, as an optional key of free text is
// throughout the configuration: a time_zone of "" asks for no time_format,
// and a time_format of "" is none, beside time_zone and beside value_map.
// dependentRequired and required alone go by the key being written, whatever
// its value, and so took time_zone: "" for a zone and time_format: "" for a
// format.
func metricRuleSchemaRule() map[string]any {
	set := func(key string) map[string]any {
		return map[string]any{"required": []string{key}, "properties": map[string]any{key: map[string]any{"not": map[string]any{"const": ""}}}}
	}
	timeFormatBesideValueMap := set("time_format")
	timeFormatBesideValueMap["required"] = []string{"time_format", "value_map"}
	return map[string]any{
		"if":   set("time_zone"),
		"then": set("time_format"),
		"not":  timeFormatBesideValueMap,
	}
}

// requestSchemaRule requires type, and for each built request type with
// required keys of its own, those keys when the type is chosen.
func requestSchemaRule() map[string]any {
	rule := map[string]any{"required": []string{"type"}, "description": "How the collector reaches its data. See docs/REQUESTS.md, docs/LOCALFILE.md for localfile, docs/GRAPHITE.md for graphite and docs/GRPC.md for grpc."}
	var conditions []any
	for _, name := range fetch.BuiltRequestTypes() {
		required := map[string]string{fetch.RequestTypeLocalFile: "root", fetch.RequestTypeGraphite: "targets", fetch.RequestTypeGRPC: "rpc"}[name]
		if required != "" {
			conditions = append(conditions, map[string]any{
				"if":   map[string]any{"properties": map[string]any{"type": map[string]any{"const": name}}, "required": []string{"type"}},
				"then": map[string]any{"required": []string{required}},
			})
		}
	}
	if len(conditions) > 0 {
		rule["allOf"] = conditions
	}
	return rule
}

// staticTargetsSchemaRules adds, by path, what the StaticTargetFile struct cannot say.
func staticTargetsSchemaRules() map[string]map[string]any {
	return map[string]map[string]any{
		"": {
			"required":    []string{"interval", "targets"},
			"description": "Static targets of the exporter, scraped by the exporter itself on their intervals and served together on the static targets endpoint (--web.static-targets-path); a target with export_via_otlp is also delivered over OTLP. Passed with --static-targets-file. See docs/STATIC-TARGETS.md.",
		},
		"interval":            {"description": "How often a target that sets no interval is scraped, such as 1m. Required; at least 1s."},
		"concurrency":         {"minimum": 0, "description": "How many targets are scraped at once. A target due while all are busy waits for a slot within its interval, and is skipped if none frees. Omit or 0 for the default, 8."},
		"targets":             {"minItems": 1, "description": "The targets. Each is scraped on its own interval with one collector of the configuration."},
		"targets[]":           {"required": []string{"collector"}, "description": "One target: a collector of the configuration, the address it reads, and what this target overrides."},
		"targets[].name":      {"pattern": targetNameRE.String(), "description": "Unique name, in logs and the static_target label. Defaults to <collector>_<index>."},
		"targets[].collector": {"description": "The collector of the configuration that scrapes this target."},
		"targets[].target":    {"description": "What the collector reads: a URL for an http collector, the Graphite server's URL for a graphite one, host:port or a grpc:// or grpcs:// URL of it for a grpc one, a file under request.root for a localfile one."},
		"targets[].interval":  {"description": "How often this target is scraped, whatever Prometheus scrapes the endpoint on and otlp.interval exports on. At least 1s, and no shorter than request.timeout; defaults to the file's interval."},
		"targets[].params": {
			"propertyNames": map[string]any{"pattern": fetch.PathParamName.String()},
			"description":   "Values of the collector's {{param_<name>}} placeholders, as a probe's param_<name> parameters give them. Each must be used by a placeholder.",
		},
		"targets[].labels": {
			"propertyNames": map[string]any{"not": map[string]any{"enum": []string{StaticTargetLabel, "job", "instance"}}},
			"description":   "Added to every metric the target produces, without overwriting a label the collector extracted. static_target is set by the endpoint, and job and instance by Prometheus when it scrapes the endpoint, so none of the three can be used.",
		},
		"targets[].export_via_otlp":              {"description": "Also deliver this target's results over OTLP, on otlp.interval, besides serving them on the static targets endpoint. Needs otlp.enabled. Defaults to false."},
		"targets[].request":                      {"description": "Overrides of the collector's request for this target, as the /probe parameters override it for a probe."},
		"targets[].request.method":               {"enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}},
		"targets[].request.path":                 {"description": "Replaces the collector's request.path. It cannot hold {{param_...}} placeholders."},
		"targets[].request.timeout":              {"description": "How long a scrape of this target may take. At most the target's interval."},
		"targets[].request.targets":              {"description": "graphite: replaces the collector's request.targets for this target. It cannot hold {{param_...}} placeholders."},
		"targets[].request.from":                 {"description": "graphite: replaces the collector's request.from for this target."},
		"targets[].request.until":                {"description": "graphite: replaces the collector's request.until for this target."},
		"targets[].request.message":              {"description": "grpc: replaces the collector's request.message for this target. It cannot hold {{param_...}} placeholders."},
		"targets[].request.metadata":             {"propertyNames": map[string]any{"pattern": "^[0-9a-z_.-]+$"}, "description": "grpc: metadata sent besides the collector's; a key of both takes this value. It cannot hold {{param_...}} placeholders."},
		"targets[].request.accept_status":        {"items": map[string]any{"type": []string{"integer", "string"}, "minimum": 100, "maximum": 599, "pattern": "^[1-5][xX][xX]$|^[1-5][0-9][0-9]$"}, "description": "http and graphite: replaces the collector's request.accept_status for this target."},
		"targets[].request.accept_codes":         {"items": map[string]any{"type": "string"}, "description": "grpc: replaces the collector's request.accept_codes for this target."},
		"targets[].request.retry.attempts":       {"maximum": fetch.MaxRetryAttempts, "description": "Replaces the collector's request.retry.attempts for this target: from 0 to 10."},
		"targets[].request.retry.codes":          {"items": map[string]any{"type": "string"}, "description": "grpc: replaces the collector's request.retry.codes for this target."},
		"targets[].request.retry":                {"description": "Each key set replaces the collector's request.retry key for this target; each left out keeps the collector's."},
		"targets[].request.retry.non_idempotent": {"description": "Retry a request whose method is not idempotent, such as POST, which sending again may repeat."},
		"targets[].otlp":                         {"description": "The OTLP resource this target's metrics are exported under, over the exporter-wide otlp settings. Only with export_via_otlp: true."},
	}
}
