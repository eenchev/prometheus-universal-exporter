//go:build !select_request_types || request_type_dns

package fetch

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every question of a dns collector is sent by the exporter itself, with
// the client below: one question, class IN, recursion desired and the AD
// bit set (RFC 6840 § 5.7), with an EDNS(0) OPT record offering 1232
// bytes over UDP, over UDP with TCP for an answer marked truncated, or TCP
// alone with transport tcp. A server that answers FORMERR to the OPT
// record is asked again without it, as resolvers do. The answer is read
// whole: its header, response code and flags, its question, which must be
// the one asked, and its answer section's records, each with its TTL;
// golang.org/x/net/dns/dnsmessage, which would read it too, is not among
// the type's imports, which are the standard library's alone.
//
// It goes to request.server, or, without one, to the nameservers of
// /etc/resolv.conf in their order, the next one asked when one fails or
// answers SERVFAIL, REFUSED, NOTIMP or FORMERR; the file's options
// (timeout, attempts, rotate, ndots), its search domains and /etc/hosts
// are not read.

// dnsResolvConf is the file the system's nameservers are read from.
const dnsResolvConf = "/etc/resolv.conf"

// dnsServerTimeout is how long a nameserver of the system's is waited for
// when another follows it, resolv.conf's default timeout: one that never
// answers leaves the next the rest of the deadline.
const dnsServerTimeout = 5 * time.Second

// dnsUDPResend is how long an answer over UDP is waited for before the
// question is sent again, a datagram being lost without a word.
const dnsUDPResend = 2 * time.Second

// dnsSystemServers reads the nameserver lines of a resolv.conf, each as
// host:port on port 53; with none, the local server, as Go's resolver
// assumes.
func dnsSystemServers(conf string) []string {
	var servers []string
	for line := range strings.SplitSeq(conf, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, net.JoinHostPort(fields[1], dnsDefaultPort))
		}
	}
	if len(servers) == 0 {
		servers = []string{"127.0.0.1:53", "[::1]:53"}
	}
	return servers
}

// dnsReply is a server's answer to a question, as the document holds it:
// its response code by name, its flags, and the records of the type asked
// for, of the name or the names its CNAME records lead to.
type dnsReply struct {
	rcode                                        string
	authoritative, recursionAvailable, authentic bool
	answers                                      []dnsAnswer
}

// dnsRetried are the response codes after which the next of the system's
// nameservers is asked, as Go's resolver and the C library's ask it.
var dnsRetried = []string{"SERVFAIL", "REFUSED", "NOTIMP", "FORMERR"}

// lookupDNS asks for the records of recordType of name, absolute and in
// lower case, or for PTR of the reverse name of the address name is, and
// answers the reply of the first server that gives one, or of the last.
func lookupDNS(ctx context.Context, c *model.Collector, recordType, name string) (dnsReply, string, error) {
	r := dnsRequest(c)
	servers := []string{}
	if r.Server != "" {
		server, _ := ParseDNSServer(r.Server)
		servers = append(servers, server)
	} else {
		conf, err := os.ReadFile(dnsResolvConf)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return dnsReply{}, "", fmt.Errorf("reading %s: %w", dnsResolvConf, err)
		}
		servers = dnsSystemServers(string(conf))
	}
	asked := name
	if recordType == "PTR" {
		asked = dnsReverseName(netip.MustParseAddr(name))
	}
	var last dnsReply
	var lastServer string
	var lastErr error
	for i, server := range servers {
		askCtx := ctx
		cancel := context.CancelFunc(func() {})
		if i < len(servers)-1 {
			askCtx, cancel = context.WithTimeout(ctx, dnsServerTimeout)
		}
		reply, err := askDNS(askCtx, server, r.Transport == dnsTCP, asked, dnsTypeNumbers[recordType])
		cancel()
		if ctx.Err() != nil {
			return dnsReply{}, server, ctx.Err()
		}
		if err == nil && !slices.Contains(dnsRetried, reply.rcode) {
			return reply, server, nil
		}
		last, lastServer, lastErr = reply, server, err
	}
	return last, lastServer, lastErr
}

// dnsReverseName is the name PTR records of addr are asked for under:
// its octets in reverse under in-addr.arpa, or its nibbles under ip6.arpa.
func dnsReverseName(addr netip.Addr) string {
	var b strings.Builder
	raw := addr.Unmap().AsSlice()
	if len(raw) == 4 {
		for i := 3; i >= 0; i-- {
			fmt.Fprintf(&b, "%d.", raw[i])
		}
		return b.String() + "in-addr.arpa"
	}
	const hex = "0123456789abcdef"
	for i := len(raw) - 1; i >= 0; i-- {
		b.WriteByte(hex[raw[i]&0x0f])
		b.WriteByte('.')
		b.WriteByte(hex[raw[i]>>4])
		b.WriteByte('.')
	}
	return b.String() + "ip6.arpa"
}

// dnsQuery is the question of name of type qtype with id: recursion
// desired, AD set, and an OPT record when edns.
func dnsQuery(id uint16, name string, qtype uint16, edns bool) []byte {
	query := binary.BigEndian.AppendUint16(make([]byte, 0, 512), id)
	additional := byte(0)
	if edns {
		additional = 1
	}
	query = append(query, 0x01, 0x20, 0, 1, 0, 0, 0, 0, 0, additional) // RD, AD; one question
	query = append(query, wireDNSName(name)...)
	query = binary.BigEndian.AppendUint16(query, qtype)
	query = append(query, 0, 1) // IN
	if edns {
		// The root name, OPT, 1232 bytes over UDP, no extended code,
		// version 0, no flags, no options.
		query = append(query, 0, 0, 41, 0x04, 0xd0, 0, 0, 0, 0, 0, 0)
	}
	return query
}

// askDNS sends the question of name of type qtype to server and reads the
// answer.
func askDNS(ctx context.Context, server string, tcp bool, name string, qtype uint16) (dnsReply, error) {
	reply, err := exchangeDNS(ctx, server, tcp, name, qtype, true)
	if err == nil && reply.rcode == "FORMERR" {
		// A server that knows no EDNS(0) refuses the OPT record.
		return exchangeDNS(ctx, server, tcp, name, qtype, false)
	}
	return reply, err
}

// exchangeDNS sends one question and reads its answer, over UDP and then
// TCP for a truncated one, or over TCP alone.
func exchangeDNS(ctx context.Context, server string, tcp bool, name string, qtype uint16, edns bool) (dnsReply, error) {
	// The id is random, as a resolver's is, so that an answer forged
	// without seeing the question is unlikely to match it.
	var random [2]byte
	_, _ = rand.Read(random[:])
	id := binary.BigEndian.Uint16(random[:])
	query := dnsQuery(id, name, qtype, edns)
	var answer []byte
	var err error
	if !tcp {
		answer, err = exchangeDNSUDP(ctx, server, query)
		// An answer too long for UDP comes truncated (TC), and is asked
		// again over TCP, as every resolver does.
		if err == nil && len(answer) > 2 && answer[2]&0x02 != 0 {
			tcp = true
		}
	}
	if tcp {
		answer, err = exchangeDNSTCP(ctx, server, query)
	}
	if err != nil {
		if ctx.Err() != nil {
			return dnsReply{}, ctx.Err()
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return dnsReply{}, &net.DNSError{Err: "i/o timeout", Name: name, Server: server, IsTimeout: true}
		}
		return dnsReply{}, &net.DNSError{Err: RedactURLErrors(err).Error(), Name: name, Server: server}
	}
	return readDNSAnswer(answer, id, name, qtype, server)
}

// wireDNSName is an absolute name as the wire writes it, in labels; the
// name has been held to the DNS's lengths (parseDNSTarget).
func wireDNSName(name string) []byte {
	var b []byte
	for label := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(label))) //nolint:gosec // G115: a label has at most 63 characters (parseDNSTarget)
		b = append(b, label...)
	}
	return append(b, 0)
}

// exchangeDNSUDP sends query over UDP and waits for its answer, sending it
// again every dnsUDPResend; a datagram that is not the answer — another
// id, or not an answer — is skipped.
func exchangeDNSUDP(ctx context.Context, server string, query []byte) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 65535)
	for {
		if _, err := conn.Write(query); err != nil {
			return nil, err
		}
		resend := time.Now().Add(dnsUDPResend)
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(resend) {
			resend = deadline
		}
		if err := conn.SetReadDeadline(resend); err != nil {
			return nil, err
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() && ctx.Err() == nil {
					break
				}
				return nil, err
			}
			if n >= 12 && buf[0] == query[0] && buf[1] == query[1] && buf[2]&0x80 != 0 {
				return append([]byte(nil), buf[:n]...), nil
			}
		}
	}
}

// exchangeDNSTCP sends query over TCP, after its length, and reads the
// answer the same way.
func exchangeDNSTCP(ctx context.Context, server string, query []byte) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	if _, err := conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(query))), query...)); err != nil { //nolint:gosec // G115: one question of a name of at most 253 characters
		return nil, err
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, err
	}
	answer := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// dnsRCodes names the response codes of RFC 1035 and RFC 2136; another is
// named by its number.
var dnsRCodes = map[byte]string{0: "NOERROR", 1: "FORMERR", 2: "SERVFAIL", 3: "NXDOMAIN", 4: "NOTIMP", 5: "REFUSED", 6: "YXDOMAIN", 7: "YXRRSET", 8: "NXRRSET", 9: "NOTAUTH", 10: "NOTZONE"}

// dnsTypeNumbers are the record types' numbers on the wire.
var dnsTypeNumbers = map[string]uint16{"A": 1, "NS": 2, "CNAME": 5, "SOA": 6, "PTR": 12, "MX": 15, "TXT": 16, "AAAA": 28, "SRV": 33, "DNSKEY": 48, "CAA": 257}

// dnsCNAMEChain is the most CNAME records followed from the name asked.
const dnsCNAMEChain = 16

// dnsRecord is a record of an answer section: its owner, type, TTL and
// where its data is in the message.
type dnsRecord struct {
	owner      string
	rtype      uint16
	ttl        uint32
	start, end int
}

// readDNSAnswer reads the answer to the question of name of type qtype
// with id: its response code and flags, whatever the code, and the records
// of qtype whose owner is the name, or, but for a CNAME question, a name
// its CNAME records lead to. An answer that is not to the question, or
// whose records do not parse, fails.
func readDNSAnswer(msg []byte, id uint16, name string, qtype uint16, server string) (dnsReply, error) {
	malformed := &net.DNSError{Err: "cannot unmarshal DNS message", Name: name, Server: server}
	if len(msg) < 12 || binary.BigEndian.Uint16(msg) != id || msg[2]&0x80 == 0 {
		return dnsReply{}, malformed
	}
	rcode := msg[3] & 0x0f
	reply := dnsReply{
		rcode:              cmp.Or(dnsRCodes[rcode], fmt.Sprintf("RCODE%d", rcode)),
		authoritative:      msg[2]&0x04 != 0,
		recursionAvailable: msg[3]&0x80 != 0,
		authentic:          msg[3]&0x20 != 0,
		answers:            []dnsAnswer{},
	}
	questions, count := binary.BigEndian.Uint16(msg[4:]), int(binary.BigEndian.Uint16(msg[6:]))
	if questions != 1 {
		// A FORMERR may come without the question.
		if questions == 0 && rcode != 0 {
			return reply, nil
		}
		return dnsReply{}, malformed
	}
	asked, off, err := readDNSName(msg, 12)
	if err != nil || off+4 > len(msg) || asked != name+"." || binary.BigEndian.Uint16(msg[off:]) != qtype {
		return dnsReply{}, malformed
	}
	off += 4
	var records []dnsRecord
	for range count {
		owner, next, err := readDNSName(msg, off)
		if err != nil || next+10 > len(msg) {
			return dnsReply{}, malformed
		}
		length := int(binary.BigEndian.Uint16(msg[next+8:]))
		data := next + 10
		if data+length > len(msg) {
			return dnsReply{}, malformed
		}
		records = append(records, dnsRecord{owner: owner, rtype: binary.BigEndian.Uint16(msg[next:]), ttl: binary.BigEndian.Uint32(msg[next+4:]), start: data, end: data + length})
		off = data + length
	}
	// The names the answer is of: the name, and those its CNAME records
	// lead to, in whatever order the server wrote them.
	owners := map[string]bool{name + ".": true}
	if qtype != dnsTypeNumbers["CNAME"] {
		for range dnsCNAMEChain {
			grew := false
			for _, r := range records {
				if r.rtype == dnsTypeNumbers["CNAME"] && owners[r.owner] {
					target, end, err := readDNSName(msg, r.start)
					if err != nil || end != r.end {
						return dnsReply{}, malformed
					}
					if !owners[target] {
						owners[target], grew = true, true
					}
				}
			}
			if !grew {
				break
			}
		}
	}
	for _, r := range records {
		if r.rtype != qtype || !owners[r.owner] || qtype == dnsTypeNumbers["CNAME"] && r.owner != name+"." {
			continue
		}
		answer, err := readDNSRecord(msg, r)
		if err != nil {
			return dnsReply{}, &net.DNSError{Err: "cannot unmarshal DNS message: " + err.Error(), Name: name, Server: server}
		}
		reply.answers = append(reply.answers, answer)
	}
	return reply, nil
}

// readDNSRecord reads the data of r, of a type the collector may ask for.
func readDNSRecord(msg []byte, r dnsRecord) (dnsAnswer, error) {
	data := msg[r.start:r.end]
	answer := dnsAnswer{TTL: r.ttl}
	name := func(at int) (string, int, error) {
		n, end, err := readDNSName(msg, r.start+at)
		if err != nil || end > r.end {
			return "", 0, cmp.Or(err, errors.New("a name past its record"))
		}
		return dnsName(n), end - r.start, nil
	}
	whole := func(end int, err error) error {
		if err == nil && end != len(data) {
			return errors.New("a record longer than its data")
		}
		return err
	}
	u16 := func(at int) *uint16 { v := binary.BigEndian.Uint16(data[at:]); return &v }
	u32 := func(at int) *uint32 { v := binary.BigEndian.Uint32(data[at:]); return &v }
	short := errors.New("a record shorter than its type's data")
	var err error
	var end int
	switch r.rtype {
	case 1, 28:
		addr, ok := netip.AddrFromSlice(data)
		if !ok || len(data) != map[uint16]int{1: 4, 28: 16}[r.rtype] {
			return answer, short
		}
		answer.Address = addr.Unmap().String()
	case 2:
		answer.Host, end, err = name(0)
		err = whole(end, err)
	case 5:
		answer.Target, end, err = name(0)
		err = whole(end, err)
	case 12:
		answer.Name, end, err = name(0)
		err = whole(end, err)
	case 15:
		if len(data) < 3 {
			return answer, short
		}
		answer.Preference = u16(0)
		answer.Host, end, err = name(2)
		err = whole(end, err)
	case 16:
		var text strings.Builder
		for i := 0; i < len(data); {
			n := int(data[i])
			if i+1+n > len(data) {
				return answer, short
			}
			text.Write(data[i+1 : i+1+n])
			i += 1 + n
		}
		joined := text.String()
		answer.Text = &joined
	case 33:
		if len(data) < 7 {
			return answer, short
		}
		answer.Priority, answer.Weight, answer.Port = u16(0), u16(2), u16(4)
		answer.Target, end, err = name(6)
		err = whole(end, err)
	case 6:
		var at int
		if answer.Host, at, err = name(0); err != nil {
			return answer, err
		}
		if answer.Mailbox, at, err = name(at); err != nil {
			return answer, err
		}
		if at+20 != len(data) {
			return answer, short
		}
		answer.Serial, answer.Refresh, answer.Retry, answer.Expire, answer.Minimum = u32(at), u32(at+4), u32(at+8), u32(at+12), u32(at+16)
	case 257:
		if len(data) < 2 || 2+int(data[1]) > len(data) {
			return answer, short
		}
		flags := uint16(data[0])
		tag, value := string(data[2:2+int(data[1])]), string(data[2+int(data[1]):])
		answer.Flags, answer.Tag, answer.Value = &flags, tag, &value
	case 48:
		if len(data) < 4 {
			return answer, short
		}
		protocol, algorithm := data[2], data[3]
		tag := dnsKeyTag(data)
		answer.Flags, answer.Protocol, answer.Algorithm, answer.KeyTag = u16(0), &protocol, &algorithm, &tag
	}
	return answer, err
}

// dnsKeyTag is the key tag of a DNSKEY record's data, as RFC 4034
// appendix B computes it, which DS records and RRSIGs name the key by.
func dnsKeyTag(data []byte) uint16 {
	var sum uint32
	for i, b := range data {
		if i&1 == 0 {
			sum += uint32(b) << 8
		} else {
			sum += uint32(b)
		}
	}
	sum += sum >> 16 & 0xffff
	return uint16(sum & 0xffff) //nolint:gosec // G115: masked to 16 bits
}

// readDNSName reads the name at off of msg, following compression pointers
// (RFC 1035 § 4.1.4), in lower case and absolute, and where what follows it
// begins.
func readDNSName(msg []byte, off int) (string, int, error) {
	var name strings.Builder
	next := -1
	for jumps := 0; ; {
		if off >= len(msg) {
			return "", 0, errors.New("name out of the message")
		}
		n := int(msg[off])
		switch {
		case n == 0:
			if next < 0 {
				next = off + 1
			}
			if name.Len() == 0 {
				return ".", next, nil
			}
			return strings.ToLower(name.String()), next, nil
		case n&0xc0 == 0xc0:
			if off+1 >= len(msg) || jumps > 64 {
				return "", 0, errors.New("bad compression pointer")
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3fff)
			jumps++
		case n&0xc0 != 0:
			return "", 0, errors.New("unknown label type")
		default:
			if off+1+n > len(msg) || name.Len()+n+1 > 255 {
				return "", 0, errors.New("label out of the message")
			}
			name.Write(msg[off+1 : off+1+n])
			name.WriteByte('.')
			off += 1 + n
		}
	}
}
