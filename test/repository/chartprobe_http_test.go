//go:build !select_request_types || request_type_http

package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The chart README's Uptime probes example is one a reader can install as
// written: its values render the Probe the section shows, of the collector
// the configuration defines, and the configuration loads as the exporter
// loads it.
func TestChartReadmeUptimeProbeExampleRenders(t *testing.T) {
	helm := requireHelm(t)
	_, section, found := strings.Cut(readChartFile(t, "README.md"), "\n## Uptime probes\n")
	if !found {
		t.Fatal("the chart README has no Uptime probes section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	blocks := yamlBlocks(section)
	if len(blocks) < 2 {
		t.Fatalf("%d examples in the section, want the values and the Probe they render", len(blocks))
	}
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte(blocks[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	var conf string
	var probe string
	for _, doc := range renderedDocuments(t, helm, "--namespace", "monitoring", "-f", values) {
		switch child(doc, "kind").Value {
		case "ConfigMap":
			if data := path(doc, "data", "config.yaml"); data != nil {
				conf = data.Value
			}
		case "Probe":
			probe = path(doc, "metadata", "name").Value
			if got := path(doc, "spec", "prober", "url"); got == nil || got.Value != "test-prometheus-universal-exporter.monitoring.svc:8080" {
				t.Errorf("the Probe's prober is %+v, not the exporter's Service", got)
			}
			if got := path(doc, "spec", "params", "0", "values", "0"); got == nil || got.Value != "site" {
				t.Errorf("the Probe's collector parameter is %+v, want site", got)
			}
		}
	}
	if probe != "test-prometheus-universal-exporter-sites" {
		t.Errorf("the example renders the Probe %q, want test-prometheus-universal-exporter-sites", probe)
	}
	if err := loadExample(t, conf); err != nil {
		t.Errorf("the example's configuration does not load: %v\n%s", err, conf)
	}
	// The Probe shown is the one rendered, for a release named exporter.
	shown := strings.ReplaceAll(blocks[1], "exporter-prometheus-universal-exporter", "test-prometheus-universal-exporter")
	for _, line := range []string{`url: "test-prometheus-universal-exporter.monitoring.svc:8080"`, "path: /probe", `values: ["site"]`, "kind: Probe"} {
		if !strings.Contains(shown, line) {
			t.Errorf("the Probe the section shows lacks %q:\n%s", line, blocks[1])
		}
	}
}
