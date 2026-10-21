//go:build !select_request_types || request_type_tlscert

package exporter

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// tlscertCollectors are two tlscert collectors of one server: one trusting
// its certificate through ca_file, one trusting nothing but the system's
// roots, each exporting the days until the certificate expires, whether it
// expires within 14 days, and whether the chain verifies.
const tlscertCollectors = `web:
  self_metrics:
    verbose: true
collectors:
  - name: certs
    request:
      type: tlscert
      tls:
        ca_file: CA_FILE
        server_name: example.com
    transform:
      type: jq
    metrics:
      - name: tls_cert_expires_in_days
        expression: .seconds_until_expiry / 86400 | floor
      - name: tls_cert_expires_within_14_days
        expression: if .seconds_until_expiry < 14 * 86400 then 1 else 0 end
      - name: tls_cert_verified
        expression: if .verified then 1 else 0 end
  - name: certs_untrusted
    request:
      type: tlscert
    transform:
      type: jq
    metrics:
      - name: tls_cert_verified
        expression: if .verified then 1 else 0 end
`

// tlscertExporter is an exporter of tlscertCollectors and the TLS server
// they are probed against, whose certificate is valid for example.com and
// 127.0.0.1.
func tlscertExporter(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	target := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(target.Close)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	path := testutil.WriteIn(t, dir, "config.yaml", strings.Replace(tlscertCollectors, "CA_FILE", caFile, 1))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t)), target
}

// A tlscert collector's probe gives the series its jq rules read from the
// document: the days to the certificate's expiry and whether it is within
// 14 days of it, and verified 1 with ca_file and 0 without, a chain that
// does not verify being an answer and not a failure. The verbose
// self-metrics label the handshake tls://host:port and HANDSHAKE.
func TestATLSCertCollectorProbe(t *testing.T) {
	server, target := tlscertExporter(t)
	host := strings.TrimPrefix(target.URL, "https://")
	days := int(target.Certificate().NotAfter.Sub(target.Certificate().NotBefore).Hours()) / 24
	recorder := probeOnce(t, server, "/probe?collector=certs&target="+host, nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "tls_cert_verified 1") || !strings.Contains(body, "tls_cert_expires_within_14_days 0") || !strings.Contains(body, "tls_cert_expires_in_days ") || days < 14 {
		t.Errorf("%d %s", recorder.Code, body)
	}
	recorder = probeOnce(t, server, "/probe?collector=certs_untrusted&target="+host, nil)
	if body := recorder.Body.String(); recorder.Code != http.StatusOK || !strings.Contains(body, "tls_cert_verified 0") {
		t.Errorf("%d %s", recorder.Code, body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="certs",http_method="HANDSHAKE",url="tls://` + host + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
}

// A debug probe shows the handshake and the document the rules read; a
// target that is no host:port is answered 400 and one the policy refuses
// 403, before anything is dialed.
func TestADebugProbeOfATLSCertCollector(t *testing.T) {
	server, target := tlscertExporter(t)
	server.SetProbeDebug(true)
	host := strings.TrimPrefix(target.URL, "https://")
	body := probeOnce(t, server, "/probe?debug=true&collector=certs&target="+host, nil).Body.String()
	for _, want := range []string{"1. HANDSHAKE tls://" + host + " -> TLS 1.3, 1 certificate, verified", "Tls-Server-Name: example.com", `"verified":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	for query, want := range map[string]int{
		"/probe?collector=certs":                        http.StatusBadRequest,
		"/probe?collector=certs&target=https://" + host: http.StatusBadRequest,
		"/probe?collector=certs&target=169.254.169.254": http.StatusForbidden,
	} {
		if recorder := probeOnce(t, server, query, nil); recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
	}
}
