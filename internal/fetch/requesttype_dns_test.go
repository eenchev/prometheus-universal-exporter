//go:build !select_request_types || request_type_dns

package fetch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The DNS wire format is not vendored (golang.org/x/net/dns/dnsmessage is
// not among the modules), so the stand-in server below reads a question and
// writes its answer by hand: RFC 1035's header, one question, and answers
// whose owner names are written out, never compressed, apart from a pointer
// to the question's name.

// DNS record types, by number.
const (
	dnsTypeA     = 1
	dnsTypeNS    = 2
	dnsTypeCNAME = 5
	dnsTypePTR   = 12
	dnsTypeMX    = 15
	dnsTypeTXT   = 16
	dnsTypeAAAA  = 28
	dnsTypeSRV   = 33
)

// stubRecord is one record the stand-in server answers: its owner name,
// lower case and absolute, its type and its data as the wire carries it.
type stubRecord struct {
	owner string
	rtype uint16
	data  []byte
}

// stubDNS is a DNS server on 127.0.0.1, over UDP and TCP on the same port.
// rcode, when set, answers every question with that response code and no
// record; silent answers nothing at all.
type stubDNS struct {
	t       *testing.T
	addr    string
	mu      sync.Mutex
	records []stubRecord
	rcode   int
	silent  bool
	// asked is every question, as "name type network".
	asked []string
}

// wireName is name as the wire writes it, in labels.
func wireName(name string) []byte {
	var b []byte
	for label := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }

// The records' data, by type.
func recA(owner, ip string) stubRecord {
	return stubRecord{strings.ToLower(owner) + ".", dnsTypeA, netip.MustParseAddr(ip).AsSlice()}
}
func recAAAA(owner, ip string) stubRecord {
	return stubRecord{strings.ToLower(owner) + ".", dnsTypeAAAA, netip.MustParseAddr(ip).AsSlice()}
}
func recName(owner string, rtype uint16, target string) stubRecord {
	return stubRecord{strings.ToLower(owner) + ".", rtype, wireName(target)}
}
func recMX(owner string, pref uint16, host string) stubRecord {
	return stubRecord{strings.ToLower(owner) + ".", dnsTypeMX, append(u16(pref), wireName(host)...)}
}
func recSRV(owner string, priority, weight, port uint16, target string) stubRecord {
	data := append(append(append(u16(priority), u16(weight)...), u16(port)...), wireName(target)...)
	return stubRecord{strings.ToLower(owner) + ".", dnsTypeSRV, data}
}
func recTXT(owner string, parts ...string) stubRecord {
	var data []byte
	for _, part := range parts {
		data = append(append(data, byte(len(part))), part...)
	}
	return stubRecord{strings.ToLower(owner) + ".", dnsTypeTXT, data}
}

// newStubDNS starts a server answering records.
func newStubDNS(t *testing.T, records ...stubRecord) *stubDNS {
	t.Helper()
	s := &stubDNS{t: t, records: records}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Skipf("no UDP on the port of the TCP listener: %v", err)
	}
	s.addr = tcp.Addr().String()
	t.Cleanup(func() { _ = tcp.Close(); _ = udp.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			if answer := s.answer(buf[:n], "udp"); answer != nil {
				_, _ = udp.WriteTo(answer, from)
			}
		}
	}()
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				for {
					var size [2]byte
					if _, err := io.ReadFull(conn, size[:]); err != nil {
						return
					}
					query := make([]byte, binary.BigEndian.Uint16(size[:]))
					if _, err := io.ReadFull(conn, query); err != nil {
						return
					}
					answer := s.answer(query, "tcp")
					if answer == nil {
						return
					}
					_, _ = conn.Write(append(u16(uint16(len(answer))), answer...))
				}
			}()
		}
	}()
	return s
}

// answer is the server's answer to query, nil to answer nothing.
func (s *stubDNS) answer(query []byte, network string) []byte {
	if len(query) < 12 {
		return nil
	}
	// The question's name, its type and class.
	i := 12
	var labels strings.Builder
	for i < len(query) && query[i] != 0 {
		n := int(query[i])
		if i+1+n > len(query) {
			return nil
		}
		labels.WriteString(strings.ToLower(string(query[i+1 : i+1+n])))
		labels.WriteByte('.')
		i += 1 + n
	}
	name := labels.String()
	if i+5 > len(query) {
		return nil
	}
	questionEnd := i + 5
	qtype := binary.BigEndian.Uint16(query[i+1 : i+3])
	s.mu.Lock()
	s.asked = append(s.asked, name+" "+dnsTypeName(qtype)+" "+network)
	records, rcode, silent := s.records, s.rcode, s.silent
	s.mu.Unlock()
	if silent {
		return nil
	}
	var answers [][]byte
	if rcode == 0 {
		// A CNAME of the name answers any question of it, followed by the
		// records of its target, as a recursive server answers.
		owner := name
		for range 8 {
			var next string
			for _, r := range records {
				if r.owner == owner && r.rtype == dnsTypeCNAME && qtype != dnsTypeCNAME {
					answers = append(answers, s.record(r, name))
					next = string(decodeWireName(r.data))
				}
			}
			for _, r := range records {
				if r.owner == owner && r.rtype == qtype {
					answers = append(answers, s.record(r, name))
				}
			}
			if next == "" {
				break
			}
			owner = next
		}
		known := false
		for _, r := range records {
			known = known || r.owner == name
		}
		if !known {
			rcode = 3 // NXDOMAIN
		}
	}
	header := make([]byte, 12)
	copy(header, query[:2])
	binary.BigEndian.PutUint16(header[2:], 0x8180|uint16(rcode)) // QR, RD, RA
	binary.BigEndian.PutUint16(header[4:], 1)
	binary.BigEndian.PutUint16(header[6:], uint16(len(answers)))
	out := append(header, query[12:questionEnd]...)
	for _, a := range answers {
		out = append(out, a...)
	}
	return out
}

// record is r written as an answer: its owner a pointer to the question's
// name when it is that name, written out otherwise; class IN; a TTL of 60.
func (s *stubDNS) record(r stubRecord, question string) []byte {
	var b []byte
	if r.owner == question {
		b = []byte{0xc0, 12}
	} else {
		b = wireName(r.owner)
	}
	b = append(b, u16(r.rtype)...)
	b = append(b, u16(1)...)
	b = binary.BigEndian.AppendUint32(b, 60)
	b = append(b, u16(uint16(len(r.data)))...)
	return append(b, r.data...)
}

// decodeWireName reads a name written in labels, as wireName writes it.
func decodeWireName(b []byte) []byte {
	var out []byte
	for i := 0; i < len(b) && b[i] != 0; i += 1 + int(b[i]) {
		out = append(append(out, b[i+1:i+1+int(b[i])]...), '.')
	}
	return out
}

func dnsTypeName(t uint16) string {
	return map[uint16]string{dnsTypeA: "A", dnsTypeNS: "NS", dnsTypeCNAME: "CNAME", dnsTypePTR: "PTR", dnsTypeMX: "MX", dnsTypeTXT: "TXT", dnsTypeAAAA: "AAAA", dnsTypeSRV: "SRV"}[t]
}

// questions is what the server was asked.
func (s *stubDNS) questions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// dnsCollector is a validated dns collector asking server for recordType.
func dnsCollector(t *testing.T, server, recordType string, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "resolve", Request: model.RequestConfig{Type: RequestTypeDNS, RecordType: recordType, Server: server}}
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
		{"CNAME", "www.example.test", `[]`},
		{" mx ", "example.test", `[{"host":"mx1.example.test","preference":10},{"host":"mx2.example.test","preference":20}]`},
		{"NS", "example.test", `[{"host":"ns1.example.test"}]`},
		{"TXT", "example.test", `[{"text":"hello"},{"text":"v=spf1 -all"}]`},
		{"SRV", "_http._tcp.example.test", `[{"target":"front.example.test","priority":0,"weight":1,"port":80},{"target":"web.example.test","priority":10,"weight":5,"port":8080}]`},
		{"PTR", "192.0.2.1", `[{"name":"www.example.test"}]`},
	} {
		c := dnsCollector(t, s.addr, tc.recordType)
		doc, err := resolveDoc(t, c, tc.target)
		if err != nil {
			t.Errorf("%s %s: %v", tc.recordType, tc.target, err)
			continue
		}
		answers, _ := json.Marshal(doc.Answers)
		if string(answers) != tc.want {
			t.Errorf("%s %s: answers %s, want %s", tc.recordType, tc.target, answers, tc.want)
		}
		if doc.NotFound || doc.Count != len(doc.Answers) || doc.RecordType != strings.ToUpper(strings.TrimSpace(tc.recordType)) || doc.Server != s.addr || doc.Transport != "udp" || doc.DurationSeconds <= 0 {
			t.Errorf("%s %s: document %+v", tc.recordType, tc.target, doc)
		}
	}
	if doc, _ := resolveDoc(t, dnsCollector(t, s.addr, "A"), "Www.Example.Test"); doc.Name != "www.example.test" {
		t.Errorf("the name is shown as %q", doc.Name)
	}
	// The name is asked as an absolute one: no search domain of the
	// system's configuration is appended to it.
	for _, q := range s.questions() {
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
		doc, err := resolveDoc(t, dnsCollector(t, s.addr, recordType), target)
		if err != nil || !doc.NotFound || doc.Count != 0 || doc.Answers == nil || len(doc.Answers) != 0 {
			t.Errorf("%s %s: %+v, %v", recordType, target, doc, err)
		}
	}
	for rcode, name := range map[int]string{2: "SERVFAIL", 5: "REFUSED"} {
		s.mu.Lock()
		s.rcode = rcode
		s.mu.Unlock()
		_, err := resolveDoc(t, dnsCollector(t, s.addr, "A"), "www.example.test")
		if err == nil || !strings.Contains(err.Error(), "on "+s.addr) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s.mu.Lock()
	s.rcode, s.silent = 0, true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := FetchCollector(ctx, "www.example.test", dnsCollector(t, s.addr, "A"), RequestOverrides{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "resolving A www.example.test on "+s.addr) {
		t.Errorf("a server that does not answer: %v", err)
	}
}

// transport tcp asks over TCP alone; udp, the default, over UDP.
func TestADNSCollectorAsksOverTheTransportItNames(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	for _, transport := range []string{"", "tcp", "udp"} {
		before := len(s.questions())
		doc, err := resolveDoc(t, dnsCollector(t, s.addr, "A", func(c *model.Collector) { c.Request.Transport = transport }), "www.example.test")
		if err != nil || doc.Count != 1 {
			t.Fatalf("%q: %+v, %v", transport, doc, err)
		}
		want := "udp"
		if transport == "tcp" {
			want = "tcp"
		}
		for _, q := range s.questions()[before:] {
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
		"an unknown record type":   func(c *model.Collector) { c.Request.RecordType = "SOA" },
		"a server with a scheme":   func(c *model.Collector) { c.Request.Server = "udp://10.0.0.53" },
		"a server with a bad port": func(c *model.Collector) { c.Request.Server = "10.0.0.53:99999" },
		"a server of blanks":       func(c *model.Collector) { c.Request.Server = "  " },
		"an unknown transport":     func(c *model.Collector) { c.Request.Transport = "quic" },
		"a transport in capitals":  func(c *model.Collector) { c.Request.Transport = "TCP" },
		"another type's key":       func(c *model.Collector) { c.Request.Path = "/x" },
	} {
		c := &model.Collector{Name: "resolve", Request: model.RequestConfig{Type: RequestTypeDNS}}
		edit(c)
		if err := ValidateRequest(c); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	for _, server := range []string{"10.0.0.53", "10.0.0.53:5353", "[2001:db8::53]:53", "[2001:db8::53]", "2001:db8::53", "dns.example.test"} {
		c := &model.Collector{Name: "resolve", Request: model.RequestConfig{Type: RequestTypeDNS, Server: server}}
		if err := ValidateRequest(c); err != nil || c.Request.RecordType != "A" {
			t.Errorf("server %q: %v, record type %q", server, err, c.Request.RecordType)
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
		c := dnsCollector(t, s.addr, tc.recordType, func(c *model.Collector) {
			c.Request.AllowedTargets, c.Request.DeniedTargets = tc.allowed, tc.denied
		})
		before := len(s.questions())
		_, err := resolveDoc(t, c, tc.target)
		if refused := errors.Is(err, ErrTargetRefused); refused != tc.refused || (!tc.refused && err != nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.refused && len(s.questions()) != before {
			t.Errorf("%s: the server was asked %v", tc.name, s.questions()[before:])
		}
	}
}

// A debug probe's request section shows the lookup: the DNS URI without its
// query, the record type, the server and the transport, and how it ended.
func TestADNSLookupIsTracedForADebugProbe(t *testing.T) {
	s := newStubDNS(t, recA("www.example.test", "192.0.2.1"))
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, "www.example.test", dnsCollector(t, s.addr, "A"), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	requests := trace.Requests()
	if len(requests) != 1 {
		t.Fatalf("traced %+v", requests)
	}
	r := requests[0]
	if r.Method != "QUERY" || r.URL != "dns://"+s.addr+"/www.example.test" || r.Header.Get("Dns-Type") != "A" || r.Header.Get("Dns-Server") != s.addr || r.Header.Get("Dns-Transport") != "udp" || r.Outcome != "1 record" {
		t.Errorf("traced %+v", r)
	}
}
