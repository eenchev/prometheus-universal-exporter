//go:build !select_request_types || (request_type_http && request_type_tcp)

package repository

import (
	"strings"
	"testing"
)

// The keys of a static target file's discovered_targets entry of its own,
// beside those it shares with a target, get one verdict from the schema and
// the exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnDiscoveredTargetsKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, discoveredTargetsSchemaKeys())
}

// oneEntry is a target file of one discovered_targets entry of the
// collector demo with one dns_sd source, with a comment where a row writes
// a key of the entry, of the source or of its dns_sd block.
const oneEntry = "interval: 1m\ndiscovered_targets:\n  - collector: demo\n    name: node\n    target_template: http://{{address}}/m\n    # entry\n    discovery:\n      - dns_sd:\n          names: [_m._tcp.a.test]\n          # dns\n        # source\n"

// discoveredTargetsSchemaKeys are the keys of an entry, of its sources and
// of their dns_sd blocks that a schema holds to a rule.
func discoveredTargetsSchemaKeys() []schemaKey {
	// tcpEntry takes a discovered address as its target, as a tcp
	// collector does, so its template may be left out.
	tcpEntry := strings.Replace(oneEntry, "    target_template: http://{{address}}/m\n", "    target_template: '{{address}}'\n", 1)
	keys := []schemaKey{
		{key: "discovered_targets[].name", file: inTargetFile, document: oneEntry, at: "    name: node\n", setting: "    name: %s\n", valid: "node", invalid: "node-a"},
		{key: "discovered_targets[].target_template", file: inTargetFile, configuration: tcpCollector, document: tcpEntry, at: "    target_template: '{{address}}'\n", setting: "    target_template: %s\n", valid: "'{{address}}'", invalid: "a.example:1", absent: true, empty: true, emptyWas: refused},
		{key: "discovered_targets[].shard_key", file: inTargetFile, document: oneEntry, at: "    # entry\n", setting: "    shard_key: %s\n", valid: "address", invalid: "name", absent: true, empty: true, emptyWas: refused},
		{key: "discovered_targets[].discovery[].name", file: inTargetFile, document: oneEntry, at: "        # source\n", setting: "        name: %s\n", valid: "eu", invalid: "'.eu'", absent: true, empty: true, emptyWas: refused},
		{key: "discovered_targets[].discovery[].labels{}", file: inTargetFile, document: oneEntry, at: "        # source\n", setting: "        labels: {%s: __meta_dns_name}\n", valid: "service", invalid: "__service", absent: true},
		{key: "discovered_targets[].discovery[].labels.*", file: inTargetFile, document: oneEntry, at: "        # source\n", setting: "        labels: {service: %s}\n", valid: "__meta_dns_name", invalid: "'a b'", absent: true},
		{key: "discovered_targets[].discovery[].dns_sd.names[]", file: inTargetFile, document: oneEntry, at: "          names: [_m._tcp.a.test]\n", setting: "          names: [%s]\n", valid: "_m._tcp.a.test", invalid: "'a b.test'"},
		{key: "discovered_targets[].discovery[].dns_sd.type", file: inTargetFile, document: oneEntry, at: "          # dns\n", setting: "          type: %s\n", valid: "SRV", invalid: "MX", absent: true, empty: true, emptyWas: refused},
		{key: "discovered_targets[].discovery[].dns_sd.refresh_interval", file: inTargetFile, document: oneEntry, at: "          # dns\n", setting: "          refresh_interval: %s\n", valid: "30s", invalid: "soon", absent: true, duration: true},
		{key: "discovered_targets[].discovery[].dns_sd.refresh_interval", of: "under its least", file: inTargetFile, document: oneEntry, at: "          # dns\n", setting: "          refresh_interval: %s\n", valid: "1s", invalid: "500ms", absent: true, invalidWas: taken},
		{key: "discovered_targets[].discovery[].dns_sd.timeout", file: inTargetFile, document: oneEntry, at: "          # dns\n", setting: "          timeout: %s\n", valid: "5s", invalid: "soon", absent: true, duration: true},
	}
	for i := range keys {
		if keys[i].configuration == "" {
			keys[i].configuration = targetsConfiguration
		}
	}
	return keys
}
