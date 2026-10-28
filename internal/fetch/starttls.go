//go:build !select_request_types || request_type_tcp || request_type_tlscert

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// STARTTLS: a server that starts in plaintext and turns to TLS when the
// client asks — SMTP on 25 and 587, IMAP on 143, POP3 on 110, FTP on 21 —
// is read by greeting it, asking for TLS in its protocol's words and making
// the handshake once it has said yes. What the client says before TLS is
// only the protocol's own request for it: EHLO and STARTTLS for SMTP (RFC
// 3207), a tagged STARTTLS for IMAP (RFC 3501, section 6.2.1), STLS for
// POP3 (RFC 2595, section 4) and AUTH TLS for FTP (RFC 4217, section 4).
// A server that does not offer TLS, refuses it or answers otherwise fails
// the exchange: it never goes on in plaintext, so stripping STARTTLS from
// the server's answers makes a failure, not a plaintext scrape.
//
// The dialogue is written apart from the request types that speak it, over
// a connection and a reader of it: tcp, which goes on over TLS with send,
// and tlscert, which reads the certificate the handshake presents. The file
// is built with either, and holds what both need of a plain connection:
// reading the greeting and a reply, and the failure of a connection the
// scrape's deadline ended (tcpFailure).

// The protocols whose request for TLS the exchange speaks.
const (
	startTLSSMTP = "smtp"
	startTLSIMAP = "imap"
	startTLSPOP3 = "pop3"
	startTLSFTP  = "ftp"
)

// startTLSProtocols are the protocols request.starttls names, in the order
// an error lists them (model.StartTLSProtocols, which the configuration
// schema names).
var startTLSProtocols = model.StartTLSProtocols

// validateStartTLS checks request.starttls of a collector of requestType,
// when it is set: one of the protocols the exchange speaks.
func validateStartTLS(name, requestType, protocol string) error {
	if protocol != "" && !slices.Contains(startTLSProtocols, protocol) {
		return fmt.Errorf("collector %q request.starttls %q is not a protocol whose STARTTLS %s %s collector speaks; want smtp, imap, pop3 or ftp", name, protocol, TypeArticle(requestType), requestType)
	}
	return nil
}

// startTLSIMAPTag is the tag of the IMAP STARTTLS command: the only command
// sent before TLS, so any tag is unique.
const startTLSIMAPTag = "a1"

// startTLSStep is one command the client sent to begin TLS and what the
// server answered it.
type startTLSStep struct {
	command string
	reply   []byte
}

// errReplyTooLarge is what readReply says of a reply that has reached the
// limit without ending.
var errReplyTooLarge = errors.New("the reply reached the limit without ending")

// readReply reads one reply of a text protocol: its first line, through the
// line feed that ends it, or, when that line begins with three digits and a
// '-', as SMTP's and FTP's multi-line replies' do ("220-mail.example.com
// ESMTP"), every line up to and including the first that begins with the
// same three digits and no '-' ("220 ready", or "220" alone). It is held to
// limit: a reply that reaches it without having ended can only grow past
// it, and is errReplyTooLarge at once rather than at the deadline. On an
// error it returns what it read before it.
func readReply(reader *bufio.Reader, limit int64) ([]byte, error) {
	var reply []byte
	lineStart := 0
	code := ""
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return reply, err
		}
		reply = append(reply, b)
		if b == '\n' {
			line := reply[lineStart:]
			lineStart = len(reply)
			if code == "" && !replyContinues(line) {
				return reply, nil
			}
			if code == "" {
				code = string(line[:3])
			} else if bytes.HasPrefix(line, []byte(code)) && !replyContinues(line) {
				return reply, nil
			}
		}
		if int64(len(reply)) >= limit {
			return reply, errReplyTooLarge
		}
	}
}

// replyContinues says whether a line of a reply is one that more lines
// follow: three digits and a '-'.
func replyContinues(line []byte) bool {
	if len(line) < 4 || line[3] != '-' {
		return false
	}
	for _, d := range line[:3] {
		if d < '0' || d > '9' {
			return false
		}
	}
	return true
}

// replyCode is the three-digit code a reply begins with, followed by a
// blank, a '-' or its line end, as SMTP's and FTP's are; "" when it begins
// with none.
func replyCode(reply []byte) string {
	if len(reply) < 4 {
		return ""
	}
	for _, d := range reply[:3] {
		if d < '0' || d > '9' {
			return ""
		}
	}
	if c := reply[3]; c != ' ' && c != '-' && c != '\r' && c != '\n' {
		return ""
	}
	return string(reply[:3])
}

// startTLSGreeted checks that a greeting is what a server of protocol greets
// with when it will serve the client: 220 for SMTP and FTP, "* OK" for
// IMAP, a "* PREAUTH" server having no STARTTLS to offer, and "+OK" for
// POP3. Anything else is a server that will not go on, as an SMTP server's
// 554 is, or not one of protocol.
func startTLSGreeted(protocol, hostPort string, greeting []byte) error {
	var want string
	switch protocol {
	case startTLSSMTP, startTLSFTP:
		want = "220"
		if replyCode(greeting) == want {
			return nil
		}
	case startTLSIMAP:
		want = "* OK"
		if hasPrefixFold(greeting, "* OK ") || hasPrefixFold(greeting, "* OK\r") || hasPrefixFold(greeting, "* OK\n") {
			return nil
		}
	case startTLSPOP3:
		want = "+OK"
		if bytes.HasPrefix(greeting, []byte(want)) {
			return nil
		}
	}
	return model.Errorf("the greeting of %s, %s, does not begin with %s, which request.starttls %s needs before it asks for TLS", hostPort, model.Quoted(greeting), want, protocol)
}

// hasPrefixFold says whether b begins with prefix, ignoring case, as IMAP
// reads its words.
func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// startTLS asks a server of protocol, which has greeted, to begin TLS, and
// returns the commands sent and the server's answers once it has said
// yes: the caller makes the handshake next, on the connection reader reads.
// conn is what the commands are written to; local is the connection's own
// address, which names the client to an SMTP server. Each answer is held
// to limit, which an error says to raise by limitKeys, the keys that set it
// for the type, and the connection's deadline ends a server that does not
// answer, as ctx's cancellation does (tcpFailure). A server that wrote more
// than its yes before the handshake fails it: those bytes came in
// plaintext, where anyone on the path could have put them, and would be
// read as if they had come over TLS (CVE-2011-0411 is the server's side of
// the same).
func startTLS(ctx context.Context, conn io.Writer, reader *bufio.Reader, protocol, hostPort string, local net.Addr, limit int64, limitKeys string) ([]startTLSStep, error) {
	d := startTLSDialogue{ctx: ctx, conn: conn, reader: reader, protocol: protocol, hostPort: hostPort, limit: limit, limitKeys: limitKeys}
	var err error
	switch protocol {
	case startTLSSMTP:
		err = d.smtp(local)
	case startTLSIMAP:
		err = d.imap()
	case startTLSPOP3:
		err = d.expect("STLS\r\n", readReply, func(reply []byte) bool { return bytes.HasPrefix(reply, []byte("+OK")) }, "+OK")
	case startTLSFTP:
		err = d.expect("AUTH TLS\r\n", readReply, func(reply []byte) bool { return replyCode(reply) == "234" }, "234")
	default:
		err = fmt.Errorf("request.starttls %q is not a protocol the exchange begins TLS in", protocol)
	}
	if err != nil {
		return d.steps, err
	}
	if n := reader.Buffered(); n > 0 {
		return d.steps, fmt.Errorf("the server %s wrote %d bytes after it said to begin TLS and before the handshake; they came in plaintext, where anyone on the way could have written them, so the scrape does not read them as the server's", hostPort, n)
	}
	return d.steps, nil
}

// startTLSDialogue is a request for TLS as it is spoken.
type startTLSDialogue struct {
	ctx      context.Context
	conn     io.Writer
	reader   *bufio.Reader
	protocol string
	hostPort string
	limit    int64
	// limitKeys are the keys an error says to raise limit by.
	limitKeys string
	steps     []startTLSStep
}

// smtp sends EHLO and, when the server's answer lists STARTTLS among its
// extensions, STARTTLS, which the server answers 220 to begin TLS. The
// client names itself by its address, [192.0.2.7] or [IPv6:2001:db8::7],
// as RFC 5321 (section 4.1.4) has a client without a meaningful name do:
// the exporter's host name is a pod's or a machine's that the mail server
// cannot look up, and some servers refuse a name that does not resolve.
func (d *startTLSDialogue) smtp(local net.Addr) error {
	ehlo := "EHLO " + smtpClientName(local) + "\r\n"
	reply, err := d.ask(ehlo, readReply)
	if err != nil {
		return err
	}
	if replyCode(reply) != "250" {
		return model.Errorf("the server %s answered EHLO with %s, not 250; request.starttls smtp asks for TLS only of a server that takes EHLO", d.hostPort, model.Quoted(reply))
	}
	if !smtpOffersStartTLS(reply) {
		return model.Errorf("the server %s does not offer STARTTLS in its answer to EHLO, %s; the scrape does not go on in plaintext", d.hostPort, model.Quoted(reply))
	}
	return d.expect("STARTTLS\r\n", readReply, func(reply []byte) bool { return replyCode(reply) == "220" }, "220")
}

// imap sends a tagged STARTTLS, which the server answers with the tag and
// OK to begin TLS, after any untagged lines of its own. The greeting's
// CAPABILITY, when it has one, is not checked: a server without STARTTLS
// answers NO or BAD, which fails the scrape as plainly, and asking for the
// capabilities would be one more command in plaintext.
func (d *startTLSDialogue) imap() error {
	tag := startTLSIMAPTag + " "
	return d.expect(tag+"STARTTLS\r\n", readTagged(tag), func(reply []byte) bool { return imapTagged(reply, tag, "OK") }, tag+"OK")
}

// readTagged reads an IMAP server's lines until the one that begins with
// tag, held to limit together. The tag is matched in any case: tags are
// case-sensitive (RFC 3501, section 2.2.1), but a server that echoes a1 as
// A1 has still answered the command, and reading its answer as an untagged
// line would wait for another until the deadline.
func readTagged(tag string) func(*bufio.Reader, int64) ([]byte, error) {
	return func(reader *bufio.Reader, limit int64) ([]byte, error) {
		var reply []byte
		for {
			line, err := readReply(reader, limit-int64(len(reply)))
			reply = append(reply, line...)
			if err != nil || hasPrefixFold(line, tag) {
				return reply, err
			}
		}
	}
}

// imapTagged says whether an IMAP reply's last line, the tagged one, is tag,
// in any case as readTagged reads it, and status: "a1 OK Begin TLS
// negotiation now".
func imapTagged(reply []byte, tag, status string) bool {
	last := reply[:len(reply)-1]
	if i := bytes.LastIndexByte(last, '\n'); i >= 0 {
		last = last[i+1:]
	}
	if !hasPrefixFold(last, tag) {
		return false
	}
	rest := last[len(tag):]
	return hasPrefixFold(rest, status) && (len(rest) == len(status) || rest[len(status)] == ' ' || rest[len(status)] == '\r')
}

// expect sends command and reads the server's answer with read, which must
// be one that ok takes, said in want, for TLS to begin: anything else is
// the server's refusal, or not an answer of protocol.
func (d *startTLSDialogue) expect(command string, read func(*bufio.Reader, int64) ([]byte, error), ok func([]byte) bool, want string) error {
	reply, err := d.ask(command, read)
	if err != nil {
		return err
	}
	if !ok(reply) {
		return model.Errorf("the server %s did not begin TLS: it answered %s with %s, not %s", d.hostPort, startTLSVerb(command), model.Quoted(reply), want)
	}
	return nil
}

// ask writes command and reads the server's answer with read, noting both
// as a step.
func (d *startTLSDialogue) ask(command string, read func(*bufio.Reader, int64) ([]byte, error)) ([]byte, error) {
	verb := startTLSVerb(command)
	if _, err := io.WriteString(d.conn, command); err != nil {
		return nil, tcpFailure(d.ctx, fmt.Errorf("sending %s to %s, as request.starttls %s asks: %w", verb, d.hostPort, d.protocol, err))
	}
	reply, err := read(d.reader, d.limit)
	d.steps = append(d.steps, startTLSStep{command: command, reply: reply})
	switch {
	case err == nil:
		return reply, nil
	case errors.Is(err, errReplyTooLarge):
		return nil, model.MarkError(model.Errorf("the answer of %s to %s is larger than the response limit of %d bytes; raise %s", d.hostPort, verb, model.Size(d.limit), d.limitKeys), model.ErrLimitExceeded)
	case errors.Is(err, io.EOF) && len(reply) == 0:
		return nil, fmt.Errorf("the server %s closed the connection without answering %s, which request.starttls %s sends", d.hostPort, verb, d.protocol)
	case errors.Is(err, io.EOF):
		return nil, model.Errorf("the server %s closed the connection before its answer to %s ended, after %d bytes: %s", d.hostPort, verb, len(reply), model.Quoted(reply))
	}
	return nil, tcpFailure(d.ctx, fmt.Errorf("waiting for the answer of %s to %s, which request.starttls %s sends, %d bytes read: %w", d.hostPort, verb, d.protocol, len(reply), err))
}

// startTLSVerb is a command as an error names it: without its line end, and
// without the IMAP tag.
func startTLSVerb(command string) string {
	verb := strings.TrimSuffix(command, "\r\n")
	if rest, found := strings.CutPrefix(verb, startTLSIMAPTag+" "); found {
		return rest
	}
	if strings.HasPrefix(verb, "EHLO ") {
		return "EHLO"
	}
	return verb
}

// smtpClientName is the name an SMTP client gives in EHLO: its address as
// an address literal (RFC 5321, section 4.1.3).
func smtpClientName(local net.Addr) string {
	if local != nil {
		if p, err := netip.ParseAddrPort(local.String()); err == nil {
			if a := p.Addr().Unmap(); a.Is4() {
				return "[" + a.String() + "]"
			}
			return "[IPv6:" + p.Addr().WithZone("").String() + "]"
		}
	}
	return "[127.0.0.1]"
}

// smtpOffersStartTLS says whether an answer to EHLO lists STARTTLS among the
// extensions on the lines after its first, which names the server.
func smtpOffersStartTLS(reply []byte) bool {
	lines := strings.Split(strings.TrimRight(string(reply), "\r\n"), "\n")
	for _, line := range lines[1:] {
		line = strings.TrimSuffix(line, "\r")
		if len(line) < 4 {
			continue
		}
		keyword, _, _ := strings.Cut(line[4:], " ")
		if strings.EqualFold(keyword, "STARTTLS") {
			return true
		}
	}
	return false
}

// startTLSShown is how a debug report tells the steps of a request for TLS:
// each command sent and what the server answered, quoted, so that a line
// end or a control character is seen and does not act on a terminal.
func startTLSShown(steps []startTLSStep) string {
	var b strings.Builder
	for i, step := range steps {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "sent %s, answered %s", tcpShown(step.command), tcpShown(string(step.reply)))
	}
	return b.String()
}

// readStartTLSGreeting reads the greeting of a server of protocol
// (readGreeting). An FTP server may say first when it will be ready, 120
// "Service ready in nnn minutes", a reply of one line or several, before
// its 220 (RFC 959, sections 4.2 and 5.4): those replies are read and
// returned apart, in before, and the greeting is the reply after them,
// which startTLSGreeted and a tcp collector's request.greeting check. All
// of it is held to limit together, and to the connection's deadline.
func readStartTLSGreeting(ctx context.Context, reader *bufio.Reader, protocol, hostPort string, limit int64, limitKeys, key string) (before, greeting []byte, err error) {
	for {
		greeting, err = readGreeting(ctx, reader, hostPort, limit, int64(len(before)), limitKeys, key)
		if err != nil || protocol != startTLSFTP || replyCode(greeting) != "120" {
			return before, greeting, err
		}
		before = append(before, greeting...)
	}
}

// readGreeting reads what a server says first, before anything is sent,
// as readReply reads a reply: its first line, or, after a line beginning
// with three digits and a '-', every line up to the one beginning with the
// same digits and no '-'. The greeting is held to limit, less the used
// bytes the server said before it, which an error says to raise by
// limitKeys, and ends at the connection's deadline: a server that writes
// no line end, or never ends its continuation, fails the scrape. Its
// failures name key, the key that expects a greeting.
func readGreeting(ctx context.Context, reader *bufio.Reader, hostPort string, limit, used int64, limitKeys, key string) ([]byte, error) {
	greeting, err := readReply(reader, limit-used)
	switch {
	case err == nil:
		return greeting, nil
	case errors.Is(err, errReplyTooLarge):
		return nil, model.MarkError(model.Errorf("the greeting of %s is larger than the response limit of %d bytes; raise %s", hostPort, model.Size(limit), limitKeys), model.ErrLimitExceeded)
	case errors.Is(err, io.EOF) && len(greeting) == 0:
		return nil, fmt.Errorf("the server %s closed the connection without a greeting; %s expects one", hostPort, key)
	case errors.Is(err, io.EOF):
		return nil, model.Errorf("the server %s closed the connection before its greeting ended, after %d bytes: %s", hostPort, len(greeting), model.Quoted(greeting))
	}
	return nil, tcpFailure(ctx, fmt.Errorf("waiting for the greeting of %s, as %s expects, %d bytes read: %w", hostPort, key, len(greeting), err))
}

// tcpShown is what a debug report shows of the bytes sent or read: quoted,
// so a carriage return and a line feed are seen, and no longer than a
// failure.
func tcpShown(send string) string {
	return model.CutTo(strconv.Quote(send), model.MaxFailureBytes, false)
}

// tcpFailure is the failure of a connection, a handshake, the write or a
// read: the scrape's deadline or cancellation when that is what ended it,
// since the connection's deadline is set to make it end. The connection's
// deadline is the scrape's, and its timer can fire before the context's
// own, so a connection that timed out is the deadline's failure whether or
// not the context says so yet.
func tcpFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%w)", context.DeadlineExceeded, err)
	}
	return err
}
