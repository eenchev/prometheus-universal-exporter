//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const secretStoresDocument = `secret_stores:
  - name: vault-prod
    type: vault
    address: https://vault.example.com:8200
    auth: {method: kubernetes, role: exporter}
    cache_ttl: 10m
    max_stale: 0s
    timeout: 3s
  - name: gcp
    type: gcp
    project: demo
collectors:
  - name: api
    request:
      type: http
      bearer_token_file: secret://vault-prod/secret/data/api#token
    decoder: {type: json}
    transform: {type: jq}
    metrics:
      - name: api_up
        expression: .up
`

// secret_stores loads with its entries as written, a reference to one of
// them is taken where a credential file is named, and the exporter's own
// credentials, a static target's reference to a store the configuration
// has not, and a mistake in an entry are each refused.
func TestSecretStoresLoadAndAreChecked(t *testing.T) {
	c, err := Load(testutil.WriteFile(t, "config.yaml", secretStoresDocument))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SecretStores) != 2 || c.SecretStores[0].Auth.Role != "exporter" || c.SecretStores[0].CacheTTL == nil || *c.SecretStores[0].MaxStale != 0 || c.SecretStores[1].Project != "demo" {
		t.Fatalf("%+v", c.SecretStores)
	}
	for want, document := range map[string]string{
		"web.basic_auth names a secret store":                       secretStoresDocument + "web:\n  basic_auth: {enabled: true, username: u, password_file: 'secret://gcp/exporter-password'}\n",
		`secret_stores entry "gcp" has no project`:                  strings.Replace(secretStoresDocument, "    project: demo\n", "", 1),
		`secret_stores entry "vault-prod" has the auth.method`:      strings.Replace(secretStoresDocument, "method: kubernetes", "method: userpass", 1),
		`bearer_token_file: it names the secret store "vault-test"`: strings.Replace(secretStoresDocument, "secret://vault-prod/", "secret://vault-test/", 1),
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	targets := &model.StaticTargetFile{Targets: []model.StaticTarget{{Name: "one", Collector: "api", Target: "https://api.example.com"}}}
	targets.Targets[0].Request.PasswordFile = "secret://elsewhere/a#b"
	if err := ValidateStaticTargetsAgainst(targets, c); err == nil || !strings.Contains(err.Error(), `target "one": password_file: it names the secret store "elsewhere"`) {
		t.Errorf("a target's reference: %v", err)
	}
	targets.Targets[0].Request.PasswordFile = "secret://vault-prod/a#b"
	// One to a store it has is taken, and the target is held to the rest
	// of its checks.
	if err := ValidateStaticTargetsAgainst(targets, c); err != nil && strings.Contains(err.Error(), "secret store") {
		t.Error(err)
	}
}
