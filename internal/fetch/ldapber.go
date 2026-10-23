//go:build request_type_ldap

package fetch

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The ldap request type speaks LDAPv3 (RFC 4511) itself, over the standard
// library alone: the few operations a scrape needs, encoded and decoded in
// BER, the Basic Encoding Rules LDAP's messages are written in, as RFC 4511
// § 5.1 restricts them: definite lengths only, and strings primitive. An
// element is a tag, a length and that many bytes of content:
//
//	30 0c                 SEQUENCE of 12 bytes: an LDAPMessage
//	   02 01 01           INTEGER 1: its messageID
//	   61 07              [APPLICATION 1] of 7 bytes: a BindResponse
//	      0a 01 00        ENUMERATED 0: success
//	      04 00 04 00     the matched DN and the diagnostic message, empty
//
// Everything the server sends is read against one budget, the collector's
// response limit, counted in the bytes of its messages as they come off the
// wire. A message's length is checked against what is left of the budget
// before a byte of its content is read, and nothing is held for it beyond
// what the server has actually sent, so a length of a gigabyte followed by
// nothing costs the bytes that came and a failed scrape, never a gigabyte.
// A message, once read whole, is decoded only as deep as the operations'
// own structures go, each element's length checked against what encloses
// it; a string that is constructed, an indefinite length, a length of more
// than four bytes and a tag of more than one byte are refused, none of them
// being LDAP.

// ldapMaxLengthBytes is the most bytes a long-form length may take: four
// say up to 4 GiB, far past any response limit.
const ldapMaxLengthBytes = 4

// errLDAPProtocol marks what the server sent that is no LDAP message, or
// not the one the scrape waited for.
var errLDAPProtocol = errors.New("not an LDAP message")

// The BER tags of what a scrape sends and reads: universal types, and the
// protocol operations and fields of RFC 4511, in their class and form.
const (
	berBoolean     = 0x01
	berInteger     = 0x02
	berOctetString = 0x04
	berEnumerated  = 0x0a
	berSequence    = 0x30
	berSet         = 0x31

	ldapBindRequest      = 0x60 // [APPLICATION 0] constructed
	ldapBindResponse     = 0x61 // [APPLICATION 1]
	ldapUnbindRequest    = 0x42 // [APPLICATION 2] primitive, NULL
	ldapSearchRequest    = 0x63 // [APPLICATION 3]
	ldapSearchEntry      = 0x64 // [APPLICATION 4]
	ldapSearchDone       = 0x65 // [APPLICATION 5]
	ldapSearchReference  = 0x73 // [APPLICATION 19]
	ldapExtendedRequest  = 0x77 // [APPLICATION 23]
	ldapExtendedResponse = 0x78 // [APPLICATION 24]
	ldapSimpleAuth       = 0x80 // [0] primitive, in a BindRequest
	ldapExtendedName     = 0x80 // [0] primitive, in an ExtendedRequest
	ldapControls         = 0xa0 // [0] constructed, after a message's operation
	ldapReferral         = 0xa3 // [3] constructed, in an LDAPResult
)

// berLength writes the length of an element's content.
func berLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)} //nolint:gosec // a length below 0x80 here
	}
	var digits []byte
	for v := n; v > 0; v >>= 8 {
		digits = append([]byte{byte(v)}, digits...)
	}
	return append([]byte{0x80 | byte(len(digits))}, digits...) //nolint:gosec // an int has at most 8 bytes
}

// berElement writes an element of the tag holding the contents given.
func berElement(tag byte, contents ...[]byte) []byte {
	n := 0
	for _, c := range contents {
		n += len(c)
	}
	out := append([]byte{tag}, berLength(n)...)
	for _, c := range contents {
		out = append(out, c...)
	}
	return out
}

// berInt writes an INTEGER or ENUMERATED, in the fewest bytes of two's
// complement.
func berInt(tag byte, v int64) []byte {
	var digits []byte
	for {
		digits = append([]byte{byte(v)}, digits...) //nolint:gosec // the low byte, as two's complement writes it
		if (v < 0x80 && v >= -0x80) || len(digits) == 8 {
			break
		}
		v >>= 8
	}
	return berElement(tag, digits)
}

// berString writes an OCTET STRING, or another primitive of the tag.
func berString(tag byte, s string) []byte { return berElement(tag, []byte(s)) }

// berBool writes a BOOLEAN.
func berBool(v bool) []byte {
	if v {
		return berElement(berBoolean, []byte{0xff})
	}
	return berElement(berBoolean, []byte{0})
}

// berReader decodes the elements of a content already read, one after
// another.
type berReader struct{ data []byte }

// next reads the next element: its tag and its content, the length checked
// against what is left.
func (r *berReader) next() (byte, []byte, error) {
	if len(r.data) < 2 {
		return 0, nil, fmt.Errorf("%w: an element is cut short", errLDAPProtocol)
	}
	tag := r.data[0]
	if tag&0x1f == 0x1f {
		return 0, nil, fmt.Errorf("%w: a tag of more than one byte, which no LDAP message has", errLDAPProtocol)
	}
	n, header, err := berDecodeLength(r.data[1:])
	if err != nil {
		return 0, nil, err
	}
	rest := r.data[1+header:]
	if n > int64(len(rest)) {
		return 0, nil, fmt.Errorf("%w: an element's length, %d, is past what encloses it, %d bytes", errLDAPProtocol, n, len(rest))
	}
	r.data = rest[n:]
	return tag, rest[:n], nil
}

// expect reads the next element, which must be of the tag; what names it
// in the failure.
func (r *berReader) expect(tag byte, what string) ([]byte, error) {
	got, content, err := r.next()
	if err != nil {
		return nil, err
	}
	if got != tag {
		return nil, fmt.Errorf("%w: %s has the tag 0x%02x, not 0x%02x", errLDAPProtocol, what, got, tag)
	}
	return content, nil
}

// more says whether elements are left.
func (r *berReader) more() bool { return len(r.data) > 0 }

// berDecodeLength reads a length, and how many bytes it took.
func berDecodeLength(b []byte) (int64, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("%w: an element is cut short", errLDAPProtocol)
	}
	first := b[0]
	switch {
	case first < 0x80:
		return int64(first), 1, nil
	case first == 0x80:
		return 0, 0, fmt.Errorf("%w: an indefinite length, which LDAP does not allow", errLDAPProtocol)
	}
	count := int(first & 0x7f)
	if count > ldapMaxLengthBytes {
		return 0, 0, fmt.Errorf("%w: a length of %d bytes, more than the %d any LDAP message needs", errLDAPProtocol, count, ldapMaxLengthBytes)
	}
	if len(b) < 1+count {
		return 0, 0, fmt.Errorf("%w: an element is cut short", errLDAPProtocol)
	}
	var n int64
	for _, digit := range b[1 : 1+count] {
		n = n<<8 | int64(digit)
	}
	return n, 1 + count, nil
}

// berDecodeInt reads the content of an INTEGER or ENUMERATED.
func berDecodeInt(content []byte) (int64, error) {
	if len(content) == 0 || len(content) > 8 {
		return 0, fmt.Errorf("%w: an integer of %d bytes", errLDAPProtocol, len(content))
	}
	v := int64(int8(content[0])) //nolint:gosec // the first byte carries the sign
	for _, digit := range content[1:] {
		v = v<<8 | int64(digit)
	}
	return v, nil
}

// ldapConn is one connection to a directory server, with the budget of what
// its messages may still take and the ID of the next request.
type ldapConn struct {
	conn  net.Conn
	r     *bufio.Reader
	limit int64
	read  int64
	id    int64
}

func newLDAPConn(conn net.Conn, limit int64) *ldapConn {
	return &ldapConn{conn: conn, r: bufio.NewReader(conn), limit: limit}
}

// startTLS puts the connection over tlsConn, after StartTLS succeeded: no
// byte of the server's may be waiting, since what came before the
// handshake would be read as if it came over TLS.
func (s *ldapConn) startTLS(tlsConn net.Conn) error {
	if s.r.Buffered() > 0 {
		return fmt.Errorf("%w: the server sent %d bytes after its StartTLS response, before the TLS handshake", errLDAPProtocol, s.r.Buffered())
	}
	s.conn, s.r = tlsConn, bufio.NewReader(tlsConn)
	return nil
}

// send writes a request: an LDAPMessage of the next ID, the operation and
// the controls, if any. It returns the ID.
func (s *ldapConn) send(op []byte, controls ...[]byte) (int64, error) {
	s.id++
	parts := [][]byte{berInt(berInteger, s.id), op}
	if len(controls) > 0 {
		parts = append(parts, berElement(ldapControls, controls...))
	}
	_, err := s.conn.Write(berElement(berSequence, parts...))
	return s.id, err
}

// charge counts n bytes of a message against the budget.
func (s *ldapConn) charge(n int64) error {
	s.read += n
	if s.read > s.limit {
		return s.overLimit()
	}
	return nil
}

func (s *ldapConn) overLimit() error {
	return model.MarkError(model.Errorf("the server's messages are larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes, or narrow the searches", model.Size(s.limit)), model.ErrLimitExceeded)
}

// ldapMessage is a message of the server's: its ID, the operation's tag and
// content, and its controls' content, if any.
type ldapMessage struct {
	id       int64
	op       byte
	content  []byte
	controls []byte
}

// readMessage reads the next message, held to the budget as it comes.
func (s *ldapConn) readMessage() (*ldapMessage, error) {
	tag, err := s.r.ReadByte()
	if err != nil {
		return nil, err
	}
	if err := s.charge(1); err != nil {
		return nil, err
	}
	if tag != berSequence {
		return nil, fmt.Errorf("%w: a message begins with 0x%02x, which no LDAP message does; is the target an LDAP server?", errLDAPProtocol, tag)
	}
	first, err := s.r.ReadByte()
	if err != nil {
		return nil, err
	}
	header := []byte{first}
	if first > 0x80 && first&0x7f <= ldapMaxLengthBytes {
		more := make([]byte, first&0x7f)
		if _, err := io.ReadFull(s.r, more); err != nil {
			return nil, err
		}
		header = append(header, more...)
	}
	if err := s.charge(int64(len(header))); err != nil {
		return nil, err
	}
	n, _, err := berDecodeLength(header)
	if err != nil {
		return nil, err
	}
	if n > s.limit-s.read {
		return nil, s.overLimit()
	}
	// Read as it comes, so that a length the server does not go on to send
	// costs what it did send.
	var data bytes.Buffer
	data.Grow(int(min(n, 64<<10)))
	if _, err := io.CopyN(&data, s.r, n); err != nil {
		return nil, err
	}
	if err := s.charge(n); err != nil {
		return nil, err
	}
	r := &berReader{data: data.Bytes()}
	idContent, err := r.expect(berInteger, "the message ID")
	if err != nil {
		return nil, err
	}
	id, err := berDecodeInt(idContent)
	if err != nil {
		return nil, err
	}
	op, content, err := r.next()
	if err != nil {
		return nil, err
	}
	m := &ldapMessage{id: id, op: op, content: content}
	if r.more() {
		if m.controls, err = r.expect(ldapControls, "what follows the operation"); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// await reads the server's message to the request of the ID: a message of
// ID 0, the server's notice that it is closing the connection, fails the
// scrape with its reason, and a message of another ID is no answer this
// client waits for.
func (s *ldapConn) await(id int64) (*ldapMessage, error) {
	m, err := s.readMessage()
	if err != nil {
		return nil, err
	}
	if m.id == 0 && m.op == ldapExtendedResponse {
		if result, err := decodeLDAPResult(m.content); err == nil {
			return nil, fmt.Errorf("the server closed the connection: %w", result.err())
		}
		return nil, fmt.Errorf("%w: a notice of disconnection that cannot be read", errLDAPProtocol)
	}
	if m.id != id {
		return nil, fmt.Errorf("%w: the answer of message %d, while the scrape waited for message %d", errLDAPProtocol, m.id, id)
	}
	return m, nil
}

// unbind tells the server the scrape is over. It has no answer.
func (s *ldapConn) unbind() {
	s.id++
	_, _ = s.conn.Write(berElement(berSequence, berInt(berInteger, s.id), []byte{ldapUnbindRequest, 0}))
}

// ldapResult is the LDAPResult that ends an operation: its code, and the
// server's diagnostic message.
type ldapResult struct {
	code       int64
	diagnostic string
	referral   bool
}

// decodeLDAPResult reads the fields of an LDAPResult, which a response
// operation's content begins with.
func decodeLDAPResult(content []byte) (*ldapResult, error) {
	r := &berReader{data: content}
	code, err := r.expect(berEnumerated, "the result code")
	if err != nil {
		return nil, err
	}
	result := &ldapResult{}
	if result.code, err = berDecodeInt(code); err != nil {
		return nil, err
	}
	if _, err := r.expect(berOctetString, "the matched DN"); err != nil {
		return nil, err
	}
	diagnostic, err := r.expect(berOctetString, "the diagnostic message")
	if err != nil {
		return nil, err
	}
	result.diagnostic = ldapShown(string(diagnostic))
	if r.more() {
		tag, _, err := r.next()
		if err != nil {
			return nil, err
		}
		result.referral = tag == ldapReferral
	}
	return result, nil
}

// err is the failure of a result that is not success: the code's name and
// number, and what the server said.
func (r *ldapResult) err() error {
	text := fmt.Sprintf("the server answered %s (%d)", ldapResultName(r.code), r.code)
	if r.diagnostic != "" {
		text += ": " + r.diagnostic
	}
	switch r.code {
	case 10:
		text += "; the exporter follows no referral: make the target the server that holds the base"
	case 4:
		text += "; the server's own size limit for this account is below what the search matches: narrow the filter, or raise the limit on the server"
	}
	return &ldapResultError{code: r.code, text: text}
}

// ldapResultError is an operation the server answered with a code other
// than success.
type ldapResultError struct {
	code int64
	text string
}

func (e *ldapResultError) Error() string { return e.text }

// ldapResultNames are the result codes of RFC 4511 § 4.1.9 and appendix A,
// by number.
var ldapResultNames = map[int64]string{
	0: "success", 1: "operationsError", 2: "protocolError", 3: "timeLimitExceeded", 4: "sizeLimitExceeded",
	5: "compareFalse", 6: "compareTrue", 7: "authMethodNotSupported", 8: "strongerAuthRequired", 10: "referral",
	11: "adminLimitExceeded", 12: "unavailableCriticalExtension", 13: "confidentialityRequired", 14: "saslBindInProgress",
	16: "noSuchAttribute", 17: "undefinedAttributeType", 18: "inappropriateMatching", 19: "constraintViolation",
	20: "attributeOrValueExists", 21: "invalidAttributeSyntax", 32: "noSuchObject", 33: "aliasProblem",
	34: "invalidDNSyntax", 36: "aliasDereferencingProblem", 48: "inappropriateAuthentication", 49: "invalidCredentials",
	50: "insufficientAccessRights", 51: "busy", 52: "unavailable", 53: "unwillingToPerform", 54: "loopDetect",
	64: "namingViolation", 65: "objectClassViolation", 66: "notAllowedOnNonLeaf", 67: "notAllowedOnRDN",
	68: "entryAlreadyExists", 69: "objectClassModsProhibited", 71: "affectsMultipleDSAs", 80: "other",
}

// ldapResultName names a result code, or says it has no name.
func ldapResultName(code int64) string {
	if name, ok := ldapResultNames[code]; ok {
		return name
	}
	return "the result code " + strconv.FormatInt(code, 10)
}

// ldapShown is text the server sent, as an error or a debug report shows
// it: bytes that are not UTF-8 and control characters written as ?, no
// longer than any failure is shown.
func ldapShown(text string) string {
	text = strings.ToValidUTF8(text, "?")
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, text)
	return model.CutTo(strings.TrimSpace(text), model.MaxFailureBytes, false)
}

// ldapEntry is an entry a search answered: its DN and its attributes, each
// with its values, by the names the server wrote them with.
type ldapEntry struct {
	DN         string              `json:"dn"`
	Attributes map[string][]string `json:"attributes"`
}

// errLDAPNotText is the failure of an entry holding bytes that are not
// UTF-8: the document is JSON, whose strings are text, and a value put in
// it would have each such byte replaced with U+FFFD, a different value,
// with nothing to say so.
var errLDAPNotText = errors.New("holds bytes that are not UTF-8 text, which the JSON document cannot hold as they are; leave binary attributes, such as objectGUID, objectSid or jpegPhoto, out of attributes")

// decodeLDAPEntry reads a SearchResultEntry. With keep false only its DN
// is checked and nothing of it is kept, as count_only asks.
func decodeLDAPEntry(content []byte, keep bool) (*ldapEntry, error) {
	r := &berReader{data: content}
	dn, err := r.expect(berOctetString, "an entry's DN")
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(dn) {
		return nil, fmt.Errorf("an entry's DN, %s, %w", ldapShown(string(dn)), errLDAPNotText)
	}
	attributes, err := r.expect(berSequence, "an entry's attributes")
	if err != nil {
		return nil, err
	}
	if !keep {
		return nil, nil
	}
	entry := &ldapEntry{DN: string(dn), Attributes: map[string][]string{}}
	list := &berReader{data: attributes}
	for list.more() {
		attribute, err := list.expect(berSequence, "an attribute of "+entry.DN)
		if err != nil {
			return nil, err
		}
		fields := &berReader{data: attribute}
		name, err := fields.expect(berOctetString, "an attribute's name in "+entry.DN)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(name) {
			return nil, fmt.Errorf("an attribute's name in %s %w", entry.DN, errLDAPNotText)
		}
		set, err := fields.expect(berSet, "the values of "+string(name)+" in "+entry.DN)
		if err != nil {
			return nil, err
		}
		values := entry.Attributes[string(name)]
		if values == nil {
			values = []string{}
		}
		items := &berReader{data: set}
		for items.more() {
			value, err := items.expect(berOctetString, "a value of "+string(name)+" in "+entry.DN)
			if err != nil {
				return nil, err
			}
			if !utf8.Valid(value) {
				return nil, fmt.Errorf("the attribute %s of %s %w", name, entry.DN, errLDAPNotText)
			}
			values = append(values, string(value))
		}
		entry.Attributes[string(name)] = values
	}
	return entry, nil
}

// ldapPagedResultsOID is the Simple Paged Results control's (RFC 2696).
const ldapPagedResultsOID = "1.2.840.113556.1.4.319"

// ldapPagedControl writes the Simple Paged Results control asking for a
// page of size entries after cookie, "" for the first. It is not critical,
// so a server without it answers every entry in one go instead of
// refusing the search.
func ldapPagedControl(size int, cookie string) []byte {
	value := berElement(berSequence, berInt(berInteger, int64(size)), berString(berOctetString, cookie))
	return berElement(berSequence, berString(berOctetString, ldapPagedResultsOID), berBool(false), berElement(berOctetString, value))
}

// ldapPagedCookie reads the cookie of the next page from the controls of a
// SearchResultDone: "" when the search is over, or when the server sent no
// paged results control, having answered in one go.
func ldapPagedCookie(controls []byte) (string, error) {
	list := &berReader{data: controls}
	for list.more() {
		control, err := list.expect(berSequence, "a control")
		if err != nil {
			return "", err
		}
		fields := &berReader{data: control}
		oid, err := fields.expect(berOctetString, "a control's type")
		if err != nil {
			return "", err
		}
		if string(oid) != ldapPagedResultsOID {
			continue
		}
		tag, value, err := fields.next()
		if err == nil && tag == berBoolean {
			tag, value, err = fields.next()
		}
		if err != nil {
			return "", err
		}
		if tag != berOctetString {
			return "", fmt.Errorf("%w: the paged results control has no value", errLDAPProtocol)
		}
		outer := &berReader{data: value}
		inner, err := outer.expect(berSequence, "the paged results control's value")
		if err != nil {
			return "", err
		}
		parts := &berReader{data: inner}
		if _, err := parts.expect(berInteger, "the paged results control's size"); err != nil {
			return "", err
		}
		cookie, err := parts.expect(berOctetString, "the paged results control's cookie")
		if err != nil {
			return "", err
		}
		return string(cookie), nil
	}
	return "", nil
}
