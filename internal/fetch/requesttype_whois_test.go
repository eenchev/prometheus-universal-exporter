//go:build request_type_whois

package fetch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// whoisStandIn is a WHOIS server: it reads one line, answers what answers
// holds for it, or what any holds for every other question, and closes.
type whoisStandIn struct {
	addr string
	mu   sync.Mutex
	// asked is the questions, in the order they came.
	asked []string
}

func startWhois(t *testing.T, answers map[string]string, any string) *whoisStandIn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	s := &whoisStandIn{addr: listener.Addr().String()}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				question := strings.TrimSuffix(line, "\r\n")
				s.mu.Lock()
				s.asked = append(s.asked, question)
				s.mu.Unlock()
				answer, known := answers[question]
				if !known {
					answer = any
				}
				_, _ = conn.Write([]byte(answer))
			}()
		}
	}()
	return s
}

func (s *whoisStandIn) questions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

func whoisCollector(t *testing.T, own WhoisConfig, edit func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "domains", Request: model.RequestConfig{Type: RequestTypeWhois, Own: own}}
	if edit != nil {
		edit(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func whoisDoc(t *testing.T, c *model.Collector, target string) (whoisDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return whoisDocument{}, err
	}
	var doc whoisDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v in %s", err, resp.Body)
	}
	return doc, nil
}

const whoisRegistryAnswer = `   Domain Name: EXAMPLE.TEST
   Registry Domain ID: 2336799_DOMAIN_TEST-VRSN
   Registrar WHOIS Server: whois.registrar.test
   Updated Date: 2026-08-14T07:01:34Z
   Creation Date: 1995-08-14T04:00:00Z
   Registry Expiry Date: 2027-08-13T04:00:00Z
   Registrar: Example Registrar, Inc.
   Registrar IANA ID: 376
   Domain Status: clientDeleteProhibited https://icann.org/epp#clientDeleteProhibited
   Domain Status: clientTransferProhibited https://icann.org/epp#clientTransferProhibited
   Name Server: A.IANA-SERVERS.NET
   Name Server: B.IANA-SERVERS.NET
   DNSSEC: signedDelegation
>>> Last update of whois database: 2026-11-23T10:00:00Z <<<

NOTICE: The expiration date displayed in this record is the date the
registrar's sponsorship of the domain name registration expires.
Expiry Date: 1999-01-01
`

// rootFor swaps the root server for a stand-in that names registry for
// every top-level domain, and forgets the referrals kept.
func rootFor(t *testing.T, registry string) *whoisStandIn {
	t.Helper()
	root := startWhois(t, nil, "% IANA WHOIS server\n\nrefer:        "+registry+"\n\ndomain:       TEST\n")
	old := whoisRootServer
	whoisRootServer = root.addr
	whoisReferrals = &whoisReferralCache{}
	t.Cleanup(func() { whoisRootServer, whoisReferrals = old, &whoisReferralCache{} })
	return root
}

// A scrape asks the root server which server answers for the domain's
// top-level domain, asks that one for the domain, and answers the
// registration as a document; what the root said is kept, so the next
// scrape asks the registry alone. Nothing under the line that says when the
// database was updated is read.
func TestAWhoisScrapeReadsTheRegistration(t *testing.T) {
	registry := startWhois(t, map[string]string{"example.test": whoisRegistryAnswer}, "No match for domain.\n")
	root := rootFor(t, registry.addr)
	c := whoisCollector(t, WhoisConfig{}, nil)
	doc, err := whoisDoc(t, c, " Example.TEST. ")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Domain != "example.test" || doc.Server != registry.addr || doc.ReferredBy != root.addr || !doc.Registered {
		t.Errorf("domain %q, server %q, referred by %q, registered %v", doc.Domain, doc.Server, doc.ReferredBy, doc.Registered)
	}
	if doc.Registrar != "Example Registrar, Inc." || doc.RegistrarIANAID != "376" || doc.DNSSEC != "signedDelegation" {
		t.Errorf("registrar %q, id %q, dnssec %q", doc.Registrar, doc.RegistrarIANAID, doc.DNSSEC)
	}
	if doc.Expires == nil || *doc.Expires != "2027-08-13T04:00:00Z" || *doc.ExpiresSeconds != 1818129600 {
		t.Errorf("expires %v", doc.Expires)
	}
	if doc.Created == nil || *doc.Created != "1995-08-14T04:00:00Z" || doc.Updated == nil || *doc.Updated != "2026-08-14T07:01:34Z" {
		t.Errorf("created %v, updated %v", doc.Created, doc.Updated)
	}
	if want := []string{"clientDeleteProhibited", "clientTransferProhibited"}; !reflect.DeepEqual(doc.Status, want) {
		t.Errorf("status %v, want %v", doc.Status, want)
	}
	if want := []string{"a.iana-servers.net", "b.iana-servers.net"}; !reflect.DeepEqual(doc.NameServers, want) {
		t.Errorf("name servers %v, want %v", doc.NameServers, want)
	}
	if got := doc.Fields["registry domain id"]; len(got) != 1 || got[0] != "2336799_DOMAIN_TEST-VRSN" {
		t.Errorf("fields %v", doc.Fields)
	}
	if _, read := doc.Fields["notice"]; read || !strings.Contains(doc.Text, "NOTICE:") {
		t.Errorf("the terms were read as fields, or the text is not whole: %v", doc.Fields)
	}
	if _, err := whoisDoc(t, c, "example.test"); err != nil {
		t.Fatal(err)
	}
	if asked := root.questions(); !reflect.DeepEqual(asked, []string{"test"}) {
		t.Errorf("the root server was asked %q, want the top-level domain once", asked)
	}
	if asked := registry.questions(); !reflect.DeepEqual(asked, []string{"example.test", "example.test"}) {
		t.Errorf("the registry was asked %q", asked)
	}
}

// request.whois_server is asked without the root server, with the prefix before
// the domain; a domain the server does not know is a document that says
// so, with no dates.
func TestAWhoisServerNamedIsAskedDirectly(t *testing.T) {
	registry := startWhois(t, nil, "% No entries found for the selected source(s).\n")
	root := rootFor(t, "unused.invalid")
	c := whoisCollector(t, WhoisConfig{Server: registry.addr, QueryPrefix: "-T dn,ace "}, nil)
	doc, err := whoisDoc(t, c, "xn--bcher-kva.test")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Registered || doc.Expires != nil || doc.ExpiresSeconds != nil || doc.ReferredBy != "" || doc.Domain != "xn--bcher-kva.test" {
		t.Errorf("%+v", doc)
	}
	if asked := registry.questions(); !reflect.DeepEqual(asked, []string{"-T dn,ace xn--bcher-kva.test"}) {
		t.Errorf("asked %q", asked)
	}
	if asked := root.questions(); len(asked) != 0 {
		t.Errorf("the root server was asked %q", asked)
	}
}

// The registration is read from the ways registries write it: keys under a
// heading, keys in brackets, dotted leaders, and the dates in each one's
// own form. A date that begins with the day in numbers is not guessed at.
func TestWhoisAnswersOfSeveralRegistriesAreRead(t *testing.T) {
	for name, test := range map[string]struct {
		answer                    string
		expires, created, updated string
		registrar                 string
		nameServers, status       []string
	}{
		"headings": {
			answer:  "    Domain name:\n        example.uk\n\n    Registrar:\n        Example Ltd [Tag = EXAMPLE]\n        URL: https://example.uk\n\n    Relevant dates:\n        Registered on: before Aug-1996\n        Expiry date:  15-Dec-2026\n        Last updated:  13-Nov-2025\n\n    Registration status:\n        Registered until expiry date.\n\n    Name servers:\n        ns1.example.uk  192.0.2.1\n        ns2.example.uk\n",
			expires: "2026-12-15T00:00:00Z", updated: "2025-11-13T00:00:00Z", registrar: "Example Ltd [Tag = EXAMPLE]", nameServers: []string{"ns1.example.uk", "ns2.example.uk"},
		},
		"brackets": {
			answer:  "[ JPRS database provides information on network administration. ]\nDomain Information:\na. [Domain Name]                EXAMPLE.JP\np. [Name Server]                ns1.example.jp\n[Created on]                    2001/03/26\n[Expires on]                    2027/03/31\n[Status]                        Active\n[Last Updated]                  2026/04/01 01:05:08 (JST)\n",
			expires: "2027-03-31T00:00:00Z", created: "2001-03-26T00:00:00Z", updated: "2026-04-01T01:05:08Z", nameServers: []string{"ns1.example.jp"}, status: []string{"Active"},
		},
		"headings without a colon": {
			answer:  "Domain:             example.it\nStatus:             ok\nCreated:            2000-01-01 00:00:00\nLast Update:        2026-01-17 00:55:37\nExpire Date:        2027-01-01\n\nRegistrant\n  Organization:     Example Owner\n\nRegistrar\n  Organization:     Example s.r.l.\n  Name:             EXAMPLE-REG\n\nNameservers\n  ns1.example.it\n  ns2.example.it\n",
			expires: "2027-01-01T00:00:00Z", created: "2000-01-01T00:00:00Z", updated: "2026-01-17T00:55:37Z", registrar: "Example s.r.l.", nameServers: []string{"ns1.example.it", "ns2.example.it"}, status: []string{"ok"},
		},
		"paid-till": {
			answer:  "% TCI Whois Service.\n\ndomain:        EXAMPLE.RU\nnserver:       ns1.example.ru.\nstate:         REGISTERED, DELEGATED, VERIFIED\nregistrar:     EXAMPLE-RU\ncreated:       1997-09-23T09:45:07Z\npaid-till:     2027-09-30T21:00:00Z\n",
			expires: "2027-09-30T21:00:00Z", created: "1997-09-23T09:45:07Z", registrar: "EXAMPLE-RU", nameServers: []string{"ns1.example.ru"}, status: []string{"REGISTERED, DELEGATED, VERIFIED"},
		},
		"dotted leaders and local forms": {
			answer:  "Domain Name................: example.kr\nRegistered Date............: 2001. 01. 05.\nExpiration Date............: 2027. 01. 05.\nexpires:     20280101\n",
			expires: "2027-01-05T00:00:00Z", created: "2001-01-05T00:00:00Z",
		},
		"a zone and a remark": {
			answer:  "Domain Name: example.tw\nRecord expires on: 2027-05-31 00:00:00 (UTC+8)\nRecord created: 1998-06-01 12:00:00 +0200\n",
			expires: "2027-05-31T00:00:00Z", created: "1998-06-01T10:00:00Z",
		},
		"a day first in numbers": {
			answer: "Domain Name: example.test\nRegistrar: R\nExpiry Date: 03/04/2027\n", registrar: "R",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var doc whoisDocument
			doc.read(test.answer)
			text := func(s *string) string {
				if s == nil {
					return ""
				}
				return *s
			}
			if text(doc.Expires) != test.expires || text(doc.Created) != test.created || text(doc.Updated) != test.updated {
				t.Errorf("expires %q, created %q, updated %q; want %q, %q, %q", text(doc.Expires), text(doc.Created), text(doc.Updated), test.expires, test.created, test.updated)
			}
			if doc.Registrar != test.registrar || !doc.Registered {
				t.Errorf("registrar %q, registered %v", doc.Registrar, doc.Registered)
			}
			if !sameStrings(doc.NameServers, test.nameServers) || !sameStrings(doc.Status, test.status) {
				t.Errorf("name servers %v, status %v; want %v, %v", doc.NameServers, doc.Status, test.nameServers, test.status)
			}
		})
	}
}

func sameStrings(a, b []string) bool {
	return len(a) == len(b) && (len(a) == 0 || reflect.DeepEqual(a, b))
}

// The servers a question goes to are held to the target policy, the answer
// to the response limit, and a server that says nothing fails the scrape,
// as does a root server that names none.
func TestAWhoisScrapeIsHeldToThePolicyAndTheLimit(t *testing.T) {
	registry := startWhois(t, map[string]string{"example.test": whoisRegistryAnswer, "quiet.test": ""}, "No match\n")
	rootFor(t, registry.addr)
	denied := whoisCollector(t, WhoisConfig{}, func(c *model.Collector) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := whoisDoc(t, denied, "example.test"); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("err=%v, want the target refused", err)
	}
	if asked := registry.questions(); len(asked) != 0 {
		t.Errorf("a server the policy denies was asked %q", asked)
	}
	small := whoisCollector(t, WhoisConfig{Server: registry.addr}, func(c *model.Collector) { c.Request.MaxResponseBytes = 64 })
	if _, err := whoisDoc(t, small, "example.test"); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("err=%v, want the limit exceeded", err)
	}
	direct := whoisCollector(t, WhoisConfig{Server: registry.addr}, nil)
	if _, err := whoisDoc(t, direct, "quiet.test"); err == nil || !strings.Contains(err.Error(), "closed the connection without an answer") {
		t.Errorf("err=%v", err)
	}
	startedRoot := startWhois(t, nil, "% This query returned 0 objects.\n")
	whoisRootServer, whoisReferrals = startedRoot.addr, &whoisReferralCache{}
	if _, err := whoisDoc(t, whoisCollector(t, WhoisConfig{}, nil), "example.nowhere"); err == nil || !strings.Contains(err.Error(), "names no WHOIS server for .nowhere; set request.whois_server") {
		t.Errorf("err=%v", err)
	}
}

// A target is a domain and nothing else, and the keys are checked when the
// configuration loads.
func TestAWhoisTargetAndKeysAreChecked(t *testing.T) {
	c := whoisCollector(t, WhoisConfig{}, nil)
	for _, target := range []string{"https://example.com", "example.com:43", "example.com/path", "user@example.com", "localhost", "192.0.2.1", "exa mple.com", ".com", "bücher.test"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("target %q was taken", target)
		}
	}
	if err := CheckTarget(c, "Example.COM", false); err != nil {
		t.Error(err)
	}
	if label, err := (whoisRequester{}).Label(" Example.COM ", c, RequestOverrides{}); err != nil || label != "whois://example.com" {
		t.Errorf("label %q, %v", label, err)
	}
	for want, own := range map[string]WhoisConfig{
		"request.whois_server":                     {Server: "whois://whois.nic.example"},
		"has the port":                             {Server: "whois.nic.example:0"},
		"request.query_prefix is 65 characters":    {QueryPrefix: strings.Repeat("x", 65)},
		"request.query_prefix has a control chara": {QueryPrefix: "a\r\nb "},
	} {
		c := &model.Collector{Name: "domains", Request: model.RequestConfig{Type: RequestTypeWhois, Own: own}}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}

// The keys the opt-in whois type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheWhoisTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeWhois)
}
