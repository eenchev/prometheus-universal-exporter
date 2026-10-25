package config

import (
	"encoding/json"
	"reflect"
	"strconv"
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

// The target allocator (internal/allocator) reads files of its own beside
// the exporter's, a static target file with discovered_targets and a probe
// targets file, and describes them with what follows, so its schemas are
// made, rendered and published as the exporter's are.

// SchemaID is where the published schema of file, in configs, lives.
func SchemaID(file string) string {
	return schemaBaseURL + file
}

// SchemaOf describes t as the exporter's schemas describe their types, with
// rules adding, by path, what the type cannot say.
func SchemaOf(t reflect.Type, rules map[string]map[string]any) map[string]any {
	return schemaFor(t, "", rules)
}

// StaticTargetsSchema is the static target file's schema, made anew at each
// call, so the caller may change it.
func StaticTargetsSchema() map[string]any {
	return staticTargetsSchema()
}

// AllowExtensionKeys lets a document's top level hold x- keys of its own,
// as every file of the exporter's may.
func AllowExtensionKeys(schema map[string]any) {
	allowExtensionKeys(schema)
}

// RenderSchema renders a schema as the exporter's are rendered.
func RenderSchema(schema map[string]any) ([]byte, error) {
	return renderSchema(schema)
}

// OptionalPattern is the pattern of a key that may also be written "", the
// key left out (optionalPattern).
func OptionalPattern(pattern string) string {
	return optionalPattern(pattern)
}

// OptionalEnum is the values of a key that may also be written "", the key
// left out (optionalEnum).
func OptionalEnum(values []string) []string {
	return optionalEnum(values)
}

// Caseless is a regular expression of text matched without regard to case,
// read alike by Go and JavaScript (caseless).
func Caseless(text string) string {
	return caseless(text)
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
//
// A pattern is of text, and YAML reads an unquoted 0 as a number. The
// exporter reads a duration from the scalar as it is written, whatever YAML
// takes it for, so 0, +0 and -0 are the zero they spell and every other
// number, lacking a unit, is no duration: timeout: 30 is refused, not read
// as 30 of anything. durationSchema therefore takes, beside the text, the
// whole number 0 and no other number. What it cannot tell is how that zero
// was written: 0.0 and 00 are the number 0 to a schema and no duration to
// the exporter.
const (
	durationNumber        = `([0-9]+(\.[0-9]*)?|\.[0-9]+)`
	durationZero          = `(0+(\.0*)?|\.0+)`
	durationUnit          = `(ns|us|µs|μs|ms|s|m|h)`
	durationPattern       = `^(\+?(0|(` + durationNumber + durationUnit + `)+)|-(0|(` + durationZero + durationUnit + `)+))$`
	signedDurationPattern = `^[-+]?(0|(` + durationNumber + durationUnit + `)+)$`
)

// durationSchema is the one description every duration key has: text that
// durationPattern takes, or the number 0. A rule of a key may give it another
// pattern, as the otlp block's does, and leaves the number as it is.
func durationSchema() map[string]any {
	return map[string]any{"type": []string{"string", "integer"}, "pattern": durationPattern, "minimum": 0, "maximum": 0, "description": "A duration such as 500ms, 30s or 5m. Zero may be written 0, the one number that needs no unit."}
}

// An optional key that takes one of a set of values, or text of a pattern,
// is to the exporter the key left out when it is written "": name_escaping:
// "" is the default, as name_escaping left out is. The schemas say the same
// in one form each, so that an editor does not flag what the exporter takes:
// optionalEnum is the values and "", and optionalPattern the pattern or
// nothing at all. A key the exporter requires, such as transform.type, has
// neither: written "", it is as missing as left out, to both.
// optionalEnum is fetch.SchemaOptionalEnum.
func optionalEnum(values []string) []string { return fetch.SchemaOptionalEnum(values) }

// optionalPattern is fetch.SchemaOptionalPattern.
func optionalPattern(pattern string) string { return fetch.SchemaOptionalPattern(pattern) }

// writtenKey is the rule that an object has the key and that it is not ""
// (fetch.SchemaWrittenKey).
func writtenKey(key string) map[string]any { return fetch.SchemaWrittenKey(key) }

// surroundingBlank is the class of the characters strings.TrimSpace takes
// off a text (fetch.SchemaSurroundingBlank).
const surroundingBlank = fetch.SchemaSurroundingBlank

// valueMapKeys is what the exporter refuses of a value_map's keys, a metric
// rule's and a label's alike (transform.checkValueRules): the empty key, and
// one with blanks around it, since a value is looked up without them and
// would never be found. "*" is a key like any other to a schema. A key
// written twice, and one YAML reads as no key at all (null, ~), are not a
// schema's to see: a validator is handed the mapping YAML made of them.
func valueMapKeys() map[string]any {
	return map[string]any{"minLength": 1, "pattern": "^([^" + surroundingBlank + "]([\\s\\S]*[^" + surroundingBlank + "])?)?$"}
}

// onlyBlanks is text of one blank or more and nothing else: what a label's
// expression may not be (validateMetricRule). It goes under not, so that the
// key keeps the types it takes, an unquoted 1 for a capture group among
// them, and "" stays the key left out (fetch.SchemaOnlyBlanks).
func onlyBlanks() map[string]any { return fetch.SchemaOnlyBlanks() }

// blanksOrPlaceholder is onlyBlanks, or text with {{ in it: what a static
// target's account and credential files may not be, a placeholder being
// used as it is written there (fetch's checkSQLTargetRequest and
// checkSSHTargetRequest).
// placeholder is the rule of a value holding a {{param_...}} placeholder,
// for a key a static target writes out in full.
func placeholder() map[string]any {
	return map[string]any{"type": "string", "pattern": `\{\{`}
}

func blanksOrPlaceholder() map[string]any {
	return map[string]any{"anyOf": []any{onlyBlanks(), placeholder()}}
}

// headerNameChars are the characters of a header name, an HTTP token
// (RFC 9110), as a regular expression class; httpguts.ValidHeaderFieldName,
// which the exporter asks, takes these and no others.
const headerNameChars = "!#$%&'*+.^_`|~0-9A-Za-z-"

// caseless is a regular expression of text matched without regard to case
// (fetch.SchemaCaseless).
func caseless(text string) string { return fetch.SchemaCaseless(text) }

// headerNames is what the exporter takes as a name of request.headers, a
// collector's or a static target's, and of otlp.headers
// (fetch.checkHeaderNames): an HTTP token, and not Accept-Encoding, in any
// case, which the exporter sets itself. Two names that are one header in
// different case are the exporter's alone to refuse: a schema sees each name
// on its own.
func headerNames() map[string]any {
	return map[string]any{
		"pattern": "^[" + headerNameChars + "]+$",
		"not":     map[string]any{"pattern": "^" + caseless("Accept-Encoding") + "$"},
	}
}

// forwardHeaderEntry is what the exporter takes as an entry of
// request.forward_headers (fetch.checkForwardHeaders): a header name, with
// blanks around it or not, as a probe's forwarding trims them, that is none
// of the headers a probe never forwards, in any case. It is text, as every
// key with a pattern is: an entry YAML reads as a number or a boolean is
// refused by both, the exporter saying to quote it.
func forwardHeaderEntry() map[string]any {
	never := []string{caseless(fetch.UnforwardablePrefix) + "[" + headerNameChars + "]*"}
	for _, name := range fetch.UnforwardableHeaderNames() {
		never = append(never, caseless(name))
	}
	blanks := "[" + surroundingBlank + "]*"
	return map[string]any{
		"pattern": "^" + blanks + "[" + headerNameChars + "]+" + blanks + "$",
		"not":     map[string]any{"type": "string", "pattern": "^" + blanks + "(" + strings.Join(never, "|") + ")" + blanks + "$"},
	}
}

// queryNames is what the exporter takes as a name of request.query
// (fetch.checkQueryNames): one of at least a character, none of them a
// control character. Any other is sent escaped.
func queryNames() map[string]any {
	return map[string]any{"minLength": 1, "pattern": `^[^\x00-\x1F\x7F]*$`}
}

// leastResponseRule is the rule of request.max_response_bytes and
// limits.max_response_bytes: beside what every size is held to, one written
// as a number is 0, the default, or at least config.MinResponseBytes, as the
// exporter has it, which, as for limits.max_output_bytes (leastOutputRule),
// the exporter alone checks of a size written as text.
func leastResponseRule(description string) map[string]any {
	return map[string]any{
		"anyOf":       []any{map[string]any{"const": 0}, map[string]any{"minimum": MinResponseBytes}},
		"description": description + " A size: a whole number of bytes, or a number with a unit such as 512KiB, 10MB or 1.5GiB, under 2^63 bytes. At least 1KiB, since a limit under it is one whose unit was left out; 0, like the key left out, is the default. The range, and the least of a size written as text, in quotes or with a unit, are checked by the exporter when the configuration loads.",
	}
}

// filterEntry is what an entry of transform.include or transform.exclude may
// not be: the empty string, and blanks alone. The key keeps the types it
// takes, as a label's expression does under onlyBlanks: an unquoted 1 is the
// pattern it spells.
func filterEntry() map[string]any {
	return map[string]any{"minLength": 1, "not": onlyBlanks()}
}

// schemaFor describes t, found at path, with rules adding what the type
// cannot say (configSchemaRules, staticTargetsSchemaRules).
func schemaFor(t reflect.Type, path string, rules map[string]map[string]any) map[string]any {
	var schema map[string]any
	switch t {
	case durationType:
		schema = durationSchema()
	case byteSizeType:
		schema = map[string]any{"type": []string{"integer", "string"}, "minimum": 0, "pattern": model.ByteSizePattern, "description": "A size: a whole number of bytes, or a number with a unit such as 512KiB, 10MB or 1.5GiB. Under 2^63 bytes, which the exporter checks when the configuration loads."}
	case metricTypeType:
		schema = map[string]any{"type": "string", "enum": optionalEnum([]string{string(model.GaugeMetricType), string(model.CounterMetricType), string(model.HistogramMetricType), string(model.SummaryMetricType), string(model.UntypedMetricType)})}
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
			if t == requestConfigType {
				ownRequestKeys(properties, path, rules)
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
			// string-only, and the load refuses a number or a boolean there
			// as the schema does (schemaKinds, yamlvalues.go).
			schema = map[string]any{"type": []string{"string", "number", "boolean"}}
		}
	}
	if rule, ok := rules[path]; ok {
		// A rule's nil takes away what the type said: the least of a whole
		// number that a rule of the block around it sets instead.
		for key, value := range rule {
			if value == nil {
				delete(schema, key)
				continue
			}
			schema[key] = value
		}
		if _, restricted := rule["enum"]; restricted {
			schema["type"] = "string"
		}
		// A duration keeps its own types under another pattern: the number
		// 0 is one whatever the text may be.
		if _, restricted := rule["pattern"]; restricted && t != durationType {
			schema["type"] = "string"
		}
	}
	return schema
}

// leastOutputRule is the rule of limits.max_output_bytes: beside what every
// size is held to, one written as a number is 0, the default, or at least the
// answer of a Python script that emits no metric
// (transform.MinPythonOutputBytes), as the exporter has it. minimum is of numbers alone, so text passes the
// second alternative and stays held by the pattern of a size: how many bytes
// a size written in quotes or with a unit comes to, 20B or 0.01KiB, no
// pattern short of a wall of one can tell, and that such a size is too small
// is the exporter's alone to say.
func leastOutputRule() map[string]any {
	return map[string]any{
		"anyOf":       []any{map[string]any{"const": 0}, map[string]any{"minimum": transform.MinPythonOutputBytes}},
		"description": "The most bytes one answer of the collector's Python worker may be, a script's metrics or a pre-script's data. A size: a whole number of bytes, or a number with a unit such as 512KiB, 10MB or 1.5GiB, under 2^63 bytes. At least " + strconv.Itoa(transform.MinPythonOutputBytes) + " bytes, the answer of a script that emits no metric; 0, like the key left out, is the default, 1MiB. The range, and the least of a size written as text, in quotes or with a unit, are checked by the exporter when the configuration loads.",
	}
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
	errorPolicy := map[string]any{"enum": optionalEnum([]string{model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore}), "description": "fail stops the probe, log carries on and logs why, ignore carries on quietly. Defaults to fail."}
	libraries := map[string]any{"enum": model.SortedKeys(transform.PythonLibraries), "description": "A bundled Python library the script uses. Declared libraries are imported when the interpreter starts."}
	return withLoginRules("collectors[].request", "http and graphite: log in before the request and send the token the login gives, in Authorization as a bearer token unless header says otherwise. type json posts a JSON object of fields and field_files to url and reads the token from the answer; type oauth2 asks the token endpoint url with the OAuth 2.0 client credentials grant. No probe parameter sets or changes it. See docs/AUTHENTICATION.md#logging-in-to-a-target.", map[string]map[string]any{
		"": {
			"anyOf": []any{
				map[string]any{"required": []string{"collectors"}, "properties": map[string]any{"collectors": map[string]any{"minItems": 1}}},
				map[string]any{"required": []string{"collector_files"}, "properties": map[string]any{"collector_files": map[string]any{"minItems": 1}}},
			},
			"description": "The exporter configuration. See docs/CONFIGURATION.md. Collectors are defined under collectors, in the files collector_files lists, or both.",
		},
		"collectors":               {"description": "The collectors. A probe names one with its collector parameter. A name must be unique across this list and every collector file."},
		"collector_files":          {"description": "Further files of collectors, as paths or glob patterns such as collectors.d/*.yaml, relative to this file. A collector file holds a collectors list and nothing else. See docs/CONFIGURATION.md#collector-files."},
		"collector_files[]":        {"type": "string", "minLength": 1},
		"rate_limits":              {"description": "Rate limits per host, shared by every collector: every http and graphite request to one of an entry's hosts, each attempt, page, followed redirect and login request counted, from any collector, probe or static target, takes a turn under the entry's rate_limit, as well as under its collector's own request.rate_limit. The first entry whose hosts match applies. Other request types are not limited. See docs/REQUESTS.md#rate-limits-per-host."},
		"rate_limits[]":            {"required": []string{"name", "hosts", "rate_limit"}},
		"rate_limits[].name":       {"pattern": `^[a-zA-Z_][a-zA-Z0-9_]*$`, "description": "Unique name of the entry, the rule label of its self-metrics http_exporter_host_rate_limit_waits_total and http_exporter_host_rate_limit_refused_total."},
		"rate_limits[].hosts":      {"minItems": 1, "description": "The hosts the entry limits, all ports and schemes of each together: host names, in any case, IP addresses, and *.example.com wildcards, which match every subdomain of example.com but not example.com itself. No scheme, port or path. A host may be listed in one entry only."},
		"rate_limits[].hosts[]":    {"minLength": 1},
		"rate_limits[].rate_limit": {"required": []string{"requests_per_second"}, "description": "The most requests a second sent to the entry's hosts together, written as request.rate_limit is. A request waits within its deadline for its turn, and one whose turn would come at or past its deadline fails at once."},
		"rate_limits[].rate_limit.requests_per_second": {"exclusiveMinimum": 0, "description": "Required. The requests a second on average, greater than 0, such as 5, or 0.5 for one every 2 seconds."},
		"rate_limits[].rate_limit.burst":               {"minimum": 1, "description": "How many requests may be sent at once after a pause, before the rest wait their turn. 1 when left out."},
		// What a collector must have, and each of its rules a name.
		"collectors[]":                              collectorSchemaRule(),
		"collectors[].name":                         {"pattern": `^[a-zA-Z_][a-zA-Z0-9_]*$`, "description": "Unique name, used as the collector parameter of /probe."},
		"collectors[].metrics_prefix":               {"pattern": optionalPattern(transform.MetricsPrefixRE.String()), "description": "Joined with _ to the front of every metric the collector exports, such as grafana for grafana_statuspage_status. Letters and digits, in parts joined by single underscores."},
		"collectors[].cache":                        {"description": "The collector's response cache. See docs/CONFIGURATION.md#response-caching."},
		"collectors[].cache.ttl":                    {"description": "Answer a repeat of the same probe from memory for this long. Omit or 0s to always go to the target."},
		"collectors[].cache.stale_if_error":         {"description": "After ttl, keep a result this much longer to answer a probe whose trip to the target fails, marked by http_exporter_result_stale 1. Omit or 0s to answer the failure."},
		"collectors[].cache.revalidate":             {"description": "http and graphite: once a cached result has expired, ask the target with If-None-Match and If-Modified-Since whether its response, kept with it when it had an ETag or Last-Modified, is still current; a 304 Not Modified reuses the kept body, which the transforms read again. Defaults to true. See docs/CONFIGURATION.md#revalidating-expired-responses."},
		"collectors[].max_concurrent_probes":        {"description": "How many trips to its targets the collector makes at once; a probe over the limit is answered 503 at once, a static target waits. Omit or 0 for the default, 32."},
		"collectors[].coalesce":                     {"description": "Share one request to the target among identical probes that arrive while it is in flight. Defaults to true."},
		"collectors[].lint":                         {"description": "The metric lint: advice --dry-run and the debug report give about the series the collector makes, which never fails anything. See docs/CONFIGURATION.md#metric-lint."},
		"collectors[].lint.ignore":                  {"description": "The ids of the lint rules whose advice is not given for this collector."},
		"collectors[].lint.ignore[]":                {"enum": model.LintRules},
		"collectors[].failure_backoff":              {"description": "Stop contacting a target that keeps failing: after `after` consecutive failed scrapes its probes and static target scrapes fail at once, as a failed fetch, for `initial`, the wait doubling with each further failure up to `max`. Left out, every scrape contacts the target. See docs/CONFIGURATION.md#backing-off-from-failing-targets."},
		"collectors[].failure_backoff.after":        {"minimum": 1, "description": "How many consecutive failed scrapes of a target start the backoff. At least 1; defaults to 3."},
		"collectors[].failure_backoff.initial":      {"description": "The first wait, after the scrape that starts the backoff. A positive duration; defaults to 30s."},
		"collectors[].failure_backoff.max":          {"description": "The longest wait, which doubling stops at. Not shorter than initial; defaults to 10m, or initial when that is longer."},
		"collectors[].failure_backoff.min_budget":   {"description": "The least budget a scrape that timed out must have had for its timeout to count as a failure. A probe's budget comes from its caller's scrape timeout, so one asking for less time than the target needs cannot back the target off for everyone; other failures count whatever the budget. A positive duration; defaults to 1s."},
		"collectors[].request":                      requestSchemaRule(),
		"collectors[].request.type":                 {"enum": fetch.BuiltRequestTypes(), "description": "Required. How the collector reaches its data."},
		"collectors[].request.method":               {"enum": optionalEnum([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "get", "post", "put", "patch", "delete", "head"}), "description": "HTTP method. Defaults to GET."},
		"collectors[].request.body":                 {"description": "http: the request body. May contain {{param_name}} placeholders, written as |json, |number, |form, |xml or |raw: {{param_service|json}}. See docs/REQUESTS.md#in-the-body-headers-and-query."},
		"collectors[].request.path":                 {"description": "http and graphite: joined onto the target URL; it cannot hold ? or #, and query parameters go under request.query. localfile: the file, relative to request.root and joined after the target. sshfile: the file on the host, relative to request.root. s3file: the object, relative to request.root, its key the two joined. May contain {{param_name}} or {{param_name:default}} path parameters, filled by param_<name> probe parameters."},
		"collectors[].request.root":                 {"description": "localfile, sshfile and s3file, required: the absolute directory the collector may read files under, on the exporter's filesystem or on the host, or, for s3file, the key prefix written as one, /exports/app for the keys under exports/app/. No read reaches outside it, through .. or a symbolic link. Not / for localfile; / is taken for sshfile, for an account confined to its own tree, and for s3file, for every key of the bucket. See docs/LOCALFILE.md, docs/SSHFILE.md and docs/S3FILE.md."},
		"collectors[].request.files":                {"description": "localfile, sshfile and s3file: read every file of the directory whose name matches one of these patterns (path.Match syntax, no /), each checked on its own and labelled file. Not with request.path. See docs/LOCALFILE.md#reading-a-directory."},
		"collectors[].request.max_files":            {"description": "localfile, sshfile and s3file with request.files: the most files one scrape reads, in name order; the rest are skipped and logged. Defaults to 100."},
		"collectors[].request.max_total_bytes":      {"description": "localfile, sshfile and s3file with request.files: the most one scrape reads across every file; a file that would go past it is refused. Defaults to 64 MiB."},
		"collectors[].request.max_age":              {"description": "localfile, sshfile and s3file: refuse a file last modified longer ago than this, so a writer that has stopped fails the scrape instead of exporting its last values forever."},
		"collectors[].request.username":             {"not": onlyBlanks(), "description": "sshfile and sql, required: the account the file is read as on the host, or the statement runs as on the database. redis, optional, with password_file: the ACL user AUTH logs in as. Not blanks alone."},
		"collectors[].request.private_key_file":     {"not": onlyBlanks(), "description": "sshfile: the file of the private key the account authenticates with, tried before password_file. sql, the snowflake driver: the unencrypted PKCS#8 RSA key of key-pair authentication, instead of password_file. Read when a connection is made, so a key replaced on disk is used by the next one. See docs/SSHFILE.md#authentication and docs/SQL.md#authentication."},
		"collectors[].request.password_file":        {"not": onlyBlanks(), "description": "sshfile: the file of the account's password, tried after private_key_file; one of the two is required. sql: the file of the account's password, read when a connection is made. redis: the file of the password AUTH sends, read at every scrape; without it the exporter sends no AUTH. ldap, with bind_dn: the file of the password of the simple bind, read at every scrape; an empty one fails the scrape."},
		"collectors[].request.pagination":           paginationRule(),
		"collectors[].request.pagination.type":      {"enum": model.PaginationTypes, "description": "Required. How the next page is found. link: the URL of the Link header's rel=\"next\" (RFC 8288). next_url: the URL next reads from the page. cursor: the value next reads from the page, sent as the query parameter param. page: a page number in param, from start, one more each page. offset: an offset in param, from start, moved on by the items each page holds, or by size."},
		"collectors[].request.pagination.items":     {"not": onlyBlanks(), "description": "Required. The jq path of the array each page holds its items in, such as .items, .data.results, or . for a page that is an array. The merged answer is the first page with this array holding the items of every page, in order."},
		"collectors[].request.pagination.next":      {"description": "Required with type next_url and cursor, and for no other: the jq expression of the next page's URL, absolute or relative to the page's, or of its cursor, read from the page, with $headers and $status as in a rule. null, false or \"\" ends the pages."},
		"collectors[].request.pagination.param":     {"description": "Required with type cursor, page and offset, and for no other: the query parameter the cursor, the page number or the offset is sent in. It replaces a parameter of that name in the target's query; request.query may not set it."},
		"collectors[].request.pagination.start":     {"description": "page and offset: the first page's number, 1 when left out, or offset, 0 when left out. The first page is asked for with it."},
		"collectors[].request.pagination.size":      {"minimum": 1, "description": "offset: how far the offset moves from one page to the next. Left out, by the number of items the page held."},
		"collectors[].request.pagination.max_pages": {"minimum": 1, "maximum": model.MaxMaxPages, "description": "The most pages one trip fetches, the first included: from 1 to 1000, 10 when left out. A trip that stops here with another page to come is answered with the pages it fetched, counted in http_exporter_pagination_max_pages_reached_total and logged."},
		"collectors[].request.retry.attempts":       {"maximum": fetch.MaxRetryAttempts, "description": "How many times a failed request is retried, from 0, the default, to 10."},
		"collectors[].request.retry.non_idempotent": {"description": "http and graphite: retry a request whose method is not idempotent, such as POST, which sending again may repeat. Unset, only GET, HEAD, OPTIONS, TRACE, PUT and DELETE requests are retried."},
		"collectors[].request.retry.retry_after":    {"description": "http and graphite: wait what a retried answer's Retry-After header asks for, seconds or an HTTP date, instead of backoff. A value that is neither falls back to backoff; one longer than the probe's time left ends the request at once with that answer. Off by default: the header is not read."},
		"collectors[].request.retry.codes":          {"items": map[string]any{"type": "string"}, "description": "grpc: the gRPC status codes that are retried, by name, such as [UNAVAILABLE, ABORTED]. Defaults to [UNAVAILABLE]; OK is refused."},
		"collectors[].request.retry.statuses":       {"items": map[string]any{"type": []string{"integer", "string"}, "minimum": 100, "maximum": 599, "pattern": "^[1-5][xX][xX]$|^[1-5][0-9][0-9]$"}, "description": "http and graphite: the HTTP statuses that are retried, written as accept_status is, such as [404, 503] or [\"5xx\", 429]. Defaults to [408, 425, 429, \"5xx\"]; a list replaces the default, and [] retries no status, while a failed connection is retried still. An entry accept_status accepts whole is refused."},
		"collectors[].response.graphite.value": {
			"enum":        optionalEnum(model.GraphiteValues),
			"description": "graphite decoder: how a series' points become its value: last, the newest point, by default, or max, min, avg or sum.",
		},
		"collectors[].response.graphite.max_age": {"description": "graphite decoder: leave out a series whose newest point is older than this, so a series nobody writes any more is not exported with its last value."},
		"collectors[].response.graphite.invalid_lines": {
			"enum":        optionalEnum(model.GraphiteInvalidLines),
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
			"enum":        optionalEnum(model.DecoderTypes),
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
		"collectors[].metrics":                           {"description": "The metric rules. No two of them may be the same rule, alike in name, expression, items, labels, value_map and time_format: each would make every series the other makes, and every scrape would fail on a duplicate series. Nor, under a prometheus transform, may the expression of one rule match the name another rule passes on the metric of, where the two give its series one name and the same labels: each would make every series of that metric. A rule with an expression whose type is histogram or summary and whose error_mode is not fail is about the metrics of that type alone, and may match such a name unless the rule of the name sets that type too. The exporter checks both when the configuration loads. A python collector's rules make no series and are not held to it."},
		"collectors[].metrics[].name":                    {"type": "string", "not": onlyBlanks(), "description": "The metric name, before metrics_prefix: letters, digits, _ and :, not starting with a digit. Under the collector's name_escaping underscores or values it may be any name, such as http.server.duration, and is exported escaped. Text: a name YAML reads as a number or a boolean, such as 404 or true, is written in quotes."},
		"collectors[].metrics[].items":                   {"description": "jq, yq and css only: selects the things the metric is about, such as table rows. The expression and labels are then evaluated once per item: for jq and yq with the item as . and the whole document as $root, for css as selectors within the item."},
		"collectors[].metrics[].expression":              {"description": "Where the value comes from, in the transform's language: jq, a regex, a CSS selector, an XPath expression, a CSV column or a source metric pattern."},
		"collectors[].metrics[].error_mode": {
			"enum":        optionalEnum([]string{model.ErrorModeFail, model.ErrorModeLog, model.ErrorModeIgnore}),
			"description": "What happens when this metric cannot be extracted. Defaults to log. Not for the python transform, whose script fails the scrape itself, with fail(...).",
		},
		"collectors[].metrics[].required":      {"description": "When false, a missing value is skipped without an error. Defaults to true. Not for the python transform, whose rules read no value."},
		"collectors[].metrics[].labels[].name": {"type": "string", "minLength": 1, "not": onlyBlanks(), "description": "The label's name: letters, digits and _, not starting with a digit. Under the collector's name_escaping underscores or values it may be any name, such as service.name, and is exported escaped. A name transform.rename_labels renames or transform.remove_labels removes is not exported, and may be any name. Text: a name YAML reads as a number or a boolean is written in quotes."},
		// A name, and one of value and expression.
		"collectors[].metrics[].labels[]":                     labelSchemaRule(),
		"collectors[].metrics[].labels[].value":               {"description": "A static label value, exported as written. May contain {{param_name}} placeholders, filled from the probe's parameters; see docs/REQUESTS.md#in-label-values."},
		"collectors[].metrics[].labels[].expression":          {"not": onlyBlanks(), "description": "Reads the label from the response, in the transform's language, like the metric's expression. Not blanks alone: a constant is a value."},
		"collectors[].metrics[].labels[].truncate":            {"description": "Cut a value longer than limits.max_label_value_length to fit, ending in …, instead of failing the scrape."},
		"collectors[].metrics[].labels[].required":            {"description": "expression labels only: a series the expression gives no value, or an empty one, fails the metric under its error_mode instead of being exported without the label. Defaults to false."},
		"collectors[].request.rate_limit":                     {"required": []string{"requests_per_second"}, "description": "http and graphite: the most requests a second the collector sends to one origin (scheme, host and port) of its targets, every attempt, page, followed redirect and login request counted where it is sent. A request waits within its deadline for its turn, and one whose turn would come at or past its deadline fails at once. Each collector has its own limit for an origin. No probe parameter sets or changes it. See docs/REQUESTS.md#rate-limiting."},
		"collectors[].request.rate_limit.requests_per_second": {"exclusiveMinimum": 0, "description": "Required. The requests a second on average, greater than 0, such as 5, or 0.5 for one every 2 seconds."},
		"collectors[].request.rate_limit.burst":               {"minimum": 1, "description": "How many requests may be sent at once after a pause, before the rest wait their turn. 1 when left out."},
		"collectors[].request.accept_status":                  {"items": map[string]any{"type": []string{"integer", "string"}, "minimum": 100, "maximum": 599, "pattern": "^[1-5][xX][xX]$|^[1-5][0-9][0-9]$"}, "description": "The HTTP statuses whose answers are decoded, such as [200, 503] or [\"2xx\", 503]; every 2xx when left out. Any other status fails the scrape in the http_status stage. An accepted status is not retried. http and graphite."},
		"collectors[].request.allowed_targets":                {"description": "Hosts, globs such as *.example.com, IP addresses and CIDR networks the collector's requests may reach: a target is allowed when its host matches by name, or every address it resolves to is in an allowed network. Checked before the request, on every redirect and on every connection. Written in ASCII, as a target's host is: an internationalised name is listed, and requested, in its xn-- form. http, graphite, grpc, sshfile, sql, tlscert, redis, tcp and ldap; dns matches its names by name alone, and a PTR target by address; for s3file, whose target is a bucket, the buckets it may read, by name or glob, and no address."},
		"collectors[].request.denied_targets":                 {"description": "Hosts, globs, IP addresses and CIDR networks the collector's requests may not reach: a target matching by name, or resolving to any address in a denied network, is refused with 403. Wins over allowed_targets. Written in ASCII, as allowed_targets is."},
		"collectors[].metrics[].labels[].value_map":           {"propertyNames": valueMapKeys(), "description": "Turns the value the label's expression gives into another, such as {\"1\": running}; \"*\" maps any value it does not list, and a value mapped to \"\" leaves the label off. Without a match and without \"*\", the value is kept."},
		"collectors[].metrics[].value_map":                    {"propertyNames": valueMapKeys(), "description": "Turns the text the expression gives into the value, such as {up: 1, down: 0}; \"*\" maps any value it does not list, numbers included. Without a match and without \"*\", the value is read as a number. Not for the prometheus and python transforms."},
		"collectors[].metrics[].scale":                        {"description": "Multiplies the value, mapped or read as a number, such as 0.001 for milliseconds to seconds. Finite and not 0. Not for the python transform; for prometheus, plain samples only."},
		"collectors[].metrics[].time_format":                  {"description": "Reads the text the expression gives as a time; the value is that time in Unix seconds. rfc3339, rfc1123, or a layout: the reference time Mon Jan 2 15:04:05 MST 2006 written as the text writes its times, such as \"2006-01-02 15:04:05\", with a year, a month and a day. scale applies after. Not with value_map, and not for the prometheus and python transforms."},
		"collectors[].metrics[].time_zone":                    {"type": "string", "description": "With time_format: the zone a text that names none of its own is read in, as an IANA name such as Europe/Sofia. Defaults to UTC."},
		"collectors[].metrics[]":                              metricRuleSchemaRule(),
		"collectors[].derived":                                {"description": "Series computed from the collector's own series once its transform made them, in order, so an entry may read an earlier one: a sum, a difference or a ratio, such as a hit ratio of hits and misses. See docs/CONFIGURATION.md#derived-series."},
		"collectors[].derived[]":                              derivedSchemaRule(),
		"collectors[].derived[].name":                         {"type": "string", "not": onlyBlanks(), "description": "The derived metric's name, before metrics_prefix, which no rule of the collector and no other entry has. Letters, digits, _ and :, not starting with a digit, unless the collector's name_escaping is underscores or values."},
		"collectors[].derived[].type":                         {"enum": optionalEnum([]string{string(model.GaugeMetricType), string(model.CounterMetricType), string(model.UntypedMetricType)}), "description": "gauge, the default, counter or untyped: a derived series is one number."},
		"collectors[].derived[].description":                  {"description": "The help text of the derived metric."},
		"collectors[].derived[].sum":                          {"minItems": 1, "description": "Adds up the series of the families listed, two or more, matched by their label sets; with by, the series of the one family listed. Each a rule's name, as written, or an earlier entry's."},
		"collectors[].derived[].sum[]":                        derivedOperandRule(),
		"collectors[].derived[].difference":                   {"minItems": 2, "maxItems": 2, "description": "[a, b]: a - b, of the series of a and b matched by their label sets. Each a rule's name, as written, or an earlier entry's."},
		"collectors[].derived[].difference[]":                 derivedOperandRule(),
		"collectors[].derived[].ratio":                        {"minItems": 2, "maxItems": 2, "description": "[a, b]: a / b, of the series of a and b matched by their label sets. A ratio over 0 makes no series. Each a rule's name, as written, or an earlier entry's."},
		"collectors[].derived[].ratio[]":                      derivedOperandRule(),
		"collectors[].derived[].by":                           {"uniqueItems": true, "description": "With a sum of one family: adds up its series into one for each set of values of these labels, as the rules name them, the others dropped; [] makes one series of them all."},
		"collectors[].derived[].by[]":                         {"type": "string", "minLength": 1, "not": onlyBlanks()},
		"collectors[].derived[].hidden":                       {"description": "Computes the entry for later entries to read, without exporting it. Defaults to false."},
		"collectors[].metrics[].histogram": {
			"required":    []string{"buckets"},
			"description": "With type: histogram: counts the values the rule reads — every value its expression gives, or one per item, node, element, match or row — in a histogram, one for each set of label values, instead of making each a series. The values of this response alone: the histogram is made anew on every scrape. Not for the prometheus and python transforms.",
		},
		"collectors[].metrics[].histogram.buckets": {
			"minItems":    1,
			"maxItems":    model.MaxHistogramBuckets,
			"description": "The upper bounds of the histogram's buckets, finite numbers in increasing order, each once, at most " + strconv.Itoa(model.MaxHistogramBuckets) + "; a value is counted in every bucket whose bound it is at most. The bucket +Inf is added. scale applies to each value before it is counted. The order and that each bound is finite are checked by the exporter when the configuration loads.",
		},
		"collectors[].limits.max_script_memory":    {"description": "The most memory, as address space, each of the collector's Python workers may use, the interpreter and its libraries included, such as 256MiB. A script that needs more fails with a MemoryError. At least 32MiB; 0, the default, leaves it unbounded. Enforced on Linux."},
		"collectors[].limits.max_output_bytes":     leastOutputRule(),
		"collectors[].limits.script_timeout":       {"description": "How long a Python script may run. Starting the interpreter is not counted. At least 1ms, which the exporter checks when the configuration loads; defaults to 100ms."},
		"collectors[].limits.max_response_bytes":   leastResponseRule("The most bytes of an answer the collector reads, decompressed, whatever its request.max_response_bytes; the smaller of the two wins."),
		"collectors[].request.max_response_bytes":  leastResponseRule("http, graphite, grpc, localfile, sshfile and s3file: the most bytes of an answer, or of a file, the collector reads, decompressed. kubernetes: the most bytes of a page of the listing, and of the objects listed without their managed fields. redis: the most bytes of the server's replies to one scrape's commands, together. tcp: the most bytes of the server's answer. ldap: the most bytes of the server's messages to one scrape, every page of every search together, count_only searches' among them. sql: the most bytes of the values the statement's rows hold, text and binary data by their length and every other value as 8, each with its column's name's length, a NULL counting nothing. 10MiB when neither it nor limits.max_response_bytes is set."),
		"collectors[].request.headers":             {"propertyNames": headerNames(), "description": "http and graphite: headers sent to the target. A name is an HTTP token, letters, digits and !#$%&'*+-.^_`|~, set once whatever its case, and not Accept-Encoding, which the exporter sets itself. Values may contain {{param_name}} placeholders."},
		"collectors[].request.forward_headers":     {"description": "http, graphite and grpc: the headers a probe forwards to the target, from its header_<name> parameters. Not Authorization, which forward_authorization forwards, Accept-Encoding, which the exporter sets itself, or Host, Connection, Content-Length, Transfer-Encoding, Trailer, TE, Upgrade and any Proxy- header, which describe the connection to the exporter; each would never be forwarded."},
		"collectors[].request.forward_headers[]":   forwardHeaderEntry(),
		"collectors[].request.basic_auth.username": {"not": basicUsernameColon(), "description": "http, graphite and grpc: the username sent in the Basic credential. Not with a colon, which ends the username in the credential (RFC 7617)."},
		"collectors[].request.query":               {"propertyNames": queryNames(), "description": "http and graphite: query parameters added to the request, after the target's own query, each name and value sent escaped. A name is not empty and holds no control character. Values may contain {{param_name}} placeholders."},
		"otlp.headers":                             {"description": "Headers sent with every export request, such as a tenant or an API key. Once the block is switched on, a name is an HTTP token, set once whatever its case, and not Accept-Encoding."},
		"otlp":                                     otlpSchemaRule(),
		"otlp.enabled":                             {"description": "Turn the export on. false keeps the block's settings without using them."},
		"web.basic_auth":                           webBasicAuthRule(),
		"web.basic_auth.enabled":                   {"description": "Require the credentials. false keeps the block's settings without using them."},
		"otlp.endpoint":                            {"description": "OTLP/HTTP metrics endpoint, such as http://otel-collector:4318/v1/metrics."},
		"web.basic_auth.username_file":             {"description": "Read the username from this file instead of username, such as a mounted Secret. Read again when it changes."},
		"web.basic_auth.password_file":             {"description": "Read the password from this file instead of password, such as a mounted Secret. Read again when it changes."},
		"collectors[].name_escaping":               {"enum": optionalEnum([]string{transform.NameEscapingFail, transform.NameEscapingUnderscores, transform.NameEscapingValues}), "description": "What to do with a metric or label name that is not a classic Prometheus name, such as http.server.duration: fail the scrape (the default), replace what a classic name may not have with underscores, or use Prometheus's reversible values encoding (U__…). A name the collector writes itself, a rule's or a label's, is held to the same when the configuration loads: refused under fail, exported escaped under the other two. See docs/CONFIGURATION.md#utf-8-names."},
		"collectors[].response.charset":            {"description": "The encoding of the response when the target does not declare it or declares it wrongly, and of local files: a WHATWG name such as windows-1252, iso-8859-2, windows-1251 or shift_jis. See docs/CONFIGURATION.md#character-encodings."},
		"otlp.max_pending_points":                  {"minimum": nil, "description": "The most data points kept waiting for export while the endpoint fails; past it the oldest are dropped and counted. Defaults to 100000."},
		"otlp.batch_max_size":                      {"minimum": nil, "description": "The most data points one export request carries; an export of more is sent as several requests, one after another. Defaults to 8192."},
		"otlp.batch_max_bytes":                     {"description": "The most bytes the body of one export request may hold before compression, such as 4MiB; an export of more is sent as several requests, one after another, and a data point larger alone is sent in a request of its own. A size: a whole number of bytes, or a number with a unit such as 512KiB, 10MB or 1.5GiB. At least 64KiB, which the exporter checks of a size written as text, in quotes or with a unit, when the configuration loads; 0, like the key left out, is the default, 4MiB."},
		"otlp.unready_after_failures":              {"minimum": nil, "description": "Answer /ready with 503 after this many failed exports in a row, until one gets through. 0, the default, never does: an exporter whose exports fail still answers probes. At most 1000."},
		"otlp.probe_attributes":                    {"description": "Add collector and target attributes to the points a probe queues, so probes of different targets or collectors answering the same series are exported apart. Off, the default, the later probe's point replaces the earlier's."},
		"collectors[].request.tls.server_name":     {"description": "The name the target's certificate is checked against, and sent as SNI, when the target is addressed by something else, such as an IP address. Unset, the target's host."},
		// A disabled block is unchecked, so compression takes any text there
		// and the two numbers above any whole number, and otlpSchemaRule
		// holds them to their values in a block that is switched on.
		"otlp.compression": {"description": "Compression of the export requests: gzip, the default, or none."},
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
		// reads as a number is still taken, as the exporter takes it. An
		// entry of nothing at all, "", is refused here as there.
		"collectors[].request.allowed_targets[]": {"minLength": 1, "not": map[string]any{"type": "string", "pattern": fetch.OutsideASCIIPattern}},
		"collectors[].request.denied_targets[]":  {"minLength": 1, "not": map[string]any{"type": "string", "pattern": fetch.OutsideASCIIPattern}},
		// An entry of include or exclude is neither empty nor blanks alone
		// (transform.checkFilterEntry): the empty regular expression matches
		// every name, and one of blanks only the names that hold them. An
		// entry is not a key, so "" is not one left out; a value of
		// transform.labels is one, the label left out
		// (transform.applyCollectorLabels).
		"collectors[].transform.include":   {"description": "prometheus without metrics rules: keep only the metrics whose name one of these regular expressions matches, anywhere in the name as the target gives it. Left out, every metric is kept. An entry is not empty and not blanks alone: '.*' matches every name."},
		"collectors[].transform.include[]": filterEntry(),
		"collectors[].transform.exclude":   {"description": "prometheus without metrics rules: drop the metrics whose name one of these regular expressions matches, anywhere in the name as the target gives it, also when include matches it. An entry is not empty and not blanks alone: '.*' matches every name."},
		"collectors[].transform.exclude[]": filterEntry(),
		"collectors[].transform.labels":    {"description": "Constant labels given to every metric the collector exports, over a label of the same name a rule gave. A value written \"\" is the label left out. Values may contain {{param_name}} placeholders, filled from the probe's parameters. See docs/CONFIGURATION.md#collector-wide-labels."},
	})
}

// otlpSchemaRule is the rule of the otlp block: one that enabled switches on
// (enabledSwitchRule), whose timeout and interval, once it is on, are not
// negative, whose compression is then one of its values, whose
// max_pending_points, unready_after_failures and batch_max_size are then not
// negative, whose unready_after_failures is then at most its most, whose
// batch_max_bytes, written as a number, is then 0 or at least its least, and
// whose headers' names are then header names
// (headerNames), as the exporter checks them then (validateOTLP); a size
// written as text the exporter alone holds to its least, as it does
// limits.max_output_bytes (leastOutputRule). Switched off, the block is
// kept unchecked by both: the keys' own pattern takes any duration, and the
// keys themselves any compression and any whole number. Compression was
// held to its values whatever the switch said, and the two numbers to zero
// and above, so an editor flagged in a block kept for later what the
// exporter loads. What is not of a key's type — text for a number, a list
// for text — is no value of the key, switched off or on, to both.
// basicUsernameColon is text with a colon: what the username of a Basic
// credential may not be (fetch.CheckBasicUsername). It goes under not, so
// that "" stays the key left out.
func basicUsernameColon() map[string]any {
	return map[string]any{"type": "string", "pattern": ":"}
}

// webBasicAuthRule is web.basic_auth: a block switched on by enabled, whose
// username, once it is, holds no colon, which no client could present
// (validateWebAuth); switched off, the block is unchecked, as the exporter
// leaves it.
func webBasicAuthRule() map[string]any {
	rule := enabledSwitchRule(reflect.TypeOf(model.ExporterBasicAuth{}), "Basic authentication on the exporter's own endpoints. Off until enabled is true; a block that sets any other key must say enabled, true or false. Once enabled, username holds no colon, which ends the username in a Basic credential (RFC 7617).")
	rule["if"] = map[string]any{"properties": map[string]any{"enabled": map[string]any{"const": true}}, "required": []string{"enabled"}}
	rule["then"] = map[string]any{"properties": map[string]any{"username": map[string]any{"not": basicUsernameColon()}}}
	return rule
}

func otlpSchemaRule() map[string]any {
	rule := enabledSwitchRule(reflect.TypeOf(model.OTLPConfig{}), "OTLP export of probe results, self-metrics and static targets with export_via_otlp. Off until enabled is true; a block that sets any other key must say enabled, true or false. See docs/OTLP.md.")
	notNegative := map[string]any{"pattern": durationPattern}
	fromZero := map[string]any{"minimum": 0}
	rule["if"] = map[string]any{"properties": map[string]any{"enabled": map[string]any{"const": true}}, "required": []string{"enabled"}}
	rule["then"] = map[string]any{"properties": map[string]any{
		"timeout": notNegative, "interval": notNegative,
		"compression":        map[string]any{"enum": optionalEnum([]string{model.OTLPCompressionGzip, model.OTLPCompressionNone})},
		"max_pending_points": fromZero, "batch_max_size": fromZero,
		"unready_after_failures": map[string]any{"minimum": 0, "maximum": model.MaxOTLPUnreadyAfterFailures},
		"batch_max_bytes":        map[string]any{"anyOf": []any{map[string]any{"const": 0}, map[string]any{"minimum": model.MinOTLPBatchMaxBytes}}},
		"headers":                map[string]any{"propertyNames": headerNames()},
	}}
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
	timeFormatBesideValueMap := writtenKey("time_format")
	timeFormatBesideValueMap["required"] = []string{"time_format", "value_map"}
	return map[string]any{
		"if":   writtenKey("time_zone"),
		"then": writtenKey("time_format"),
		"not":  timeFormatBesideValueMap,
		// A histogram block makes a histogram, so its rule is of that type
		// (config.checkHistogramRule).
		"allOf": []any{map[string]any{
			"if":   map[string]any{"required": []string{"histogram"}},
			"then": map[string]any{"required": []string{"type"}, "properties": map[string]any{"type": map[string]any{"const": string(model.HistogramMetricType)}}},
		}},
	}
}

// collectorSchemaRule requires what a collector must have, and of its rules
// a name, as the exporter does (validateMetricRule, checkPythonRule): every
// transform but prometheus, whose rule may pass series through under their
// own names. A name written "" is no name, to both, so it is the rule and
// not the key's pattern that refuses it, and only where a name is needed.
//
// A rule of a python collector names a series the script makes, and says
// nothing the script says itself (checkPythonRule): no type, description,
// required or error_mode. Each of its labels names a label of that series to
// cut, so it sets truncate: true and no value (checkPythonRuleLabels). A
// text key written "" is the key left out there as everywhere; required is
// a boolean, written when it is there.
//
// A rule's name, a derived entry's and the names of a rule's labels are
// classic Prometheus names unless the collector's name_escaping is
// underscores or values, under which
// a scrape exports any name escaped and the exporter takes any written one
// (transform.TakesLabelName): the patterns of the two keys hold where the
// collector does not say one of those two. A label's name that the
// collector's transform.rename_labels renames or remove_labels removes is
// taken whatever its characters (transform.TakesLabelNameBeforeRenames),
// which a schema cannot tell, since it would have to read the name among
// the keys of one setting or the entries of the other: the pattern of a
// label's name holds where the collector writes neither of the two, and
// where it writes one, a name that neither takes off is the exporter's alone
// to refuse. The two keys are text under every
// name_escaping, as a key with a pattern is throughout, by the type they
// have themselves (configSchemaRules): a name YAML reads as a number or a
// boolean is refused by the schemas and, read from them, by the load, and is
// written in quotes where it is meant, such as "404" under values.
//
// A prometheus rule's expression is not blanks alone, as the exporter
// refuses one (transform.checkRulePattern): it is a regular expression over
// metric names, like an entry of transform.include. And the rule has a name
// or an expression, written with something other than "": with neither it
// says which metrics it is about by nothing, and the exporter refuses it
// (checkPrometheusRuleSelects).
func collectorSchemaRule() map[string]any {
	transformIs := func(name string) map[string]any {
		return map[string]any{"properties": map[string]any{"transform": map[string]any{"properties": map[string]any{"type": map[string]any{"const": name}}, "required": []string{"type"}}}, "required": []string{"transform"}}
	}
	rules := func(rule map[string]any) map[string]any {
		return map[string]any{"properties": map[string]any{"metrics": map[string]any{"items": rule}}}
	}
	pythonRule := map[string]any{
		"not":        map[string]any{"anyOf": []any{writtenKey("type"), writtenKey("description"), writtenKey("required"), writtenKey("error_mode"), map[string]any{"required": []string{"histogram"}}}},
		"properties": map[string]any{"labels": map[string]any{"items": map[string]any{"not": writtenKey("value"), "required": []string{"truncate"}, "properties": map[string]any{"truncate": map[string]any{"const": true}}}}},
	}
	prometheusRule := map[string]any{
		"properties": map[string]any{"expression": map[string]any{"not": onlyBlanks()}},
		"anyOf":      []any{writtenKey("name"), writtenKey("expression")},
		"not":        map[string]any{"required": []string{"histogram"}},
	}
	escapesNames := map[string]any{"properties": map[string]any{"name_escaping": map[string]any{"enum": []string{transform.NameEscapingUnderscores, transform.NameEscapingValues}}}, "required": []string{"name_escaping"}}
	classicNames := map[string]any{"properties": map[string]any{
		"name": map[string]any{"type": "string", "pattern": optionalPattern(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)},
	}}
	classicLabelNames := map[string]any{"properties": map[string]any{
		"labels": map[string]any{"items": map[string]any{"properties": map[string]any{"name": map[string]any{"type": "string", "pattern": `^[a-zA-Z_][a-zA-Z0-9_]*$`}}}},
	}}
	takesOffLabels := map[string]any{"properties": map[string]any{"transform": map[string]any{"anyOf": []any{
		map[string]any{"properties": map[string]any{"rename_labels": map[string]any{"minProperties": 1}}, "required": []string{"rename_labels"}},
		map[string]any{"properties": map[string]any{"remove_labels": map[string]any{"minItems": 1}}, "required": []string{"remove_labels"}},
	}}}, "required": []string{"transform"}}
	return map[string]any{
		"required":    []string{"name", "request", "transform"},
		"description": "How to reach a kind of target and turn its response into metrics.",
		"if":          map[string]any{"properties": map[string]any{"transform": map[string]any{"properties": map[string]any{"type": map[string]any{"not": map[string]any{"const": "prometheus"}}}}}},
		"then":        rules(writtenKey("name")),
		"allOf": []any{
			map[string]any{"if": transformIs("python"), "then": rules(pythonRule)},
			map[string]any{"if": transformIs("prometheus"), "then": rules(prometheusRule)},
			map[string]any{"if": map[string]any{"not": escapesNames}, "then": rules(classicNames)},
			map[string]any{"if": map[string]any{"not": escapesNames}, "then": map[string]any{"properties": map[string]any{"derived": map[string]any{"items": classicNames}}}},
			map[string]any{"if": map[string]any{"not": map[string]any{"anyOf": []any{escapesNames, takesOffLabels}}}, "then": rules(classicLabelNames)},
		},
	}
}

// derivedSchemaRule is a derived entry's, as the exporter checks it
// (transform.CheckDerived): a name, not "", and exactly one of sum, difference and
// ratio; by only beside sum, which then reads one family, and without it a
// sum reads two or more.
func derivedSchemaRule() map[string]any {
	required := func(key string) map[string]any { return map[string]any{"required": []string{key}} }
	return map[string]any{
		"required": []string{"name"},
		"allOf":    []any{writtenKey("name")},
		"oneOf":    []any{required("sum"), required("difference"), required("ratio")},
		"if":       required("by"),
		"then":     map[string]any{"required": []string{"sum"}, "properties": map[string]any{"sum": map[string]any{"maxItems": 1}}},
		"else":     map[string]any{"properties": map[string]any{"sum": map[string]any{"minItems": 2}}},
	}
}

// derivedOperandRule is an operand of a derived entry: the name of a family,
// text that is not blanks alone.
func derivedOperandRule() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "not": onlyBlanks()}
}

// labelSchemaRule is a label's: a name, and exactly one of value and
// expression, either of which written "" is the key left out, as it is to
// the exporter (validateMetricRule), so a label with expression and value:
// "" reads the response and one with both "" has neither. A required label
// takes no value_map that maps a value to "", which would leave the label
// off (transform.checkLabelValueMaps).
func labelSchemaRule() map[string]any {
	return map[string]any{
		"required":    []string{"name"},
		"oneOf":       []any{writtenKey("value"), writtenKey("expression")},
		"if":          map[string]any{"properties": map[string]any{"required": map[string]any{"const": true}}, "required": []string{"required"}},
		"then":        map[string]any{"properties": map[string]any{"value_map": map[string]any{"additionalProperties": map[string]any{"not": map[string]any{"const": ""}}}}},
		"description": "Set value for a static label, or expression to read it from the response.",
	}
}

// requestSchemaRule requires type, and for each built request type with a
// rule of its own beyond its keys (fetch.RequestType.SchemaRule), that rule
// when the type is chosen.
func requestSchemaRule() map[string]any {
	rule := map[string]any{"required": []string{"type"}, "description": "How the collector reaches its data. See docs/REQUESTS.md, docs/LOCALFILE.md for localfile, docs/GRAPHITE.md for graphite, docs/GRPC.md for grpc, docs/SSHFILE.md for sshfile, docs/SQL.md for sql, docs/DNS.md for dns, docs/TLSCERT.md for tlscert, docs/S3FILE.md for s3file, docs/KUBERNETES.md for kubernetes, docs/REDIS.md for redis, docs/TCP.md for tcp and docs/LDAP.md for ldap."}
	var conditions []any
	for _, name := range fetch.BuiltRequestTypes() {
		if rt := fetch.RequestTypes[name]; rt.SchemaRule != nil {
			conditions = append(conditions, map[string]any{
				"if":   map[string]any{"properties": map[string]any{"type": map[string]any{"const": name}}, "required": []string{"type"}},
				"then": rt.SchemaRule(),
			})
		}
	}
	if len(conditions) > 0 {
		rule["allOf"] = conditions
	}
	return rule
}

// paginationRule is the rule of request.pagination: type and items
// required, each written with something other than "", next with type
// next_url and cursor, and param with cursor, page and offset, as the
// exporter has them (fetch.validatePagination).
func paginationRule() map[string]any {
	when := func(types ...string) map[string]any {
		return map[string]any{"properties": map[string]any{"type": map[string]any{"enum": types}}, "required": []string{"type"}}
	}
	return map[string]any{
		"description": "http: fetch the further pages of a paginated answer and merge them into one JSON document, the first page with its items array holding every page's items, which the decoder and the rules read as the answer. Every page is sent the request's headers, credentials and body, and must stay on the target's scheme, host and port. The decoder is json. See docs/REQUESTS.md#pagination.",
		"allOf": []any{
			writtenKey("type"),
			writtenKey("items"),
			map[string]any{"if": when(model.PaginationNextURL, model.PaginationCursor), "then": writtenKey("next")},
			map[string]any{"if": when(model.PaginationCursor, model.PaginationPage, model.PaginationOffset), "then": writtenKey("param")},
		},
	}
}

// staticTargetsSchemaRules adds, by path, what the StaticTargetFile struct cannot say.
func staticTargetsSchemaRules() map[string]map[string]any {
	return withLoginRules("targets[].request", "http and graphite: replaces the collector's request.login for this target, as its own credential replaces the collector's. See docs/AUTHENTICATION.md#logging-in-to-a-target.", map[string]map[string]any{
		"": {
			"required":    []string{"interval", "targets"},
			"description": "Static targets of the exporter, scraped by the exporter itself on their intervals and served together on the static targets endpoint (--web.static-targets-path); a target with export_via_otlp is also delivered over OTLP. Passed with --static-targets-file. See docs/STATIC-TARGETS.md.",
		},
		"interval":            {"description": "How often a target that sets no interval is scraped, such as 1m. Required; at least 1s."},
		"concurrency":         {"minimum": 0, "description": "How many targets are scraped at once. A target due while all are busy waits for a slot within its interval, and is skipped if none frees. Omit or 0 for the default, 8."},
		"targets":             {"minItems": 1, "description": "The targets. Each is scraped on its own interval with one collector of the configuration."},
		"targets[]":           {"required": []string{"collector"}, "description": "One target: a collector of the configuration, the address it reads, and what this target overrides."},
		"targets[].name":      {"pattern": optionalPattern(targetNameRE.String()), "description": "Unique name, in logs and the static_target label. Defaults to <collector>_<index>."},
		"targets[].collector": {"minLength": 1, "description": "The collector of the configuration that scrapes this target."},
		"targets[].target":    {"description": "What the collector reads: a URL for an http collector, the Graphite server's URL for a graphite one, host:port or a grpc:// or grpcs:// URL of it for a grpc one, a file under request.root for a localfile one, the host, host:port or ssh://host:port for an sshfile one, the bucket or s3://bucket for an s3file one, the database server, host or host:port, for a sql one, or the account identifier for a snowflake one, the name to resolve, or for PTR the address, for a dns one, or host, host:port or [v6]:port for a tlscert one, or host, host:port, [v6]:port or a redis:// or rediss:// URL of one for a redis one, host:port or [v6]:port, the port required, for a tcp one, or host, host:port, [v6]:port or an ldap:// or ldaps:// URL of one for an ldap one. A kubernetes collector takes none: leave it out."},
		"targets[].interval":  {"description": "How often this target is scraped, whatever Prometheus scrapes the endpoint on and otlp.interval exports on. At least 1s, and no shorter than request.timeout; defaults to the file's interval."},
		"targets[].params": {
			"propertyNames": map[string]any{"pattern": fetch.PathParamName.String()},
			"description":   "Values of the collector's {{param_<name>}} placeholders, as a probe's param_<name> parameters give them. Each must be used by a placeholder.",
		},
		"targets[].labels": {
			"propertyNames": map[string]any{"minLength": 1, "not": map[string]any{"enum": []string{StaticTargetLabel, "job", "instance"}}},
			"description":   "Added to every metric the target produces, without overwriting a label the collector extracted. A label written \"\" is the label left out. static_target is set by the endpoint, and job and instance by Prometheus when it scrapes the endpoint, so none of the three can be used.",
		},
		"targets[].export_via_otlp":             {"description": "Also deliver this target's results over OTLP, on otlp.interval, besides serving them on the static targets endpoint. Needs otlp.enabled. Defaults to false."},
		"targets[].request":                     {"description": "Overrides of the collector's request for this target, as the /probe parameters override it for a probe."},
		"targets[].request.method":              {"enum": optionalEnum([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"})},
		"targets[].request.path":                {"description": "Replaces the collector's request.path. It cannot hold {{param_...}} placeholders."},
		"targets[].request.timeout":             {"description": "How long a scrape of this target may take. At most the target's interval."},
		"targets[].request.targets":             {"description": "graphite: replaces the collector's request.targets for this target. It cannot hold {{param_...}} placeholders."},
		"targets[].request.from":                {"description": "graphite: replaces the collector's request.from for this target."},
		"targets[].request.until":               {"description": "graphite: replaces the collector's request.until for this target."},
		"targets[].request.message":             {"description": "grpc: replaces the collector's request.message for this target. It cannot hold {{param_...}} placeholders."},
		"targets[].request.headers":             {"propertyNames": headerNames(), "description": "http and graphite: headers sent to this target, as written, without placeholders. A name is an HTTP token, set once whatever its case, and not Accept-Encoding."},
		"targets[].request.metadata":            {"propertyNames": map[string]any{"pattern": "^[0-9a-z_.-]+$"}, "description": "grpc: metadata sent besides the collector's; a key of both takes this value. It cannot hold {{param_...}} placeholders."},
		"targets[].request.basic_auth.username": {"not": basicUsernameColon(), "description": "http, graphite and grpc: the username of the target's own Basic credential. Not with a colon, which ends the username in the credential (RFC 7617)."},
		"targets[].request.username":            {"not": blanksOrPlaceholder(), "description": "sshfile and sql: replaces the collector's request.username for this target. redis: replaces the collector's ACL user for this target, with the target's own password_file, which it requires. Neither it nor a credential file key of a target holds {{param_...}} placeholders: they are used as written."},
		"targets[].request.bind_dn":             {"not": blanksOrPlaceholder(), "description": "ldap: replaces the collector's bind_dn for this target, with the target's own password_file, which it requires."},
		"targets[].request.tls": {
			// A client certificate is both files or neither, "" being a
			// file left out, as the exporter checks a target's block.
			"allOf": []any{
				map[string]any{"if": writtenKey("cert_file"), "then": writtenKey("key_file")},
				map[string]any{"if": writtenKey("key_file"), "then": writtenKey("cert_file")},
			},
			"description": "redis, ldap, tcp, sql, grpc, http and graphite: replaces the collector's whole request.tls block for this target, keys it leaves out not being taken from the collector's. Written, even as tls: {}, it asks for TLS, so a target cannot leave out TLS its collector asks for; for http and graphite, whose URL's scheme decides, it is the block every https request of the target's trips is made with, its redirects' and its login's, and is not written beside the target's insecure_skip_verify. A client certificate is cert_file and key_file together. Its values hold no {{param_...}} placeholders. See docs/STATIC-TARGETS.md#credentials-and-tls-of-a-target.",
		},
		"targets[].request.tls.ca_file":                     {"not": placeholder()},
		"targets[].request.tls.cert_file":                   {"not": placeholder()},
		"targets[].request.tls.key_file":                    {"not": placeholder()},
		"targets[].request.tls.server_name":                 {"not": placeholder()},
		"targets[].request.private_key_file":                {"not": blanksOrPlaceholder(), "description": "sshfile: the target's own private key file. A target that sets any of private_key_file, passphrase_file and password_file replaces all three of the collector's with its own. sql, the snowflake driver: the target's own key, which replaces the collector's credential file as password_file does."},
		"targets[].request.passphrase_file":                 {"not": blanksOrPlaceholder(), "description": "sshfile, with the target's private_key_file: the file of its passphrase."},
		"targets[].request.password_file":                   {"not": blanksOrPlaceholder(), "description": "sshfile and sql: the target's own password file; see private_key_file. redis and ldap: replaces the collector's password_file for this target: the password of the target's own username or bind_dn, or, without one, of the collector's."},
		"targets[].request.connections":                     {"not": map[string]any{"required": []string{"max_targets"}}, "description": "sql: replaces, key by key, the collector's request.connections for this target; not max_targets, which is the collector's and refused here."},
		"targets[].request.connections.max_per_target":      {"minimum": 0, "description": "sql: the most connections open to this target at once; 0 sets no limit."},
		"targets[].request.connections.max_idle_per_target": {"minimum": 0, "description": "sql: how many connections to this target are kept idle; 0 keeps none."},
		"targets[].request.connections.max_targets":         {"description": "A collector's setting, which a static target cannot set: refused here, whatever its value."},
		"targets[].request.connections.keepalive":           {"description": "sql: how a kept connection to this target is found broken."},
		"targets[].request.connections.idle_timeout":        {"description": "sql: how long an idle connection to this target is kept; 0 keeps it."},
		"targets[].request.connections.max_lifetime":        {"description": "sql: how long a connection to this target is used; 0 uses it as long as it works."},
		"targets[].request.connections.keepalive.interval":  {"description": "sql: how often a connection to this target is checked; at least 1s, 0 for none."},
		"targets[].request.connections.keepalive.check":     {"enum": optionalEnum([]string{"tcp", "ping"}), "description": "sql: tcp or ping."},
		"targets[].request.rate_limit":                      {"required": []string{"requests_per_second"}, "description": "http and graphite: replaces the collector's request.rate_limit for this target's requests, which have a limit of their own for each origin they go to, apart from the collector's other targets'. See docs/REQUESTS.md#rate-limiting."},
		"targets[].request.rate_limit.requests_per_second":  {"exclusiveMinimum": 0, "description": "Required. The requests a second on average, greater than 0."},
		"targets[].request.rate_limit.burst":                {"minimum": 1, "description": "How many requests may be sent at once after a pause. 1 when left out."},
		"targets[].request.accept_status":                   {"items": map[string]any{"type": []string{"integer", "string"}, "minimum": 100, "maximum": 599, "pattern": "^[1-5][xX][xX]$|^[1-5][0-9][0-9]$"}, "description": "http and graphite: replaces the collector's request.accept_status for this target."},
		"targets[].request.accept_codes":                    {"items": map[string]any{"type": "string"}, "description": "grpc: replaces the collector's request.accept_codes for this target."},
		"targets[].request.retry.attempts":                  {"maximum": fetch.MaxRetryAttempts, "description": "Replaces the collector's request.retry.attempts for this target: from 0 to 10."},
		"targets[].request.retry.codes":                     {"items": map[string]any{"type": "string"}, "description": "grpc: replaces the collector's request.retry.codes for this target."},
		"targets[].request.retry.statuses":                  {"items": map[string]any{"type": []string{"integer", "string"}, "minimum": 100, "maximum": 599, "pattern": "^[1-5][xX][xX]$|^[1-5][0-9][0-9]$"}, "description": "http and graphite: replaces the collector's request.retry.statuses for this target."},
		"targets[].request.retry":                           {"description": "Each key set replaces the collector's request.retry key for this target; each left out keeps the collector's."},
		"targets[].request.retry.retry_after":               {"description": "http and graphite: replaces the collector's request.retry.retry_after for this target."},
		"targets[].request.retry.non_idempotent":            {"description": "Retry a request whose method is not idempotent, such as POST, which sending again may repeat."},
		"targets[].otlp":                                    {"description": "The OTLP resource this target's metrics are exported under, over the exporter-wide otlp settings. Only with export_via_otlp: true."},
		"targets[].shard_key":                               {"description": "For the target allocator (pue-allocator): what it hashes to choose the replica that scrapes this target, in place of the target's name, so targets with one key share a replica. The exporter itself ignores it. See docs/ALLOCATOR.md."},
	})
}

// withLoginRules adds to rules those of the login block under request, a
// collector's or a static target's, as the exporter checks it
// (fetch.validateLogin): type and url required; for json, exactly one of
// token, token_header and token_cookie, and field_files; for oauth2,
// client_secret_file and exactly one of client_id and client_id_file; no
// key of the other kind; and neither header nor prefix beside token_cookie.
// "" is a key left out, to both (writtenKey), but for prefix, where it is
// the token sent alone.
func withLoginRules(request, description string, rules map[string]map[string]any) map[string]map[string]any {
	kind := func(name string) map[string]any {
		return map[string]any{"properties": map[string]any{"type": map[string]any{"const": name}}, "required": []string{"type"}}
	}
	none := func(keys ...string) map[string]any {
		var written []any
		for _, key := range keys {
			written = append(written, writtenKey(key))
		}
		return map[string]any{"not": map[string]any{"anyOf": written}}
	}
	oneOf := func(keys ...string) map[string]any {
		var written []any
		for _, key := range keys {
			written = append(written, writtenKey(key))
		}
		return map[string]any{"oneOf": written}
	}
	login := request + ".login"
	rules[login] = map[string]any{
		"description": description,
		"allOf": []any{
			writtenKey("type"),
			writtenKey("url"),
			map[string]any{"if": kind("json"), "then": map[string]any{"allOf": []any{oneOf("token", "token_header", "token_cookie"), map[string]any{"required": []string{"field_files"}, "properties": map[string]any{"field_files": map[string]any{"minProperties": 1}}}, none("client_id", "client_id_file", "client_secret_file", "scopes", "endpoint_params", "auth_style")}}},
			map[string]any{"if": kind("oauth2"), "then": map[string]any{"allOf": []any{oneOf("client_id", "client_id_file"), writtenKey("client_secret_file"), none("fields", "field_files", "token", "token_header", "token_cookie", "expires_in")}}},
			// prefix written "" is a prefix of nothing, not one left out.
			map[string]any{"if": writtenKey("token_cookie"), "then": map[string]any{"allOf": []any{none("header"), map[string]any{"not": map[string]any{"required": []string{"prefix"}}}}}},
		},
	}
	for key, rule := range map[string]map[string]any{
		"type":               {"enum": model.LoginTypes, "description": "Required. json: post a JSON object of fields and field_files to url and read the token from the answer. oauth2: ask the token endpoint url for a token with the client credentials grant (RFC 6749 section 4.4)."},
		"url":                {"pattern": `^([A-Za-z][A-Za-z0-9+.-]*:|/([^/]|$))`, "description": "Required. Where the login is sent: an absolute http or https URL, the same for every target, or a path from the root such as /api/login, resolved against the target's scheme, host and port alone, so each target is logged in to; a path relative to the target's is refused. Of a scheme allowed_schemes allows, held to allowed_targets and denied_targets; without credentials, a fragment or placeholders. A login follows no redirect."},
		"fields":             {"description": "json: members of the JSON object posted, as written: for what is no secret, such as a tenant."},
		"field_files":        {"description": "json: members of the JSON object posted, each read from its file at every login, such as {username: /var/run/secrets/login/username, password: /var/run/secrets/login/password}. Required: at least one. A field is in fields or here, not both."},
		"token":              {"description": "json: the jq expression of the token in the answer's JSON body, such as .access_token, with $status and $headers as in a rule. Exactly one of token, token_header and token_cookie."},
		"token_header":       {"pattern": optionalPattern("^[" + headerNameChars + "]+$"), "description": "json: the header of the answer that holds the token, such as X-Subject-Token."},
		"token_cookie":       {"pattern": optionalPattern("^[" + headerNameChars + "]+$"), "description": "json: the cookie the answer sets that is the session, such as JSESSIONID; it is sent back in the Cookie header, and its Max-Age or Expires is its lifetime."},
		"expires_in":         {"description": "json: the jq expression of the token's lifetime in seconds in the answer's body, such as .expires_in; null when the answer does not say. Left out, or null, ttl applies."},
		"client_id":          {"description": "oauth2: the client's id. Exactly one of client_id and client_id_file."},
		"client_id_file":     {"description": "oauth2: the file holding the client's id, read at every login."},
		"client_secret_file": {"description": "oauth2: required. The file holding the client's secret, read at every login."},
		"scopes":             {"description": "oauth2: the scopes asked for, sent as the scope parameter."},
		"scopes[]":           {"type": "string", "pattern": `^[^\s"\\]+$`},
		"endpoint_params":    {"propertyNames": map[string]any{"minLength": 1, "not": map[string]any{"enum": []string{"grant_type", "scope", "client_id", "client_secret"}}}, "description": "oauth2: further parameters of the token request, such as audience or resource. Not grant_type, scope, client_id or client_secret, which the grant sets."},
		"auth_style":         {"enum": optionalEnum(model.LoginAuthStyles), "description": "oauth2: how the client's id and secret are sent: header, as a Basic credential, the default, or params, as parameters of the form."},
		"ttl":                {"description": "How long a token is used when the answer does not say: no expires_in, an oauth2 answer without expires_in, a cookie without Max-Age or Expires, a token_header. Left out, such a token is used until the target answers 401."},
		"header":             {"pattern": optionalPattern("^[" + headerNameChars + "]+$"), "description": "The header the token is sent in, Authorization when left out. Its name reads as a credential's — it has auth, token, key, session or the like in it — so that what shows a request's headers withholds it. Not with token_cookie, whose cookie is sent in Cookie."},
		"prefix":             {"description": "What the token is sent after: \"Bearer \" in Authorization and nothing in another header when left out; \"\" sends the token alone. Not with token_cookie."},
		"tls":                {"description": "The login's own tls settings, for a login server whose certificate the collector's request.tls does not verify; left out, request.tls. Never what a probe or a static target changes of request.tls."},
	} {
		rules[login+"."+key] = rule
	}
	return rules
}
