//go:build !select_request_types || request_type_graphite

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// --dry-run lists how a graphite collector connects, as it does an http
// collector's: one without a proxy of its own follows the environment's,
// whose value it never shows. In a file of its own so that a build with
// graphite alone runs it and one with http alone does not.
func TestDryRunReportsHowAGraphiteCollectorConnects(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-user:env-secret@env-proxy.example:3128")
	config := testutil.WriteFile(t, "config.yaml", `collectors:
  - name: graphite
    request: {type: graphite, targets: [app.requests]}
    decoder: {type: graphite}
    transform: {type: jq}
    metrics: [{name: v, items: '.series[]', expression: .value}]
  - name: direct
    request: {type: graphite, targets: [app.requests], proxy_url: direct}
    decoder: {type: graphite}
    transform: {type: jq}
    metrics: [{name: v, items: '.series[]', expression: .value}]
`)
	check := runCheckCLI(t, "--config.file="+config)
	policies, _ := check.result(t, "config").Details["request_policies"].(map[string]any)
	want := map[string]any{
		"graphite": map[string]any{"proxy": "environment"},
		"direct":   map[string]any{"proxy": "direct"},
	}
	if check.code != 0 || !reflect.DeepEqual(policies, want) {
		t.Fatalf("exit=%d, request_policies %v, want %v\n%s", check.code, policies, want, check.stdout)
	}
	for _, secret := range []string{"env-secret", "env-user", "env-proxy"} {
		if strings.Contains(check.stdout, secret) {
			t.Errorf("the report shows %q:\n%s", secret, check.stdout)
		}
	}
}
