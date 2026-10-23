// Package ldaptest is a stand-in LDAP server for the tests of the ldap
// request type: a TCP server, or a TLS one for ldaps://, with a self-signed
// certificate whose file it writes, that answers LDAPv3 messages in BER as a
// directory server does. It answers a simple bind, anonymous or as one of
// the accounts it was given, invalidCredentials to a wrong password,
// StartTLS, unless told to refuse it, and a search of the entries it was
// given, by base, scope and filter, with the attributes asked for, a page at
// a time when the Simple Paged Results control asks for pages, unless told
// to ignore it, and the references it was given; a test may answer a request
// itself, with any bytes it likes, to play a broken or hostile server. It
// records every request it was sent, the filter written back as RFC 4515
// text. It imports nothing but the standard library.
package ldaptest

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is a stand-in LDAP server.
type Server struct {
	// Addr is the server's host:port, on 127.0.0.1.
	Addr string
	// CAFile is the file of the certificate the server presents, over
	// ldaps:// and after StartTLS, which verifies it for 127.0.0.1 and
	// localhost.
	CAFile string

	listener        net.Listener
	certificate     tls.Certificate
	mu              sync.Mutex
	accounts        map[string]string
	anonymousDenied bool
	refuseStartTLS  bool
	ignorePaging    bool
	entries         []Entry
	references      []string
	searchResult    *Result
	answer          func(r Request) ([]byte, bool)
	requests        []Request
	conns           map[net.Conn]bool
	wg              sync.WaitGroup
}

// Entry is an entry of the directory: its DN and its attributes, each with
// its values, by the names the server writes them with.
type Entry struct {
	DN         string
	Attributes map[string][]string
}

// Result is the code and the diagnostic message a search is answered with.
type Result struct {
	Code       int
	Diagnostic string
}

// Request is a request the server was sent.
type Request struct {
	// Op is BIND, STARTTLS, SEARCH, UNBIND or the tag of another operation
	// in hex.
	Op string
	// ID is the request's message ID.
	ID int
	// DN and Password are a bind's.
	DN, Password string
	// Base, Scope, Filter and Attributes are a search's, Scope as base, one
	// or sub, Filter as RFC 4515 writes it.
	Base       string
	Scope      string
	Filter     string
	Attributes []string
	// Paged says a search came with the Simple Paged Results control, its
	// Size and Cookie, and whether the control was Critical.
	Paged    bool
	Critical bool
	Size     int
	Cookie   string
	// TLS says the request came over TLS.
	TLS bool

	rawFilter []byte
}

// New starts a plain server, which the test's cleanup stops.
func New(t testing.TB) *Server {
	t.Helper()
	certificate, caFile := selfSigned(t)
	listener, err := listen()
	if err != nil {
		t.Fatal(err)
	}
	return start(t, listener, certificate, caFile)
}

// NewTLS starts a server that speaks TLS from the start, as for ldaps://.
func NewTLS(t testing.TB) *Server {
	t.Helper()
	certificate, caFile := selfSigned(t)
	listener, err := listen()
	if err != nil {
		t.Fatal(err)
	}
	return start(t, tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}), certificate, caFile)
}

func listen() (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(context.Background(), "tcp", "127.0.0.1:0")
}

func start(t testing.TB, listener net.Listener, certificate tls.Certificate, caFile string) *Server {
	s := &Server{Addr: listener.Addr().String(), CAFile: caFile, listener: listener, certificate: certificate, accounts: map[string]string{}, conns: map[net.Conn]bool{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Close stops the server, closes its connections and waits for them to
// end.
func (s *Server) Close() {
	_ = s.listener.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// AddAccount lets a simple bind as dn with password succeed.
func (s *Server) AddAccount(dn, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[strings.ToLower(dn)] = password
}

// DenyAnonymous answers a search on a connection with no bind but an
// anonymous one insufficientAccessRights, as Active Directory does.
func (s *Server) DenyAnonymous() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anonymousDenied = true
}

// RefuseStartTLS answers StartTLS protocolError, as a server without TLS
// does.
func (s *Server) RefuseStartTLS() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseStartTLS = true
}

// IgnorePaging answers every entry of a search in one go, with no paged
// results control, as a server without the control does.
func (s *Server) IgnorePaging() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ignorePaging = true
}

// AddEntry adds entries to the directory.
func (s *Server) AddEntry(entries ...Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entries...)
}

// SetReferences sets the continuation references each search answers after
// its entries.
func (s *Server) SetReferences(urls ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.references = urls
}

// FailSearches answers every search with the result given instead of
// success, after its entries.
func (s *Server) FailSearches(code int, diagnostic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchResult = &Result{Code: code, Diagnostic: diagnostic}
}

// Answer has answer reply to every request it says it answers, with any
// bytes, before the server looks at it; it may block, to play a server
// that never answers, until the connection is closed.
func (s *Server) Answer(answer func(r Request) (raw []byte, ok bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

// Requests returns every request the server was sent, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// session is what a connection has done so far.
type session struct {
	conn    net.Conn
	r       *bufio.Reader
	tls     bool
	bound   bool
	boundDN string
}

func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_, isTLS := conn.(*tls.Conn)
	sess := &session{conn: conn, r: bufio.NewReader(conn), tls: isTLS}
	for {
		id, op, content, controls, err := readMessage(sess.r)
		if err != nil {
			return
		}
		req := decodeRequest(id, op, content, controls)
		req.TLS = sess.tls
		s.mu.Lock()
		s.requests = append(s.requests, req)
		answer := s.answer
		s.mu.Unlock()
		if answer != nil {
			if raw, ok := answer(req); ok {
				if _, err := sess.conn.Write(raw); err != nil {
					return
				}
				continue
			}
		}
		if !s.reply(sess, req) {
			return
		}
	}
}

// The result codes the server answers with.
const (
	success                  = 0
	protocolError            = 2
	noSuchObject             = 32
	invalidCredentials       = 49
	insufficientAccessRights = 50
)

// reply answers a request; false ends the connection.
func (s *Server) reply(sess *session, req Request) bool {
	write := func(raw []byte) bool {
		_, err := sess.conn.Write(raw)
		return err == nil
	}
	switch req.Op {
	case "UNBIND":
		return false
	case "BIND":
		s.mu.Lock()
		password, ok := s.accounts[strings.ToLower(req.DN)]
		s.mu.Unlock()
		switch {
		case req.DN == "" && req.Password == "":
			sess.bound, sess.boundDN = true, ""
			return write(Message(req.ID, ResultOp(0x61, success, "")))
		case ok && password == req.Password:
			sess.bound, sess.boundDN = true, req.DN
			return write(Message(req.ID, ResultOp(0x61, success, "")))
		}
		return write(Message(req.ID, ResultOp(0x61, invalidCredentials, "80090308: LdapErr: DSID-0C09044E, comment: AcceptSecurityContext error, data 52e, v4563")))
	case "STARTTLS":
		s.mu.Lock()
		refuse := s.refuseStartTLS
		s.mu.Unlock()
		if refuse || sess.tls {
			return write(Message(req.ID, ResultOp(0x78, protocolError, "unsupported extended operation")))
		}
		if !write(Message(req.ID, ResultOp(0x78, success, ""))) {
			return false
		}
		tlsConn := tls.Server(sess.conn, &tls.Config{Certificates: []tls.Certificate{s.certificate}, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(context.Background()); err != nil {
			return false
		}
		sess.conn, sess.r, sess.tls = tlsConn, bufio.NewReader(tlsConn), true
		return true
	case "SEARCH":
		return s.search(sess, req, write)
	}
	return write(Message(req.ID, ResultOp(0x78, protocolError, "unknown operation")))
}

// search answers a search: its entries, a page of them when it asks for
// pages, the references, and its end.
func (s *Server) search(sess *session, req Request, write func([]byte) bool) bool {
	s.mu.Lock()
	denied := s.anonymousDenied && sess.boundDN == ""
	ignorePaging := s.ignorePaging
	entries := append([]Entry(nil), s.entries...)
	references := append([]string(nil), s.references...)
	failure := s.searchResult
	s.mu.Unlock()
	if denied {
		return write(Message(req.ID, ResultOp(0x65, insufficientAccessRights, "000004DC: LdapErr: DSID-0C090A5C, comment: In order to perform this operation a successful bind must be completed on the connection., data 0, v4563")))
	}
	filter, _, err := parseFilter(req.rawFilter)
	if err != nil {
		return write(Message(req.ID, ResultOp(0x65, protocolError, err.Error())))
	}
	if req.Base != "" && !anyUnder(entries, req.Base) {
		return write(Message(req.ID, ResultOp(0x65, noSuchObject, "")))
	}
	var matched []Entry
	for _, e := range entries {
		if inScope(e.DN, req.Base, req.Scope) && filter.match(e) {
			matched = append(matched, e)
		}
	}
	start, end := 0, len(matched)
	paged := req.Paged && !ignorePaging
	if paged {
		start, _ = strconv.Atoi(req.Cookie)
		end = min(start+max(req.Size, 1), len(matched))
		start = min(start, end)
	}
	for _, e := range matched[start:end] {
		if !write(Message(req.ID, entryOp(e, req.Attributes))) {
			return false
		}
	}
	if !paged || end == len(matched) {
		for _, url := range references {
			if !write(Message(req.ID, Element(0x73, Element(0x04, []byte(url))))) {
				return false
			}
		}
	}
	code, diagnostic := success, ""
	if failure != nil {
		code, diagnostic = failure.Code, failure.Diagnostic
	}
	done := ResultOp(0x65, code, diagnostic)
	if !paged {
		return write(Message(req.ID, done))
	}
	cookie := ""
	if end < len(matched) {
		cookie = strconv.Itoa(end)
	}
	return write(Message(req.ID, done, PagedControl(0, cookie)))
}

// entryOp is a SearchResultEntry of e with the attributes asked for: none
// for 1.1, every one for * or for none asked, and those named otherwise,
// in any case.
func entryOp(e Entry, asked []string) []byte {
	var attributes [][]byte
	for name, values := range e.Attributes {
		if !wanted(name, asked) {
			continue
		}
		var items [][]byte
		for _, v := range values {
			items = append(items, Element(0x04, []byte(v)))
		}
		attributes = append(attributes, Element(0x30, Element(0x04, []byte(name)), Element(0x31, items...)))
	}
	return Element(0x64, Element(0x04, []byte(e.DN)), Element(0x30, attributes...))
}

func wanted(name string, asked []string) bool {
	if len(asked) == 0 {
		return true
	}
	for _, a := range asked {
		if a == "*" || a == "+" || strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

// anyUnder says whether an entry is the base or under it.
func anyUnder(entries []Entry, base string) bool {
	for _, e := range entries {
		if inScope(e.DN, base, "sub") {
			return true
		}
	}
	return false
}

// inScope says whether dn is in the scope of a search of base.
func inScope(dn, base, scope string) bool {
	dn, base = strings.ToLower(dn), strings.ToLower(base)
	switch scope {
	case "base":
		return dn == base
	case "one":
		_, parent, _ := strings.Cut(dn, ",")
		return parent == base
	}
	return base == "" || dn == base || strings.HasSuffix(dn, ","+base)
}

// Message is an LDAPMessage of the ID, holding the operation and, if any
// are given, controls.
func Message(id int, op []byte, controls ...[]byte) []byte {
	parts := [][]byte{Element(0x02, intBytes(id)), op}
	if len(controls) > 0 {
		parts = append(parts, Element(0xa0, controls...))
	}
	return Element(0x30, parts...)
}

// ResultOp is a response operation of the tag holding an LDAPResult of the
// code and the diagnostic message.
func ResultOp(tag byte, code int, diagnostic string) []byte {
	return Element(tag, Element(0x0a, intBytes(code)), Element(0x04, nil), Element(0x04, []byte(diagnostic)))
}

// PagedControl is the Simple Paged Results control a server answers a page
// with.
func PagedControl(size int, cookie string) []byte {
	value := Element(0x30, Element(0x02, intBytes(size)), Element(0x04, []byte(cookie)))
	return Element(0x30, Element(0x04, []byte("1.2.840.113556.1.4.319")), Element(0x04, value))
}

// Element is a BER element of the tag holding the contents given, with a
// length of the fewest bytes.
func Element(tag byte, contents ...[]byte) []byte {
	var body []byte
	for _, c := range contents {
		body = append(body, c...)
	}
	return append(append([]byte{tag}, Length(len(body))...), body...)
}

// Length is a BER length of n.
func Length(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)} //nolint:gosec // a length below 0x80 here
	}
	var digits []byte
	for v := n; v > 0; v >>= 8 {
		digits = append([]byte{byte(v)}, digits...)
	}
	return append([]byte{0x80 | byte(len(digits))}, digits...) //nolint:gosec // an int has at most 8 bytes
}

func intBytes(v int) []byte {
	var digits []byte
	for {
		digits = append([]byte{byte(v)}, digits...) //nolint:gosec // the low byte, as two's complement writes it
		if v < 0x80 && v >= -0x80 {
			return digits
		}
		v >>= 8
	}
}

// readMessage reads an LDAPMessage as a client sends it.
func readMessage(r *bufio.Reader) (id int, op byte, content, controls []byte, err error) {
	tag, err := r.ReadByte()
	if err != nil {
		return 0, 0, nil, nil, err
	}
	if tag != 0x30 {
		return 0, 0, nil, nil, fmt.Errorf("a message begins with 0x%02x", tag)
	}
	first, err := r.ReadByte()
	if err != nil {
		return 0, 0, nil, nil, err
	}
	n := int(first)
	if first >= 0x80 {
		count := int(first & 0x7f)
		if count == 0 || count > 3 {
			return 0, 0, nil, nil, errors.New("a message's length is too long")
		}
		n = 0
		for range count {
			b, err := r.ReadByte()
			if err != nil {
				return 0, 0, nil, nil, err
			}
			n = n<<8 | int(b)
		}
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, 0, nil, nil, err
	}
	rest := data
	_, idContent, rest, err := next(rest)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	op, content, rest, err = next(rest)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	if len(rest) > 0 {
		if _, controls, _, err = next(rest); err != nil {
			return 0, 0, nil, nil, err
		}
	}
	return toInt(idContent), op, content, controls, nil
}

// next reads an element off data.
func next(data []byte) (byte, []byte, []byte, error) {
	if len(data) < 2 {
		return 0, nil, nil, errors.New("an element is cut short")
	}
	tag, n, header := data[0], int(data[1]), 2
	if data[1] >= 0x80 {
		count := int(data[1] & 0x7f)
		if count == 0 || count > 3 || len(data) < 2+count {
			return 0, nil, nil, errors.New("a length that cannot be read")
		}
		n = 0
		for _, b := range data[2 : 2+count] {
			n = n<<8 | int(b)
		}
		header += count
	}
	if len(data) < header+n {
		return 0, nil, nil, errors.New("an element is cut short")
	}
	return tag, data[header : header+n], data[header+n:], nil
}

func toInt(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	v := int(int8(b[0])) //nolint:gosec // the first byte carries the sign
	for _, d := range b[1:] {
		v = v<<8 | int(d)
	}
	return v
}

// decodeRequest reads a request's operation.
func decodeRequest(id int, op byte, content, controls []byte) Request {
	req := Request{ID: id, Op: fmt.Sprintf("0x%02x", op)}
	switch op {
	case 0x42:
		req.Op = "UNBIND"
	case 0x60:
		req.Op = "BIND"
		_, _, rest, _ := next(content)
		_, dn, rest, _ := next(rest)
		_, password, _, _ := next(rest)
		req.DN, req.Password = string(dn), string(password)
	case 0x77:
		_, name, _, _ := next(content)
		if string(name) == "1.3.6.1.4.1.1466.20037" {
			req.Op = "STARTTLS"
		}
	case 0x63:
		req.Op = "SEARCH"
		_, base, rest, _ := next(content)
		_, scope, rest, _ := next(rest)
		_, _, rest, _ = next(rest) // derefAliases
		_, _, rest, _ = next(rest) // sizeLimit
		_, _, rest, _ = next(rest) // timeLimit
		_, _, rest, _ = next(rest) // typesOnly
		filterTag, filterContent, rest, _ := next(rest)
		_, attributes, _, _ := next(rest)
		req.Base = string(base)
		req.Scope = []string{"base", "one", "sub"}[min(max(toInt(scope), 0), 2)]
		req.rawFilter = Element(filterTag, filterContent)
		if _, text, err := parseFilter(req.rawFilter); err == nil {
			req.Filter = text
		}
		for len(attributes) > 0 {
			var a []byte
			_, a, attributes, _ = next(attributes)
			req.Attributes = append(req.Attributes, string(a))
		}
		for len(controls) > 0 {
			var control []byte
			_, control, controls, _ = next(controls)
			_, oid, rest, _ := next(control)
			if string(oid) != "1.2.840.113556.1.4.319" {
				continue
			}
			tag, value, rest, _ := next(rest)
			if tag == 0x01 {
				req.Critical = len(value) > 0 && value[0] != 0
				_, value, _, _ = next(rest)
			}
			_, inner, _, _ := next(value)
			_, size, rest2, _ := next(inner)
			_, cookie, _, _ := next(rest2)
			req.Paged, req.Size, req.Cookie = true, toInt(size), string(cookie)
		}
	}
	return req
}

// filter is a parsed search filter.
type filter struct {
	op        byte
	attribute string
	value     string
	initial   string
	any       []string
	final     string
	children  []*filter
	// rule and dnAttributes are an extensible match's.
	rule         string
	dnAttributes bool
}

// parseFilter decodes a filter's BER, and writes it back as RFC 4515
// text.
func parseFilter(raw []byte) (*filter, string, error) {
	tag, content, _, err := next(raw)
	if err != nil {
		return nil, "", err
	}
	f := &filter{op: tag}
	switch tag {
	case 0xa0, 0xa1, 0xa2:
		var parts []string
		for len(content) > 0 {
			t, c, rest, err := next(content)
			if err != nil {
				return nil, "", err
			}
			child, text, err := parseFilter(Element(t, c))
			if err != nil {
				return nil, "", err
			}
			f.children = append(f.children, child)
			parts = append(parts, text)
			content = rest
		}
		return f, "(" + string("&|!"[tag-0xa0]) + strings.Join(parts, "") + ")", nil
	case 0x87:
		f.attribute = string(content)
		return f, "(" + f.attribute + "=*)", nil
	case 0xa3, 0xa5, 0xa6, 0xa8:
		_, a, rest, err := next(content)
		if err != nil {
			return nil, "", err
		}
		_, v, _, err := next(rest)
		if err != nil {
			return nil, "", err
		}
		f.attribute, f.value = string(a), string(v)
		operator := map[byte]string{0xa3: "=", 0xa5: ">=", 0xa6: "<=", 0xa8: "~="}[tag]
		return f, "(" + f.attribute + operator + escape(f.value) + ")", nil
	case 0xa4:
		_, a, rest, err := next(content)
		if err != nil {
			return nil, "", err
		}
		_, list, _, err := next(rest)
		if err != nil {
			return nil, "", err
		}
		// RFC 4511 gives substrings SIZE (1..MAX): a server answers one of
		// none protocolError, as this one does.
		if len(list) == 0 {
			return nil, "", errors.New("a SubstringFilter with no substring")
		}
		f.attribute = string(a)
		text := "(" + f.attribute + "="
		for len(list) > 0 {
			t, v, rest, err := next(list)
			if err != nil {
				return nil, "", err
			}
			switch t {
			case 0x80:
				f.initial = string(v)
				text += escape(f.initial)
			case 0x81:
				f.any = append(f.any, string(v))
				text += "*" + escape(string(v))
			case 0x82:
				f.final = string(v)
				text += "*" + escape(f.final)
			}
			list = rest
		}
		if f.final == "" {
			text += "*"
		}
		return f, text + ")", nil
	case 0xa9:
		text := "("
		for len(content) > 0 {
			t, v, rest, err := next(content)
			if err != nil {
				return nil, "", err
			}
			switch t {
			case 0x81:
				f.rule = string(v)
			case 0x82:
				f.attribute = string(v)
			case 0x83:
				f.value = string(v)
			case 0x84:
				f.dnAttributes = len(v) > 0 && v[0] != 0
			}
			content = rest
		}
		text += f.attribute
		if f.dnAttributes {
			text += ":dn"
		}
		if f.rule != "" {
			text += ":" + f.rule
		}
		return f, text + ":=" + escape(f.value) + ")", nil
	}
	return nil, "", fmt.Errorf("a filter of the tag 0x%02x", tag)
}

// escape writes a value as RFC 4515 escapes it.
func escape(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '*', '(', ')', '\\', 0:
			fmt.Fprintf(&b, "\\%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// match says whether an entry matches the filter, attribute names and
// values compared in any case, as most of a directory's attributes are.
func (f *filter) match(e Entry) bool {
	switch f.op {
	case 0xa0:
		for _, c := range f.children {
			if !c.match(e) {
				return false
			}
		}
		return true
	case 0xa1:
		for _, c := range f.children {
			if c.match(e) {
				return true
			}
		}
		return false
	case 0xa2:
		return !f.children[0].match(e)
	case 0x87:
		return strings.EqualFold(f.attribute, "objectClass") || len(values(e, f.attribute)) > 0
	case 0xa9:
		return f.extensibleMatch(e)
	}
	for _, v := range values(e, f.attribute) {
		lv, lf := strings.ToLower(v), strings.ToLower(f.value)
		switch f.op {
		case 0xa3, 0xa8:
			if lv == lf {
				return true
			}
		case 0xa5, 0xa6:
			a, errA := strconv.ParseInt(v, 10, 64)
			b, errB := strconv.ParseInt(f.value, 10, 64)
			cmp := strings.Compare(lv, lf)
			if errA == nil && errB == nil {
				cmp = 0
				if a < b {
					cmp = -1
				} else if a > b {
					cmp = 1
				}
			}
			if f.op == 0xa5 && cmp >= 0 || f.op == 0xa6 && cmp <= 0 {
				return true
			}
		case 0xa4:
			rest := lv
			if !strings.HasPrefix(rest, strings.ToLower(f.initial)) {
				continue
			}
			rest = rest[len(f.initial):]
			ok := true
			for _, part := range f.any {
				i := strings.Index(rest, strings.ToLower(part))
				if i < 0 {
					ok = false
					break
				}
				rest = rest[i+len(part):]
			}
			if ok && strings.HasSuffix(rest, strings.ToLower(f.final)) {
				return true
			}
		}
	}
	return false
}

// extensibleMatch matches an extensible match: Active Directory's bitwise
// AND and OR rules, 1.2.840.113556.1.4.803 and .804, on numbers, and any
// other rule as equality in any case; with no attribute, any attribute's
// value matches.
func (f *filter) extensibleMatch(e Entry) bool {
	var candidates []string
	if f.attribute == "" {
		for _, vs := range e.Attributes {
			candidates = append(candidates, vs...)
		}
	} else {
		candidates = values(e, f.attribute)
	}
	for _, v := range candidates {
		switch f.rule {
		case "1.2.840.113556.1.4.803", "1.2.840.113556.1.4.804":
			a, errA := strconv.ParseInt(v, 10, 64)
			b, errB := strconv.ParseInt(f.value, 10, 64)
			if errA != nil || errB != nil {
				continue
			}
			if f.rule == "1.2.840.113556.1.4.803" && a&b == b || f.rule == "1.2.840.113556.1.4.804" && a&b != 0 {
				return true
			}
		default:
			if strings.EqualFold(v, f.value) {
				return true
			}
		}
	}
	return false
}

func values(e Entry, attribute string) []string {
	for name, vs := range e.Attributes {
		if strings.EqualFold(name, attribute) {
			return vs
		}
	}
	return nil
}

// selfSigned makes a certificate for 127.0.0.1 and localhost, and writes it
// to a file of the test's own directory.
func selfSigned(t testing.TB) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ldaptest"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ldap-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, caFile
}
