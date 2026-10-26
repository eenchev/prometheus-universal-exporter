//go:build !select_request_types || request_type_dns

package fetch

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A CNAME question is sent by the exporter itself. Go's resolver has no
// lookup of CNAME records: LookupCNAME asks for A and AAAA records beside the
// CNAME and gives the canonical name of whichever answers, the name itself
// for a name that is no alias, and "not found" for a name that has neither
// addresses nor a CNAME, so it cannot tell a name without a CNAME record
// from one that does not exist, nor from one that is its own alias. The
// question below is the one RFC 1035 has a client send: one question of type
// CNAME, class IN, recursion desired, over UDP with TCP for an answer marked
// truncated, or TCP alone with transport tcp. Only what the answer needs is
// read: its header, its question, and its answer section's CNAME records;
// golang.org/x/net/dns/dnsmessage, which would read all of it, is not among
// the modules.
//
// It goes to request.server, or, without one, to the nameservers of
// /etc/resolv.conf in their order, the next one asked when one fails, as
// Go's resolver asks them; the file's options (timeout, attempts, rotate)
// and /etc/hosts are not read for it.

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

// lookupCNAME asks for the CNAME record of name, absolute and in lower
// case, and answers the name it points to. An answer without a CNAME record
// of the name — the name does not exist, or exists and is no alias — is not
// found, as a name without records of the type is for every other type.
func lookupCNAME(ctx context.Context, c *model.Collector, name string) ([]dnsAnswer, error) {
	r := dnsRequest(c)
	servers := []string{}
	if r.Server != "" {
		server, _ := ParseDNSServer(r.Server)
		servers = append(servers, server)
	} else {
		conf, err := os.ReadFile(dnsResolvConf)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading %s: %w", dnsResolvConf, err)
		}
		servers = dnsSystemServers(string(conf))
	}
	var lastErr error
	for i, server := range servers {
		askCtx := ctx
		cancel := context.CancelFunc(func() {})
		if i < len(servers)-1 {
			askCtx, cancel = context.WithTimeout(ctx, dnsServerTimeout)
		}
		answers, err := askCNAME(askCtx, server, r.Transport == dnsTCP, name)
		cancel()
		var dnsErr *net.DNSError
		if err == nil || errors.As(err, &dnsErr) && dnsErr.IsNotFound || ctx.Err() != nil {
			return answers, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// askCNAME sends the CNAME question of name to server and reads the answer.
func askCNAME(ctx context.Context, server string, tcp bool, name string) ([]dnsAnswer, error) {
	// The id is random, as a resolver's is, so that an answer forged
	// without seeing the question is unlikely to match it.
	query := make([]byte, 2, 512)
	_, _ = rand.Read(query)
	id := binary.BigEndian.Uint16(query)
	query = append(query, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0) // RD; one question
	query = append(query, wireDNSName(name)...)
	query = append(query, 0, 5, 0, 1) // CNAME, IN
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
			return nil, ctx.Err()
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, &net.DNSError{Err: "i/o timeout", Name: name, Server: server, IsTimeout: true}
		}
		return nil, &net.DNSError{Err: RedactURLErrors(err).Error(), Name: name, Server: server}
	}
	return readCNAMEAnswer(answer, id, name, server)
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

// dnsRCodes names the response codes a failed CNAME lookup reports.
var dnsRCodes = map[byte]string{1: "FORMERR", 2: "SERVFAIL", 4: "NOTIMP", 5: "REFUSED"}

// readCNAMEAnswer reads the answer to the CNAME question of name with id:
// NXDOMAIN and an answer without a CNAME record of the name are not found;
// another response code than NOERROR, or an answer that is not to the
// question, fails.
func readCNAMEAnswer(msg []byte, id uint16, name, server string) ([]dnsAnswer, error) {
	malformed := &net.DNSError{Err: "cannot unmarshal DNS message", Name: name, Server: server}
	if len(msg) < 12 || binary.BigEndian.Uint16(msg) != id || msg[2]&0x80 == 0 {
		return nil, malformed
	}
	switch rcode := msg[3] & 0x0f; rcode {
	case 0:
	case 3:
		return nil, &net.DNSError{Err: "no such host", Name: name, Server: server, IsNotFound: true}
	default:
		shown := dnsRCodes[rcode]
		if shown == "" {
			shown = fmt.Sprintf("response code %d", rcode)
		}
		return nil, &net.DNSError{Err: "server misbehaving (" + shown + ")", Name: name, Server: server}
	}
	questions, answers := binary.BigEndian.Uint16(msg[4:]), int(binary.BigEndian.Uint16(msg[6:]))
	if questions != 1 {
		return nil, malformed
	}
	asked, off, err := readDNSName(msg, 12)
	if err != nil || off+4 > len(msg) || asked != name+"." || binary.BigEndian.Uint16(msg[off:]) != 5 {
		return nil, malformed
	}
	off += 4
	target := ""
	for range answers {
		owner, next, err := readDNSName(msg, off)
		if err != nil || next+10 > len(msg) {
			return nil, malformed
		}
		rtype, length := binary.BigEndian.Uint16(msg[next:]), int(binary.BigEndian.Uint16(msg[next+8:]))
		data := next + 10
		if data+length > len(msg) {
			return nil, malformed
		}
		if rtype == 5 {
			alias, end, err := readDNSName(msg, data)
			if err != nil || end != data+length {
				return nil, malformed
			}
			if owner == name+"." {
				target = alias
			}
		}
		off = data + length
	}
	if target == "" {
		return nil, &net.DNSError{Err: "no such host", Name: name, Server: server, IsNotFound: true}
	}
	return []dnsAnswer{{Target: dnsName(target)}}, nil
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
