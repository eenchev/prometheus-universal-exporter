package testutil

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The DNS wire format is not vendored (golang.org/x/net/dns/dnsmessage is
// not among the modules), so the stand-in server below reads a question and
// writes its answer by hand: RFC 1035's header, one question, and answers
// whose owner names are written out, never compressed, apart from a pointer
// to the question's name. The dns request type's tests and the allocator's
// dns_sd tests share it.

// DNS record types, by number.
const (
	DNSTypeA     = 1
	DNSTypeNS    = 2
	DNSTypeCNAME = 5
	DNSTypePTR   = 12
	DNSTypeMX    = 15
	DNSTypeTXT   = 16
	DNSTypeAAAA  = 28
	DNSTypeSRV   = 33
)

// DNSRecord is one record the stand-in server answers: its owner name,
// lower case and absolute, its type and its data as the wire carries it.
type DNSRecord struct {
	Owner string
	Type  uint16
	Data  []byte
}

// DNSStub is a DNS server on 127.0.0.1, over UDP and TCP on the same port
// Addr. A name it has no record of is answered NXDOMAIN; a name it has
// records of, but none of the type asked, is answered with no record.
type DNSStub struct {
	Addr    string
	mu      sync.Mutex
	records []DNSRecord
	// rcode, when set, answers every question with that response code and
	// no record; silent answers nothing at all.
	rcode  int
	silent bool
	// truncate answers every question over UDP with no record, marked
	// truncated, as a server does an answer too long for UDP.
	truncate bool
	// asked is every question, as "name type network".
	asked []string
}

// wireName is name as the wire writes it, in labels.
func wireName(name string) []byte {
	var b []byte
	for label := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		b = append(b, len8(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }

// len8 and len16 are a length as the wire writes it, in a byte or two. The
// stand-in writes only what a test hands it, so a length that does not fit
// is the test's mistake, told at once rather than written wrong.
func len8(n int) byte {
	if n < 0 || n > math.MaxUint8 {
		panic(fmt.Sprintf("a DNS length of %d does not fit in a byte", n))
	}
	return byte(n) // #nosec G115 -- checked above
}

func len16(n int) uint16 {
	if n < 0 || n > math.MaxUint16 {
		panic(fmt.Sprintf("a DNS length of %d does not fit in 16 bits", n))
	}
	return uint16(n) // #nosec G115 -- checked above
}

// DNSA is an A record of owner.
func DNSA(owner, ip string) DNSRecord {
	return DNSRecord{strings.ToLower(owner) + ".", DNSTypeA, netip.MustParseAddr(ip).AsSlice()}
}

// DNSAAAA is an AAAA record of owner.
func DNSAAAA(owner, ip string) DNSRecord {
	return DNSRecord{strings.ToLower(owner) + ".", DNSTypeAAAA, netip.MustParseAddr(ip).AsSlice()}
}

// DNSNamed is a record of owner whose data is a name alone: CNAME, NS or
// PTR.
func DNSNamed(owner string, rtype uint16, target string) DNSRecord {
	return DNSRecord{strings.ToLower(owner) + ".", rtype, wireName(target)}
}

// DNSMX is an MX record of owner.
func DNSMX(owner string, pref uint16, host string) DNSRecord {
	return DNSRecord{strings.ToLower(owner) + ".", DNSTypeMX, append(u16(pref), wireName(host)...)}
}

// DNSSRV is an SRV record of owner.
func DNSSRV(owner string, priority, weight, port uint16, target string) DNSRecord {
	data := append(append(append(u16(priority), u16(weight)...), u16(port)...), wireName(target)...)
	return DNSRecord{strings.ToLower(owner) + ".", DNSTypeSRV, data}
}

// DNSTXT is a TXT record of owner, of the character strings parts.
func DNSTXT(owner string, parts ...string) DNSRecord {
	var data []byte
	for _, part := range parts {
		data = append(append(data, len8(len(part))), part...)
	}
	return DNSRecord{strings.ToLower(owner) + ".", DNSTypeTXT, data}
}

// NewDNSStub starts a server answering records, stopped when t ends.
func NewDNSStub(t testing.TB, records ...DNSRecord) *DNSStub {
	t.Helper()
	s := &DNSStub{records: slices.Clone(records)}
	var lc net.ListenConfig
	tcp, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := lc.ListenPacket(context.Background(), "udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Skipf("no UDP on the port of the TCP listener: %v", err)
	}
	s.Addr = tcp.Addr().String()
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
					_, _ = conn.Write(append(u16(len16(len(answer))), answer...))
				}
			}()
		}
	}()
	return s
}

// SetRcode answers every question from now on with rcode and no record, or
// with 0 as the records say.
func (s *DNSStub) SetRcode(rcode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rcode = rcode
}

// SetSilent answers no question from now on, or answers them again.
func (s *DNSStub) SetSilent(silent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = silent
}

// SetTruncate answers every question over UDP from now on with no record,
// marked truncated, or answers them again.
func (s *DNSStub) SetTruncate(truncate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncate = truncate
}

// SetRecords answers records from now on.
func (s *DNSStub) SetRecords(records ...DNSRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = slices.Clone(records)
}

// answer is the server's answer to query, nil to answer nothing.
func (s *DNSStub) answer(query []byte, network string) []byte {
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
	records, rcode, silent, truncated := s.records, s.rcode, s.silent, s.truncate && network == "udp"
	s.mu.Unlock()
	if silent {
		return nil
	}
	var answers [][]byte
	if rcode == 0 && !truncated {
		// A CNAME of the name answers any question of it, followed by the
		// records of its target, as a recursive server answers.
		owner := name
		for range 8 {
			var next string
			for _, r := range records {
				if r.Owner == owner && r.Type == DNSTypeCNAME && qtype != DNSTypeCNAME {
					answers = append(answers, record(r, name))
					next = string(decodeWireName(r.Data))
				}
			}
			for _, r := range records {
				if r.Owner == owner && r.Type == qtype {
					answers = append(answers, record(r, name))
				}
			}
			if next == "" {
				break
			}
			owner = next
		}
		known := false
		for _, r := range records {
			known = known || r.Owner == name
		}
		if !known {
			rcode = 3 // NXDOMAIN
		}
	}
	header := make([]byte, 12)
	copy(header, query[:2])
	flags := 0x8180 | uint16(rcode) // QR, RD, RA
	if truncated {
		flags |= 0x0200 // TC
	}
	binary.BigEndian.PutUint16(header[2:], flags)
	binary.BigEndian.PutUint16(header[4:], 1)
	binary.BigEndian.PutUint16(header[6:], len16(len(answers)))
	out := append(header, query[12:questionEnd]...)
	for _, a := range answers {
		out = append(out, a...)
	}
	return out
}

// record is r written as an answer: its owner a pointer to the question's
// name when it is that name, written out otherwise; class IN; a TTL of 60.
func record(r DNSRecord, question string) []byte {
	var b []byte
	if r.Owner == question {
		b = []byte{0xc0, 12}
	} else {
		b = wireName(r.Owner)
	}
	b = append(b, u16(r.Type)...)
	b = append(b, u16(1)...)
	b = binary.BigEndian.AppendUint32(b, 60)
	b = append(b, u16(len16(len(r.Data)))...)
	return append(b, r.Data...)
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
	return map[uint16]string{DNSTypeA: "A", DNSTypeNS: "NS", DNSTypeCNAME: "CNAME", DNSTypePTR: "PTR", DNSTypeMX: "MX", DNSTypeTXT: "TXT", DNSTypeAAAA: "AAAA", DNSTypeSRV: "SRV"}[t]
}

// Questions is what the server was asked, each as "name type network", the
// name absolute and in lower case.
func (s *DNSStub) Questions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}
