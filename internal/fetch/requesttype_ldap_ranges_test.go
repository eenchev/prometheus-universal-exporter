//go:build !select_request_types || request_type_ldap

package fetch

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An attribute a server answers in ranges, as Active Directory answers a
// group's member past 1500 values, member;range=0-1499, is followed with a
// search of the entry alone for each next range, member;range=1500-*,
// until the last, and its values are under its name without the option,
// in order; an attribute that is not ranged is as the server sent it; and
// each search of a range is a request of the trace.
func TestAnLDAPRangedAttributeIsFollowed(t *testing.T) {
	server := ldaptest.New(t)
	members := make([]string, 25)
	for i := range members {
		members[i] = fmt.Sprintf("uid=u%02d,ou=people,dc=example,dc=org", i)
	}
	server.AddEntry(
		ldaptest.Entry{DN: "ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"organizationalUnit"}}},
		ldaptest.Entry{DN: "cn=big,ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"group"}, "cn": {"big"}, "member": members}},
		ldaptest.Entry{DN: "cn=small,ou=groups,dc=example,dc=org", Attributes: map[string][]string{"objectClass": {"group"}, "cn": {"small"}, "member": members[:3]}},
	)
	server.SetRangeSize(10)
	c := ldapCollector(t, func(c *model.Collector) {
		editLDAP(c, func(o *LDAPConfig) {
			o.Searches = []LDAPSearch{{Name: "groups", Base: "ou=groups,dc=example,dc=org", Filter: "(objectClass=group)", Attributes: []string{"cn", "member"}}}
		})
	})
	ctx, trace := WithRequestTrace(t.Context())
	resp, err := FetchCollector(ctx, server.Addr, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Searches map[string]struct {
			Entries []ldapEntry
		}
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	entries := doc.Searches["groups"].Entries
	if len(entries) != 2 {
		t.Fatalf("entries %+v", entries)
	}
	for _, e := range entries {
		want := members
		if e.DN == "cn=small,ou=groups,dc=example,dc=org" {
			want = members[:3]
		}
		if !slices.Equal(e.Attributes["member"], want) || len(e.Attributes) != 2 {
			t.Errorf("%s: %v", e.DN, e.Attributes)
		}
	}
	var asked []string
	for _, r := range server.Requests() {
		if r.Op == "SEARCH" && r.Scope == "base" {
			asked = append(asked, r.Base+" "+strings.Join(r.Attributes, ","))
		}
	}
	if want := []string{"cn=big,ou=groups,dc=example,dc=org member;range=10-*", "cn=big,ou=groups,dc=example,dc=org member;range=20-*"}; !slices.Equal(asked, want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
	var outcomes []string
	for _, r := range trace.Requests() {
		if strings.Contains(r.Header.Get("Ldap-Operation"), "range=") {
			outcomes = append(outcomes, r.Outcome)
		}
	}
	if !slices.Equal(outcomes, []string{"success, 10 values", "success, 5 values"}) {
		t.Errorf("traced %q", outcomes)
	}
}

// The range option is read wherever it is among an attribute's options, in
// any case, and a name with none, or a range that is not one, is no range.
func TestLDAPRangesAreRead(t *testing.T) {
	for name, want := range map[string]string{
		"member;range=0-1499":     "member 0 1499 false true",
		"member;Range=1500-*":     "member 1500 0 true true",
		"member;binary;range=3-4": "member;binary 3 4 false true",
		"member":                  "member 0 0 false false",
		"member;range=5-2":        "member;range=5-2 0 0 false false",
		"member;range=x-*":        "member;range=x-* 0 0 false false",
	} {
		attribute, low, high, last, ok := ldapRange(name)
		if got := fmt.Sprint(attribute, " ", low, " ", high, " ", last, " ", ok); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}
