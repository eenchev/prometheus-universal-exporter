//go:build !select_request_types || request_type_tlscert

package repository

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/TLSCERT.md work as written against a TLS server, the
// CA file replaced by the server's certificate: the main collector's chain
// verifies and its certificate does not expire within 14 days, the days
// example labels the subject; and the static target loads against them.
func TestTLSCertDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/TLSCERT.md", "yaml")
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := strings.ReplaceAll(pageBlock(t, blocks, "collectors:\n")+pageBlock(t, blocks, "  - name: certificate_days\n"), "/etc/exporter/tls/internal-ca.pem", caFile)
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	target := strings.TrimPrefix(server.URL, "https://")
	for query, wants := range map[string][]string{
		"collector=certificates&target=" + target:     {"tls_certificate_verified 1", "tls_certificate_expires_within_14_days 0", "tls_certificate_expiry_seconds ", "tls_chain_earliest_expiry_timestamp_seconds "},
		"collector=certificate_days&target=" + target: {"tls_certificate_days_left ", "tls_certificate_expiring 0"},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?"+query, nil))
		for _, want := range wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s: %d, want %q in\n%s", query, recorder.Code, want, recorder.Body)
			}
		}
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", pageBlock(t, blocks, "interval: 1h\ntargets:\n")))
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
