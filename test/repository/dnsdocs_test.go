//go:build !select_request_types || request_type_dns

package repository

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/DNS.md load as written: the mail exchangers'
// collector, the addresses' and the SPF record's, their jq rules compiled,
// and the static target against them.
func TestDNSDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/DNS.md", "yaml")
	conf := pageBlock(t, blocks, "collectors:\n") + pageBlock(t, blocks, "  - name: service_addresses\n") + pageBlock(t, blocks, "  - name: spf\n")
	dir := t.TempDir()
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	if len(cfg.Collectors) != 3 || cfg.Collectors[0].Request.RecordType != "MX" || cfg.Collectors[2].Request.RecordType != "TXT" {
		t.Errorf("collectors %+v", cfg.Collectors)
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", pageBlock(t, blocks, "interval: 5m\ntargets:\n")))
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
}
