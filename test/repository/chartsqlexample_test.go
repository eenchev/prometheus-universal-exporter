//go:build !select_request_types || request_type_sql

package repository

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The sql collectors the chart README shows load with the exporter as
// written: each configuration of its SQL collectors section that has an sql
// collector is the one config.yaml of its config.data, and config.Load takes
// it, so a key the exporter renames or refuses cannot stay in the example.
// The credential files are not read when the configuration loads, so the
// mounted paths need not exist.
func TestChartReadmeSQLCollectorsLoad(t *testing.T) {
	_, section, found := strings.Cut(readChartFile(t, "README.md"), "## SQL collectors\n")
	if !found {
		t.Fatal("the chart README has no SQL collectors section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	loaded := 0
	for _, block := range yamlBlocksOf(section) {
		if !strings.HasPrefix(block, "config:\n") {
			continue
		}
		var values struct {
			Config struct {
				Data map[string]string `yaml:"data"`
			} `yaml:"config"`
		}
		if err := yaml.Unmarshal([]byte(block), &values); err != nil {
			t.Fatalf("the example does not parse: %v\n%s", err, block)
		}
		text := values.Config.Data["config.yaml"]
		if !strings.Contains(text, "type: sql\n") {
			continue
		}
		cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", text))
		if err != nil {
			t.Errorf("the exporter does not load the example: %v\n%s", err, text)
			continue
		}
		for i := range cfg.Collectors {
			if c := &cfg.Collectors[i]; c.Request.Type != "sql" {
				t.Errorf("collector %s has request type %q, want sql", c.Name, c.Request.Type)
			}
		}
		loaded++
	}
	if loaded == 0 {
		t.Fatal("the SQL collectors section shows no sql collector")
	}
}
