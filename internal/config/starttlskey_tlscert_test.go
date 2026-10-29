//go:build !select_request_types || request_type_tlscert

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// start_tls, ldap's boolean StartTLS, is no key of a tlscert collector,
// which names the protocol that asks for TLS in starttls: the refusal of the
// key says so, written as a boolean or not.
func TestATLSCertCollectorsStartTLSIsToldItsOwnKey(t *testing.T) {
	const want = `collector "certs" sets request.start_tls, which does not apply to request.type "tlscert"; a tlscert collector begins TLS on its plaintext connection with starttls, which names the protocol that asks for it: smtp, imap, pop3 or ftp`
	for _, written := range []string{"true", "smtp"} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: certs\n    request:\n      type: tlscert\n      start_tls: "+written+"\n    transform:\n      type: jq\n    metrics:\n      - name: days\n        expression: .certificates[0].not_after\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("start_tls: %s: %v\nwant %s", written, err, want)
		}
	}
}

// The decoder's word on the value of a key the type does not take is not
// said before the key is refused, but a word on another key on the same
// line is: one line of a flow mapping holds both.
func TestAKeyOfAnotherTypeHidesNoOtherKeysMistakeOnItsLine(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: certs\n    request: {type: tlscert, start_tls: smtp, allowed_targets: {a: b}}\n    transform:\n      type: jq\n    metrics:\n      - name: days\n        expression: .certificates[0].not_after\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3: expected a list") {
		t.Errorf("the timeout's mistake is not said: %v", err)
	}
}
