//go:build request_type_ldap

package repository

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/LDAP.md work as written against a stand-in server,
// in a build with the type: the OpenLDAP collector's connections,
// statistics and operations from cn=Monitor, the Active Directory
// collector's disabled and locked accounts, and the members of the group a
// probe names; the TLS block and the search with placeholders load, and so
// do the static targets against the collectors.
func TestLDAPDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/LDAP.md", "yaml")
	server := ldaptest.New(t)
	for _, dn := range []string{"cn=monitor,dc=example,dc=org", "CN=prometheus,OU=Service Accounts,DC=corp,DC=example,DC=com", "cn=exporter,ou=services,dc=example,dc=org"} {
		server.AddAccount(dn, "s3cret")
	}
	counter := func(dn, value string) ldaptest.Entry {
		return ldaptest.Entry{DN: dn, Attributes: map[string][]string{"objectClass": {"monitorCounterObject"}, "monitorCounter": {value}}}
	}
	operation := func(dn, initiated, completed string) ldaptest.Entry {
		return ldaptest.Entry{DN: dn, Attributes: map[string][]string{"objectClass": {"monitorOperation"}, "monitorOpInitiated": {initiated}, "monitorOpCompleted": {completed}}}
	}
	user := func(cn, control, lockout string) ldaptest.Entry {
		return ldaptest.Entry{DN: "CN=" + cn + ",OU=Users,DC=corp,DC=example,DC=com", Attributes: map[string][]string{
			"objectClass": {"top", "person", "organizationalPerson", "user"}, "objectCategory": {"person"}, "userAccountControl": {control}, "lockoutTime": {lockout},
		}}
	}
	server.AddEntry(
		ldaptest.Entry{DN: "cn=Monitor", Attributes: map[string][]string{"objectClass": {"monitorServer"}}},
		ldaptest.Entry{DN: "cn=Connections,cn=Monitor", Attributes: map[string][]string{"objectClass": {"monitorContainer"}}},
		counter("cn=Current,cn=Connections,cn=Monitor", "7"),
		counter("cn=Total,cn=Connections,cn=Monitor", "1234"),
		ldaptest.Entry{DN: "cn=Statistics,cn=Monitor", Attributes: map[string][]string{"objectClass": {"monitorContainer"}}},
		counter("cn=Bytes,cn=Statistics,cn=Monitor", "56789"),
		counter("cn=PDU,cn=Statistics,cn=Monitor", "300"),
		operation("cn=Operations,cn=Monitor", "35", "34"),
		operation("cn=Bind,cn=Operations,cn=Monitor", "10", "10"),
		operation("cn=Search,cn=Operations,cn=Monitor", "25", "24"),
		ldaptest.Entry{DN: "DC=corp,DC=example,DC=com", Attributes: map[string][]string{"objectClass": {"domain"}}},
		user("Ada", "512", "0"),
		user("Alan", "514", "0"),
		user("Grace", "66050", "0"),
		user("Linus", "512", "133400000000000000"),
		ldaptest.Entry{DN: "cn=admins,ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"groupOfNames"}, "member": {"uid=ada,ou=people,dc=example,dc=org", "uid=alan,ou=people,dc=example,dc=org", "uid=grace,ou=people,dc=example,dc=org"}}},
	)
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := pageBlock(t, blocks, "collectors:\n") + pageBlock(t, blocks, "  - name: ad_accounts\n") + pageBlock(t, blocks, "  - name: group_members\n")
	conf = strings.ReplaceAll(conf, "/var/run/secrets/ldap/password", passwordFile)
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	if len(cfg.Collectors) != 3 {
		t.Fatalf("%d collectors loaded, want the three examples", len(cfg.Collectors))
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for probe, wants := range map[string][]string{
		"collector=openldap": {
			"openldap_connections_current 7", "openldap_connections_total 1234",
			`openldap_statistics_total{statistic="bytes"} 56789`, `openldap_statistics_total{statistic="pdu"} 300`,
			`openldap_operations_completed_total{operation="bind"} 10`, `openldap_operations_completed_total{operation="search"} 24`,
		},
		"collector=ad_accounts":                      {"ad_accounts_disabled 2", "ad_accounts_locked 1"},
		"collector=group_members&param_group=admins": {`ldap_group_members{group="admins"} 3`},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+server.Addr+"&"+probe, nil))
		for _, want := range wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s: %d, want %q in\n%s", probe, recorder.Code, want, recorder.Body)
			}
		}
		if probe == "collector=openldap" && strings.Contains(recorder.Body.String(), `operation="operations"`) {
			t.Errorf("the operations' container is counted as an operation:\n%s", recorder.Body)
		}
	}
	collector := func(request string) string {
		return "collectors:\n  - name: c\n    " + strings.ReplaceAll(strings.TrimSuffix(request, "\n"), "\n", "\n    ") + "\n    transform:\n      type: jq\n    metrics:\n      - name: v\n        expression: .searches[].count\n"
	}
	withTLS := collector(pageBlock(t, blocks, "request:\n  type: ldap\n  start_tls: true\n"))
	if _, err := config.Load(testutil.WriteIn(t, dir, "tls.yaml", withTLS)); err != nil {
		t.Errorf("%v\n%s", err, withTLS)
	}
	placeholders := collector("request:\n  type: ldap\n  " + strings.ReplaceAll(strings.TrimSuffix(pageBlock(t, blocks, "searches:\n  - name: user\n"), "\n"), "\n", "\n  ") + "\n")
	if _, err := config.Load(testutil.WriteIn(t, dir, "placeholders.yaml", placeholders)); err != nil {
		t.Errorf("%v\n%s", err, placeholders)
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", pageBlock(t, blocks, "interval: 60s\ntargets:\n")))
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
