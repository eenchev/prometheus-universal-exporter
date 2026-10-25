//go:build request_type_ldap

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// An ldap collector's placeholders are in its searches' base and filter.
func init() {
	ownPlaceholders[RequestTypeLDAP] = func(c *model.Collector) {
		c.Request.Own = LDAPConfig{Searches: []LDAPSearch{{Name: "n", Base: "ou={{param_ou:x}},dc=org", Filter: "(uid={{param_uid:x}})"}}}
	}
}

// editLDAP edits an ldap collector's own keys.
func editLDAP(c *model.Collector, edit func(*LDAPConfig)) {
	r := ldapRequest(c)
	edit(&r)
	c.Request.Own = r
}

// ldapCollector is a validated ldap collector of one search of people, as
// edit makes it.
func ldapCollector(t *testing.T, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "directory", Request: model.RequestConfig{Type: RequestTypeLDAP, Own: LDAPConfig{Searches: []LDAPSearch{
		{Name: "people", Base: "ou=people,dc=example,dc=org", Filter: "(objectClass=person)", Attributes: []string{"cn", "mail"}},
	}}}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// ldapScrape is a scrape of target through c, with the probe's parameters,
// and its document as JSON.
func ldapScrape(t *testing.T, c *model.Collector, target string, params map[string]string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{Params: params}, nil)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// ldapPeople is a small directory: two people, a group of them and a
// disabled account.
func ldapPeople(server *ldaptest.Server) {
	server.AddEntry(
		ldaptest.Entry{DN: "ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"organizationalUnit"}, "ou": {"people"}}},
		ldaptest.Entry{DN: "uid=ada,ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"person"}, "cn": {"Ada Lovelace"}, "mail": {"ada@example.org"}, "uid": {"ada"}}},
		ldaptest.Entry{DN: "uid=alan,ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"person"}, "cn": {"Alan Turing"}, "uid": {"alan"}, "userAccountControl": {"514"}}},
		ldaptest.Entry{DN: "cn=admins,ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"groupOfNames"}, "cn": {"admins"}, "member": {"uid=ada,ou=people,dc=example,dc=org", "uid=alan,ou=people,dc=example,dc=org"}}},
	)
}

// An ldap collector binds anonymously, searches, and answers each search's
// entries under its name: each entry's DN and the attributes asked for,
// named as the server names them, each a list of values, with the count of
// the entries and of the references. A search with no attributes asks for
// 1.1, the DNs alone, and the scope is sub when left out; the connection
// ends with an unbind.
func TestAnLDAPCollectorAnswersItsSearchesAsADocument(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	server.SetReferences("ldap://other.example.org/ou=people,dc=example,dc=org")
	c := ldapCollector(t, func(c *model.Collector) {
		editLDAP(c, func(r *LDAPConfig) {
			r.Searches = append(r.Searches, LDAPSearch{Name: "groups", Base: "dc=example,dc=org", Scope: "sub", Filter: "(objectClass=groupOfNames)"})
		})
	})
	body, err := ldapScrape(t, c, server.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"searches":{"groups":{"count":1,"references":1,"entries":[{"dn":"cn=admins,ou=groups,dc=example,dc=org","attributes":{}}]},` +
		`"people":{"count":2,"references":1,"entries":[{"dn":"uid=ada,ou=people,dc=example,dc=org","attributes":{"cn":["Ada Lovelace"],"mail":["ada@example.org"]}},{"dn":"uid=alan,ou=people,dc=example,dc=org","attributes":{"cn":["Alan Turing"]}}]}}}`
	if body != want {
		t.Errorf("document\n%s\nwant\n%s", body, want)
	}
	var ops []string
	for _, r := range server.Requests() {
		op := r.Op
		if r.Op == "SEARCH" {
			op += fmt.Sprintf(" %s %s %s %v paged=%v critical=%v", r.Base, r.Scope, r.Filter, r.Attributes, r.Paged, r.Critical)
		}
		if r.Op == "BIND" {
			op += fmt.Sprintf(" %q %q", r.DN, r.Password)
		}
		ops = append(ops, op)
	}
	wantOps := []string{
		`BIND "" ""`,
		"SEARCH ou=people,dc=example,dc=org sub (objectClass=person) [cn mail] paged=true critical=false",
		"SEARCH dc=example,dc=org sub (objectClass=groupOfNames) [1.1] paged=true critical=false",
	}
	if len(ops) > 0 && ops[len(ops)-1] == "UNBIND" {
		ops = ops[:len(ops)-1]
	}
	if strings.Join(ops, "\n") != strings.Join(wantOps, "\n") {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(ops, "\n"), strings.Join(wantOps, "\n"))
	}
	// The unbind is sent last, and the server reads it in its own time.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		requests := server.Requests()
		if requests[len(requests)-1].Op == "UNBIND" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("no unbind: %+v", requests)
			break
		}
	}
	if method := RequestMethodFor(c, RequestOverrides{}); method != "SEARCH" {
		t.Errorf("method %q", method)
	}
	if label, err := RequestLabelFor(server.Addr, c, RequestOverrides{}); err != nil || label != "ldap://"+server.Addr {
		t.Errorf("label %q, %v", label, err)
	}
}

// With bind_dn the scrape binds as the account with the password of
// password_file, read at every scrape; a wrong password fails the scrape
// with invalidCredentials and the server's words, and never the password,
// and a server that refuses an anonymous search fails it with
// insufficientAccessRights. An empty password file fails the scrape before
// anything is sent, a bind with a DN and no password being unauthenticated.
func TestAnLDAPCollectorBindsWithItsDNAndPasswordFile(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	server.AddAccount("cn=exporter,dc=example,dc=org", "s3cret")
	server.DenyAnonymous()
	if _, err := ldapScrape(t, ldapCollector(t), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "insufficientAccessRights (50): 000004DC") {
		t.Errorf("anonymous: %v", err)
	}
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bound := ldapCollector(t, func(c *model.Collector) {
		c.Request.PasswordFile = passwordFile
		editLDAP(c, func(r *LDAPConfig) { r.BindDN = "cn=exporter,dc=example,dc=org" })
	})
	if _, err := ldapScrape(t, bound, server.Addr, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordFile, []byte("wrong-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ldapScrape(t, bound, server.Addr, nil)
	if err == nil || !strings.Contains(err.Error(), "BIND cn=exporter,dc=example,dc=org ***: the server answered invalidCredentials (49): 80090308: LdapErr") || strings.Contains(err.Error(), "wrong-one") {
		t.Errorf("a wrong password: %v", err)
	}
	var binds []string
	for _, r := range server.Requests() {
		if r.Op == "BIND" {
			binds = append(binds, r.DN+" "+r.Password)
		}
	}
	if strings.Join(binds, "|") != " |cn=exporter,dc=example,dc=org s3cret|cn=exporter,dc=example,dc=org wrong-one" {
		t.Errorf("bound with %q", binds)
	}
	if err := os.WriteFile(passwordFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(server.Requests())
	if _, err := ldapScrape(t, bound, server.Addr, nil); err == nil || !strings.Contains(err.Error(), "is empty; a simple bind with a DN and an empty password is an unauthenticated bind") {
		t.Errorf("an empty password: %v", err)
	}
	if len(server.Requests()) != before {
		t.Errorf("a scrape with an empty password sent %v", server.Requests()[before:])
	}
}

// start_tls begins TLS with the StartTLS operation on a plaintext
// connection, before the bind, verifying the server against ca_file; a
// server that refuses StartTLS fails the scrape, and so does one whose
// certificate does not verify. An ldaps:// target, or a bare one with a
// tls block, speaks TLS from the start.
func TestAnLDAPCollectorSpeaksTLS(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	startTLS := ldapCollector(t, func(c *model.Collector) {
		c.Request.TLS.CAFile = server.CAFile
		editLDAP(c, func(r *LDAPConfig) { r.StartTLS = true })
	})
	for _, target := range []string{server.Addr, "ldap://" + server.Addr} {
		if _, err := ldapScrape(t, startTLS, target, nil); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}
	requests := server.Requests()
	if len(requests) < 3 || requests[0].Op != "STARTTLS" || requests[0].TLS || requests[1].Op != "BIND" || !requests[1].TLS || !requests[2].TLS {
		t.Errorf("requests %+v", requests)
	}
	if label, _ := RequestLabelFor(server.Addr, startTLS, RequestOverrides{}); label != "ldap://"+server.Addr {
		t.Errorf("label %q", label)
	}
	if _, err := ldapScrape(t, ldapCollector(t, func(c *model.Collector) { editLDAP(c, func(r *LDAPConfig) { r.StartTLS = true }) }), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "STARTTLS: TLS handshake with "+server.Addr) {
		t.Errorf("an unknown authority: %v", err)
	}
	server.RefuseStartTLS()
	if _, err := ldapScrape(t, startTLS, server.Addr, nil); err == nil || !strings.Contains(err.Error(), "STARTTLS: the server answered protocolError (2)") {
		t.Errorf("StartTLS refused: %v", err)
	}

	secure := ldaptest.NewTLS(t)
	ldapPeople(secure)
	verified := ldapCollector(t, func(c *model.Collector) { c.Request.TLS.CAFile = secure.CAFile })
	for _, target := range []string{secure.Addr, "ldaps://" + secure.Addr} {
		if _, err := ldapScrape(t, verified, target, nil); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}
	if label, _ := RequestLabelFor(secure.Addr, verified, RequestOverrides{}); label != "ldaps://"+secure.Addr {
		t.Errorf("label %q", label)
	}
	if _, err := ldapScrape(t, ldapCollector(t), "ldaps://"+secure.Addr, nil); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+secure.Addr) {
		t.Errorf("an unknown authority: %v", err)
	}
	if _, err := ldapScrape(t, ldapCollector(t), secure.Addr, nil); err == nil {
		t.Error("plaintext to a TLS server was taken")
	}
}

// ldapMany is a directory of n people under ou=people.
func ldapMany(server *ldaptest.Server, n int) {
	server.AddEntry(ldaptest.Entry{DN: "ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"organizationalUnit"}}})
	for i := range n {
		server.AddEntry(ldaptest.Entry{DN: fmt.Sprintf("uid=u%04d,ou=people,dc=example,dc=org", i), Attributes: map[string][]string{"objectClass": {"person"}, "cn": {fmt.Sprintf("User %d", i)}}})
	}
}

// A search is read a page of 500 entries at a time, with the Simple Paged
// Results control, not critical, each page after the first sent the
// cookie the last one ended with, until a page ends with none; a server
// that ignores the control answers in one go, and the result is the same.
func TestAnLDAPSearchIsReadPageByPage(t *testing.T) {
	server := ldaptest.New(t)
	ldapMany(server, 1200)
	c := ldapCollector(t, func(c *model.Collector) {
		ldapRequest(c).Searches[0].MaxEntries = 2000
		ldapRequest(c).Searches[0].Attributes = []string{"cn"}
	})
	var doc struct {
		Searches map[string]struct {
			Count   int
			Entries []struct{ DN string }
		}
	}
	for _, ignore := range []bool{false, true} {
		if ignore {
			server.IgnorePaging()
		}
		body, err := ldapScrape(t, c, server.Addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatal(err)
		}
		people := doc.Searches["people"]
		if people.Count != 1200 || len(people.Entries) != 1200 || people.Entries[1199].DN != "uid=u1199,ou=people,dc=example,dc=org" {
			t.Errorf("ignoring paging %v: %d entries, %d held", ignore, people.Count, len(people.Entries))
		}
	}
	var pages []string
	for _, r := range server.Requests() {
		if r.Op == "SEARCH" {
			pages = append(pages, fmt.Sprintf("%d:%q", r.Size, r.Cookie))
		}
	}
	if want := `500:""|500:"500"|500:"1000"|500:""`; strings.Join(pages, "|") != want {
		t.Errorf("pages %s, want %s", strings.Join(pages, "|"), want)
	}
}

// A count_only search counts every entry it matches, past max_entries'
// default, across pages, asking for no attribute and holding no entry:
// its result has no entries. A search that is not count_only and matches
// more entries than its max_entries fails the scrape, saying what to do.
func TestAnLDAPSearchCountsOrBoundsItsEntries(t *testing.T) {
	server := ldaptest.New(t)
	ldapMany(server, 1500)
	counted := ldapCollector(t, func(c *model.Collector) {
		ldapRequest(c).Searches[0].Attributes = nil
		ldapRequest(c).Searches[0].CountOnly = true
	})
	body, err := ldapScrape(t, counted, server.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"searches":{"people":{"count":1500,"references":0}}}` {
		t.Errorf("document %s", body)
	}
	for _, r := range server.Requests() {
		if r.Op == "SEARCH" && strings.Join(r.Attributes, ",") != "1.1" {
			t.Errorf("count_only asked for %v", r.Attributes)
		}
	}
	if _, err := ldapScrape(t, ldapCollector(t), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "matches more than its max_entries, 1000; narrow its base or filter, raise max_entries, or set count_only") {
		t.Errorf("past the default max_entries: %v", err)
	}
	if _, err := ldapScrape(t, ldapCollector(t, func(c *model.Collector) { ldapRequest(c).Searches[0].MaxEntries = 10 }), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "max_entries, 10;") {
		t.Errorf("past max_entries: %v", err)
	}
}

// Filters are parsed as RFC 4515 writes them and sent as RFC 4511 encodes
// them: what the server reads back is the filter written, its escapes
// kept, an extensible match's attribute, dn and matching rule among them,
// Active Directory's bitwise AND for one; a filter that is not one is
// refused with what is wrong with it.
func TestLDAPFiltersAreParsed(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	for filter, count := range map[string]int{
		"(objectClass=person)":                           2,
		"(&(objectClass=person)(cn=a*))":                 2,
		"(&(objectClass=person)(cn=*Turing))":            1,
		"(cn=A*a*e)":                                     1,
		"(|(uid=ada)(uid=alan))":                         2,
		"(!(uid=ada))":                                   3,
		"(&(objectClass=person)(!(mail=*)))":             1,
		"(userAccountControl>=514)":                      1,
		"(userAccountControl<=513)":                      0,
		"(cn~=ada lovelace)":                             1,
		`(cn=Ada\20Lovelace)`:                            1,
		`(cn=\2a)`:                                       0,
		"(2.5.4.3=admins)":                               0,
		"(&(objectClass=person)(|(uid=ada)(uid=bob)))":   1,
		"(userAccountControl:1.2.840.113556.1.4.803:=2)": 1,
		"(userAccountControl:1.2.840.113556.1.4.803:=1)": 0,
		"(cn:dn:caseIgnoreMatch:=admins)":                1,
		"(:caseIgnoreMatch:=ada)":                        1,
		"(uid:=alan)":                                    1,
	} {
		c := ldapCollector(t, func(c *model.Collector) {
			ldapRequest(c).Searches[0].Base = "dc=example,dc=org"
			ldapRequest(c).Searches[0].Filter = filter
			ldapRequest(c).Searches[0].Attributes = nil
			ldapRequest(c).Searches[0].CountOnly = true
		})
		body, err := ldapScrape(t, c, server.Addr, nil)
		if err != nil {
			t.Errorf("%s: %v", filter, err)
			continue
		}
		if want := fmt.Sprintf(`{"searches":{"people":{"count":%d,"references":0}}}`, count); body != want {
			t.Errorf("%s: %s, want %s", filter, body, want)
		}
		if got := ldapLastSearch(server).Filter; !strings.EqualFold(got, strings.ReplaceAll(filter, `\20`, " ")) {
			t.Errorf("%s reached the server as %s", filter, got)
		}
	}
	for filter, want := range map[string]string{
		"objectClass=person":     "does not begin with (; write it in parentheses",
		"(objectClass=person":    "ends before its closing parenthesis",
		"(objectClass=person))":  "goes on after its closing parenthesis",
		"(&)":                    "has a & with no filter in it",
		"(:=Ada)":                "with neither an attribute nor a matching rule",
		"(cn:dn:rule:more:=Ada)": "of more parts than attr:dn:rule:=value",
		"(cn:ru le:=Ada)":        `names the matching rule "ru le"`,
		"(cn:dn:=A*)":            "in the value of an extensible match",
		"(cn:dn=Ada)":            `names the attribute "cn:dn"`,
		"(cn)":                   `has no =`,
		"(c n=x)":                `names the attribute "c n"`,
		`(cn=\zz)`:               `has the escape \zz`,
		`(cn=a\2)`:               "ends in an escape",
		"(cn>=a*)":               "has * in a value of ~=, >= or <=",
		"(cn=a(b)":               "has ( inside",
		"(cn=**)":                "is a substring match with no substring between its wildcards, which RFC 4511 does not allow; write (cn=*)",
		"(&(uid=a)(cn=***))":     `the filter's item "cn=***" is a substring match with no substring`,
		strings.Repeat("(!", 40) + "(cn=a)" + strings.Repeat(")", 40): "nests &, | and ! more than 32 deep",
	} {
		if _, err := parseLDAPFilter(filter); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", filter, err, want)
		}
		c := &model.Collector{Name: "d", Request: model.RequestConfig{Type: RequestTypeLDAP, Own: LDAPConfig{Searches: []LDAPSearch{{Name: "s", Filter: filter}}}}}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), "request.searches[0].filter: ") {
			t.Errorf("%s loaded: %v", filter, err)
		}
	}
}

// A substring filter whose substrings placeholders fill in empty,
// (cn=*{{param_x:}}*), asks for a value holding the empty string, which
// every value does: it is sent as the presence filter (cn=*), as
// (cn={{param_x:}}*) fills in to, never as a SubstringFilter of no
// substring, which RFC 4511 forbids and a server refuses. With a value it
// is the substring filter it is written as.
func TestAnLDAPSubstringFilterFilledEmptyIsPresence(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	c := ldapCollector(t, func(c *model.Collector) {
		ldapRequest(c).Searches[0].Base = "dc=example,dc=org"
		ldapRequest(c).Searches[0].Filter = "(&(objectClass=person)(cn=*{{param_x:}}*))"
		ldapRequest(c).Searches[0].Attributes = nil
		ldapRequest(c).Searches[0].CountOnly = true
	})
	body, err := ldapScrape(t, c, server.Addr, nil)
	if err != nil || !strings.Contains(body, `"count":2,`) {
		t.Errorf("an empty value: %s %v", body, err)
	}
	if last := ldapLastSearch(server); last.Filter != "(&(objectClass=person)(cn=*))" {
		t.Errorf("the filter sent %s", last.Filter)
	}
	body, err = ldapScrape(t, c, server.Addr, map[string]string{"param_x": "Turing"})
	if err != nil || !strings.Contains(body, `"count":1,`) {
		t.Errorf("a value: %s %v", body, err)
	}
	if last := ldapLastSearch(server); last.Filter != "(&(objectClass=person)(cn=*Turing*))" {
		t.Errorf("the filter sent %s", last.Filter)
	}
}

// A placeholder in a filter's value is filled with the probe's value
// escaped as RFC 4515 escapes one, so a value of *)( matches itself and
// widens nothing, and one in a base's attribute value escaped as RFC 4514
// escapes one, so it adds no RDN. A placeholder elsewhere — an attribute's
// name, the filter's structure, a whole DN, the scope, an attribute asked
// for, bind_dn — is refused when the configuration loads.
func TestLDAPPlaceholdersCannotChangeASearchsStructure(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	c := ldapCollector(t, func(c *model.Collector) {
		ldapRequest(c).Searches[0].Base = "ou={{param_ou}},dc=example,dc=org"
		ldapRequest(c).Searches[0].Filter = "(&(objectClass=person)(uid={{param_user}}))"
	})
	body, err := ldapScrape(t, c, server.Addr, map[string]string{"param_ou": "people", "param_user": "ada"})
	if err != nil || !strings.Contains(body, `"count":1,`) {
		t.Errorf("a plain value: %s %v", body, err)
	}
	body, err = ldapScrape(t, c, server.Addr, map[string]string{"param_ou": "people", "param_user": "*)(uid=*"})
	if err != nil || !strings.Contains(body, `"count":0,`) {
		t.Errorf("a value of *)(: %s %v", body, err)
	}
	if last := ldapLastSearch(server); last.Filter != `(&(objectClass=person)(uid=\2a\29\28uid=\2a))` {
		t.Errorf("the filter sent %s", last.Filter)
	}
	_, err = ldapScrape(t, c, server.Addr, map[string]string{"param_ou": "people,dc=evil", "param_user": "ada"})
	if base := ldapLastSearch(server).Base; base != `ou=people\,dc\=evil,dc=example,dc=org` {
		t.Errorf("the base sent %s (%v)", base, err)
	}
	if err == nil || !strings.Contains(err.Error(), "noSuchObject (32)") {
		t.Errorf("a base of no entry: %v", err)
	}
	for value, want := range map[string]string{"a*b": `a\2ab`, "(x)": `\28x\29`, `back\slash`: `back\5cslash`, "nul\x00": `nul\00`, "\xff": `\ff`, "café": "café"} {
		if got := ldapEscapeFilterValue(value); got != want {
			t.Errorf("filter value %q escaped %q, want %q", value, got, want)
		}
	}
	for value, want := range map[string]string{"a,b": `a\,b`, " lead": `\ lead`, "#x": `\#x`, "trail ": `trail\ `, `q"<>;+=\`: `q\"\<\>\;\+\=\\`, "nul\x00": `nul\00`} {
		if got, err := ldapEscapeDNValue(value); err != nil || got != want {
			t.Errorf("DN value %q escaped %q, want %q (%v)", value, got, want, err)
		}
	}
	if _, err := ldapEscapeDNValue("\xff"); err == nil {
		t.Error("a DN value that is not UTF-8 was taken")
	}
	for name, tc := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"an attribute's name":    {func(c *model.Collector) { ldapRequest(c).Searches[0].Filter = "({{param_a}}=x)" }, "a placeholder may stand only in a value"},
		"the filter's structure": {func(c *model.Collector) { ldapRequest(c).Searches[0].Filter = "{{param_f}}" }, "does not begin with ("},
		"a whole DN":             {func(c *model.Collector) { ldapRequest(c).Searches[0].Base = "{{param_base}}" }, "a placeholder may stand only in a value"},
		"a DN's type":            {func(c *model.Collector) { ldapRequest(c).Searches[0].Base = "{{param_t}}=x,dc=org" }, "a placeholder may stand only in a value"},
		"the scope":              {func(c *model.Collector) { ldapRequest(c).Searches[0].Scope = "{{param_scope}}" }, "scope \"{{param_scope}}\" has a placeholder"},
		"an attribute":           {func(c *model.Collector) { ldapRequest(c).Searches[0].Attributes = []string{"{{param_a}}"} }, "has a placeholder; a placeholder may stand in base and filter alone"},
		"bind_dn": {func(c *model.Collector) {
			c.Request.PasswordFile = "/p"
			editLDAP(c, func(r *LDAPConfig) { r.BindDN = "cn={{param_u}},dc=org" })
		}, "request.bind_dn \"cn={{param_u}},dc=org\" has a placeholder"},
		"a broken placeholder": {func(c *model.Collector) { ldapRequest(c).Searches[0].Filter = "(cn={{param_)" }, "request.searches[0].filter"},
	} {
		c := &model.Collector{Name: "d", Request: model.RequestConfig{Type: RequestTypeLDAP, Own: LDAPConfig{Searches: []LDAPSearch{{Name: "s", Base: "dc=org"}}}}}
		tc.edit(c)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if err := CheckPathParams(c, RequestOverrides{Params: map[string]string{"param_ou": "a", "param_user": "b", "param_other": "c"}}); err == nil || !strings.Contains(err.Error(), "param_other") || !strings.Contains(err.Error(), "the base or filter of its request.searches") {
		t.Errorf("an unused parameter: %v", err)
	}
}

// ldapLastSearch is the last search the server was sent.
func ldapLastSearch(server *ldaptest.Server) ldaptest.Request {
	requests := server.Requests()
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].Op == "SEARCH" {
			return requests[i]
		}
	}
	return ldaptest.Request{}
}

// ldapAnswer has the server answer each search with raw bytes.
func ldapAnswer(server *ldaptest.Server, raw func(id int) []byte) {
	server.Answer(func(r ldaptest.Request) ([]byte, bool) {
		if r.Op != "SEARCH" {
			return nil, false
		}
		return raw(r.ID), true
	})
}

// What a server answers is held to the response limit as it is read: the
// messages of a scrape together, a message whose length is past what is
// left before a byte of it is read; a length of more than four bytes, an
// indefinite length, an element longer than what encloses it, a tag of
// more than one byte, a constructed string where a value stands and a
// message of another ID are refused as no LDAP. A length the server does
// not go on to send costs only the bytes that came.
func TestAnLDAPServersMessagesAreHeldToTheResponseLimit(t *testing.T) {
	server := ldaptest.New(t)
	ldapMany(server, 50)
	if _, err := ldapScrape(t, ldapCollector(t, func(c *model.Collector) { c.Request.MaxResponseBytes = 1000 }), server.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the server's messages are larger than the response limit") {
		t.Errorf("entries past the limit: %v", err)
	}
	// A count_only search's entries count against it too.
	counted := ldapCollector(t, func(c *model.Collector) {
		c.Request.MaxResponseBytes = 1000
		ldapRequest(c).Searches[0].Attributes, ldapRequest(c).Searches[0].CountOnly = nil, true
	})
	if _, err := ldapScrape(t, counted, server.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("counted entries past the limit: %v", err)
	}
	entry := func(content []byte) func(int) []byte {
		return func(id int) []byte { return ldaptest.Message(id, content) }
	}
	for name, tc := range map[string]struct {
		raw  func(id int) []byte
		want error
		text string
	}{
		"a message past the limit":   {func(int) []byte { return []byte{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff} }, model.ErrLimitExceeded, "larger than the response limit"},
		"a length of five bytes":     {func(int) []byte { return []byte{0x30, 0x85, 0, 0, 0, 0, 1, 0} }, errLDAPProtocol, "a length of 5 bytes"},
		"an indefinite length":       {func(int) []byte { return []byte{0x30, 0x80, 0, 0} }, errLDAPProtocol, "an indefinite length"},
		"no LDAP at all":             {func(int) []byte { return []byte("HTTP/1.1 400 Bad Request\r\n\r\n") }, errLDAPProtocol, "is the target an LDAP server?"},
		"an element past its parent": {entry([]byte{0x64, 0x7f, 0x04, 0x00}), errLDAPProtocol, "is past what encloses it"},
		"a tag of two bytes":         {entry([]byte{0x7f, 0x22, 0x00}), errLDAPProtocol, "a tag of more than one byte"},
		"a constructed value": {entry(ldaptest.Element(0x64, ldaptest.Element(0x04, []byte("cn=x")), ldaptest.Element(0x30,
			ldaptest.Element(0x30, ldaptest.Element(0x04, []byte("cn")), ldaptest.Element(0x31, ldaptest.Element(0x24, ldaptest.Element(0x24, ldaptest.Element(0x04, []byte("x"))))))))), errLDAPProtocol, "has the tag 0x24, not 0x04"},
		"another message's answer": {func(id int) []byte { return ldaptest.Message(id+7, ldaptest.ResultOp(0x65, 0, "")) }, errLDAPProtocol, "while the scrape waited for message"},
		"a message cut short":      {func(int) []byte { return []byte{0x30, 0x03, 0x02, 0x01} }, errLDAPProtocol, "cut short"},
	} {
		ldapAnswer(server, tc.raw)
		_, err := FetchCollector(context.Background(), server.Addr, ldapCollector(t), RequestOverrides{Timeout: 2 * time.Second}, nil)
		if name == "a message cut short" {
			// The server sends no more, so the read ends at the deadline.
			if err == nil {
				t.Errorf("%s: taken", name)
			}
			continue
		}
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A limit large enough to take the length the server declares: what is
	// allocated is what came, not what was declared.
	ldapAnswer(server, func(int) []byte { return append([]byte{0x30, 0x84, 0x3b, 0x9a, 0xca, 0x00}, "only this"...) })
	big := ldapCollector(t, func(c *model.Collector) { c.Request.MaxResponseBytes = 2 << 30 })
	var err error
	allocated := alloctest.BytesAtMost(1, 32<<20, func() {
		_, err = FetchCollector(context.Background(), server.Addr, big, RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	})
	if err == nil {
		t.Error("a message cut short was taken")
	}
	if allocated > 32<<20 {
		t.Errorf("a declared length of 1 GB allocated %d bytes", allocated)
	}
}

// An entry holding bytes that are not UTF-8 fails the scrape naming the
// attribute and the entry, never put in the document with U+FFFD in their
// place; the document is held to the response limit, so values whose
// control characters JSON writes six bytes each fail it as a limit
// exceeded.
func TestAnLDAPValueThatIsNotTextFailsTheScrape(t *testing.T) {
	server := ldaptest.New(t)
	server.AddEntry(ldaptest.Entry{DN: "ou=people,dc=example,dc=org"},
		ldaptest.Entry{DN: "uid=ada,ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"person"}, "cn": {"Ada"}, "objectGUID": {"\x8f\xff\x01\x02"}}})
	c := ldapCollector(t, func(c *model.Collector) { ldapRequest(c).Searches[0].Attributes = []string{"cn", "objectGUID"} })
	_, err := ldapScrape(t, c, server.Addr, nil)
	if !errors.Is(err, errLDAPNotText) || !strings.Contains(err.Error(), "the attribute objectGUID of uid=ada,ou=people,dc=example,dc=org holds bytes that are not UTF-8") || !strings.Contains(err.Error(), "SEARCH people") {
		t.Errorf("a binary value: %v", err)
	}
	if body, err := ldapScrape(t, ldapCollector(t, func(c *model.Collector) { ldapRequest(c).Searches[0].Attributes = []string{"cn"} }), server.Addr, nil); err != nil || !strings.Contains(body, `"cn":["Ada"]`) {
		t.Errorf("without it: %s %v", body, err)
	}
	escaped := ldaptest.New(t)
	escaped.AddEntry(ldaptest.Entry{DN: "ou=people,dc=example,dc=org"},
		ldaptest.Entry{DN: "uid=x,ou=people,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"person"}, "cn": {strings.Repeat("\x01", 400)}}})
	limited := ldapCollector(t, func(c *model.Collector) { c.Request.MaxResponseBytes = 1000 })
	if _, err := ldapScrape(t, limited, escaped.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "as JSON") {
		t.Errorf("a document past the limit: %v", err)
	}
}

// A search the server ends with a code other than success fails the scrape
// with the code's name and number and the server's diagnostic message; a
// referral says it is not followed, and a notice of disconnection gives
// the server's reason.
func TestAnLDAPResultCodeFailsTheScrape(t *testing.T) {
	server := ldaptest.New(t)
	ldapPeople(server)
	server.FailSearches(53, "server is read-only for maintenance")
	if _, err := ldapScrape(t, ldapCollector(t), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "SEARCH people base=\"ou=people,dc=example,dc=org\" scope=sub filter=(objectClass=person) attributes=cn,mail: the server answered unwillingToPerform (53): server is read-only for maintenance") {
		t.Errorf("unwillingToPerform: %v", err)
	}
	server.FailSearches(10, "")
	if _, err := ldapScrape(t, ldapCollector(t), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "referral (10); the exporter follows no referral") {
		t.Errorf("a referral: %v", err)
	}
	server.FailSearches(4, "")
	if _, err := ldapScrape(t, ldapCollector(t), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "sizeLimitExceeded (4); the server's own size limit") {
		t.Errorf("sizeLimitExceeded: %v", err)
	}
	other := ldaptest.New(t)
	ldapPeople(other)
	if _, err := ldapScrape(t, ldapCollector(t, func(c *model.Collector) { ldapRequest(c).Searches[0].Base = "ou=nobody,dc=example,dc=org" }), other.Addr, nil); err == nil || !strings.Contains(err.Error(), "noSuchObject (32)") {
		t.Errorf("noSuchObject: %v", err)
	}
	ldapAnswer(other, func(int) []byte { return ldaptest.Message(0, ldaptest.ResultOp(0x78, 52, "server shutting down")) })
	if _, err := ldapScrape(t, ldapCollector(t), other.Addr, nil); err == nil || !strings.Contains(err.Error(), "the server closed the connection: the server answered unavailable (52): server shutting down") {
		t.Errorf("a notice of disconnection: %v", err)
	}
}

// allowed_targets and denied_targets are kept to, and the cloud metadata
// service is refused without them: a refused target is never connected to.
func TestAnLDAPCollectorKeepsToItsTargetLists(t *testing.T) {
	if _, err := ldapScrape(t, ldapCollector(t), "169.254.169.254:389", nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("the metadata service: %v", err)
	}
	server := ldaptest.New(t)
	ldapPeople(server)
	for name, edit := range map[string]func(*model.Collector){
		"denied":      func(c *model.Collector) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} },
		"not allowed": func(c *model.Collector) { c.Request.AllowedTargets = []string{"10.0.0.0/8"} },
	} {
		if _, err := ldapScrape(t, ldapCollector(t, edit), server.Addr, nil); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := server.Requests(); len(got) != 0 {
		t.Errorf("a refused target was sent %v", got)
	}
	if _, err := ldapScrape(t, ldapCollector(t, func(c *model.Collector) { c.Request.AllowedTargets = []string{"127.0.0.0/8"} }), server.Addr, nil); err != nil {
		t.Errorf("allowed: %v", err)
	}
}

// A collector's keys are checked when it loads: searches, each under a
// name of its own, with a DN for base, a scope of base, one or sub, a
// filter, attributes by name, count_only without attributes or
// max_entries; bind_dn with password_file and the other way round. A
// target is host, host:port or an ldap:// or ldaps:// URL of one, and
// ldap:// with a tls block but no start_tls, and ldaps:// with start_tls,
// contradict each other.
func TestAnLDAPCollectorsKeysAndTargetsAreChecked(t *testing.T) {
	search := func(edit func(*LDAPSearch)) func(*model.Collector) {
		return func(c *model.Collector) { edit(&ldapRequest(c).Searches[0]) }
	}
	for name, tc := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"no searches":            {func(c *model.Collector) { editLDAP(c, func(r *LDAPConfig) { r.Searches = nil }) }, "has no request.searches"},
		"a name jq cannot reach": {search(func(s *LDAPSearch) { s.Name = "all-people" }), `request.searches[0].name "all-people"`},
		"a name twice": {func(c *model.Collector) {
			editLDAP(c, func(r *LDAPConfig) { r.Searches = append(r.Searches, r.Searches[0]) })
		}, "names people twice"},
		"a base that is no DN":       {search(func(s *LDAPSearch) { s.Base = "people" }), `base "people" is not a DN`},
		"a base with an odd escape":  {search(func(s *LDAPSearch) { s.Base = `cn=a\q,dc=org` }), `has the escape \q`},
		"a scope in capitals":        {search(func(s *LDAPSearch) { s.Scope = "SUB" }), `scope "SUB" is not base, one or sub`},
		"a scope of subtree":         {search(func(s *LDAPSearch) { s.Scope = "subtree" }), "is not base, one or sub"},
		"an attribute with a blank":  {search(func(s *LDAPSearch) { s.Attributes = []string{"given name"} }), `attributes[0] "given name" is not an attribute's name`},
		"an attribute twice":         {search(func(s *LDAPSearch) { s.Attributes = []string{"cn", "CN"} }), "names CN twice"},
		"count_only with attributes": {search(func(s *LDAPSearch) { s.CountOnly = true }), "sets count_only and attributes"},
		"count_only with max_entries": {search(func(s *LDAPSearch) {
			s.CountOnly, s.Attributes, s.MaxEntries = true, nil, 5
		}), "sets count_only and max_entries"},
		"a negative max_entries": {search(func(s *LDAPSearch) { s.MaxEntries = -1 }), "max_entries must not be negative"},
		"bind_dn alone":          {func(c *model.Collector) { editLDAP(c, func(r *LDAPConfig) { r.BindDN = "cn=exporter,dc=org" }) }, "without request.password_file; a simple bind with a DN and no password is an unauthenticated bind"},
		"password_file alone":    {func(c *model.Collector) { c.Request.PasswordFile = "/p" }, "without request.bind_dn"},
		"a bind_dn that is no DN": {func(c *model.Collector) {
			c.Request.PasswordFile = "/p"
			editLDAP(c, func(r *LDAPConfig) { r.BindDN = "exporter" })
		}, `request.bind_dn "exporter" is not a DN`},
		"a key of another type":     {func(c *model.Collector) { c.Request.Statement = "SELECT 1" }, "sets request.statement, which does not apply"},
		"half a client certificate": {func(c *model.Collector) { c.Request.TLS.CertFile = "/c.pem" }, "only one of cert_file and key_file"},
	} {
		c := &model.Collector{Name: "directory", Request: model.RequestConfig{Type: RequestTypeLDAP, Own: LDAPConfig{Searches: []LDAPSearch{{Name: "people", Base: "ou=people,dc=example,dc=org", Attributes: []string{"cn"}}}}}}
		tc.edit(c)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	rootDSE := ldapCollector(t, func(c *model.Collector) {
		editLDAP(c, func(r *LDAPConfig) {
			r.Searches = []LDAPSearch{{Name: "root", Scope: "base", Attributes: []string{"+", "*", "namingContexts", "2.5.4.3;lang-de"}}}
		})
	})
	if ldapRequest(rootDSE).Searches[0].Base != "" {
		t.Errorf("base %q", ldapRequest(rootDSE).Searches[0].Base)
	}
	plain := ldapCollector(t)
	for target, want := range map[string]string{
		"dir.internal":             "",
		"dir.internal:3389":        "",
		"[2001:db8::1]:389":        "",
		"ldap://dir.internal":      "",
		"ldaps://dir.internal/":    "",
		"http://dir.internal":      "has the scheme http://",
		"ldap://user@dir":          "has a path, query or user",
		"ldap://dir/dc=org":        "has a path, query or user",
		"dir.internal:0":           `has the port "0"`,
		"dir.internal:ldap":        `has the port "ldap"`,
		"":                         "target is required",
		"ldaps://":                 "names no host",
		"ldaps://dir.internal:636": "",
	} {
		err := CheckTarget(plain, target, false)
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", target, err, want)
		}
	}
	withTLS := ldapCollector(t, func(c *model.Collector) { c.Request.TLS.CAFile = "/ca.pem" })
	if err := CheckTarget(withTLS, "ldap://dir.internal", false); err == nil || !strings.Contains(err.Error(), "asks for plaintext with ldap://, but the collector sets request.tls") {
		t.Errorf("ldap:// with a tls block: %v", err)
	}
	startTLS := ldapCollector(t, func(c *model.Collector) {
		c.Request.TLS.CAFile = "/ca.pem"
		editLDAP(c, func(r *LDAPConfig) { r.StartTLS = true })
	})
	if err := CheckTarget(startTLS, "ldaps://dir.internal", false); err == nil || !strings.Contains(err.Error(), "asks for TLS from the start with ldaps://, but the collector sets request.start_tls") {
		t.Errorf("ldaps:// with start_tls: %v", err)
	}
	for target, want := range map[string]string{
		"dir.internal":         "ldaps://dir.internal:636",
		"ldaps://dir.internal": "ldaps://dir.internal:636",
	} {
		if label, err := RequestLabelFor(target, withTLS, RequestOverrides{}); err != nil || label != want {
			t.Errorf("%s: label %q, %v", target, label, err)
		}
	}
	for target, want := range map[string]string{"dir.internal": "ldap://dir.internal:389", "ldap://dir.internal:3389": "ldap://dir.internal:3389"} {
		if label, err := RequestLabelFor(target, startTLS, RequestOverrides{}); err != nil || label != want {
			t.Errorf("%s with start_tls: label %q, %v", target, label, err)
		}
	}
}

// A debug probe's request section shows each operation — STARTTLS, BIND
// with the DN and the password as ***, each page of each SEARCH with its
// base, scope, filter and attributes — and how each ended; --dry-run reads
// the password file as a scrape would.
func TestAnLDAPScrapeIsTracedWithoutItsPassword(t *testing.T) {
	server := ldaptest.New(t)
	ldapMany(server, 600)
	server.AddAccount("cn=exporter,dc=example,dc=org", "s3cret")
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := ldapCollector(t, func(c *model.Collector) {
		c.Request.PasswordFile = passwordFile
		editLDAP(c, func(r *LDAPConfig) { r.BindDN = "cn=exporter,dc=example,dc=org" })
		c.Request.TLS.CAFile = server.CAFile
		editLDAP(c, func(r *LDAPConfig) { r.StartTLS = true })
		ldapRequest(c).Searches[0].Attributes, ldapRequest(c).Searches[0].CountOnly = nil, true
	})
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range trace.Requests() {
		got = append(got, fmt.Sprintf("%s %s %s page %d -> %s", r.Method, r.URL, r.Header.Get("Ldap-Operation"), r.Page, r.Outcome))
	}
	label := "ldap://" + server.Addr
	search := `SEARCH people base="ou=people,dc=example,dc=org" scope=sub filter=(objectClass=person) attributes=1.1`
	want := []string{
		"STARTTLS " + label + " STARTTLS page 0 -> success, TLS 1.3",
		"BIND " + label + " BIND cn=exporter,dc=example,dc=org *** page 0 -> success",
		"SEARCH " + label + " " + search + " page 1 -> success, 500 entries, more to come",
		"SEARCH " + label + " " + search + " page 2 -> success, 100 entries",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("traced\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range trace.Requests() {
		if strings.Contains(fmt.Sprint(r.Header), "s3cret") {
			t.Errorf("the trace shows the password: %v", r.Header)
		}
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	missing := ldapCollector(t, func(c *model.Collector) {
		c.Request.PasswordFile = filepath.Join(t.TempDir(), "missing")
		editLDAP(c, func(r *LDAPConfig) { r.BindDN = "cn=exporter,dc=example,dc=org" })
	})
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.password_file") {
		t.Errorf("warnings %v", warnings)
	}
}

// A server that never answers fails the scrape at its deadline, and a
// connection refused names the address.
func TestAnLDAPServerThatNeverAnswersFailsAtTheDeadline(t *testing.T) {
	server := ldaptest.New(t)
	release := make(chan struct{})
	defer close(release)
	server.Answer(func(ldaptest.Request) ([]byte, bool) {
		<-release
		return nil, false
	})
	start := time.Now()
	_, err := FetchCollector(context.Background(), server.Addr, ldapCollector(t), RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "BIND anonymous") || time.Since(start) > 3*time.Second {
		t.Errorf("err %v after %s", err, time.Since(start))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	if _, err := ldapScrape(t, ldapCollector(t), addr, nil); err == nil || !strings.Contains(err.Error(), "connecting to "+addr) {
		t.Errorf("err %v", err)
	}
}

// optInRequestKeys lists the keys ldap accepts that no type of a default
// build does: bind_dn, start_tls and searches (checkOptInRequestKeys).
func TestTheOptInRequestKeysAreLDAPsOwn(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeLDAP)
}
