package allocator

import (
	"maps"
	"reflect"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The allocator's own files have JSON Schemas of their own, for editors, as
// the exporter's files have: configs/allocator-static-targets.schema.json,
// the static target file the allocator reads, which is the exporter's with
// discovered_targets beside targets, and configs/probe-targets.schema.json,
// the probe targets file. Each is generated from the structs the allocator
// decodes, with rules by path adding what a struct cannot say, printed by
// pue-allocator --static-targets-file-schema and --probe-targets-file-schema
// and written by make schemas; a test fails when a committed file and its
// generation differ, and another when a schema's keys and the decoder's do.
// The exporter's own static target schema keeps refusing discovered_targets,
// as the exporter does. As for the exporter's, the allocator's checks at load
// remain the authority: they also check what no schema can, such as that a
// collector is the configuration's.

// The published schemas' file names, in configs.
const (
	StaticTargetsSchemaFile = "allocator-static-targets.schema.json"
	ProbeTargetsSchemaFile  = "probe-targets.schema.json"
)

// StaticTargetsSchemaJSON renders the schema of the allocator's static
// target file.
func StaticTargetsSchemaJSON() ([]byte, error) {
	return config.RenderSchema(staticTargetsSchema())
}

// ProbeTargetsSchemaJSON renders the schema of the probe targets file.
func ProbeTargetsSchemaJSON() ([]byte, error) {
	return config.RenderSchema(probeTargetsSchema())
}

// staticTargetsSchema is the exporter's static target file schema with
// discovered_targets: targets may then be empty or left out, as the
// allocator takes a file of discovered targets alone.
func staticTargetsSchema() map[string]any {
	schema := config.StaticTargetsSchema()
	properties := schema["properties"].(map[string]any)
	delete(properties["targets"].(map[string]any), "minItems")
	properties[config.DiscoveredTargetsKey] = map[string]any{
		"type":        "array",
		"minItems":    1,
		"items":       discoveredEntrySchema(properties["targets"].(map[string]any)["items"].(map[string]any)),
		"description": "Static targets written once for every target their discovery finds: each entry is a static target without target, whose other keys every target it makes takes. Only the allocator reads it; the exporter refuses it. See docs/ALLOCATOR.md#discovering-targets.",
	}
	schema["required"] = []string{"interval"}
	schema["anyOf"] = []any{
		map[string]any{"required": []string{"targets"}, "properties": map[string]any{"targets": map[string]any{"minItems": 1}}},
		map[string]any{"required": []string{config.DiscoveredTargetsKey}},
	}
	schema["description"] = "The static target file of the target allocator (pue-allocator --static-targets-file): the exporter's static target file, split among the replicas, with discovered_targets beside targets, or in their place. See docs/ALLOCATOR.md#static-targets."
	schema["$id"] = config.SchemaID(StaticTargetsSchemaFile)
	schema["title"] = "prometheus-universal-exporter allocator static target file"
	return schema
}

// discoveredEntrySchema describes a discovered_targets entry from target,
// the schema of a static target: its keys but target, whose value discovery
// finds, and the keys the entry reads itself (readDiscoveredEntry): name,
// required, discovery, target_template and shard_key, which allocates a
// target by its address.
func discoveredEntrySchema(target map[string]any) map[string]any {
	// The copies are of the two maps changed here: the rest is shared with
	// targets, and left as it is.
	entry := maps.Clone(target)
	properties := maps.Clone(target["properties"].(map[string]any))
	entry["properties"] = properties
	delete(properties, "target")
	properties["name"] = map[string]any{
		"type":        "string",
		"pattern":     targetName.String(),
		"description": "The entry's name, which each target it makes is named after: <name>_<address>, the address escaped (10.0.0.5:9100 is 10_0_0_5_3A9100) or, when it holds other characters or is long, cut and followed by _X and 16 hex digits of its SHA-256. Required, unique among the entries, and not another entry's name followed by _.",
	}
	discovery := discoveryListSchema()
	discovery["minItems"] = 1
	properties["discovery"] = discovery
	properties["target_template"] = targetTemplateSchema()
	properties["shard_key"] = map[string]any{
		"type":        "string",
		"enum":        config.OptionalEnum([]string{ShardKeyAddress}),
		"description": "address allocates each target by its address, so the targets of several entries at one address share a replica; left out, a target is allocated by its name.",
	}
	entry["required"] = []string{"name", "collector", "discovery"}
	entry["description"] = "A static target written for every target its discovery finds, checked at load with the address 192.0.2.1:9100 in place of a discovered one."
	return entry
}

// probeTargetsSchema describes the probe targets file (ProbeFile): probe_jobs,
// and x- keys of its own for YAML anchors.
func probeTargetsSchema() map[string]any {
	schema := config.SchemaOf(reflect.TypeOf(ProbeFile{}), probeTargetsSchemaRules())
	properties := schema["properties"].(map[string]any)
	job := properties["probe_jobs"].(map[string]any)["items"].(map[string]any)
	job["properties"].(map[string]any)["discovery"] = discoveryListSchema()
	job["properties"].(map[string]any)["target_template"] = targetTemplateSchema()
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = config.SchemaID(ProbeTargetsSchemaFile)
	config.AllowExtensionKeys(schema)
	schema["title"] = "prometheus-universal-exporter allocator probe targets file"
	return schema
}

// probeTargetsSchemaRules adds, by path, what the ProbeFile struct cannot say
// (checkProbeFile, checkProbeJob).
func probeTargetsSchemaRules() map[string]map[string]any {
	paramNames := map[string]any{"pattern": labelName.String(), "not": map[string]any{"enum": []string{"target", "collector", "debug"}}}
	labelNames := map[string]any{"pattern": labelName.String(), "not": map[string]any{"anyOf": []any{map[string]any{"pattern": "^__"}, map[string]any{"const": "instance"}}}}
	return map[string]map[string]any{
		"": {
			"required":    []string{"probe_jobs"},
			"description": "The probe targets file of the target allocator (pue-allocator --probe-targets-file): probe jobs, whose targets the allocator hands Prometheus through HTTP service discovery at /v1/sd/<name>, each pointed at the replica that probes it. See docs/ALLOCATOR.md#probe-targets.",
		},
		"probe_jobs": {"minItems": 1, "description": "The probe jobs."},
		"probe_jobs[]": {
			"required": []string{"name", "collector"},
			"anyOf": []any{
				map[string]any{"required": []string{"targets"}, "properties": map[string]any{"targets": map[string]any{"minItems": 1}}},
				map[string]any{"required": []string{"discovery"}, "properties": map[string]any{"discovery": map[string]any{"minItems": 1}}},
			},
			"description": "Targets probed with one collector, at least one written or found by discovery.",
		},
		"probe_jobs[].name":      {"pattern": probeJobName.String(), "description": "Unique: the job's path, /v1/sd/<name>. Letters, digits, _, . and -, not starting with . or -."},
		"probe_jobs[].collector": {"minLength": 1, "description": "The collector of the configuration every probe of the job names."},
		"probe_jobs[].targets":   {"description": "The probes' targets, each listed once and checked as a static target's target is for the collector's request type."},
		"probe_jobs[].targets[]": {"minLength": 1},
		"probe_jobs[].params":    {"propertyNames": paramNames, "description": "Probe parameters sent on every probe, as __param_<name> labels: timeout, path, param_<name> and the others the collector's request type takes, checked as a probe checks them. Not target, collector or debug."},
		"probe_jobs[].labels":    {"propertyNames": labelNames, "description": "Labels on every target of the job; one written \"\" is left out. Not instance, which the allocator sets, nor a name starting with __."},
		"probe_jobs[].shard_key": {"enum": config.OptionalEnum([]string{ShardKeyTarget, ShardKeyAddress}), "description": "What a probe is allocated by: target, the default, its target as written, or address, the host and port it reaches."},
	}
}

// targetTemplateSchema describes a target_template, a probe job's or an
// entry's (checkTargetTemplate).
func targetTemplateSchema() map[string]any {
	return map[string]any{
		"type":        "string",
		"pattern":     config.OptionalPattern(`\{\{address\}\}`),
		"description": "Makes a target of each discovered address, " + AddressPlaceholder + " standing for it, such as https://" + AddressPlaceholder + "/health; without one the address is the target.",
	}
}

// discoveryListSchema describes a discovery list, a probe job's or an
// entry's, from DiscoveryConfig, with what checkDiscovery checks that a
// schema can say.
func discoveryListSchema() map[string]any {
	list := config.SchemaOf(reflect.TypeOf([]DiscoveryConfig{}), discoverySchemaRules())
	list["description"] = "Discovery sources, whose targets are allocated as written ones are. See docs/ALLOCATOR.md#sources."
	return list
}

// discoverySchemaRules adds, by path from a discovery list, what the
// DiscoveryConfig struct cannot say.
func discoverySchemaRules() map[string]map[string]any {
	reserved := model.SortedKeys(reservedTargetLabels)
	// written is the rule that an object has the key and that it is not
	// "", which the allocator takes for the key left out, as the exporter
	// does.
	written := func(key string) map[string]any {
		return map[string]any{"required": []string{key}, "properties": map[string]any{key: map[string]any{"not": map[string]any{"const": ""}}}}
	}
	labelPattern := labelName.String()
	// A refresh_interval is at least minDiscoveryRefresh, or 0, the default.
	refreshLeast := fetch.SchemaLeastDuration(minDiscoveryRefresh, true)
	return map[string]map[string]any{
		"[]": {
			"oneOf": []any{
				map[string]any{"required": []string{"file_sd"}},
				map[string]any{"required": []string{"http_sd"}},
				map[string]any{"required": []string{"kubernetes"}},
				map[string]any{"required": []string{"dns_sd"}},
			},
			"description": "One source: exactly one of file_sd, http_sd, kubernetes and dns_sd.",
		},
		"[].name": {"pattern": config.OptionalPattern(discoveryName.String()), "description": "Unique in its list, in the metrics and logs. Letters, digits, _, . and -, not starting with . or -. Defaults to the source's kind and place, such as file_sd_1."},
		"[].labels": {
			"propertyNames": map[string]any{"pattern": labelPattern, "not": map[string]any{"anyOf": []any{map[string]any{"pattern": "^__"}, map[string]any{"enum": reserved}}}},
			"description":   "Labels set from the source's own, each target's label of the key taking the value of the label the value names, such as namespace: __meta_kubernetes_namespace. Not job, instance or static_target, nor a name starting with __.",
		},
		"[].labels.*":      {"pattern": labelPattern},
		"[].max_targets":   {"description": "How many targets one answer may hold; one with more is a failure. 0 or left out takes --discovery.max-targets."},
		"[].file_sd":       {"required": []string{"files"}, "description": "Prometheus file_sd files: JSON or YAML lists of target groups."},
		"[].file_sd.files": {"minItems": 1, "description": "Paths or patterns, relative to the directory of the file that names them; a pattern that matches nothing gives no targets."},
		"[].file_sd.files[]": {
			"pattern":     `\.(` + config.Caseless("json") + "|" + config.Caseless("yml") + "|" + config.Caseless("yaml") + `)$`,
			"description": "Ends in .json, .yml or .yaml, which tells how its files are read.",
		},
		"[].file_sd.refresh_interval":  {"not": refreshLeast, "description": "How often the files are read again, at least 1s. Defaults to 1m."},
		"[].http_sd":                   {"required": []string{"url"}, "description": "A URL asked for target groups as Prometheus's HTTP service discovery asks it."},
		"[].http_sd.url":               {"pattern": "^" + config.Caseless("http") + "[sS]?://[^/?#@]+([/?#]|$)", "description": "An absolute http or https URL, without credentials."},
		"[].http_sd.refresh_interval":  {"not": refreshLeast, "description": "How often the URL is asked, at least 1s. Defaults to 1m."},
		"[].http_sd.timeout":           {"description": "How long a request may take, no longer than refresh_interval. Defaults to 10s, or refresh_interval when that is shorter."},
		"[].http_sd.bearer_token_file": {"description": "A file holding a token sent as Authorization: Bearer, read again at every refresh. Relative to the directory of the file that names it."},
		"[].http_sd.tls": {"allOf": []any{
			map[string]any{"if": written("cert_file"), "then": written("key_file")},
			map[string]any{"if": written("key_file"), "then": written("cert_file")},
		}, "description": "TLS for an https URL, as the exporter's tls block: ca_file, cert_file with key_file, server_name and insecure_skip_verify."},
		"[].kubernetes": {
			"required":    []string{"port"},
			"not":         map[string]any{"required": []string{"namespaces", "all_namespaces"}, "properties": map[string]any{"namespaces": map[string]any{"minItems": 1}, "all_namespaces": map[string]any{"const": true}}},
			"description": "The discovery.k8s.io/v1 EndpointSlices of the cluster the allocator runs in.",
		},
		"[].kubernetes.namespaces":     {"uniqueItems": true, "description": "The namespaces listed, each once; none lists the allocator's own. Not with all_namespaces."},
		"[].kubernetes.namespaces[]":   {"pattern": dnsLabel.String(), "maxLength": 63},
		"[].kubernetes.all_namespaces": {"description": "List the EndpointSlices of every namespace, which needs a ClusterRole."},
		"[].kubernetes.label_selector": {"description": "A Kubernetes label selector, checked at load."},
		"[].kubernetes.port": {
			"type": nil,
			"anyOf": []any{
				map[string]any{"type": "string", "pattern": portName.String(), "maxLength": 15},
				map[string]any{"type": "integer", "minimum": 1, "maximum": 65535},
			},
			"description": "The endpoint port the targets are at: its name, as the Service names it, or its number.",
		},
		"[].kubernetes.include_not_ready": {"description": "Take the endpoints that are not ready too."},
		"[].kubernetes.address_type":      {"enum": []string{"IPv4", "IPv6", "any"}, "description": "The addressType of the EndpointSlices taken: IPv4, the default, IPv6, or any, which takes FQDN slices too. A dual-stack Service has a slice of each family."},
		"[].kubernetes.refresh_interval":  {"not": refreshLeast, "description": "How often the EndpointSlices are listed, at least 1s. Defaults to 30s."},
		"[].kubernetes.timeout":           {"description": "How long a listing, every page of it, may take, no longer than refresh_interval. Defaults to 10s, or refresh_interval when that is shorter."},
		"[].dns_sd": {
			"required":    []string{"names"},
			"if":          map[string]any{"properties": map[string]any{"type": map[string]any{"enum": []string{"A", "AAAA"}}}, "required": []string{"type"}},
			"then":        map[string]any{"required": []string{"port"}},
			"else":        map[string]any{"not": map[string]any{"required": []string{"port"}}},
			"description": "Names looked up in the DNS as Prometheus's dns_sd_configs looks them up: each SRV record a target at its target and port, or each A or AAAA record a target at its address and port.",
		},
		"[].dns_sd.names":            {"minItems": 1, "uniqueItems": true, "description": "The names looked up, each once, as absolute names, such as _metrics._tcp.example.com."},
		"[].dns_sd.names[]":          {"minLength": 1, "pattern": `^[A-Za-z0-9_.-]+$`},
		"[].dns_sd.type":             {"enum": config.OptionalEnum([]string{"SRV", "A", "AAAA"}), "description": "The record type asked for: SRV, the default, A or AAAA."},
		"[].dns_sd.port":             {"minimum": 1, "maximum": 65535, "description": "The port of the targets of an A or AAAA source, required for those and refused for SRV, whose records carry their own."},
		"[].dns_sd.server":           {"description": "A DNS server, host or host:port (port 53 by default), asked instead of the system's nameservers."},
		"[].dns_sd.refresh_interval": {"not": refreshLeast, "description": "How often the names are looked up, at least 1s. Defaults to 30s."},
		"[].dns_sd.timeout":          {"description": "How long the lookups of all the names may take together, no longer than refresh_interval. Defaults to 10s, or refresh_interval when that is shorter."},
	}
}
