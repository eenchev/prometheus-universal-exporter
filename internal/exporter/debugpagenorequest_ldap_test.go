//go:build !select_request_types || request_type_ldap

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An ldap playground reads the pasted entries, the JSON document the
// exporter makes of them, and binds to no server; it is written without
// the password file its bind DN would need, which it may not name.
func TestAnLDAPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			password := ""
			if source {
				password = "password_file: " + testutil.WriteFile(t, "password", "pw\n") + ", "
			}
			return "name: " + name + "\nrequest: {type: ldap, bind_dn: 'cn=exporter,dc=example,dc=org', " + password + "searches: [{name: people, base: 'dc=example,dc=org', filter: '(objectClass=person)', count_only: true}]}\ntransform: {type: jq}\nmetrics: [{name: people, expression: .searches.people.count}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"searches": {"people": {"count": 3}}}`, series: "people 3\n",
	})
	requirePlaygroundRefusesFiles(t, "ldap")
}
