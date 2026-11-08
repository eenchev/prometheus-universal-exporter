//go:build request_type_ldap

package fetch

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An ldap connection's phases are its first operation's: with StartTLS,
// STARTTLS's, the connection and then the handshake STARTTLS began; the
// BIND and the SEARCH after it have none.
func TestAnLDAPConnectionsPhasesAreItsFirstOperations(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	startTLS := ldapCollector(t, func(c *model.Collector) {
		c.Request.TLS.CAFile = server.CAFile
		editLDAP(c, func(r *LDAPConfig) { r.StartTLS = true })
	})
	requests := spanTracedRequests(t, startTLS, server.Addr)
	var got []string
	for _, r := range requests {
		got = append(got, r.Method+":"+phaseNames(t, r))
	}
	if len(got) != 3 || got[0] != "STARTTLS:connection.connect,connection.tls" || got[1] != "BIND:" || got[2] != "SEARCH:" {
		t.Errorf("phases %q", got)
	}
}
