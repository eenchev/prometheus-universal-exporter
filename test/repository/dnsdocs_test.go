//go:build !select_request_types || request_type_dns

package repository

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/DNS.md load as written: the mail exchangers'
// collector, the addresses', the SPF record's, the TTL's, the SOA
// serial's, the signatures' and DNS over TLS's, their jq rules compiled,
// and the static target against them.
func TestDNSDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/DNS.md", "yaml")
	conf := pageBlock(t, blocks, "collectors:\n") + pageBlock(t, blocks, "  - name: service_addresses\n") + pageBlock(t, blocks, "  - name: spf\n") + pageBlock(t, blocks, "  - name: record_ttl\n") + pageBlock(t, blocks, "  - name: zone_serial_ns1\n") + pageBlock(t, blocks, "  - name: zone_signatures\n") + pageBlock(t, blocks, "  - name: dot_resolver\n")
	dir := t.TempDir()
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	recordType := func(c int) string { return cfg.Collectors[c].Request.Own.(fetch.DNSConfig).RecordType }
	if len(cfg.Collectors) != 7 || recordType(0) != "MX" || recordType(2) != "TXT" || recordType(4) != "SOA" || recordType(5) != "RRSIG" || cfg.Collectors[6].Request.Own.(fetch.DNSConfig).Transport != "tls" {
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
