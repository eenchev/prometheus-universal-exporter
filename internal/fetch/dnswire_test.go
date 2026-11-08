//go:build !select_request_types || request_type_dns

package fetch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Each record carries its TTL, as the server answered it, and the
// document the answer's flags: AA, RA and AD as the server set them.
func TestADNSDocumentHoldsTheTTLsAndTheFlags(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1").WithTTL(300), recA("www.example.test", "192.0.2.2").WithTTL(7))
	doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "A"), "www.example.test")
	if err != nil {
		t.Fatal(err)
	}
	answers, _ := json.Marshal(doc.Answers)
	if string(answers) != `[{"address":"192.0.2.1","ttl":300},{"address":"192.0.2.2","ttl":7}]` || doc.Authoritative || !doc.RecursionAvailable || doc.AuthenticatedData || doc.Rcode != "NOERROR" {
		t.Errorf("document %+v, answers %s", doc, answers)
	}
	s.SetFlags(true, true)
	if doc, err = resolveDoc(t, dnsCollector(t, s.Addr, "A"), "www.example.test"); err != nil || !doc.Authoritative || !doc.AuthenticatedData {
		t.Errorf("AA and AD: %+v, %v", doc, err)
	}
}

// SOA, CAA and DNSKEY are asked for and answered with their fields: SOA's
// primary server, mailbox and five numbers; CAA's flags, tag and value;
// DNSKEY's flags, protocol, algorithm and key tag, as RFC 4034 computes it.
func TestADNSCollectorAsksForSOACAAAndDNSKEY(t *testing.T) {
	key := []byte{3, 1, 0, 1, 0xa0, 0xb1, 0xc2, 0xd3}
	s := newStubDNS(t,
		testutil.DNSSOA("example.test", "ns1.example.test", "hostmaster.example.test", 2026110801, 7200, 900, 1209600, 300),
		testutil.DNSCAA("example.test", 0, "issue", "letsencrypt.org"), testutil.DNSCAA("example.test", 128, "iodef", "mailto:security@example.test"),
		testutil.DNSDNSKEY("example.test", 257, 13, key),
	)
	for recordType, want := range map[string]string{
		"SOA":    `[{"host":"ns1.example.test","mailbox":"hostmaster.example.test","serial":2026110801,"refresh":7200,"retry":900,"expire":1209600,"minimum":300,"ttl":60}]`,
		"CAA":    `[{"flags":128,"tag":"iodef","value":"mailto:security@example.test","ttl":60},{"flags":0,"tag":"issue","value":"letsencrypt.org","ttl":60}]`,
		"DNSKEY": `[{"flags":257,"protocol":3,"algorithm":13,"key_tag":` + keyTagJSON(dnsKeyTag(append([]byte{1, 1, 3, 13}, key...))) + `,"ttl":60}]`,
	} {
		doc, err := resolveDoc(t, dnsCollector(t, s.Addr, recordType), "example.test")
		answers, _ := json.Marshal(doc.Answers)
		if err != nil || string(answers) != want {
			t.Errorf("%s: %s, %v; want %s", recordType, answers, err, want)
		}
	}
	// RFC 4034 appendix B's sum, worked by hand for a short key.
	if got := dnsKeyTag([]byte{0x01, 0x01, 0x03, 0x08, 0x03, 0x01}); got != 0x0101+0x0308+0x0301 {
		t.Errorf("key tag %d", got)
	}
}

func keyTagJSON(v uint16) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A name's records behind a CNAME are its answer, the chain followed to
// its end in whatever order the server writes it; records of the type of
// a name the chain does not lead to are not.
func TestADNSAnswerFollowsTheCNAMEChain(t *testing.T) {
	s := newStubDNS(t,
		recName("a.example.test", dnsTypeCNAME, "b.example.test"),
		recName("b.example.test", dnsTypeCNAME, "c.example.test"),
		recA("c.example.test", "192.0.2.3"),
	)
	doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "A"), "a.example.test")
	if err != nil || doc.Count != 1 || doc.Answers[0].Address != "192.0.2.3" {
		t.Errorf("%+v, %v", doc, err)
	}
	// A message with an A record of a name the chain does not lead to.
	msg := dnsTestAnswer(t, 7, "a.example.test", 1, [][]byte{dnsTestRecord("other.example.test", 1, []byte{192, 0, 2, 9})})
	reply, err := readDNSAnswer(msg, 7, "a.example.test", 1, "s")
	if err != nil || len(reply.answers) != 0 {
		t.Errorf("a stray record: %+v, %v", reply, err)
	}
}

// The question goes with AD set and an OPT record offering 1232 bytes; a
// server that answers FORMERR to it is asked again without it.
func TestADNSQuestionAsksForADWithEDNSAndFallsBackWithout(t *testing.T) {
	query := dnsQuery(1, "example.test", 1, true, false)
	if query[2] != 0x01 || query[3] != 0x20 || binary.BigEndian.Uint16(query[10:]) != 1 || !strings.HasSuffix(string(query), string([]byte{0, 0, 41, 0x04, 0xd0, 0, 0, 0, 0, 0, 0})) {
		t.Errorf("query % x", query)
	}
	if plain := dnsQuery(1, "example.test", 1, false, false); binary.BigEndian.Uint16(plain[10:]) != 0 || len(plain) != len(query)-11 {
		t.Errorf("query without EDNS % x", plain)
	}
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	s.SetNoEDNS(true)
	before := len(s.Questions())
	doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "A"), "www.example.test")
	if err != nil || doc.Rcode != "NOERROR" || doc.Count != 1 || len(s.Questions())-before != 2 {
		t.Errorf("%+v, %v, asked %q", doc, err, s.Questions()[before:])
	}
}

// PTR is asked of the reverse name: an IPv4 address's octets reversed
// under in-addr.arpa, an IPv6 one's nibbles under ip6.arpa.
func TestADNSPTRQuestionIsOfTheReverseName(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.1":   "1.2.0.192.in-addr.arpa",
		"2001:db8::1": "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa",
	} {
		if got := dnsReverseName(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %s", addr, got)
		}
	}
}

// An answer whose records do not parse, or that is to another question,
// fails, saying it cannot be read.
func TestAMalformedDNSAnswerFails(t *testing.T) {
	for name, tc := range map[string]struct {
		msg   []byte
		qtype uint16
	}{
		"another id":           {dnsTestAnswer(t, 8, "a.example.test", 1, nil), 1},
		"another name":         {dnsTestAnswer(t, 7, "b.example.test", 1, nil), 1},
		"another type":         {dnsTestAnswer(t, 7, "a.example.test", 28, nil), 1},
		"an A record of 3":     {dnsTestAnswer(t, 7, "a.example.test", 1, [][]byte{dnsTestRecord("a.example.test", 1, []byte{1, 2, 3})}), 1},
		"an MX of one byte":    {dnsTestAnswer(t, 7, "a.example.test", 15, [][]byte{dnsTestRecord("a.example.test", 15, []byte{1})}), 15},
		"a record cut short":   {dnsTestAnswer(t, 7, "a.example.test", 1, [][]byte{dnsTestRecord("a.example.test", 1, []byte{1, 2, 3, 4})})[:40], 1},
		"a TXT string past it": {dnsTestAnswer(t, 7, "a.example.test", 16, [][]byte{dnsTestRecord("a.example.test", 16, []byte{9, 'x'})}), 16},
	} {
		if _, err := readDNSAnswer(tc.msg, 7, "a.example.test", tc.qtype, "s"); err == nil || !strings.Contains(err.Error(), "cannot unmarshal DNS message") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// dnsTestAnswer is an answer of id to the question of name of type qtype,
// NOERROR, with records.
func dnsTestAnswer(t *testing.T, id uint16, name string, qtype uint16, records [][]byte) []byte {
	t.Helper()
	msg := binary.BigEndian.AppendUint16(nil, id)
	msg = append(msg, 0x81, 0x80, 0, 1)
	msg = binary.BigEndian.AppendUint16(msg, uint16(len(records))) //nolint:gosec // a test's few records
	msg = append(msg, 0, 0, 0, 0)
	msg = append(msg, wireDNSName(name)...)
	msg = binary.BigEndian.AppendUint16(msg, qtype)
	msg = append(msg, 0, 1)
	for _, r := range records {
		msg = append(msg, r...)
	}
	return msg
}

// dnsTestRecord is a record of owner of type rtype, class IN, TTL 60.
func dnsTestRecord(owner string, rtype uint16, data []byte) []byte {
	r := wireDNSName(owner)
	r = binary.BigEndian.AppendUint16(r, rtype)
	r = append(r, 0, 1, 0, 0, 0, 60)
	r = binary.BigEndian.AppendUint16(r, uint16(len(data))) //nolint:gosec // a test's short data
	return append(r, data...)
}

// Of the system's servers, the next is asked after one that fails or
// answers SERVFAIL; an answer, even SERVFAIL, is kept over a later
// server's failure, and a good answer of a later server over both.
func TestTheNextDNSServerIsAskedAndAnAnswerIsKeptOverAFailure(t *testing.T) {
	failing := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	failing.SetRcode(2)
	good := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	closed := closedTCPPort(t)
	ctx := context.Background()
	reply, server, err := askDNSServers(ctx, []string{failing.Addr, closed}, dnsVia{tcp: true}, "www.example.test", 1)
	if err != nil || reply.rcode != "SERVFAIL" || server != failing.Addr {
		t.Errorf("SERVFAIL then a failure: %+v from %s, %v", reply, server, err)
	}
	reply, server, err = askDNSServers(ctx, []string{closed, failing.Addr, good.Addr}, dnsVia{tcp: true}, "www.example.test", 1)
	if err != nil || reply.rcode != "NOERROR" || len(reply.answers) != 1 || server != good.Addr {
		t.Errorf("a failure, SERVFAIL, then an answer: %+v from %s, %v", reply, server, err)
	}
	if _, server, err = askDNSServers(ctx, []string{closed}, dnsVia{tcp: true}, "www.example.test", 1); err == nil || server != closed {
		t.Errorf("a failure alone: %s, %v", server, err)
	}
}

// closedTCPPort is an address of 127.0.0.1 nothing listens on, over TCP.
func closedTCPPort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// An RRSIG question sets the DO bit and is answered with each signature of
// the name: the type it covers, its algorithm, labels, original TTL, key
// tag and signer, and its inception and expiration as times, with the
// seconds it has left. A server that refuses the OPT record is not asked
// again without it, which would get no signature: its FORMERR is the
// document's.
func TestADNSCollectorAnswersTheSignaturesOfAName(t *testing.T) {
	now := uint32(time.Now().Unix()) //nolint:gosec // until 2106
	s := newStubDNS(t,
		recA("example.test", "192.0.2.1"),
		testutil.DNSRRSIG("example.test", testutil.DNSTypeA, 13, 2, 3600, now+10*86400, now-86400, 2371, "example.test"),
		testutil.DNSRRSIG("example.test", testutil.DNSTypeSOA, 13, 2, 3600, now+86400, now-86400, 2371, "example.test"),
	)
	doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "RRSIG"), "example.test")
	if err != nil || doc.Count != 2 {
		t.Fatalf("%+v, %v", doc, err)
	}
	a, soa := doc.Answers[0], doc.Answers[1]
	if a.TypeCovered != "A" || soa.TypeCovered != "SOA" || *a.Algorithm != 13 || *a.Labels != 2 || *a.OriginalTTL != 3600 || *a.KeyTag != 2371 || a.Signer != "example.test" ||
		*a.Expiration != int64(now)+10*86400 || *a.Inception != int64(now)-86400 {
		t.Errorf("signatures %+v %+v", a, soa)
	}
	if left := *soa.SecondsUntilExpiration; left < 86400-5 || left > 86400 {
		t.Errorf("the SOA's signature has %d seconds left, want a day", left)
	}
	if do := s.DNSSEC(); len(do) == 0 || !do[len(do)-1] {
		t.Errorf("the RRSIG question set no DO bit: %v", do)
	}
	if _, err := resolveDoc(t, dnsCollector(t, s.Addr, "A"), "example.test"); err != nil {
		t.Fatal(err)
	}
	if do := s.DNSSEC(); do[len(do)-1] {
		t.Error("an A question set the DO bit")
	}
	s.SetNoEDNS(true)
	before := len(s.Questions())
	if doc, err := resolveDoc(t, dnsCollector(t, s.Addr, "RRSIG"), "example.test"); err != nil || doc.Rcode != "FORMERR" || len(s.Questions())-before != 1 {
		t.Errorf("a server without EDNS: %+v, %v, asked %d times", doc, err, len(s.Questions())-before)
	}
}

// A signature's times are serial numbers of 32 bits, each placed nearest
// to now: past 2106 the wire's small numbers are the next era's.
func TestADNSSignaturesTimesWrapAsSerialNumbers(t *testing.T) {
	now := time.Unix(1<<32-100, 0)
	answers := []dnsAnswer{{TypeCovered: "A", expiration: 50, inception: 1<<32 - 1000}}
	dnsSignatureTimes(answers, now)
	if *answers[0].Expiration != 1<<32+50 || *answers[0].Inception != 1<<32-1000 || *answers[0].SecondsUntilExpiration != 150 {
		t.Errorf("times %d %d %d", *answers[0].Expiration, *answers[0].Inception, *answers[0].SecondsUntilExpiration)
	}
}

// transport tls asks the server over TLS, port 853 when it names none,
// verifying its certificate with the tls block; tls needs a server, and a
// tls block needs transport tls.
func TestADNSCollectorAsksOverTLS(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	addr, caFile := s.ServeTLS(t)
	c := dnsCollector(t, addr, "A", dnsKeys(func(r *DNSConfig) { r.Transport = "tls" }), func(c *model.Collector) { c.Request.TLS.CAFile = caFile })
	doc, err := resolveDoc(t, c, "www.example.test")
	if err != nil || doc.Count != 1 || doc.Transport != "tls" {
		t.Fatalf("%+v, %v", doc, err)
	}
	if asked := s.Questions(); asked[len(asked)-1] != "www.example.test. A tls" {
		t.Errorf("asked %q", asked)
	}
	untrusted := dnsCollector(t, addr, "A", dnsKeys(func(r *DNSConfig) { r.Transport = "tls" }))
	if _, err := resolveDoc(t, untrusted, "www.example.test"); err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Errorf("an unknown authority: %v", err)
	}
	if got := dnsServerOf(DNSConfig{Server: "dns.example.test", Transport: "tls"}); got != "dns.example.test:853" {
		t.Errorf("a server without a port over TLS: %s", got)
	}
	if got := dnsServerOf(DNSConfig{Server: "dns.example.test:5353", Transport: "tls"}); got != "dns.example.test:5353" {
		t.Errorf("a server with a port over TLS: %s", got)
	}
	for name, edit := range map[string]func(*model.Collector){
		"tls without a server":    dnsKeys(func(r *DNSConfig) { r.Transport, r.Server = "tls", "" }),
		"a tls block without tls": func(c *model.Collector) { c.Request.TLS.CAFile = caFile },
	} {
		c := &model.Collector{Name: "resolve", Request: model.RequestConfig{Type: RequestTypeDNS, Own: DNSConfig{Server: "10.0.0.53"}}}
		edit(c)
		if err := ValidateRequest(c); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// A dry run reads the files of DNS over TLS's tls block, as a scrape would,
// and says what a scrape would find wrong with them.
func TestADryRunReadsTheFilesOfDNSOverTLS(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	addr, caFile := s.ServeTLS(t)
	c := dnsCollector(t, addr, "A", dnsKeys(func(r *DNSConfig) { r.Transport = "tls" }), func(c *model.Collector) { c.Request.TLS.CAFile = caFile })
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	missing := dnsCollector(t, addr, "A", dnsKeys(func(r *DNSConfig) { r.Transport = "tls" }), func(c *model.Collector) { c.Request.TLS.CAFile = caFile + ".missing" })
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.tls") {
		t.Errorf("warnings %v", warnings)
	}
}
