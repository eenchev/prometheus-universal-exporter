//go:build request_type_ldap

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// starttls, the protocol tcp and tlscert speak STARTTLS in, is no key of an
// ldap collector, which asks for StartTLS with its own boolean, start_tls.
// Written true, it was refused as text that YAML read as a boolean, told to
// write smtp, imap, pop3 or ftp, which would have been refused next as a key
// ldap does not take: the refusal now points to start_tls: true, however the
// key is written.
func TestAnLDAPCollectorsStartTLSIsToldItsOwnKey(t *testing.T) {
	const want = `collector "directory" sets request.starttls, which does not apply to request.type "ldap"; an ldap collector asks for StartTLS on its plaintext connection with its own key, start_tls: true`
	for _, written := range []string{"true", "smtp", "'true'", "yes", "1"} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: directory
    request:
      type: ldap
      starttls: `+written+`
      searches:
        - name: people
          base: ou=people,dc=example,dc=org
          count_only: true
    transform:
      type: jq
    metrics:
      - name: people
        expression: .searches.people.count
`))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "pop3") && !strings.Contains(err.Error(), want) {
			t.Errorf("starttls: %s: %v\nwant %s", written, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "write one of smtp") {
			t.Errorf("starttls: %s: told tcp's protocols: %v", written, err)
		}
	}
}
