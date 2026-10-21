//go:build !select_request_types || request_type_http

package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
)

// The allocator takes every key the schema declares of a discovered_targets
// entry, and refuses a key it does not: an entry is a static target in
// all but its own keys, which readDiscoveredEntry reads by name, so the
// struct comparison of schemaallocator_test.go cannot see that the decoder
// still takes them. Each key is written, with a value that need not be good,
// into an entry the allocator takes: what it refuses then is the value,
// never the key.
func TestTheAllocatorTakesTheKeysTheSchemaDeclaresOfAnEntry(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	paths := allocator.Paths{Config: write("config.yaml", "collectors:\n  - name: node\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: node_up\n        expression: 'up=(\\d+)'\n")}
	const base = "interval: 1m\ndiscovered_targets:\n  - name: node\n    collector: node\n    target_template: http://{{address}}/metrics\n    discovery:\n      - file_sd: {files: [sd.json]}\n"
	load := func(extra string) error {
		paths.StaticTargets = write("targets.yaml", base+extra)
		_, err := allocator.LoadInputs(paths)
		return err
	}
	if err := load(""); err != nil {
		t.Fatalf("the base entry is refused: %v", err)
	}
	entry := at(t, loadSchemaFile(t, allocatorStaticTargetsSchemaFile), "discovered_targets", "[]")
	properties, _ := entry["properties"].(map[string]any)
	if len(properties) < 10 {
		t.Fatalf("the entry declares only %d keys", len(properties))
	}
	unknown := func(err error, key string) bool {
		return err != nil && (strings.Contains(err.Error(), `unknown key "`+key+`"`) || strings.Contains(err.Error(), key+" is set"))
	}
	for key := range properties {
		if strings.Contains(base, "    "+key+":") || strings.Contains(base, "  - "+key+":") {
			continue
		}
		if err := load("    " + key + ": x\n"); unknown(err, key) {
			t.Errorf("the schema declares %s of an entry, which the allocator refuses as a key: %v", key, err)
		}
	}
	for _, key := range []string{"target", "targets", "bogus"} {
		if _, declared := properties[key]; declared {
			t.Errorf("the schema declares %s of an entry", key)
		}
		if err := load("    " + key + ": x\n"); !unknown(err, key) {
			t.Errorf("the allocator does not refuse %s of an entry as a key: %v", key, err)
		}
	}
}
