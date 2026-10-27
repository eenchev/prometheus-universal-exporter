//go:build !select_request_types || request_type_tlscert

package repository

import (
	"encoding/pem"
	"io"
	"net"
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

// certStartTLSStandIn greets, answers each line sent in plaintext with the
// next of replies, the last of which says to begin TLS, and makes the
// server's side of the handshake.
func certStartTLSStandIn(greeting string, replies ...string) func(net.Conn, func() (net.Conn, error)) {
	return func(conn net.Conn, begin func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, greeting)
		for _, reply := range replies {
			if _, err := testutil.ReadUntil(conn, "\n"); err != nil {
				return
			}
			_, _ = io.WriteString(conn, reply)
		}
		_, _ = begin()
	}
}

// The STARTTLS examples of docs/TLSCERT.md work as written against
// stand-ins that begin TLS as an SMTP, an IMAP and a POP3 server do: each
// collector's days left and its flag for a certificate expiring within 14
// days, the stand-ins' certificate being one no system root signed, which
// SMTP's verified series says, and does not fail the scrape.
func TestTLSCertStartTLSDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/TLSCERT.md", "yaml")
	smtp := testutil.NewStartTLSStub(t, certStartTLSStandIn("220 mx.example.com ESMTP\r\n", "250-mx.example.com\r\n250-PIPELINING\r\n250 STARTTLS\r\n", "220 2.0.0 Ready to start TLS\r\n"))
	imap := testutil.NewStartTLSStub(t, certStartTLSStandIn("* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n", "a1 OK Begin TLS negotiation now\r\n"))
	pop3 := testutil.NewStartTLSStub(t, certStartTLSStandIn("+OK POP3 ready\r\n", "+OK Begin TLS negotiation now\r\n"))
	conf := "collectors:\n" + pageBlock(t, blocks, "  - name: smtp_certificate\n")
	dir := t.TempDir()
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for collector, tc := range map[string]struct {
		target string
		wants  []string
	}{
		"smtp_certificate": {smtp.Addr, []string{"smtp_certificate_days_left ", "smtp_certificate_expiring 0", "smtp_certificate_verified 0"}},
		"imap_certificate": {imap.Addr, []string{"imap_certificate_days_left ", "imap_certificate_expiring 0"}},
		"pop3_certificate": {pop3.Addr, []string{"pop3_certificate_days_left ", "pop3_certificate_expiring 0"}},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+tc.target+"&collector="+collector, nil))
		for _, want := range tc.wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "\n"+want) {
				t.Errorf("%s: %d, want %q in\n%s", collector, recorder.Code, want, recorder.Body)
			}
		}
	}
}
