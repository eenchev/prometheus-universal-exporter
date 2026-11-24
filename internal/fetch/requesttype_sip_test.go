//go:build request_type_sip

package fetch

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sipSeen is a request as the stand-in read it: its start line and its
// headers by name in lower case.
type sipSeen struct {
	start   string
	headers map[string]string
	raw     string
}

// sipStandIn is a SIP server that answers OPTIONS, over UDP at addr and
// over a stream through serve. answer says what goes back for a request,
// each a whole message, none to say nothing; unset, a 200 is.
type sipStandIn struct {
	addr string
	mu   sync.Mutex
	seen []sipSeen
	// answer is what the server says to the nth request it read, from 1.
	answer func(n int, request sipSeen) []string
}

// sipReply is a response to request with status and the headers given,
// each "Name: value", and a body: the Via, From, To, Call-ID and CSeq are
// the request's, as a server echoes them.
func sipReply(request sipSeen, status string, extra []string, body string) string {
	lines := []string{"SIP/2.0 " + status,
		"Via: " + request.headers["via"] + ";received=127.0.0.1",
		"From: " + request.headers["from"],
		"To: " + request.headers["to"] + ";tag=as1",
		"Call-ID: " + request.headers["call-id"],
		"CSeq: " + request.headers["cseq"]}
	lines = append(lines, extra...)
	lines = append(lines, "Content-Length: "+strconv.Itoa(len(body)))
	return strings.Join(lines, "\r\n") + "\r\n\r\n" + body
}

// sipParse reads a request's text.
func sipParse(text string) sipSeen {
	head, _, _ := strings.Cut(text, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	seen := sipSeen{start: lines[0], headers: map[string]string{}, raw: text}
	for _, line := range lines[1:] {
		name, value, _ := strings.Cut(line, ":")
		seen.headers[strings.ToLower(name)] = strings.TrimSpace(value)
	}
	return seen
}

// take records a request and returns the answers to it.
func (s *sipStandIn) take(text string) []string {
	request := sipParse(text)
	s.mu.Lock()
	s.seen = append(s.seen, request)
	n := len(s.seen)
	s.mu.Unlock()
	if s.answer == nil {
		return []string{sipReply(request, "200 OK", []string{"Server: stand-in/1.0", "Allow: INVITE, ACK, CANCEL", "Allow: OPTIONS,BYE", "Accept: application/sdp", "Supported: path, replaces"}, "")}
	}
	return s.answer(n, request)
}

func (s *sipStandIn) requests() []sipSeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sipSeen(nil), s.seen...)
}

// startSIP starts the stand-in over UDP.
func startSIP(t *testing.T, answer func(n int, request sipSeen) []string) *sipStandIn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sipStandIn{addr: conn.LocalAddr().String(), answer: answer}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			for _, reply := range s.take(string(buffer[:n])) {
				_, _ = conn.WriteTo([]byte(reply), from)
			}
		}
	}()
	return s
}

// serve answers the requests of a stream.
func (s *sipStandIn) serve(conn net.Conn) {
	r := bufio.NewReader(conn)
	for {
		var text strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			text.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		for _, reply := range s.take(text.String()) {
			if _, err := conn.Write([]byte(reply)); err != nil {
				return
			}
		}
	}
}

// sipCollector is a validated sip collector, changed by edit first.
func sipCollector(t *testing.T, edit func(*model.Collector, *SIPConfig)) *model.Collector {
	t.Helper()
	own := SIPConfig{}
	c := &model.Collector{Name: "pbx", Request: model.RequestConfig{Type: RequestTypeSIP}}
	if edit != nil {
		edit(c, &own)
	}
	c.Request.Own = own
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// sipAccount gives a collector the account monitor, with password.
func sipAccount(t *testing.T, password string) func(*model.Collector, *SIPConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(c *model.Collector, _ *SIPConfig) { c.Request.Username, c.Request.PasswordFile = "monitor", path }
}

// sipDoc is a scrape of c within timeout, and its document.
func sipDoc(t *testing.T, c *model.Collector, target string, timeout time.Duration) (sipDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return sipDocument{}, err
	}
	var doc sipDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v in %s", err, resp.Body)
	}
	return doc, nil
}

// quickSIPTimers shortens the timers a request is sent again by.
func quickSIPTimers(t *testing.T) {
	t.Helper()
	t1, t2 := sipT1, sipT2
	sipT1, sipT2 = 20*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { sipT1, sipT2 = t1, t2 })
}

// A scrape sends one OPTIONS over UDP, a request a server takes, and the
// document is the response: its status, what the server says of itself and
// every header, with how the request went. The request is traced, and
// labelled by the server.
func TestASIPScrapeAsksWithOPTIONS(t *testing.T) {
	s := startSIP(t, nil)
	c := sipCollector(t, nil)
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, s.addr, c, RequestOverrides{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc sipDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(s.addr)
	if doc.StatusCode != http.StatusOK || doc.Reason != "OK" || !doc.OK || doc.Requests != 1 || doc.Attempts != 1 || doc.Challenged || doc.Authenticated ||
		doc.Host != "127.0.0.1" || strconv.Itoa(doc.Port) != port || doc.Address != s.addr || doc.Transport != "udp" || doc.Server != "stand-in/1.0" ||
		strings.Join(doc.Allow, "|") != "INVITE|ACK|CANCEL|OPTIONS|BYE" || strings.Join(doc.Supported, "|") != "path|replaces" || strings.Join(doc.Accept, "|") != "application/sdp" ||
		doc.Headers["cseq"][0] != "1 OPTIONS" || doc.TLSVersion != "" || doc.ResponseSeconds <= 0 || doc.DurationSeconds < doc.ResponseSeconds {
		t.Errorf("document %+v", doc)
	}
	if !resp.NoStatus || resp.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("response %+v", resp)
	}
	seen := s.requests()
	if len(seen) != 1 {
		t.Fatalf("the server read %d requests", len(seen))
	}
	request := seen[0]
	if request.start != "OPTIONS sip:"+s.addr+" SIP/2.0" || !strings.HasPrefix(request.headers["via"], "SIP/2.0/UDP 127.0.0.1:") || !strings.Contains(request.headers["via"], ";branch=z9hG4bK") || !strings.HasSuffix(request.headers["via"], ";rport") ||
		request.headers["max-forwards"] != "70" || !strings.HasPrefix(request.headers["from"], "<sip:prometheus-universal-exporter@127.0.0.1>;tag=") || request.headers["to"] != "<sip:"+s.addr+">" ||
		request.headers["cseq"] != "1 OPTIONS" || request.headers["call-id"] == "" || request.headers["user-agent"] != "prometheus-universal-exporter" || request.headers["content-length"] != "0" || !strings.HasSuffix(request.raw, "\r\n\r\n") {
		t.Errorf("the request:\n%s", request.raw)
	}
	requests := trace.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodOptions || requests[0].URL != "sip://"+s.addr || requests[0].Outcome != "200 OK, sent once" || requests[0].Header.Get("Sip-Transport") != "udp" {
		t.Errorf("traced %+v", requests)
	}
	// A datagram that does not say how long its body is has the rest of it
	// for one, however long.
	long := strings.Repeat("a=x\r\n", 2000)
	nolength := startSIP(t, func(_ int, request sipSeen) []string {
		reply := sipReply(request, "200 OK", nil, "")
		return []string{strings.Replace(reply, "Content-Length: 0\r\n", "", 1) + long}
	})
	if doc, err := sipDoc(t, c, nolength.addr, 5*time.Second); err != nil || doc.BodyBytes != len(long) {
		t.Errorf("a body without its length: %d bytes, %v; want %d", doc.BodyBytes, err, len(long))
	}
	if label, err := RequestLabelFor(s.addr, c, RequestOverrides{}); err != nil || label != "sip://"+s.addr {
		t.Errorf("label %q, %v", label, err)
	}
	if method, stage := RequestMethodFor(c, RequestOverrides{}), FetchStage(c); method != http.MethodOptions || stage != "sip" {
		t.Errorf("method %q, stage %q", method, stage)
	}
}

// A response is an answer whatever its status: a server that is up and
// says 404 is a scrape that succeeded, with ok false. The request_uri,
// from_uri and user_agent of the collector are the request's.
func TestASIPResponseIsAnAnswerWhateverItsStatus(t *testing.T) {
	s := startSIP(t, func(_ int, request sipSeen) []string {
		return []string{sipReply(request, "404 Not Found", []string{"User-Agent: pbx 7"}, "")}
	})
	c := sipCollector(t, func(_ *model.Collector, r *SIPConfig) {
		r.RequestURI, r.FromURI, r.UserAgent = "sip:1000@pbx.example.com", "sips:probe@monitoring.example.com", "probe/2"
	})
	doc, err := sipDoc(t, c, s.addr, 5*time.Second)
	if err != nil || doc.StatusCode != http.StatusNotFound || doc.Reason != "Not Found" || doc.OK || doc.UserAgent != "pbx 7" {
		t.Fatalf("document %+v, %v", doc, err)
	}
	request := s.requests()[0]
	if request.start != "OPTIONS sip:1000@pbx.example.com SIP/2.0" || request.headers["to"] != "<sip:1000@pbx.example.com>" || !strings.HasPrefix(request.headers["from"], "<sips:probe@monitoring.example.com>;tag=") || request.headers["user-agent"] != "probe/2" {
		t.Errorf("the request:\n%s", request.raw)
	}
}

// Over UDP the request is sent again while no response comes, the same
// request, each wait twice the last; a datagram that answers another
// request, one that is no SIP message and a provisional response are
// passed over; and when the deadline comes first the failure says how
// often the request was sent and why the last message was ignored.
func TestASIPRequestIsSentAgainOverUDP(t *testing.T) {
	quickSIPTimers(t)
	s := startSIP(t, func(n int, request sipSeen) []string {
		if n < 3 {
			return nil
		}
		other := request
		other.headers = map[string]string{"via": "SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bKother", "from": request.headers["from"], "to": request.headers["to"], "call-id": request.headers["call-id"], "cseq": request.headers["cseq"]}
		return []string{"not sip at all", sipReply(other, "200 OK", nil, ""), sipReply(request, "100 Trying", nil, ""), sipReply(request, "200 OK", nil, "")}
	})
	doc, err := sipDoc(t, sipCollector(t, nil), s.addr, 5*time.Second)
	if err != nil || doc.Attempts != 3 || doc.Requests != 1 || doc.StatusCode != http.StatusOK {
		t.Fatalf("document %+v, %v", doc, err)
	}
	seen := s.requests()
	if len(seen) != 3 || seen[0].raw != seen[1].raw || seen[1].raw != seen[2].raw {
		t.Errorf("the server read %d requests, and they are not one request sent again", len(seen))
	}

	silent := startSIP(t, func(_ int, request sipSeen) []string {
		return []string{sipReply(request, "180 Ringing", nil, "")}
	})
	_, err = sipDoc(t, sipCollector(t, nil), silent.addr, 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no response from "+silent.addr+" before the deadline, the request sent ") || !strings.Contains(err.Error(), "the last as it is provisional, 180 Ringing") {
		t.Errorf("err=%v", err)
	}
	if sent := len(silent.requests()); sent < 4 || sent > 8 {
		t.Errorf("the request was sent %d times in 300ms of timers from 20ms to 80ms", sent)
	}
}

// sipExpectedDigest is the response a server expects of the account
// monitor to its challenge, worked out as RFC 7616 writes it.
func sipExpectedDigest(newHash func() hash.Hash, sess bool, params map[string]string, password, realm string) string {
	sum := func(text string) string {
		h := newHash()
		h.Write([]byte(text))
		return hex.EncodeToString(h.Sum(nil))
	}
	ha1 := sum("monitor:" + realm + ":" + password)
	if sess {
		ha1 = sum(ha1 + ":" + params["nonce"] + ":" + params["cnonce"])
	}
	ha2 := sum("OPTIONS:" + params["uri"])
	if params["qop"] != "" {
		return sum(ha1 + ":" + params["nonce"] + ":" + params["nc"] + ":" + params["cnonce"] + ":" + params["qop"] + ":" + ha2)
	}
	return sum(ha1 + ":" + params["nonce"] + ":" + ha2)
}

// A server that asks who is asking is answered with the collector's
// account, by the digest of its challenge: MD5 and SHA-256, their -sess
// forms, with qop auth and without, to a 401 and to a 407. The second
// request is the scrape's next, under the same Call-ID. The stronger of
// two challenges is the one answered.
func TestASIPChallengeIsAnsweredWithTheAccount(t *testing.T) {
	for name, test := range map[string]struct {
		status, challenge string
		hash              func() hash.Hash
		sess, qop         bool
	}{
		"md5":            {"401 Unauthorized", `Digest realm="pbx", nonce="n1"`, md5.New, false, false},
		"md5 qop":        {"401 Unauthorized", `Digest realm="pbx", nonce="n1", algorithm=MD5, qop="auth,auth-int", opaque="op"`, md5.New, false, true},
		"md5-sess":       {"401 Unauthorized", `Digest realm="pbx",nonce="n1",algorithm=MD5-sess,qop="auth"`, md5.New, true, true},
		"sha-256":        {"401 Unauthorized", `Digest realm="a \"quoted\" realm", nonce="n1", algorithm=SHA-256, qop="auth"`, sha256.New, false, true},
		"sha-256-sess":   {"401 Unauthorized", `Digest realm="pbx", nonce="n1", algorithm=SHA-256-sess`, sha256.New, true, false},
		"proxy":          {"407 Proxy Authentication Required", `Digest realm="pbx", nonce="n1", qop="auth"`, md5.New, false, true},
		"the stronger":   {"401 Unauthorized", "Digest realm=\"pbx\", nonce=\"weak\", algorithm=MD5\r\nWWW-Authenticate: Digest realm=\"pbx\", nonce=\"n1\", algorithm=SHA-256", sha256.New, false, false},
		"basic is other": {"401 Unauthorized", "Basic realm=\"pbx\"\r\nWWW-Authenticate: Digest realm=\"pbx\", nonce=\"n1\"", md5.New, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			challenge, answer := "WWW-Authenticate", "authorization"
			if strings.HasPrefix(test.status, "407") {
				challenge, answer = "Proxy-Authenticate", "proxy-authorization"
			}
			realm := "pbx"
			if strings.Contains(test.challenge, "quoted") {
				realm = `a "quoted" realm`
			}
			s := startSIP(t, func(_ int, request sipSeen) []string {
				credentials, given := strings.CutPrefix(request.headers[answer], "Digest ")
				if !given {
					return []string{sipReply(request, test.status, []string{challenge + ": " + test.challenge}, "")}
				}
				params := sipAuthParams(credentials)
				if params["response"] != sipExpectedDigest(test.hash, test.sess, params, "s3cret", realm) || params["nonce"] != "n1" || params["username"] != "monitor" || params["realm"] != realm || (params["qop"] == "auth") != test.qop {
					return []string{sipReply(request, "403 Forbidden", nil, "")}
				}
				return []string{sipReply(request, "200 OK", nil, "")}
			})
			doc, err := sipDoc(t, sipCollector(t, sipAccount(t, "s3cret")), s.addr, 5*time.Second)
			if err != nil || doc.StatusCode != http.StatusOK || !doc.Challenged || !doc.Authenticated || doc.Requests != 2 || doc.Attempts != 1 {
				t.Fatalf("document %+v, %v", doc, err)
			}
			seen := s.requests()
			if len(seen) != 2 || seen[1].headers["cseq"] != "2 OPTIONS" || seen[0].headers["call-id"] != seen[1].headers["call-id"] || seen[0].headers["via"] == seen[1].headers["via"] ||
				!strings.HasPrefix(seen[0].headers["from"], "<sip:monitor@127.0.0.1>;tag=") || seen[0].headers["from"] != seen[1].headers["from"] {
				t.Errorf("the two requests:\n%s\n%s", seen[0].raw, seen[1].raw)
			}
			if params := sipAuthParams(strings.TrimPrefix(seen[1].headers[answer], "Digest ")); params["uri"] != "sip:"+s.addr || (params["opaque"] == "op") != strings.Contains(test.challenge, "opaque") {
				t.Errorf("the credentials: %s", seen[1].headers[answer])
			}
		})
	}
}

// A challenge is the answer when the collector has no account, and when
// the server does not take its credentials; one that says the nonce went
// stale is answered once more; and one that cannot be answered, of no
// Digest or of an algorithm that is not spoken, fails the scrape saying so.
// The password is in no request and no failure.
func TestASIPChallengeThatIsNotMetIsTheAnswer(t *testing.T) {
	challenge := func(extra string) func(int, sipSeen) []string {
		return func(n int, request sipSeen) []string {
			return []string{sipReply(request, "401 Unauthorized", []string{fmt.Sprintf(`WWW-Authenticate: Digest realm="pbx", nonce="n%d"%s`, n, extra)}, "")}
		}
	}
	s := startSIP(t, challenge(""))
	doc, err := sipDoc(t, sipCollector(t, nil), s.addr, 5*time.Second)
	if err != nil || doc.StatusCode != http.StatusUnauthorized || !doc.Challenged || doc.Authenticated || doc.Requests != 1 || doc.OK {
		t.Errorf("without an account: %+v, %v", doc, err)
	}

	s = startSIP(t, challenge(""))
	doc, err = sipDoc(t, sipCollector(t, sipAccount(t, "wr0ng-s3cret")), s.addr, 5*time.Second)
	if err != nil || doc.StatusCode != http.StatusUnauthorized || !doc.Challenged || doc.Authenticated || doc.Requests != 2 {
		t.Errorf("credentials the server does not take: %+v, %v", doc, err)
	}
	for _, request := range s.requests() {
		if strings.Contains(request.raw, "wr0ng-s3cret") {
			t.Errorf("the password is in a request:\n%s", request.raw)
		}
	}

	s = startSIP(t, func(n int, request sipSeen) []string {
		if n == 3 && strings.Contains(request.headers["authorization"], `nonce="n2"`) {
			return []string{sipReply(request, "200 OK", nil, "")}
		}
		return challenge(", stale=true")(n, request)
	})
	doc, err = sipDoc(t, sipCollector(t, sipAccount(t, "s3cret")), s.addr, 5*time.Second)
	if err != nil || doc.StatusCode != http.StatusOK || !doc.Authenticated || doc.Requests != 3 {
		t.Errorf("a stale nonce: %+v, %v", doc, err)
	}

	for want, header := range map[string]string{
		"its challenge is not Digest":                                                              `WWW-Authenticate: Basic realm="pbx"`,
		`its Digest challenge is of algorithm "AKAV1-MD5"`:                                         `WWW-Authenticate: Digest realm="pbx", nonce="n", algorithm=AKAv1-MD5`,
		`its Digest challenge is of algorithm "MD5", without a nonce, or of a qop other than auth`: `WWW-Authenticate: Digest realm="pbx", nonce="n", qop="auth-int"`,
	} {
		s = startSIP(t, func(_ int, request sipSeen) []string {
			return []string{sipReply(request, "401 Unauthorized", []string{header}, "")}
		})
		_, err = sipDoc(t, sipCollector(t, sipAccount(t, "s3cret")), s.addr, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "the server "+s.addr+" asks for credentials (401), and "+want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}

// Over TCP and over TLS a response is read as a message of the stream: its
// compact header names written out, a header continued on a next line as
// one value, and its body by its Content-Length, after the blank lines a
// server keeps the connection with. Over TLS the server is verified, the
// document names the connection's version, and the port is 5061's.
func TestASIPScrapeOverAStream(t *testing.T) {
	answer := func(_ int, request sipSeen) []string {
		body := "v=0\r\n"
		return []string{"\r\n\r\n" + strings.Join([]string{"SIP/2.0 200 Fine",
			"v: " + request.headers["via"], "f: " + request.headers["from"], "t: " + request.headers["to"], "i: " + request.headers["call-id"], "CSeq: " + request.headers["cseq"],
			"k: path", "Allow: INVITE,", "  OPTIONS", "c: application/sdp", "l: " + strconv.Itoa(len(body))}, "\r\n") + "\r\n\r\n" + body}
	}
	plain := &sipStandIn{answer: answer}
	stub := testutil.NewTCPStub(t, plain.serve)
	doc, err := sipDoc(t, sipCollector(t, func(_ *model.Collector, r *SIPConfig) { r.Transport = "tcp" }), stub.Addr, 5*time.Second)
	if err != nil || doc.StatusCode != http.StatusOK || doc.Reason != "Fine" || doc.Transport != "tcp" || doc.Attempts != 1 || doc.BodyBytes != 5 ||
		strings.Join(doc.Allow, "|") != "INVITE|OPTIONS" || strings.Join(doc.Supported, "|") != "path" || doc.Headers["content-type"][0] != "application/sdp" || doc.Headers["call-id"] == nil || doc.TLSVersion != "" {
		t.Fatalf("over tcp: %+v, %v", doc, err)
	}
	if via := plain.requests()[0].headers["via"]; !strings.HasPrefix(via, "SIP/2.0/TCP 127.0.0.1:") {
		t.Errorf("Via %q", via)
	}

	secure := &sipStandIn{answer: answer}
	tlsStub := testutil.NewTLSTCPStub(t, secure.serve)
	c := sipCollector(t, func(c *model.Collector, r *SIPConfig) { r.Transport, c.Request.TLS.CAFile = "tls", tlsStub.CAFile })
	doc, err = sipDoc(t, c, tlsStub.Addr, 5*time.Second)
	if err != nil || doc.StatusCode != http.StatusOK || doc.Transport != "tls" || !strings.HasPrefix(doc.TLSVersion, "TLS 1.") || doc.TLSCipherSuite == "" {
		t.Fatalf("over tls: %+v, %v", doc, err)
	}
	if via := secure.requests()[0].headers["via"]; !strings.HasPrefix(via, "SIP/2.0/TLS 127.0.0.1:") {
		t.Errorf("Via %q", via)
	}
	if label, err := RequestLabelFor("pbx.example.com", c, RequestOverrides{}); err != nil || label != "sips://pbx.example.com:5061" {
		t.Errorf("label %q, %v", label, err)
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	// Without the CA the server is not verified, and nothing is sent.
	unverified := sipCollector(t, func(_ *model.Collector, r *SIPConfig) { r.Transport = "tls" })
	if _, err := sipDoc(t, unverified, tlsStub.Addr, 5*time.Second); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+tlsStub.Addr) || len(secure.requests()) != 1 {
		t.Errorf("an unverified server: %v, %d requests", err, len(secure.requests()))
	}
}

// What a stream answers that is no response fails the scrape, saying what
// it is: something that is not SIP, a message without its Content-Length,
// a body past the response limit, and a connection closed without a word.
func TestASIPStreamThatIsNoResponseFailsTheScrape(t *testing.T) {
	for want, said := range map[string]string{
		`not a SIP response: it starts "HTTP/1.1 400 Bad Request"`:           "HTTP/1.1 400 Bad Request\r\n\r\n",
		"not a SIP response: it has no Content-Length":                       "SIP/2.0 200 OK\r\nCSeq: 1 OPTIONS\r\n\r\n",
		"its body is 2048 bytes, larger than the response limit of 1024 byt": "SIP/2.0 200 OK\r\nContent-Length: 2048\r\n\r\n" + strings.Repeat("x", 2048),
		"closed the connection before it answered":                           "",
	} {
		stub := testutil.NewTCPStub(t, func(conn net.Conn) {
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = conn.Write([]byte(said))
			_ = conn.Close()
		})
		c := sipCollector(t, func(c *model.Collector, r *SIPConfig) { r.Transport, c.Request.MaxResponseBytes = "tcp", 1024 })
		_, err := sipDoc(t, c, stub.Addr, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
		if strings.Contains(want, "limit") && !errors.Is(err, model.ErrLimitExceeded) {
			t.Errorf("a body past the limit is not the limit's error: %v", err)
		}
	}
}

// A target is the server, host or host:port, and the keys are checked when
// the configuration loads; a host the collector's lists refuse is not
// asked.
func TestASIPTargetAndKeysAreChecked(t *testing.T) {
	c := sipCollector(t, nil)
	for _, target := range []string{"", "sip:pbx.example.com", "sip://pbx.example.com", "pbx.example.com/path", "user@pbx.example.com", "pbx.example.com:0", "pbx.example.com:port"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("target %q was taken", target)
		}
	}
	if err := CheckTarget(c, "SIPS:1000@pbx.example.com", false); err == nil || !strings.Contains(err.Error(), "is a SIP URI; a sip target is the server, host or host:port, and the URI asked is request.request_uri") {
		t.Errorf("a URI for a target: %v", err)
	}
	for target, label := range map[string]string{"PBX.example.com": "sip://pbx.example.com:5060", "10.0.0.1:5080": "sip://10.0.0.1:5080", "[2001:db8::1]": "sip://[2001:db8::1]:5060"} {
		if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != label {
			t.Errorf("target %q: label %q, %v; want %q", target, got, err, label)
		}
	}
	for want, edit := range map[string]func(*model.Collector, *SIPConfig){
		"request.sip_transport is \"sctp\"":             func(_ *model.Collector, r *SIPConfig) { r.Transport = "sctp" },
		"sets request.tls with sip_transport: udp":      func(c *model.Collector, _ *SIPConfig) { c.Request.TLS.CAFile = "/ca.pem" },
		"sets only one of request.username and passwor": func(c *model.Collector, _ *SIPConfig) { c.Request.Username = "monitor" },
		"request.tls sets only one of cert_file":        func(c *model.Collector, r *SIPConfig) { r.Transport, c.Request.TLS.CertFile = "tls", "/c.pem" },
		"request.username or password_file has a place": func(c *model.Collector, _ *SIPConfig) {
			c.Request.Username, c.Request.PasswordFile = "{{param_user}}", "/p"
		},
		"request.tls.server_name":                     func(c *model.Collector, r *SIPConfig) { r.Transport, c.Request.TLS.ServerName = "tls", "bad name" },
		"request.username has a quote":                func(c *model.Collector, _ *SIPConfig) { c.Request.Username, c.Request.PasswordFile = `a"b`, "/p" },
		"request.user_agent is longer than 128 bytes": func(_ *model.Collector, r *SIPConfig) { r.UserAgent = "a\r\nVia: x" },
		"request.request_uri is \"pbx.example.com\"":  func(_ *model.Collector, r *SIPConfig) { r.RequestURI = "pbx.example.com" },
		"request.request_uri is \"sip:\"":             func(_ *model.Collector, r *SIPConfig) { r.RequestURI = "sip:" },
		"request.from_uri is \"sip:a b@example.com\"": func(_ *model.Collector, r *SIPConfig) { r.FromURI = "sip:a b@example.com" },
		"request.from_uri is \"sip:a>\\r\\nVia: x\"":  func(_ *model.Collector, r *SIPConfig) { r.FromURI = "sip:a>\r\nVia: x" },
		"request.from_uri is 259 bytes":               func(_ *model.Collector, r *SIPConfig) { r.FromURI = "sip:" + strings.Repeat("a", 255) },
		"which has a placeholder":                     func(_ *model.Collector, r *SIPConfig) { r.RequestURI = "sip:{{param_user}}@example.com" },
	} {
		own := SIPConfig{}
		c := &model.Collector{Name: "pbx", Request: model.RequestConfig{Type: RequestTypeSIP}}
		edit(c, &own)
		c.Request.Own = own
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}

	s := startSIP(t, nil)
	denied := sipCollector(t, func(c *model.Collector, _ *SIPConfig) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := sipDoc(t, denied, s.addr, 5*time.Second); !errors.Is(err, ErrTargetRefused) || len(s.requests()) != 0 {
		t.Errorf("a refused host: %v, %d requests", err, len(s.requests()))
	}
	// A file of the account that is not there is what --dry-run warns of.
	missing := sipCollector(t, func(c *model.Collector, _ *SIPConfig) {
		c.Request.Username, c.Request.PasswordFile = "monitor", filepath.Join(t.TempDir(), "none")
	})
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.password_file") {
		t.Errorf("warnings %v", warnings)
	}
}

// The keys the opt-in sip type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheSIPTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeSIP)
}
