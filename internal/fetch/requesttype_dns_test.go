//go:build !select_request_types || request_type_dns

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The stand-in server is testutil.DNSStub; these are its parts by shorter
// names.
const (
	dnsTypeNS    = testutil.DNSTypeNS
	dnsTypeCNAME = testutil.DNSTypeCNAME
	dnsTypePTR   = testutil.DNSTypePTR
)

var (
	newStubDNS = testutil.NewDNSStub
	recA       = testutil.DNSA
	recAAAA    = testutil.DNSAAAA
	recName    = testutil.DNSNamed
	recMX      = testutil.DNSMX
	recSRV     = testutil.DNSSRV
	recTXT     = testutil.DNSTXT
)

// dnsKeys is an edit of a dns collector's own keys.
func dnsKeys(edit func(*DNSConfig)) func(*model.Collector) {
	return func(c *model.Collector) {
		r := dnsRequest(c)
		edit(&r)
		c.Request.Own = r
	}
}

// dnsCollector is a validated dns collector asking server for recordType.
func dnsCollector(t *testing.T, server, recordType string, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "resolve", Request: model.RequestConfig{Type: RequestTypeDNS, Own: DNSConfig{RecordType: recordType, Server: server}}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// resolveDoc is a scrape of target through c, and its document.
func resolveDoc(t *testing.T, c *model.Collector, target string) (dnsDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return dnsDocument{}, err
	}
	if got := resp.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q", got)
	}
	var doc dnsDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v\n%s", err, resp.Body)
	}
	return doc, nil
}

// Every record type is asked for of the configured server and answered as
// the document's records, each with the fields of its type, the names
// without their trailing dot, in an order of their own; and the document
// says what was asked, of which server, and how long it took.
func TestADNSCollectorAnswersEachRecordTypeAsADocument(t *testing.T) {
	s := newStubDNS(t,
		recA("www.example.test", "192.0.2.2"), recA("www.example.test", "192.0.2.1"),
		recAAAA("www.example.test", "2001:db8::1"),
		recName("alias.example.test", dnsTypeCNAME, "www.example.test"),
		recMX("example.test", 20, "mx2.example.test"), recMX("example.test", 10, "mx1.example.test"),
		recName("example.test", dnsTypeNS, "ns1.example.test"),
		recTXT("example.test", "v=spf1 ", "-all"), recTXT("example.test", "hello"),
		recSRV("_http._tcp.example.test", 10, 5, 8080, "web.example.test"), recSRV("_http._tcp.example.test", 0, 1, 80, "front.example.test"),
		recName("1.2.0.192.in-addr.arpa", dnsTypePTR, "www.example.test"),
	)
	for _, tc := range []struct {
		recordType, target, want string
	}{
		{"A", "WWW.example.test.", `[{"address":"192.0.2.1"},{"address":"192.0.2.2"}]`},
		{"aaaa", "www.example.test", `[{"address":"2001:db8::1"}]`},
		{"CNAME", "alias.example.test", `[{"target":"www.example.test"}]`},
		{" mx ", "example.test", `[{"host":"mx1.example.test","preference":10},{"host":"mx2.example.test","preference":20}]`},
		{"NS", "example.test", `[{"host":"ns1.example.test"}]`},
		{"TXT", "example.test", `[{"text":"hello"},{"text":"v=spf1 -all"}]`},
		{"SRV", "_http._tcp.example.test", `[{"target":"front.example.test","priority":0,"weight":1,"port":80},{"target":"web.example.test","priority":10,"weight":5,"port":8080}]`},
		{"PTR", "192.0.2.1", `[{"name":"www.example.test"}]`},
	} {
		c := dnsCollector(t, s.Addr, tc.recordType)
		doc, err := resolveDoc(t, c, tc.target)
		if err != nil {
			t.Errorf("%s %s: %v", tc.recordType, tc.target, err)
			continue
		}
		answers, _ := json.Marshal(doc.Answers)
		if string(answers) != tc.want {
			t.Errorf("%s %s: answers %s, want %s", tc.recordType, tc.target, answers, tc.want)
		}
		if doc.NotFound || doc.Count != len(doc.Answers) || doc.RecordType != strings.ToUpper(strings.TrimSpace(tc.recordType)) || doc.Server != s.Addr || doc.Transport != "udp" || doc.DurationSeconds <= 0 {
			t.Errorf("%s %s: document %+v", tc.recordType, tc.target, doc)
		}
	}
	if doc, _ := resolveDoc(t, dnsCollector(t, s.Addr, "A"), "Www.Example.Test"); doc.Name != "www.example.test" {
		t.Errorf("the name is shown as %q", doc.Name)
	}
	// The name is asked as an absolute one: no search domain of the
	// system's configuration is appended to it.
	for _, q := range s.Questions() {
		if !strings.HasSuffix(strings.Fields(q)[0], ".test.") && !strings.HasSuffix(strings.Fields(q)[0], ".arpa.") {
			t.Errorf("the server was asked %q", q)
		}
	}
}

// A name that does not exist, and one without records of the type, are
// answers: no records and not_found true. A server failure, a refusal and
// a server that does not answer within the deadline fail the scrape,
// naming the server asked, not the system's.
func TestADNSNameNotFoundIsAnAnswerAndAServerFailureFails(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	for _, target := range []string{"missing.example.test", "www.example.test"} {
		recordType := "A"
		if target == "www.example.test" {
			recordType = "MX"
		}
		doc, err := resolveDoc(t, dnsCollector(t, s.Addr, recordType), target)
		if err != nil || !doc.NotFound || doc.Count != 0 || doc.Answers == nil || len(doc.Answers) != 0 {
			t.Errorf("%s %s: %+v, %v", recordType, target, doc, err)
		}
	}
	for rcode, name := range map[int]string{2: "SERVFAIL", 5: "REFUSED"} {
		s.SetRcode(rcode)
		_, err := resolveDoc(t, dnsCollector(t, s.Addr, "A"), "www.example.test")
		if err == nil || !strings.Contains(err.Error(), "on "+s.Addr) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s.SetRcode(0)
	s.SetSilent(true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := FetchCollector(ctx, "www.example.test", dnsCollector(t, s.Addr, "A"), RequestOverrides{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "resolving A www.example.test on "+s.Addr) {
		t.Errorf("a server that does not answer: %v", err)
	}
}

// transport tcp asks over TCP alone; udp, the default, over UDP.
func TestADNSCollectorAsksOverTheTransportItNames(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	for _, transport := range []string{"", "tcp", "udp"} {
		before := len(s.Questions())
		doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "A", dnsKeys(func(r *DNSConfig) { r.Transport = transport })), "www.example.test")
		if err != nil || doc.Count != 1 {
			t.Fatalf("%q: %+v, %v", transport, doc, err)
		}
		want := "udp"
		if transport == "tcp" {
			want = "tcp"
		}
		for _, q := range s.Questions()[before:] {
			if !strings.HasSuffix(q, " "+want) {
				t.Errorf("transport %q: the server was asked %q", transport, q)
			}
		}
		if doc.Transport != want {
			t.Errorf("transport %q: the document says %q", transport, doc.Transport)
		}
	}
}

// A dns collector's keys are held to their rules when the configuration
// loads: a record type of the list, in any case; a server that is host or
// host:port; udp or tcp. A key of another type is refused.
func TestADNSCollectorsKeysAreCheckedAtLoad(t *testing.T) {
	for name, edit := range map[string]func(*model.Collector){
		"an unknown record type":   dnsKeys(func(r *DNSConfig) { r.RecordType = "SOA" }),
		"a server with a scheme":   dnsKeys(func(r *DNSConfig) { r.Server = "udp://10.0.0.53" }),
		"a server with a bad port": dnsKeys(func(r *DNSConfig) { r.Server = "10.0.0.53:99999" }),
		"a server of blanks":       dnsKeys(func(r *DNSConfig) { r.Server = "  " }),
		"an unknown transport":     dnsKeys(func(r *DNSConfig) { r.Transport = "quic" }),
		"a transport in capitals":  dnsKeys(func(r *DNSConfig) { r.Transport = "TCP" }),
		"another type's key":       func(c *model.Collector) { c.Request.Path = "/x" },
	} {
		c := &model.Collector{Name: "resolve", Request: model.RequestConfig{Type: RequestTypeDNS}}
		edit(c)
		if err := ValidateRequest(c); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	for _, server := range []string{"10.0.0.53", "10.0.0.53:5353", "[2001:db8::53]:53", "[2001:db8::53]", "2001:db8::53", "dns.example.test"} {
		c := &model.Collector{Name: "resolve", Request: model.RequestConfig{Type: RequestTypeDNS, Own: DNSConfig{Server: server}}}
		if err := ValidateRequest(c); err != nil || dnsRequest(c).RecordType != "A" {
			t.Errorf("server %q: %v, record type %q", server, err, dnsRequest(c).RecordType)
		}
	}
}

// A target is a name, or for PTR an address: a URL, a name with a port, an
// address for another record type, a name too long or with an empty or long
// label are refused before anything is asked, as the probe's 400 refuses
// them; the url label is the DNS URI of RFC 4501, and the log shows the
// target as written.
func TestADNSTargetIsANameOrForPTRAnAddress(t *testing.T) {
	a := dnsCollector(t, "", "A")
	ptr := dnsCollector(t, "10.0.0.53", "PTR")
	for _, target := range []string{"https://example.test", "example.test:53", "example.test/x", "user@example.test", "192.0.2.1", "a..b", strings.Repeat("a", 64) + ".test", strings.Repeat("abcdefghi.", 26) + "test", "exa mple.test", "bücher.test"} {
		if err := CheckTarget(a, target, false); err == nil {
			t.Errorf("A %q: taken", target)
		}
	}
	for _, target := range []string{"example.test", "fe80::1%eth0"} {
		if err := CheckTarget(ptr, target, false); err == nil {
			t.Errorf("PTR %q: taken", target)
		}
	}
	for _, target := range []string{"example.test", "Example.Test.", "_http._tcp.example.test", "xn--bcher-kva.test"} {
		if err := CheckTarget(a, target, false); err != nil {
			t.Errorf("A %q: %v", target, err)
		}
	}
	for _, target := range []string{"192.0.2.1", "2001:db8::1", "[2001:db8::1]"} {
		if err := CheckTarget(ptr, target, false); err != nil {
			t.Errorf("PTR %q: %v", target, err)
		}
	}
	if label, err := RequestLabelFor("Example.Test", a, RequestOverrides{}); err != nil || label != "dns:example.test?type=A" {
		t.Errorf("label %q, %v", label, err)
	}
	if label, err := RequestLabelFor("192.0.2.1", ptr, RequestOverrides{}); err != nil || label != "dns://10.0.0.53:53/192.0.2.1?type=PTR" {
		t.Errorf("label %q, %v", label, err)
	}
	if method := RequestMethodFor(a, RequestOverrides{}); method != "QUERY" {
		t.Errorf("method %q", method)
	}
	if shown := DisplayTarget(a, " example.test "); shown != "example.test" {
		t.Errorf("shown %q", shown)
	}
	if shown := DisplayTarget(a, "http://user:secret@example.test"); shown != "<invalid target>" {
		t.Errorf("shown %q", shown)
	}
	if stage := FetchStage(a); stage != "dns" {
		t.Errorf("stage %q", stage)
	}
}

// allowed_targets and denied_targets match a name by name, never by what it
// resolves to: a denied name, and with allowed_targets a name it does not
// list, are refused before the server is asked; a network in
// allowed_targets allows no name; a PTR target is matched as an address.
func TestADNSCollectorsTargetListsMatchTheName(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"), recA("db.internal.test", "10.0.0.5"), recName("5.0.0.10.in-addr.arpa", dnsTypePTR, "db.internal.test"))
	for _, tc := range []struct {
		name            string
		allowed, denied []string
		recordType      string
		target          string
		refused         bool
	}{
		{"a denied glob", nil, []string{"*.internal.test"}, "A", "db.internal.test", true},
		{"a name not allowed", []string{"*.example.test"}, nil, "A", "db.internal.test", true},
		{"an allowed name", []string{"*.example.test"}, nil, "A", "www.example.test", false},
		{"a network allows no name", []string{"192.0.2.0/24"}, nil, "A", "www.example.test", true},
		{"a denied network, whose name resolves into it", nil, []string{"10.0.0.0/8"}, "A", "db.internal.test", false},
		{"a denied address", nil, []string{"10.0.0.0/8"}, "PTR", "10.0.0.5", true},
		{"an allowed address", []string{"10.0.0.0/8"}, nil, "PTR", "10.0.0.5", false},
	} {
		c := dnsCollector(t, s.Addr, tc.recordType, func(c *model.Collector) {
			c.Request.AllowedTargets, c.Request.DeniedTargets = tc.allowed, tc.denied
		})
		before := len(s.Questions())
		_, err := resolveDoc(t, c, tc.target)
		if refused := errors.Is(err, ErrTargetRefused); refused != tc.refused || (!tc.refused && err != nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.refused && len(s.Questions()) != before {
			t.Errorf("%s: the server was asked %v", tc.name, s.Questions()[before:])
		}
	}
}

// A debug probe's request section shows the lookup: the DNS URI without its
// query, the record type, the server and the transport, and how it ended.
func TestADNSLookupIsTracedForADebugProbe(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, "www.example.test", dnsCollector(t, s.Addr, "A"), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	requests := trace.Requests()
	if len(requests) != 1 {
		t.Fatalf("traced %+v", requests)
	}
	r := requests[0]
	if r.Method != "QUERY" || r.URL != "dns://"+s.Addr+"/www.example.test" || r.Header.Get("Dns-Type") != "A" || r.Header.Get("Dns-Server") != s.Addr || r.Header.Get("Dns-Transport") != "udp" || r.Outcome != "1 record" {
		t.Errorf("traced %+v", r)
	}
}

// A CNAME collector asks the CNAME question itself, alone: an alias answers
// the name its record points to; a name that is no alias — with addresses,
// with a TXT record alone — and a name that does not exist have no CNAME
// record, and are not found, as a name without records of the type is for
// every other type. A server failure fails, naming the server; transport tcp
// asks over TCP.
func TestADNSCNAMELookupAsksForTheCNAMERecordAlone(t *testing.T) {
	s := newStubDNS(t,
		recName("alias.example.test", dnsTypeCNAME, "www.example.test"),
		recA("www.example.test", "192.0.2.1"),
		recTXT("text.example.test", "only text"),
	)
	for target, want := range map[string]string{
		"alias.example.test":   `[{"target":"www.example.test"}]`,
		"www.example.test":     "not found",
		"text.example.test":    "not found",
		"missing.example.test": "not found",
	} {
		before := len(s.Questions())
		doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "CNAME"), target)
		answers, _ := json.Marshal(doc.Answers)
		got := string(answers)
		if doc.NotFound {
			got = "not found"
		}
		if err != nil || got != want || doc.Count != len(doc.Answers) || (doc.NotFound && (doc.Count != 0 || doc.Answers == nil)) {
			t.Errorf("CNAME %s: %+v, %v; want %s", target, doc, err, want)
		}
		if asked := s.Questions()[before:]; len(asked) != 1 || asked[0] != target+". CNAME udp" {
			t.Errorf("CNAME %s: the server was asked %q, want the CNAME question alone", target, asked)
		}
	}
	before := len(s.Questions())
	if doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "CNAME", dnsKeys(func(r *DNSConfig) { r.Transport = "tcp" })), "alias.example.test"); err != nil || doc.Count != 1 {
		t.Errorf("CNAME over tcp: %+v, %v", doc, err)
	}
	if asked := s.Questions()[before:]; len(asked) != 1 || asked[0] != "alias.example.test. CNAME tcp" {
		t.Errorf("transport tcp: the server was asked %q", asked)
	}
	// An answer marked truncated over UDP is asked again over TCP.
	s.SetTruncate(true)
	before = len(s.Questions())
	if doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "CNAME"), "alias.example.test"); err != nil || doc.Count != 1 {
		t.Errorf("CNAME truncated over udp: %+v, %v", doc, err)
	}
	if asked := s.Questions()[before:]; strings.Join(asked, ",") != "alias.example.test. CNAME udp,alias.example.test. CNAME tcp" {
		t.Errorf("a truncated answer: the server was asked %q", asked)
	}
	s.SetRcode(2)
	s.SetTruncate(false)
	if _, err := resolveDoc(t, dnsCollector(t, s.Addr, "CNAME"), "alias.example.test"); err == nil || !strings.Contains(err.Error(), "resolving CNAME alias.example.test on "+s.Addr) || !strings.Contains(err.Error(), "SERVFAIL") {
		t.Errorf("SERVFAIL: %v", err)
	}
	s.SetRcode(0)
	s.SetSilent(true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := FetchCollector(ctx, "alias.example.test", dnsCollector(t, s.Addr, "CNAME"), RequestOverrides{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "resolving CNAME alias.example.test on "+s.Addr) {
		t.Errorf("a server that does not answer: %v", err)
	}
}

// Without request.server, a CNAME question goes to the nameservers of
// /etc/resolv.conf, on port 53, or to the local server when it names none.
func TestTheSystemsNameserversAreReadForACNAMELookup(t *testing.T) {
	conf := "# comment\nsearch example.test\nnameserver 10.0.0.53\nnameserver 2001:db8::53\noptions ndots:2\n"
	if got := strings.Join(dnsSystemServers(conf), " "); got != "10.0.0.53:53 [2001:db8::53]:53" {
		t.Errorf("servers %q", got)
	}
	if got := strings.Join(dnsSystemServers(""), " "); got != "127.0.0.1:53 [::1]:53" {
		t.Errorf("servers of an empty file %q", got)
	}
}
