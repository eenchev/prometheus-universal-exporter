//go:build !select_request_types || request_type_http

package exporter

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One caching http collector, with no client certificate of its own, serves
// an API that asks for one through a static target's own tls block: the
// target with the API's CA and a certificate reads it; the target of the
// collector's block, and one whose own block, tls: {}, verifies with the
// system roots alone, are refused by the handshake rather than answered
// from the cache entry of the target beside them. A probe cannot name a
// tls block: no probe parameter is the target's tls.
func TestHTTPStaticTargetsConnectWithTheirOwnTLSBlocks(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "value=7\n")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	// The server's own certificate and key are the client's too: the
	// server takes any certificate, and refuses a handshake without one.
	dir := t.TempDir()
	key, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, certFile, keyFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key")
	for file, block := range map[string]*pem.Block{
		ca:       {Type: "CERTIFICATE", Bytes: server.Certificate().Raw},
		certFile: {Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]},
		keyFile:  {Type: "PRIVATE KEY", Bytes: key},
	} {
		if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exp := staticServerFromFiles(t, dir, `collectors:
  - name: api
    request:
      type: http
      tls:
        ca_file: `+ca+`
    cache:
      ttl: 1h
    transform:
      type: regex
    metrics:
      - name: api_value
        expression: 'value=(\d+)'
`, `interval: 1m
concurrency: 1
targets:
  - name: own_certificate
    collector: api
    target: `+server.URL+`/data
    request:
      tls: {ca_file: `+ca+`, cert_file: `+certFile+`, key_file: `+keyFile+`}
  - name: system_roots
    collector: api
    target: `+server.URL+`/data
    request:
      tls: {}
  - name: collectors
    collector: api
    target: `+server.URL+`/data
`)
	exp.scrapeStaticTargets(t.Context(), 0)
	body := probeOnce(t, exp, "/static-targets", nil).Body.String()
	if want := `api_value{static_target="own_certificate"} 7`; !strings.Contains(body, want) {
		t.Errorf("no %s in\n%s", want, body)
	}
	checkTargetsUp(t, body, map[string]string{"own_certificate": "1", "system_roots": "0", "collectors": "0"})
	query := url.Values{"collector": {"api"}, "target": {server.URL + "/data"}, "tls": {"true"}, "cert_file": {certFile}, "key_file": {keyFile}, "tls.cert_file": {certFile}, "tls.key_file": {keyFile}}
	if recorder := probeOnce(t, exp, "/probe?"+query.Encode(), nil); recorder.Code == http.StatusOK && strings.Contains(recorder.Body.String(), "api_value 7") {
		t.Errorf("a probe naming a client certificate was answered with it: %s", recorder.Body)
	}
}
