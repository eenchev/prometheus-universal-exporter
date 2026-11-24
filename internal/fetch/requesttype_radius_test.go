//go:build request_type_radius

package fetch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// radiusStandIn is a RADIUS server of one client and one account: it reads
// each request as a server does, checking its Message-Authenticator and its
// password with the shared secret, and answers an Access-Accept to the
// right password, an Access-Reject to another, as its fields make it.
type radiusStandIn struct {
	addr             string
	secret, password string
	// drop is how many requests are not answered before one is; unsigned
	// leaves the Message-Authenticator out of the reply; replySecret signs
	// the reply with another secret; noise is sent before each reply.
	// badSignature spoils the reply's Message-Authenticator and leaves its
	// Response Authenticator right.
	// accounting answers a Status-Server as an accounting port does.
	accounting   bool
	drop         int
	unsigned     bool
	badSignature bool
	replySecret  string
	noise        []byte
	// attributes are added to an Access-Accept.
	attributes []byte

	mu sync.Mutex
	// packets is the requests as they came, and seen what each said: its
	// code, the user it named and whether it was signed well.
	packets [][]byte
	seen    []radiusSeen
}

type radiusSeen struct {
	code   int
	user   string
	nas    string
	signed bool
}

func startRADIUS(t *testing.T, s *radiusStandIn) *radiusStandIn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.addr = conn.LocalAddr().String()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := s.answer(bytes.Clone(buf[:n])); reply != nil {
				if s.noise != nil {
					_, _ = conn.WriteTo(s.noise, from)
				}
				_, _ = conn.WriteTo(reply, from)
			}
		}
	}()
	return s
}

func (s *radiusStandIn) answer(request []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var authenticator [16]byte
	copy(authenticator[:], request[4:20])
	seen := radiusSeen{code: int(request[0])}
	code := byte(radiusAccessReject)
	for at := 20; at < len(request); at += int(request[at+1]) {
		value := request[at+2 : at+int(request[at+1])]
		switch request[at] {
		case radiusUserName:
			seen.user = string(value)
		case radiusNASIdentifier:
			seen.nas = string(value)
		case radiusUserPassword:
			if bytes.Equal(value, radiusHidePassword(s.password, s.secret, authenticator)) {
				code = radiusAccessAccept
			}
		case radiusCHAPPassword:
			digest := md5.New()
			digest.Write(value[:1])
			digest.Write([]byte(s.password))
			digest.Write(authenticator[:])
			if bytes.Equal(value[1:], digest.Sum(nil)) {
				code = radiusAccessAccept
			}
		case radiusMessageAuthenticator:
			zeroed := bytes.Clone(request)
			copy(zeroed[at+2:], make([]byte, 16))
			mac := hmac.New(md5.New, []byte(s.secret))
			mac.Write(zeroed)
			seen.signed = hmac.Equal(mac.Sum(nil), value)
		}
	}
	if seen.code == radiusStatusServer {
		code = radiusAccessAccept
		if s.accounting {
			code = radiusAccountingResponse
		}
	}
	s.packets, s.seen = append(s.packets, request), append(s.seen, seen)
	if len(s.packets) <= s.drop {
		return nil
	}
	secret := s.secret
	if s.replySecret != "" {
		secret = s.replySecret
	}
	reply := []byte{code, request[1], 0, 0}
	reply = append(reply, authenticator[:]...)
	signature := -1
	if !s.unsigned {
		signature = len(reply) + 2
		reply = append(reply, radiusMessageAuthenticator, 18)
		reply = append(reply, make([]byte, 16)...)
	}
	if code == radiusAccessAccept {
		reply = append(reply, s.attributes...)
	} else {
		reply = append(reply, radiusReplyMessage, 2+13)
		reply = append(reply, "wrong account"...)
	}
	binary.BigEndian.PutUint16(reply[2:], uint16(len(reply)))
	if signature >= 0 {
		mac := hmac.New(md5.New, []byte(secret))
		mac.Write(reply)
		copy(reply[signature:], mac.Sum(nil))
		if s.badSignature {
			reply[signature] ^= 0xff
		}
	}
	digest := md5.New()
	digest.Write(reply)
	digest.Write([]byte(secret))
	copy(reply[4:20], digest.Sum(nil))
	return reply
}

func (s *radiusStandIn) saw() []radiusSeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]radiusSeen(nil), s.seen...)
}

// radiusCollector is a collector of the account monitor, whose password
// file holds password and whose secret file holds secret.
func radiusCollector(t *testing.T, secret, password string, edit func(*model.Collector, *RADIUSConfig)) *model.Collector {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	own := RADIUSConfig{SecretFile: write("secret", secret), ResendInterval: model.Duration(100 * time.Millisecond)}
	c := &model.Collector{Name: "login", Request: model.RequestConfig{Type: RequestTypeRADIUS, Username: "monitor", PasswordFile: write("password", password)}}
	if edit != nil {
		edit(c, &own)
	}
	c.Request.Own = own
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func radiusDoc(t *testing.T, c *model.Collector, target string, timeout time.Duration) (radiusDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return radiusDocument{}, err
	}
	var doc radiusDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v in %s", err, resp.Body)
	}
	return doc, nil
}

// A scrape sends an Access-Request the server takes: signed with the
// shared secret, naming the account and the sender, the password hidden as
// PAP or digested as CHAP. The right password is accepted, and what the
// reply carries is in the document; another is rejected, which is an answer
// and not a failure.
func TestARADIUSScrapeAuthenticatesTheAccount(t *testing.T) {
	for _, mode := range []string{radiusModePAP, radiusModeCHAP} {
		t.Run(mode, func(t *testing.T) {
			// A password longer than one block of PAP's.
			const password = "correct horse battery staple"
			s := startRADIUS(t, &radiusStandIn{secret: "shared", password: password, attributes: []byte{radiusSessionTimeout, 6, 0, 0, 14, 16, radiusReplyMessage, 9, 'w', 'e', 'l', 'c', 'o', 'm', 'e'}})
			c := radiusCollector(t, "shared", password, func(_ *model.Collector, r *RADIUSConfig) { r.Mode = mode })
			doc, err := radiusDoc(t, c, s.addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !doc.Accepted || doc.Code != 2 || doc.Reply != "access-accept" || doc.Mode != mode || doc.Attempts != 1 || !doc.MessageAuthenticator {
				t.Errorf("%+v", doc)
			}
			if doc.SessionTimeout == nil || *doc.SessionTimeout != 3600 || !reflect.DeepEqual(doc.ReplyMessages, []string{"welcome"}) || !reflect.DeepEqual(doc.AttributeTypes, []int{80, 27, 18}) {
				t.Errorf("%+v", doc)
			}
			if doc.Address != s.addr || doc.ResponseSeconds <= 0 || doc.ResponseSeconds > doc.DurationSeconds {
				t.Errorf("%+v", doc)
			}
			if saw := s.saw(); !reflect.DeepEqual(saw, []radiusSeen{{code: radiusAccessRequest, user: "monitor", nas: radiusDefaultNAS, signed: true}}) {
				t.Errorf("the server saw %+v", saw)
			}
			wrong := radiusCollector(t, "shared", "another", func(_ *model.Collector, r *RADIUSConfig) { r.Mode = mode })
			doc, err = radiusDoc(t, wrong, s.addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Accepted || doc.Code != 3 || doc.Reply != "access-reject" || !reflect.DeepEqual(doc.ReplyMessages, []string{"wrong account"}) || doc.SessionTimeout != nil {
				t.Errorf("%+v", doc)
			}
		})
	}
}

// mode: status-server asks whether the server is alive, with no account.
func TestARADIUSStatusServerScrapeSendsNoAccount(t *testing.T) {
	s := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p"})
	c := radiusCollector(t, "shared", "", func(c *model.Collector, r *RADIUSConfig) {
		r.Mode, r.NASIdentifier = radiusModeStatus, "probe-1"
		c.Request.Username, c.Request.PasswordFile = "", ""
	})
	doc, err := radiusDoc(t, c, s.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Accepted || doc.Mode != radiusModeStatus {
		t.Errorf("%+v", doc)
	}
	if saw := s.saw(); !reflect.DeepEqual(saw, []radiusSeen{{code: radiusStatusServer, nas: "probe-1", signed: true}}) {
		t.Errorf("the server saw %+v", saw)
	}
	// An accounting port answers one with an Accounting-Response.
	accounting := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", accounting: true})
	if doc, err := radiusDoc(t, c, accounting.addr, 5*time.Second); err != nil || !doc.Accepted || doc.Reply != "accounting-response" {
		t.Errorf("%+v, %v", doc, err)
	}
	if method := (radiusRequester{}).Method(c, RequestOverrides{}); method != "STATUS-SERVER" {
		t.Errorf("method %q", method)
	}
}

// A request no reply comes to is sent again, the same packet, retries
// times and no more; the document says how many times it was sent.
func TestARADIUSRequestIsSentAgainWhileNoReplyComes(t *testing.T) {
	s := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", drop: 2})
	c := radiusCollector(t, "shared", "p", nil)
	doc, err := radiusDoc(t, c, s.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Attempts != 3 || !doc.Accepted || doc.ResponseSeconds < 0.2 {
		t.Errorf("%+v", doc)
	}
	s.mu.Lock()
	same := len(s.packets) == 3 && bytes.Equal(s.packets[0], s.packets[1]) && bytes.Equal(s.packets[1], s.packets[2])
	s.mu.Unlock()
	if !same {
		t.Error("the request sent again is not the packet sent first")
	}
	quiet := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", drop: 100})
	once := radiusCollector(t, "shared", "p", func(_ *model.Collector, r *RADIUSConfig) { r.Resends = new(int) })
	_, err = radiusDoc(t, once, quiet.addr, 400*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the request sent once") {
		t.Errorf("err=%v", err)
	}
	if saw := quiet.saw(); len(saw) != 1 {
		t.Errorf("with resends: 0 the request was sent %d times", len(saw))
	}
}

// A datagram that is not the reply the shared secret makes is ignored, and
// the reply that follows is taken; when none does, the failure says what
// was ignored and why, as a secret that is not the server's reads. A reply
// without a Message-Authenticator is taken unless the collector requires
// one.
func TestARADIUSReplyIsVerified(t *testing.T) {
	noisy := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", noise: []byte("not radius at all, but long enough")})
	c := radiusCollector(t, "shared", "p", nil)
	if doc, err := radiusDoc(t, c, noisy.addr, 5*time.Second); err != nil || !doc.Accepted {
		t.Errorf("%+v, %v", doc, err)
	}
	forged := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", replySecret: "other"})
	_, err := radiusDoc(t, radiusCollector(t, "shared", "p", func(_ *model.Collector, r *RADIUSConfig) { r.Resends = new(int) }), forged.addr, 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "1 datagram was ignored, as its Response Authenticator is not what the shared secret makes") {
		t.Errorf("err=%v", err)
	}
	// Without a Message-Authenticator the Response Authenticator alone
	// tells a reply made with another secret.
	bare := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", replySecret: "other", unsigned: true})
	_, err = radiusDoc(t, radiusCollector(t, "shared", "p", func(_ *model.Collector, r *RADIUSConfig) { r.Resends = new(int) }), bare.addr, 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "as its Response Authenticator is not what the shared secret makes") {
		t.Errorf("err=%v", err)
	}
	spoiled := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", badSignature: true})
	_, err = radiusDoc(t, radiusCollector(t, "shared", "p", func(_ *model.Collector, r *RADIUSConfig) { r.Resends = new(int) }), spoiled.addr, 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "as its Message-Authenticator is not what the shared secret makes") {
		t.Errorf("err=%v", err)
	}
	unsigned := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p", unsigned: true})
	doc, err := radiusDoc(t, c, unsigned.addr, 5*time.Second)
	if err != nil || !doc.Accepted || doc.MessageAuthenticator {
		t.Errorf("%+v, %v", doc, err)
	}
	strict := radiusCollector(t, "shared", "p", func(_ *model.Collector, r *RADIUSConfig) { r.RequireMessageAuthenticator = true })
	if _, err := radiusDoc(t, strict, unsigned.addr, 5*time.Second); err == nil || !strings.Contains(err.Error(), "has no Message-Authenticator") {
		t.Errorf("err=%v", err)
	}
}

// PAP's hiding of a password is RFC 2865's: the example of its section 7.1.
func TestTheRADIUSPasswordIsHiddenAsTheRFCDoes(t *testing.T) {
	authenticator := [16]byte{0x0f, 0x40, 0x3f, 0x94, 0x73, 0x97, 0x80, 0x57, 0xbd, 0x83, 0xd5, 0xcb, 0x98, 0xf4, 0x22, 0x7a}
	want := []byte{0x0d, 0xbe, 0x70, 0x8d, 0x93, 0xd4, 0x13, 0xce, 0x31, 0x96, 0xe4, 0x3f, 0x78, 0x2a, 0x0a, 0xee}
	if got := radiusHidePassword("arctangent", "xyzzy5461", authenticator); !bytes.Equal(got, want) {
		t.Errorf("hidden as %x, want %x", got, want)
	}
}

// The server a request goes to is held to the target policy, a target is a
// host and its port alone, and the keys are checked when the configuration
// loads: the secret's file, the account the mode needs, and the bounds.
func TestARADIUSTargetAndKeysAreChecked(t *testing.T) {
	s := startRADIUS(t, &radiusStandIn{secret: "shared", password: "p"})
	denied := radiusCollector(t, "shared", "p", func(c *model.Collector, _ *RADIUSConfig) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := radiusDoc(t, denied, s.addr, time.Second); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("err=%v, want the target refused", err)
	}
	if saw := s.saw(); len(saw) != 0 {
		t.Errorf("a server the policy denies was sent %d requests", len(saw))
	}
	c := radiusCollector(t, "shared", "p", nil)
	for _, target := range []string{"radius://host", "user@host", "host/path", "host:0", "2001:db8::1", ""} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("target %q was taken", target)
		}
	}
	if label, err := (radiusRequester{}).Label(" Radius.Example.COM ", c, RequestOverrides{}); err != nil || label != "radius://radius.example.com:1812" {
		t.Errorf("label %q, %v", label, err)
	}
	minus, many := -1, 6
	for want, edit := range map[string]func(*model.Collector, *RADIUSConfig){
		`request.mode is "eap"`:                    func(_ *model.Collector, r *RADIUSConfig) { r.Mode = "eap" },
		"has no request.secret_file":               func(_ *model.Collector, r *RADIUSConfig) { r.SecretFile = "" },
		"has no request.username":                  func(c *model.Collector, _ *RADIUSConfig) { c.Request.Username = "" },
		"has no request.password_file":             func(c *model.Collector, _ *RADIUSConfig) { c.Request.PasswordFile = "" },
		"with mode: status-server":                 func(_ *model.Collector, r *RADIUSConfig) { r.Mode = radiusModeStatus },
		"request.resends is -1":                    func(_ *model.Collector, r *RADIUSConfig) { r.Resends = &minus },
		"request.resends is 6":                     func(_ *model.Collector, r *RADIUSConfig) { r.Resends = &many },
		"request.resend_interval is 50ms":          func(_ *model.Collector, r *RADIUSConfig) { r.ResendInterval = model.Duration(50 * time.Millisecond) },
		"request.nas_identifier is longer":         func(_ *model.Collector, r *RADIUSConfig) { r.NASIdentifier = strings.Repeat("n", 254) },
		"has a placeholder; the account and the s": func(c *model.Collector, _ *RADIUSConfig) { c.Request.Username = "{{user}}" },
	} {
		c := &model.Collector{Name: "login", Request: model.RequestConfig{Type: RequestTypeRADIUS, Username: "monitor", PasswordFile: "/p"}}
		own := RADIUSConfig{SecretFile: "/s"}
		edit(c, &own)
		c.Request.Own = own
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	empty := radiusCollector(t, "", "p", nil)
	if _, err := radiusDoc(t, empty, s.addr, time.Second); err == nil || !strings.Contains(err.Error(), "request.secret_file") || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("err=%v", err)
	}
}

// The keys the opt-in radius type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheRADIUSTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeRADIUS)
}
