//go:build !select_request_types || request_type_tlscert

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// --dry-run reads the files a tlscert collector's tls block names, which
// loading leaves to the handshake, and warns of one it would fail on, the
// check still passing: a CA file that is missing.
func TestDryRunWarnsOfTheFilesOfATLSCertCollector(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "ca.pem")
	config := testutil.WriteIn(t, dir, "config.yaml", `collectors:
  - name: certs
    request:
      type: tlscert
      tls:
        ca_file: `+missing+`
    transform:
      type: jq
    metrics:
      - name: tls_certificate_expiry_seconds
        expression: .seconds_until_expiry
`)
	out := runCheckCLI(t, "--config.file="+config)
	if out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stderr)
	}
	result := out.result(t, "config")
	warnings, _ := result.Details["warnings"].([]any)
	if result.Status != checkOK || len(warnings) != 1 || !strings.HasPrefix(warnings[0].(string), `collector "certs": request.tls: open `+missing) {
		t.Errorf("%+v", result)
	}
}
