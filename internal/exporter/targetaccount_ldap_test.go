//go:build request_type_ldap

package exporter

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
)

// One caching ldap collector, binding as cn=exporter, serves directories
// that each take an account of their own: a static target's bind_dn and
// password_file bind where the collector's account is refused, its own tls
// block reaches an ldaps server the collector would speak plaintext to,
// and a target with a wrong password is refused, not answered from the
// cache entry of the target beside it with the right one. A probe cannot
// name an account: bind_dn and password_file are not the probe's to set,
// and the collector's account is used. The debug report of a target shows
// its bind with the password masked.
func TestLDAPStaticTargetsBindWithTheirOwnAccounts(t *testing.T) {
	entries := []ldaptest.Entry{
		{DN: "dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"domain"}}},
		{DN: "uid=ada,ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"person"}, "uid": {"ada"}}},
	}
	tenant := ldaptest.New(t)
	tenant.AddAccount("cn=tenant,dc=example,dc=org", "t-pass")
	tenant.DenyAnonymous()
	tenant.AddEntry(entries...)
	secure := ldaptest.NewTLS(t)
	secure.AddAccount("cn=secure,dc=example,dc=org", "tls-pass")
	secure.DenyAnonymous()
	secure.AddEntry(entries...)
	dir := t.TempDir()
	files := writeSecrets(t, dir, map[string]string{"collector": "s3cret", "tenant": "t-pass", "wrong": "nope", "secure": "tls-pass"})
	server := staticServerFromFiles(t, dir, `collectors:
  - name: directory
    request:
      type: ldap
      bind_dn: cn=exporter,dc=example,dc=org
      password_file: `+files["collector"]+`
      searches:
        - name: people
          base: dc=example,dc=org
          filter: (objectClass=person)
          count_only: true
    cache:
      ttl: 1h
    transform:
      type: jq
    metrics:
      - name: ldap_people
        expression: .searches.people.count
`, `interval: 1m
concurrency: 1
targets:
  - name: own
    collector: directory
    target: `+tenant.Addr+`
    request: {bind_dn: "cn=tenant,dc=example,dc=org", password_file: `+files["tenant"]+`}
  - name: wrong
    collector: directory
    target: `+tenant.Addr+`
    request: {bind_dn: "cn=tenant,dc=example,dc=org", password_file: `+files["wrong"]+`}
  - name: collectors_account
    collector: directory
    target: `+tenant.Addr+`
  - name: secure
    collector: directory
    target: `+secure.Addr+`
    request:
      bind_dn: cn=secure,dc=example,dc=org
      password_file: `+files["secure"]+`
      tls: {ca_file: `+secure.CAFile+`}
`)
	server.scrapeStaticTargets(t.Context(), 0)
	body := probeOnce(t, server, "/static-targets", nil).Body.String()
	for _, want := range []string{`ldap_people{static_target="own"} 1`, `ldap_people{static_target="secure"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s in\n%s", want, body)
		}
	}
	checkTargetsUp(t, body, map[string]string{"own": "1", "wrong": "0", "collectors_account": "0", "secure": "1"})
	binds := func() []string {
		var sent []string
		for _, r := range tenant.Requests() {
			if r.Op == "BIND" {
				sent = append(sent, r.DN+" "+r.Password)
			}
		}
		return sent
	}
	if got, want := binds(), []string{"cn=tenant,dc=example,dc=org t-pass", "cn=tenant,dc=example,dc=org nope", "cn=exporter,dc=example,dc=org s3cret"}; !slices.Equal(got, want) {
		t.Errorf("the server was sent the binds %q, want %q", got, want)
	}

	before := len(binds())
	query := url.Values{"collector": {"directory"}, "target": {tenant.Addr}, "bind_dn": {"cn=tenant,dc=example,dc=org"}, "password_file": {files["tenant"]}}
	if recorder := probeOnce(t, server, "/probe?"+query.Encode(), nil); recorder.Code == http.StatusOK {
		t.Errorf("a probe naming the target's account was answered: %s", recorder.Body)
	}
	if got := binds()[before:]; !slices.Equal(got, []string{"cn=exporter,dc=example,dc=org s3cret"}) {
		t.Errorf("a probe naming an account bound with %q, want the collector's account", got)
	}

	server.SetDebug(DebugFull)
	report := probeOnce(t, server, "/static-targets?debug=own", nil).Body.String()
	if !strings.Contains(report, "cn=tenant,dc=example,dc=org ***") || strings.Contains(report, "t-pass") {
		t.Errorf("the report does not show the bind with its password masked:\n%s", report)
	}
}
