//go:build !select_request_types || request_type_tlscert

package exporter

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// certSubmission is a stand-in SMTP server: it greets, answers EHLO with
// ehlo and STARTTLS with starttls, and, when that is 220, makes the
// server's side of the handshake. Each line it read in plaintext is sent
// on plain.
func certSubmission(ehlo, starttls string, plain chan<- string) func(net.Conn, func() (net.Conn, error)) {
	return func(conn net.Conn, begin func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, "220 mx.internal.example ESMTP\r\n")
		for _, reply := range []string{ehlo, starttls} {
			line, err := testutil.ReadUntil(conn, "\n")
			if err != nil {
				return
			}
			plain <- line
			_, _ = io.WriteString(conn, reply)
		}
		if strings.HasPrefix(starttls, "220 ") {
			_, _ = begin()
		}
	}
}

// Through the exporter a tlscert collector with request.starttls smtp reads
// a mail server's certificate: EHLO and STARTTLS in plaintext, then the
// handshake, and jq rules read the expiry, verified and starttls from the
// document. A server that refuses STARTTLS fails the probe in the tls
// stage, its caller told the step with the server's words withheld, which
// the log and a debug probe's report show, the report naming the protocol
// and each step. A static target cannot set starttls: it is the
// collector's alone.
func TestATLSCertSTARTTLSThroughTheExporter(t *testing.T) {
	plain := make(chan string, 8)
	good := testutil.NewStartTLSStub(t, certSubmission("250-mx.internal.example\r\n250 STARTTLS\r\n", "220 2.0.0 Ready\r\n", plain))
	refusing := testutil.NewStartTLSStub(t, certSubmission("250-mx.internal.example\r\n250 STARTTLS\r\n", "454 4.7.0 TLS not available at mx.internal.example\r\n", plain))
	dir := t.TempDir()
	collectors := `collectors:
  - name: mail_cert
    request:
      type: tlscert
      starttls: smtp
      tls:
        ca_file: ` + good.CAFile + `
    transform:
      type: jq
    metrics:
      - name: tls_cert_expires_in_days
        expression: .seconds_until_expiry / 86400 | floor
      - name: tls_cert_verified
        expression: if .verified then 1 else 0 end
      - name: tls_cert_starttls_smtp
        expression: if .starttls == "smtp" then 1 else 0 end
`
	path := testutil.WriteIn(t, dir, "config.yaml", collectors)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	exp := NewServer(config.NewManager(cfg, path, logger), "python3", logger)
	recorder := probeOnce(t, exp, "/probe?collector=mail_cert&target="+good.Addr, nil)
	got := seriesLines(recorder.Body.String())
	if recorder.Code != http.StatusOK || len(got) != 3 || !strings.HasPrefix(got[0], "tls_cert_expires_in_days ") || got[1] != "tls_cert_starttls_smtp 1" || got[2] != "tls_cert_verified 1" {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if ehlo, starttls := <-plain, <-plain; ehlo != "EHLO [127.0.0.1]\r\n" || starttls != "STARTTLS\r\n" {
		t.Errorf("sent in plaintext: %q, %q", ehlo, starttls)
	}
	recorder = probeOnce(t, exp, "/probe?collector=mail_cert&target="+refusing.Addr, nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusBadGateway || !strings.Contains(body, "collector mail_cert tls failed: the server "+refusing.Addr+" did not begin TLS: it answered STARTTLS with [withheld; in the exporter's log], not 220") || strings.Contains(body, "mx.internal.example") {
		t.Errorf("a refusal: %d %s", recorder.Code, body)
	}
	if !strings.Contains(logs.String(), "TLS not available at mx.internal.example") {
		t.Errorf("the log does not show the refusal:\n%s", logs.String())
	}
	exp.SetDebug(DebugFull)
	report := probeOnce(t, exp, "/probe?debug=true&collector=mail_cert&target="+good.Addr, nil).Body.String()
	for _, want := range []string{
		"HANDSHAKE tls://" + good.Addr + ` -> greeted with "220 mx.internal.example ESMTP\r\n"; sent "EHLO [127.0.0.1]\r\n", answered "250-mx.internal.example\r\n250 STARTTLS\r\n"; sent "STARTTLS\r\n", answered "220 2.0.0 Ready\r\n"; TLS 1.3, 1 certificate, verified`,
		"Tls-Starttls: smtp",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("no %q in the report:\n%s", want, report)
		}
	}
	targets := `targets:
  - name: mail
    collector: mail_cert
    target: ` + good.Addr + `
    request:
      starttls: imap
`
	file, err := config.ParseStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", targets), []byte(targets))
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err == nil || !strings.Contains(err.Error(), `unknown key "starttls" in a static target's request`) {
		t.Errorf("a static target setting starttls: %v", err)
	}
}
