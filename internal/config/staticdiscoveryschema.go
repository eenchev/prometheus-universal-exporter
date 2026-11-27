package config

import (
	"maps"
	"reflect"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/discovery"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// DNSSDSchemaRules are the rules of a dns_sd source's keys, by path from
// prefix, the path of the source: what the exporter's and the allocator's
// schemas both say of one.
func DNSSDSchemaRules(prefix string) map[string]map[string]any {
	// A refresh_interval is at least 1s, or 0, the default.
	least := fetch.SchemaLeastDuration(time.Second, true)
	return map[string]map[string]any{
		prefix + "dns_sd": {
			"required":    []string{"names"},
			"if":          map[string]any{"properties": map[string]any{"type": map[string]any{"enum": []string{"A", "AAAA"}}}, "required": []string{"type"}},
			"then":        map[string]any{"required": []string{"port"}},
			"else":        map[string]any{"not": map[string]any{"required": []string{"port"}}},
			"description": "Names looked up in the DNS as Prometheus's dns_sd_configs looks them up: each SRV record a target at its target and port, or each A or AAAA record a target at its address and port.",
		},
		prefix + "dns_sd.names":            {"minItems": 1, "uniqueItems": true, "description": "The names looked up, each once, as absolute names, such as _metrics._tcp.example.com."},
		prefix + "dns_sd.names[]":          {"minLength": 1, "pattern": `^[A-Za-z0-9_.-]+$`},
		prefix + "dns_sd.type":             {"enum": optionalEnum([]string{"SRV", "A", "AAAA"}), "description": "The record type asked for: SRV, the default, A or AAAA."},
		prefix + "dns_sd.port":             {"minimum": 1, "maximum": 65535, "description": "The port of the targets of an A or AAAA source, required for those and refused for SRV, whose records carry their own."},
		prefix + "dns_sd.server":           {"description": "A DNS server, host or host:port (port 53 by default), asked instead of the system's nameservers."},
		prefix + "dns_sd.refresh_interval": {"not": least, "description": "How often the names are looked up, at least 1s. Defaults to 30s."},
		prefix + "dns_sd.timeout":          {"description": "How long the lookups of all the names may take together, no longer than refresh_interval. Defaults to 10s, or refresh_interval when that is shorter."},
	}
}

// exporterDiscoverySource is a source of an entry as the exporter's schema
// describes it: a dns_sd one, the one kind the exporter asks.
type exporterDiscoverySource struct {
	Name       string             `yaml:"name"`
	DNSSD      *model.DNSSDConfig `yaml:"dns_sd"`
	Labels     map[string]string  `yaml:"labels"`
	MaxTargets int                `yaml:"max_targets"`
}

// discoveredTargetsSchema describes discovered_targets as the exporter
// reads it, from target, the schema of a static target: each entry has the
// target's keys but target, whose value discovery finds, and its own,
// name, discovery of dns_sd sources, target_template and shard_key.
func discoveredTargetsSchema(target map[string]any) map[string]any {
	entry := maps.Clone(target)
	properties := maps.Clone(target["properties"].(map[string]any))
	entry["properties"] = properties
	delete(properties, "target")
	properties["name"] = map[string]any{
		"type":        "string",
		"pattern":     targetNameRE.String(),
		"description": "The entry's name, which each target it makes is named after: <name>_<address>, the address escaped (10.0.0.5:9100 is 10_0_0_5_3A9100) or, when it holds other characters or is long, cut and followed by _X and 16 hex digits of its SHA-256. Required, unique among the entries, and not another entry's name followed by _.",
	}
	reserved := model.SortedKeys(discovery.ReservedTargetLabels)
	labelName := `^[a-zA-Z_][a-zA-Z0-9_]*$`
	rules := map[string]map[string]any{
		"":        {"minItems": 1, "description": "dns_sd sources, the one kind the exporter asks itself; the target allocator asks file_sd, http_sd and kubernetes ones too. See docs/STATIC-TARGETS.md#discovering-targets-by-dns."},
		"[]":      {"required": []string{"dns_sd"}, "description": "One dns_sd source."},
		"[].name": {"pattern": optionalPattern(discoverySourceName.String()), "description": "Unique in its list, in the self-metrics and logs. Letters, digits, _, . and -, not starting with . or -. Defaults to dns_sd_ and its place, such as dns_sd_0."},
		"[].labels": {
			"propertyNames": map[string]any{"pattern": labelName, "not": map[string]any{"anyOf": []any{map[string]any{"pattern": "^__"}, map[string]any{"enum": reserved}}}},
			"description":   "Labels set from the source's own, each target's label of the key taking the value of the label the value names, such as service: __meta_dns_name. Not job, instance or static_target, nor a name starting with __.",
		},
		"[].labels.*":    {"pattern": labelName},
		"[].max_targets": {"minimum": 0, "description": "How many targets one answer may hold; one with more is a failure. 0 or left out takes 10000."},
	}
	maps.Copy(rules, DNSSDSchemaRules("[]."))
	properties["discovery"] = schemaFor(reflect.TypeOf([]exporterDiscoverySource{}), "", rules)
	properties["target_template"] = map[string]any{
		"type":        "string",
		"pattern":     optionalPattern(`\{\{address\}\}`),
		"description": "Makes a target of each discovered address, " + discovery.AddressPlaceholder + " standing for it, such as http://" + discovery.AddressPlaceholder + "/metrics; without one the address is the target.",
	}
	properties["shard_key"] = map[string]any{
		"type":        "string",
		"enum":        optionalEnum([]string{"address"}),
		"description": "The target allocator's: address allocates each target by its address. The exporter scrapes every target, and reads it for the file to be the allocator's too.",
	}
	entry["required"] = []string{"name", "collector", "discovery"}
	entry["description"] = "A static target written for every target its discovery finds, checked at load with the address 192.0.2.1:9100 in place of a discovered one."
	return map[string]any{
		"type":        "array",
		"minItems":    1,
		"items":       entry,
		"description": "Static targets written once for every target that their dns_sd sources find, beside or in place of targets. The exporter asks the sources itself, and puts the targets in force as they change. See docs/STATIC-TARGETS.md#discovering-targets-by-dns.",
	}
}
