//go:build !select_request_types || request_type_http

package exporter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector whose bearer_token_file is a reference to a secret store
// sends the token the store holds, read once for the probes within
// cache_ttl; the store's requests are counted in the self-metrics; and a
// configuration whose reference names no store it has is refused.
func TestACollectorReadsItsTokenFromASecretStore(t *testing.T) {
	testutil.CaptureLogs(t)
	var reads atomic.Int64
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/data/api" || r.Header.Get("X-Vault-Token") != "root" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		reads.Add(1)
		_, _ = io.WriteString(w, `{"data":{"data":{"token":"tok-123","user":"monitor","password":"p4ss"},"metadata":{"version":1}}}`)
	}))
	t.Cleanup(vault.Close)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, basic := r.BasicAuth()
		if r.Header.Get("Authorization") != "Bearer tok-123" && !(basic && user == "monitor" && password == "p4ss" && r.URL.Path == "/basic") {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"up": 1}`)
	}))
	t.Cleanup(target.Close)
	token := testutil.WriteFile(t, "vault-token", "root\n")
	document := func(store string) string {
		return `secret_stores:
  - name: vault-prod
    type: vault
    address: ` + vault.URL + `
    auth: {method: token, token_file: ` + token + `}
collectors:
  - name: api
    request:
      type: http
      bearer_token_file: secret://` + store + `/secret/data/api#token
    decoder: {type: json}
    transform: {type: jq}
    metrics:
      - name: api_up
        expression: .up
  - name: basic
    request:
      type: http
      path: /basic
      basic_auth_file:
        username: secret://vault-prod/secret/data/api#user
        password: secret://vault-prod/secret/data/api#password
    decoder: {type: json}
    transform: {type: jq}
    metrics:
      - name: basic_up
        expression: .up
`
	}
	server := labelServer(t, document("vault-prod"))
	for range 2 {
		r := probeOnce(t, server, "/probe?collector=api&target="+url.QueryEscape(target.URL), nil)
		if got := strings.Join(seriesLines(r.Body.String()), "\n"); r.Code != http.StatusOK || got != "api_up 1" {
			t.Fatalf("%d %q: %s", r.Code, got, r.Body)
		}
	}
	// A block of credential files, basic_auth_file's username and password,
	// takes references too, and they are fields of the secret read already.
	r := probeOnce(t, server, "/probe?collector=basic&target="+url.QueryEscape(target.URL), nil)
	if got := strings.Join(seriesLines(r.Body.String()), "\n"); r.Code != http.StatusOK || got != "basic_up 1" {
		t.Fatalf("basic auth from the store: %d %q: %s", r.Code, got, r.Body)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("the store was asked %d times for three probes of one secret within cache_ttl", n)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	for _, want := range []string{
		`http_exporter_secret_store_requests_total{store="vault-prod"} 1`,
		`http_exporter_secret_store_failures_total{store="vault-prod"} 0`,
		`http_exporter_secret_store_stale_reads_total{store="vault-prod"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("no %s in the self-metrics", want)
		}
	}
	_, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", document("vault-test")))
	if err == nil || !strings.Contains(err.Error(), `bearer_token_file: it names the secret store "vault-test", and secret_stores has none of that name`) {
		t.Errorf("a reference to a store the configuration has not: %v", err)
	}
}
