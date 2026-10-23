//go:build request_type_ldap

package exporter

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// ldapExporter is an exporter, with verbose self-metrics, of an ldap
// collector counting people and listing a group's members, and one
// finding the user a probe names, both binding to the stand-in server it
// returns as cn=exporter.
func ldapExporter(t *testing.T) (*Server, *ldaptest.Server) {
	t.Helper()
	server := ldaptest.New(t)
	server.AddAccount("cn=exporter,dc=example,dc=org", "s3cret")
	server.DenyAnonymous()
	server.AddEntry(
		ldaptest.Entry{DN: "dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"domain"}}},
		ldaptest.Entry{DN: "uid=ada,ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"person"}, "uid": {"ada"}, "cn": {"Ada Lovelace"}}},
		ldaptest.Entry{DN: "uid=alan,ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"person"}, "uid": {"alan"}, "cn": {"Alan Turing"}}},
		ldaptest.Entry{DN: "cn=admins,ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"groupOfNames"}, "member": {"uid=ada,ou=people,dc=example,dc=org", "uid=alan,ou=people,dc=example,dc=org"}}},
	)
	dir := t.TempDir()
	passwordFile := filepath.Join(dir, "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := testutil.WriteIn(t, dir, "config.yaml", `web:
  self_metrics:
    verbose: true
collectors:
  - name: directory
    request:
      type: ldap
      bind_dn: cn=exporter,dc=example,dc=org
      password_file: `+passwordFile+`
      searches:
        - name: people
          base: dc=example,dc=org
          filter: (objectClass=person)
          count_only: true
        - name: admins
          base: cn=admins,ou=groups,dc=example,dc=org
          scope: base
          attributes: [member]
    transform:
      type: jq
    metrics:
      - name: ldap_people
        expression: .searches.people.count
      - name: ldap_group_members
        expression: '.searches.admins.entries[0].attributes.member | length'
  - name: user
    request:
      type: ldap
      bind_dn: cn=exporter,dc=example,dc=org
      password_file: `+passwordFile+`
      searches:
        - name: user
          base: dc=example,dc=org
          filter: (&(objectClass=person)(uid={{param_user}}))
          attributes: [cn]
      denied_targets: [10.0.0.0/8]
    transform:
      type: jq
    metrics:
      - name: ldap_user_matches
        expression: .searches.user.count
        labels:
          - name: user
            value: "{{param_user}}"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t)), server
}

// An ldap collector's probe gives the series its jq rules read from the
// document, the count of a count_only search and the members of a group;
// a probe's value fills the filter escaped, so *)( matches no one; the
// verbose self-metrics label the scrape with ldap://host:port and SEARCH;
// a refused target is answered 403 and counted; a target with a path is
// answered 400; and the collectors page hints the target as a server.
func TestAnLDAPCollectorProbe(t *testing.T) {
	exp, server := ldapExporter(t)
	recorder := probeOnce(t, exp, "/probe?collector=directory&target="+server.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != "ldap_group_members 2\nldap_people 2" {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	for user, want := range map[string]string{"ada": `ldap_user_matches{user="ada"} 1`, "*": `ldap_user_matches{user="*"} 0`, "*)(uid=*": `ldap_user_matches{user="*)(uid=*"} 0`} {
		recorder = probeOnce(t, exp, "/probe?collector=user&param_user="+url.QueryEscape(user)+"&target=ldap://"+server.Addr, nil)
		if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
			t.Errorf("%s: %d %v", user, recorder.Code, got)
		}
	}
	if recorder := probeOnce(t, exp, "/probe?collector=user&param_user=ada&target=10.1.2.3", nil); recorder.Code != http.StatusForbidden {
		t.Errorf("a denied target: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := probeOnce(t, exp, "/probe?collector=directory&target="+server.Addr+"/dc=org", nil); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "the base is each search's") {
		t.Errorf("a target with a path: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, exp, "/self-metrics", nil).Body.String()
	for _, want := range []string{
		`http_exporter_scrapes_total{collector="directory",http_method="SEARCH",url="ldap://` + server.Addr + `"} 1`,
		`http_exporter_scrapes_total{collector="user",http_method="SEARCH",url="ldap://` + server.Addr + `"} 3`,
		`http_exporter_targets_refused_total{collector="user"} 1`,
		`http_exporter_targets_refused_total{collector="directory"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("no %s in\n%s", want, metrics)
		}
	}
	if page := probeOnce(t, exp, "/collectors", nil).Body.String(); !strings.Contains(page, `placeholder="host, host:port or ldaps://host:port"`) {
		t.Errorf("the collectors page hints no ldap target:\n%s", page)
	}
}

// A debug probe shows each operation, the bind with its DN and its password
// masked and each search with its filter, the probe's value escaped in it,
// and the document; the password is nowhere in the report.
func TestADebugProbeOfAnLDAPCollector(t *testing.T) {
	exp, server := ldapExporter(t)
	exp.SetDebug(DebugFull)
	body := probeOnce(t, exp, "/probe?debug=true&collector=user&param_user="+url.QueryEscape("a*")+"&target="+server.Addr, nil).Body.String()
	for _, want := range []string{
		"1. BIND ldap://" + server.Addr + " -> success",
		"Ldap-Operation: BIND cn=exporter,dc=example,dc=org ***",
		"2. SEARCH ldap://" + server.Addr + " (page 1) -> success, 0 entries",
		`Ldap-Operation: SEARCH user base="dc=example,dc=org" scope=sub filter=(&(objectClass=person)(uid=a\2a)) attributes=cn`,
		`{"searches":{"user":{"count":0,"references":0,"entries":[]}}}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	if strings.Contains(body, "s3cret") {
		t.Errorf("the report shows the password:\n%s", body)
	}
}

// An ldap collector's document is read by a python transform as data, each
// entry with its attributes' lists of values, and its label values take the
// probe's param_<name> parameters, as every type's do.
func TestAnLDAPCollectorsDocumentInPython(t *testing.T) {
	requirePython(t)
	server := ldaptest.New(t)
	server.AddEntry(
		ldaptest.Entry{DN: "ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"organizationalUnit"}}},
		ldaptest.Entry{DN: "cn=admins,ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"groupOfNames"}, "cn": {"admins"}, "member": {"uid=a,dc=org", "uid=b,dc=org"}}},
		ldaptest.Entry{DN: "cn=staff,ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"groupOfNames"}, "cn": {"staff"}, "member": {"uid=a,dc=org"}}},
	)
	exp := labelServer(t, `collectors:
  - name: groups
    request:
      type: ldap
      searches:
        - name: groups
          base: ou=groups,dc=example,dc=org
          scope: one
          filter: (objectClass=groupOfNames)
          attributes: [cn, member]
    transform:
      type: python
      labels:
        env: "{{param_env:dev}}"
      script: |
        for entry in data["searches"]["groups"]["entries"]:
            metric(name="ldap_group_members", type="gauge", value=len(entry["attributes"]["member"]), labels={"group": entry["attributes"]["cn"][0]})
    metrics: []
`)
	recorder := probeOnce(t, exp, "/probe?collector=groups&param_env=prod&target="+server.Addr, nil)
	want := []string{`ldap_group_members{env="prod",group="admins"} 2`, `ldap_group_members{env="prod",group="staff"} 1`}
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v\n%s", recorder.Code, got, want, recorder.Body)
	}
}
