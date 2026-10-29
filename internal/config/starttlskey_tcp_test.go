//go:build request_type_tcp

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// start_tls, ldap's boolean StartTLS, is no key of a tcp collector, which
// names the protocol that asks for TLS in starttls: the refusal of the key
// says so.
func TestATCPCollectorsStartTLSIsToldItsOwnKey(t *testing.T) {
	const want = `collector "smtp" sets request.start_tls, which does not apply to request.type "tcp"; a tcp collector begins TLS on its plaintext connection with starttls, which names the protocol that asks for it: smtp, imap, pop3 or ftp`
	_, err := Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: smtp\n    request:\n      type: tcp\n      start_tls: true\n    transform:\n      type: regex\n    metrics:\n      - name: up\n        expression: '(1)'\n"))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%v\nwant %s", err, want)
	}
}
