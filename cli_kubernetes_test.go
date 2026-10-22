//go:build !select_request_types || request_type_kubernetes

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// --dry-run reads the token file a kubernetes collector naming api_server
// reaches it with, which loading leaves to the scrape, and warns of one a
// scrape would fail on, the check still passing: a token file that is
// missing.
func TestDryRunWarnsOfTheTokenFileOfAKubernetesCollector(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "token")
	config := testutil.WriteIn(t, dir, "config.yaml", `collectors:
  - name: deployments
    request:
      type: kubernetes
      api_server: https://k8s.example:6443
      bearer_token_file: `+missing+`
      group: apps
      resource: deployments
      namespaces: [shop]
    transform:
      type: jq
    metrics:
      - name: kube_deployments
        expression: .count
`)
	out := runCheckCLI(t, "--config.file="+config)
	if out.code != 0 {
		t.Fatalf("exit=%d\n%s", out.code, out.stderr)
	}
	result := out.result(t, "config")
	warnings, _ := result.Details["warnings"].([]any)
	if result.Status != checkOK || len(warnings) != 1 || !strings.HasPrefix(warnings[0].(string), `collector "deployments": request.bearer_token_file: open `+missing) {
		t.Errorf("%+v", result)
	}
}
