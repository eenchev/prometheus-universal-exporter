//go:build request_type_pop3

package fetch

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // G501: the digest APOP is
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// pop3Stub is a POP3 server: it greets, answers CAPA, begins TLS for STLS,
// takes each login a pop3 collector speaks and checks it as a server does,
// and answers STAT once logged in.
type pop3Stub struct {
	*testutil.TCPStub
	mu    sync.Mutex
	lines []string
	// greeting is its greeting, whose <...> is the APOP timestamp; capa
	// are the lines of its CAPA beside STLS, nil for a server that knows
	// no CAPA; stls is whether it offers STLS before TLS.
	greeting   string
	capa       []string
	stls       bool
	user, pass string
	token      string
	stat       string
	// list are the lines of its LIST, nil for a server that refuses it.
	list []string
	// refusal is what it refuses a login with, its own words when "".
	refusal string
	// oneLine is a server that takes XOAUTH2 only with the credential in
	// the AUTH command.
	oneLine bool
}

const pop3StubTimestamp = "<1896.697170952@mail.test>"

func (s *pop3Stub) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *pop3Stub) serve(conn net.Conn, upgrade func() (net.Conn, error)) {
	reader := bufio.NewReader(conn)
	say := func(text string) { _, _ = fmt.Fprint(conn, text+"\r\n") }
	read := func() (string, bool) {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if err == nil {
			s.mu.Lock()
			s.lines = append(s.lines, line)
			s.mu.Unlock()
		}
		return line, err == nil
	}
	b64 := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	secure, in, named := false, false, ""
	login := func(accepted bool) {
		if in = accepted; accepted {
			say("+OK Welcome.")
		} else if s.refusal != "" {
			say(s.refusal)
		} else {
			say("-ERR [AUTH] Username and password not accepted.")
		}
	}
	say(s.greeting)
	for {
		line, ok := read()
		if !ok {
			return
		}
		verb, args, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "CAPA":
			if s.capa == nil {
				say("-ERR Unknown command")
				break
			}
			say("+OK Capability list follows")
			for _, capability := range s.capa {
				say(capability)
			}
			if s.stls && !secure {
				say("STLS")
			}
			say(".")
		case "STLS":
			say("+OK Begin TLS negotiation")
			upgraded, err := upgrade()
			if err != nil {
				return
			}
			conn, reader, secure = upgraded, bufio.NewReader(upgraded), true
		case "USER":
			named = args
			say("+OK send PASS")
		case "PASS":
			login(named == s.user && args == s.pass)
		case "APOP":
			digest := md5.Sum([]byte(pop3StubTimestamp + s.pass)) //nolint:gosec // G401: APOP's digest
			login(args == s.user+" "+hex.EncodeToString(digest[:]))
		case "AUTH":
			switch args {
			case "PLAIN":
				say("+ ")
				answer, _ := read()
				login(answer == b64("\x00"+s.user+"\x00"+s.pass))
			case "LOGIN":
				say("+ " + b64("Username:"))
				user, _ := read()
				say("+ " + b64("Password:"))
				pass, _ := read()
				login(user == b64(s.user) && pass == b64(s.pass))
			case "CRAM-MD5":
				challenge := "<17.42@mail.test>"
				say("+ " + b64(challenge))
				answer, _ := read()
				login(answer == netTestCRAMMD5(s.user, s.pass, challenge))
			case "OAUTHBEARER":
				say("+ ")
				answer, _ := read()
				host, port, _ := net.SplitHostPort(s.Addr)
				accepted := answer == b64("n,a="+s.user+",\x01host="+host+"\x01port="+port+"\x01auth=Bearer "+s.token+"\x01\x01")
				if !accepted {
					// The error, which the client answers with one byte.
					say("+ " + b64(`{"status":"invalid_token"}`))
					_, _ = read()
				}
				login(accepted)
			case "XOAUTH2":
				if s.oneLine {
					say("-ERR Unrecognized authentication type")
					break
				}
				say("+ ")
				answer, _ := read()
				accepted := answer == b64("user="+s.user+"\x01auth=Bearer "+s.token+"\x01\x01")
				if !accepted {
					// The error, which the client answers with an empty line.
					say("+ " + b64(`{"status":"400","schemes":"Bearer"}`))
					_, _ = read()
				}
				login(accepted)
			default:
				// A server that takes XOAUTH2 with its credential in the
				// command and not after its name alone, as oneLine says.
				if credential, given := strings.CutPrefix(args, "XOAUTH2 "); given && s.oneLine {
					login(credential == b64("user="+s.user+"\x01auth=Bearer "+s.token+"\x01\x01"))
					break
				}
				// A mechanism given with its credential in the command, or
				// one the server does not speak.
				say("-ERR Unrecognized authentication type")
			}
		case "STAT":
			if in {
				say(s.stat)
			} else {
				say("-ERR not logged in")
			}
		case "LIST":
			if !in || s.list == nil {
				say("-ERR no list for you")
				break
			}
			say(fmt.Sprintf("+OK %d messages", len(s.list)))
			for _, line := range s.list {
				say(line)
			}
			say(".")
		case "QUIT":
			say("+OK bye")
			return
		default:
			say("-ERR Unknown command")
		}
	}
}

// newPOP3Stub starts the server, as configure changed it before it took a
// connection.
func newPOP3Stub(t *testing.T, configure ...func(*pop3Stub)) *pop3Stub {
	t.Helper()
	s := &pop3Stub{greeting: "+OK POP3 mail.test ready " + pop3StubTimestamp, stls: true, user: "me@example.com", pass: "app pass word", token: "ya29.token", stat: "+OK 12 123456",
		capa: []string{"USER", "TOP", "UIDL", "Sasl plain LOGIN CRAM-MD5 XOAUTH2 OAUTHBEARER", "EXPIRE 31 USER", "IMPLEMENTATION stub 1.0"}}
	for _, fn := range configure {
		fn(s)
	}
	s.TCPStub = testutil.NewStartTLSStub(t, s.serve)
	return s
}

// pop3Collector is a validated pop3 collector of the stub, changed by
// change first, which verifies the stub's certificate.
func pop3Collector(t *testing.T, s *pop3Stub, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypePOP3, Own: POP3Config{}}}
	if s != nil {
		c.Request.TLS = model.TLSConfig{CAFile: s.CAFile}
	}
	for _, fn := range change {
		fn(&c.Request)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func pop3Doc(t *testing.T, c *model.Collector, target string) pop3Document {
	t.Helper()
	resp, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc pop3Document
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v: %s", err, resp.Body)
	}
	return doc
}

func pop3Password(t *testing.T) string {
	t.Helper()
	return testutil.WriteFile(t, "password", "app pass word\n")
}

// Without an account a scrape reads the greeting, asks CAPA, begins TLS
// where STLS is offered and asks CAPA again within it, then quits: the
// document holds the greeting's text, that the session is over TLS by
// STLS, the capabilities by their first words, in upper case and sorted,
// the SASL mechanisms, whether the greeting has the timestamp APOP needs,
// and nothing of a mailbox.
func TestAPOP3ScrapeReadsTheGreetingAndTheCapabilities(t *testing.T) {
	s := newPOP3Stub(t)
	resp, err := FetchCollector(context.Background(), s.Addr, pop3Collector(t, s), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"greeting":"POP3 mail.test ready \u003c1896.697170952@mail.test\u003e","tls":true,"starttls":true,` +
		`"capabilities":["EXPIRE","IMPLEMENTATION","SASL","TOP","UIDL","USER"],"sasl":["CRAM-MD5","LOGIN","OAUTHBEARER","PLAIN","XOAUTH2"],"apop":true,` +
		`"authenticated":false,"auth":null,"messages":null,"size_bytes":null,"largest_bytes":null}`
	if string(resp.Body) != want || resp.Trip == nil || resp.Trip.TLSConnection == nil {
		t.Errorf("the document is\n%s\nwant\n%s\ntrip %+v", resp.Body, want, resp.Trip)
	}
	if sent := strings.Join(s.sent(), "|"); sent != "CAPA|STLS|CAPA|QUIT" {
		t.Errorf("sent %s", sent)
	}
	// A server older than CAPA names nothing, and is one all the same.
	old := newPOP3Stub(t, func(s *pop3Stub) { s.capa, s.greeting = nil, "+OK ready" })
	if doc := pop3Doc(t, pop3Collector(t, old), old.Addr); doc.TLS || doc.APOP || len(doc.Capabilities) != 0 || len(doc.SASL) != 0 || doc.Greeting != "ready" {
		t.Errorf("a server without CAPA: %+v", doc)
	}
}

// A scrape with an account logs in by the mechanism request.auth names,
// each as its RFC has it, and asks STAT: USER and PASS, APOP with the
// digest of the greeting's timestamp and the password, and AUTH with
// PLAIN, LOGIN, CRAM-MD5 and XOAUTH2, whose credential is sent in answer to
// the server's challenge and never in the command. The document says the
// mechanism used, the messages and their size. Nothing is listed, fetched
// or deleted.
func TestAPOP3ScrapeLogsInWithEachMechanismAndCountsTheMailbox(t *testing.T) {
	password := pop3Password(t)
	token := testutil.WriteFile(t, "token", "ya29.token\n")
	b64 := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	digest := md5.Sum([]byte(pop3StubTimestamp + "app pass word")) //nolint:gosec // G401: APOP's digest
	for auth, tc := range map[string]struct {
		change func(*model.RequestConfig)
		sent   string
	}{
		"user":        {withLogin("user", nil, withAccount("me@example.com", password)), "USER me@example.com|PASS app pass word|STAT"},
		"apop":        {withLogin("apop", nil, withAccount("me@example.com", password)), "APOP me@example.com " + hex.EncodeToString(digest[:]) + "|STAT"},
		"plain":       {withLogin("plain", nil, withAccount("me@example.com", password)), "AUTH PLAIN|" + b64("\x00me@example.com\x00app pass word") + "|STAT"},
		"login":       {withLogin("login", nil, withAccount("me@example.com", password)), "AUTH LOGIN|" + b64("me@example.com") + "|" + b64("app pass word") + "|STAT"},
		"cram-md5":    {withLogin("cram-md5", nil, withAccount("me@example.com", password)), "AUTH CRAM-MD5|"},
		"xoauth2":     {withLogin("", &model.XOAuth2Config{TokenFile: token}, withAccount("me@example.com", "")), "AUTH XOAUTH2|" + b64("user=me@example.com\x01auth=Bearer ya29.token\x01\x01") + "|STAT"},
		"oauthbearer": {withLogin("oauthbearer", &model.XOAuth2Config{TokenFile: token}, withAccount("me@example.com", "")), "AUTH OAUTHBEARER|bixh"},
	} {
		s := newPOP3Stub(t)
		doc := pop3Doc(t, pop3Collector(t, s, tc.change), s.Addr)
		sent := strings.Join(s.sent(), "|")
		if !doc.Authenticated || doc.Auth == nil || *doc.Auth != auth || doc.Messages == nil || *doc.Messages != 12 || doc.SizeBytes == nil || *doc.SizeBytes != 123456 {
			t.Errorf("%s: %+v, sent %s", auth, doc, sent)
		}
		if !strings.Contains(sent, "CAPA|STLS|CAPA|"+tc.sent) || !strings.HasSuffix(sent, "|STAT|QUIT") {
			t.Errorf("%s: sent %s, want %s", auth, sent, tc.sent)
		}
		for _, line := range s.sent() {
			switch verb, _, _ := strings.Cut(line, " "); verb {
			case "LIST", "UIDL", "RETR", "TOP", "DELE", "RSET", "NOOP":
				t.Errorf("%s: sent %q, which a scrape never does", auth, line)
			}
		}
	}
}

// Without request.auth the login is the one the server takes: USER and
// PASS where it names USER, and where it answers no CAPA; AUTH PLAIN where
// it names that and no USER; AUTH LOGIN where it names that alone.
func TestAPOP3ScrapeChoosesTheLoginTheServerTakes(t *testing.T) {
	password := pop3Password(t)
	for _, tc := range []struct {
		want string
		capa []string
	}{
		{"user", []string{"USER", "SASL PLAIN LOGIN"}},
		{"plain", []string{"TOP", "SASL LOGIN PLAIN"}},
		{"login", []string{"SASL LOGIN"}},
		// A server without CAPA has no STLS to name either.
		{"user", nil},
	} {
		s := newPOP3Stub(t, func(s *pop3Stub) { s.capa, s.stls = tc.capa, tc.capa != nil })
		c := pop3Collector(t, s, withAccount("me@example.com", password), func(r *model.RequestConfig) { r.AllowPlaintext = tc.capa == nil })
		if doc := pop3Doc(t, c, s.Addr); doc.Auth == nil || *doc.Auth != tc.want {
			t.Errorf("a server with %v logged in by %v, want %s; sent %v", tc.capa, doc.Auth, tc.want, s.sent())
		}
	}
}

// A refused login fails the scrape, saying the command, the account and
// what the server answered, and never the password, the digest's source or
// the token, which the lines traced for a debug report do not hold
// either; whoever called the probe is not told the server's words, and the
// session does not go on to STAT. A refused XOAUTH2 is ended with the
// empty line its error asks for.
// XOAUTH2 is sent as Microsoft 365 takes it, the credential in answer to
// the server's +. A server that refuses the mechanism's name alone, having
// seen no credential, is asked once more with the credential in the
// command, as Google writes Gmail's exchange: the login is made, the token
// is in no trace, and a token such a server refuses fails as any does.
func TestAPOP3ServerThatTakesXOAUTH2InOneLineIsLoggedInTo(t *testing.T) {
	token := testutil.WriteFile(t, "token", "ya29.fresh-token\n")
	change := withLogin("xoauth2", &model.XOAuth2Config{TokenFile: token}, withAccount("me@example.com", ""))
	b64 := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	credential := b64("user=me@example.com\x01auth=Bearer ya29.fresh-token\x01\x01")
	s := newPOP3Stub(t, func(s *pop3Stub) { s.oneLine, s.token = true, "ya29.fresh-token" })
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, s.Addr, pop3Collector(t, s, change), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	sent := strings.Join(s.sent(), "|")
	if !strings.Contains(sent, "|AUTH XOAUTH2|AUTH XOAUTH2 "+credential+"|STAT") {
		t.Errorf("sent %s", sent)
	}
	if shown := fmt.Sprint(trace.Requests()); strings.Contains(shown, credential) || strings.Contains(shown, "ya29") || !strings.Contains(shown, "AUTH XOAUTH2 *** (the credential)") {
		t.Errorf("the trace shows %s", shown)
	}
	refusing := newPOP3Stub(t, func(s *pop3Stub) { s.oneLine, s.token = true, "another" })
	_, err := FetchCollector(context.Background(), refusing.Addr, pop3Collector(t, refusing, change), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "refused AUTH XOAUTH2 for me@example.com with -ERR [AUTH] Username and password not accepted.") || strings.Contains(err.Error(), "ya29") {
		t.Errorf("a refused token: err=%v", err)
	}
	if sent := strings.Join(refusing.sent(), "|"); strings.Count(sent, "AUTH XOAUTH2") != 2 {
		t.Errorf("a refused token: sent %s", sent)
	}
}

// A server that says back the command it refuses, password and all, has
// that masked in the failure and in the trace, as any server's text is.
func TestAPOP3RefusalThatSaysThePasswordBackIsMasked(t *testing.T) {
	wrong := testutil.WriteFile(t, "password", "hunter2-wrong\n")
	s := newPOP3Stub(t, func(s *pop3Stub) { s.refusal = "-ERR [AUTH] no login with password=hunter2-wrong for me@example.com" })
	ctx, trace := WithRequestTrace(context.Background())
	_, err := FetchCollector(ctx, s.Addr, pop3Collector(t, s, withLogin("user", nil, withAccount("me@example.com", wrong))), RequestOverrides{}, nil)
	shown := fmt.Sprint(err, trace.Requests())
	if err == nil || strings.Contains(shown, "hunter2") || !strings.Contains(err.Error(), "refused PASS for me@example.com with -ERR [AUTH] no login with password="+Redacted) {
		t.Fatalf("err=%v\ntrace %v", err, trace.Requests())
	}
}

func TestAPOP3ScrapeFailsOnARefusedLoginWithoutItsSecrets(t *testing.T) {
	wrong := testutil.WriteFile(t, "password", "hunter2-wrong\n")
	stale := testutil.WriteFile(t, "stale", "ya29.stale-token\n")
	b64 := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	for how, change := range map[string]func(*model.RequestConfig){
		"PASS":             withLogin("user", nil, withAccount("me@example.com", wrong)),
		"APOP":             withLogin("apop", nil, withAccount("me@example.com", wrong)),
		"AUTH PLAIN":       withLogin("plain", nil, withAccount("me@example.com", wrong)),
		"AUTH LOGIN":       withLogin("login", nil, withAccount("me@example.com", wrong)),
		"AUTH CRAM-MD5":    withLogin("cram-md5", nil, withAccount("me@example.com", wrong)),
		"AUTH XOAUTH2":     withLogin("xoauth2", &model.XOAuth2Config{TokenFile: stale}, withAccount("me@example.com", "")),
		"AUTH OAUTHBEARER": withLogin("oauthbearer", &model.XOAuth2Config{TokenFile: stale}, withAccount("me@example.com", "")),
	} {
		s := newPOP3Stub(t)
		ctx, trace := WithRequestTrace(context.Background())
		_, err := FetchCollector(ctx, s.Addr, pop3Collector(t, s, change), RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), "refused "+how+" for me@example.com with -ERR [AUTH] Username and password not accepted.") {
			t.Fatalf("%s: err=%v, want the refusal", how, err)
		}
		if told := model.CallerText(err); strings.Contains(told, "not accepted") || !strings.Contains(told, "refused "+how+" for me@example.com with -ERR "+model.WithheldMark) {
			t.Errorf("%s: a probe's caller is told %q", how, told)
		}
		shown := fmt.Sprint(err, trace.Requests())
		for _, secret := range []string{"hunter2", "ya29.stale", b64("hunter2-wrong"), b64("\x00me@example.com\x00hunter2-wrong"), b64("user=me@example.com\x01auth=Bearer ya29.stale-token\x01\x01")} {
			if strings.Contains(shown, secret) {
				t.Errorf("%s: the error or the trace holds %s: %s", how, secret, shown)
			}
		}
		if !strings.Contains(shown, "***") {
			t.Errorf("%s: the trace does not show the credential masked: %s", how, shown)
		}
		sent := s.sent()
		if strings.Contains(strings.Join(sent, "|"), "STAT") {
			t.Errorf("%s: the session went on after the refusal: %v", how, sent)
		}
		if how == "AUTH XOAUTH2" && sent[len(sent)-1] != "" {
			t.Errorf("a refused XOAUTH2 was not ended with an empty line: %q", sent)
		}
		if how == "AUTH OAUTHBEARER" && sent[len(sent)-1] != "AQ==" {
			t.Errorf("a refused OAUTHBEARER was not ended with its one byte: %q", sent)
		}
	}
}

// A mechanism the server does not name fails the scrape before anything of
// the account is sent, saying what it does name; APOP fails so where the
// greeting has no timestamp; and a name or a password with a line ending
// in it, which would be a second command, is sent nowhere.
func TestAPOP3ScrapeSendsNoCredentialTheServerCannotTake(t *testing.T) {
	password := pop3Password(t)
	for name, tc := range map[string]struct {
		configure func(*pop3Stub)
		change    func(*model.RequestConfig)
		want      string
	}{
		"a mechanism not offered":       {func(s *pop3Stub) { s.capa = []string{"USER", "SASL PLAIN"} }, withLogin("cram-md5", nil, withAccount("me@example.com", password)), "does not offer the SASL mechanism CRAM-MD5, which request.auth asks for; it offers PLAIN"},
		"no SASL at all":                {func(s *pop3Stub) { s.capa = []string{"USER"} }, withLogin("plain", nil, withAccount("me@example.com", password)), "does not offer the SASL mechanism PLAIN, which request.auth asks for; it offers none"},
		"APOP without a timestamp":      {func(s *pop3Stub) { s.greeting = "+OK ready" }, withLogin("apop", nil, withAccount("me@example.com", password)), "holds no timestamp, so the server takes no APOP"},
		"a line ending in the password": {nil, withAccount("me@example.com", testutil.WriteFile(t, "odd", "a\rDELE 1")), "holds a control character, which no POP3 login carries"},
	} {
		s := newPOP3Stub(t, func(s *pop3Stub) {
			if tc.configure != nil {
				tc.configure(s)
			}
		})
		_, err := FetchCollector(context.Background(), s.Addr, pop3Collector(t, s, tc.change), RequestOverrides{}, nil)
		sent := strings.Join(s.sent(), "|")
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(sent, "AUTH") || strings.Contains(sent, "USER") || strings.Contains(sent, "APOP") || strings.Contains(sent, "DELE") {
			t.Errorf("%s: err=%v, sent %s", name, err, sent)
		}
	}
}

// A credential is not sent over a connection that is not TLS: a server
// that offers no STLS fails the scrape before the login, unless
// allow_plaintext says it has none. A pop3s:// target is TLS from the
// first byte, and its connection is there for $tls. A greeting that is no
// +OK and a refused STAT fail the scrape saying so, the server's words
// kept from the probe's caller.
func TestAPOP3ScrapeHoldsTheSessionToTLS(t *testing.T) {
	password := pop3Password(t)
	s := newPOP3Stub(t, func(s *pop3Stub) { s.stls = false })
	_, err := FetchCollector(context.Background(), s.Addr, pop3Collector(t, s, withAccount("me@example.com", password)), RequestOverrides{}, nil)
	if sent := strings.Join(s.sent(), "|"); err == nil || !strings.Contains(err.Error(), "the account's credential is not sent over a connection that is not TLS") || sent != "CAPA" {
		t.Errorf("err=%v, sent %v; want no login in plaintext", err, sent)
	}
	if doc := pop3Doc(t, pop3Collector(t, s, withAccount("me@example.com", password), func(r *model.RequestConfig) { r.AllowPlaintext = true }), s.Addr); !doc.Authenticated || doc.TLS || doc.Messages == nil {
		t.Errorf("allow_plaintext: %+v", doc)
	}

	implicit := newPOP3Stub(t, func(s *pop3Stub) { s.stls = false })
	implicit.TCPStub = testutil.NewTLSTCPStub(t, func(conn net.Conn) { implicit.serve(conn, nil) })
	resp, err := FetchCollector(context.Background(), "pop3s://"+implicit.Addr, pop3Collector(t, implicit, withAccount("me@example.com", password)), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc pop3Document
	if err := json.Unmarshal(resp.Body, &doc); err != nil || !doc.TLS || doc.StartTLS || !doc.Authenticated || resp.Trip == nil || resp.Trip.TLSConnection == nil {
		t.Errorf("pop3s: %v: %s, trip %+v", err, resp.Body, resp.Trip)
	}

	busy := newPOP3Stub(t, func(s *pop3Stub) { s.greeting = "-ERR [IN-USE] no service for you" })
	_, err = FetchCollector(context.Background(), busy.Addr, pop3Collector(t, busy), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), `greeted with "-ERR [IN-USE] no service for you", not +OK`) || strings.Contains(model.CallerText(err), "no service") {
		t.Errorf("a greeting that is no +OK: err=%v, the caller is told %q", err, model.CallerText(err))
	}
	for stat, want := range map[string]string{
		"-ERR [IN-USE] maildrop locked": "answered STAT with -ERR [IN-USE] maildrop locked",
		"+OK many":                      `answered STAT with "many", which is no count of messages and their size`,
		"+OK -1 5":                      `which is no count of messages and their size`,
	} {
		locked := newPOP3Stub(t, func(s *pop3Stub) { s.stat = stat })
		_, err = FetchCollector(context.Background(), locked.Addr, pop3Collector(t, locked, withAccount("me@example.com", password)), RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(model.CallerText(err), "maildrop") {
			t.Errorf("STAT answered %q: err=%v, the caller is told %q", stat, err, model.CallerText(err))
		}
	}
}

// What a pop3 collector's keys must be to one another: a mechanism it
// speaks, a password for each but xoauth2, which has its block and none,
// a name for any login, a name without a blank for APOP, and no
// placeholder in the account.
func TestPOP3ConfigurationRules(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"a mechanism it does not speak": {withLogin("ntlm", nil, withAccount("me", "/password")), `request.auth "ntlm" is not user, apop, plain, login, cram-md5, xoauth2 or oauthbearer`},
		"apop without a password":       {withLogin("apop", nil, withAccount("me", "")), "sets request.username without request.password_file"},
		"apop without an account":       {withLogin("apop", nil, nil), "logs in without request.username"},
		"a blank in an APOP name":       {withLogin("apop", nil, withAccount("first last", "/password")), "request.username holds a blank, which the name of an APOP command may not"},
		"xoauth2 beside a password":     {withLogin("", &model.XOAuth2Config{TokenFile: "/token"}, withAccount("me", "/password")), "sets request.password_file beside xoauth2"},
		"cram-md5 beside the block":     {withLogin("cram-md5", &model.XOAuth2Config{TokenFile: "/token"}, withAccount("me", "")), "sets request.xoauth2, but request.auth is cram-md5"},
		"a placeholder in the account":  {withAccount("{{param_user}}", "/password"), "has a placeholder; the account is used as it is written"},
		"a password without an account": {withAccount("", "/password"), "request.username"},
	} {
		c := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypePOP3, Own: POP3Config{}}}
		tc.change(&c.Request)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	list := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypePOP3, Own: POP3Config{List: true}}}
	if err := ValidateRequest(list); err == nil || !strings.Contains(err.Error(), "sets request.list without an account") {
		t.Errorf("list without an account: %v", err)
	}
	for _, auth := range []string{"", "user", "apop", "plain", "login", "cram-md5"} {
		c := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypePOP3, Own: POP3Config{}}}
		withLogin(auth, nil, withAccount("me@example.com", "/password"))(&c.Request)
		if err := ValidateRequest(c); err != nil {
			t.Errorf("auth %q: %v", auth, err)
		}
	}
}

// With request.list a scrape also asks LIST and gives the size of the
// largest message, of a line for each message read to the dot; a mailbox
// without a message has none, and a collector that does not ask sends no
// LIST. A LIST refused, and a line that is no number and size, fail the
// scrape, the server's words kept from the probe's caller.
func TestAPOP3ScrapeListsTheMailboxForItsLargestMessage(t *testing.T) {
	account := withAccount("me@example.com", pop3Password(t))
	listing := func(r *model.RequestConfig) { r.Own = POP3Config{List: true} }
	s := newPOP3Stub(t, func(s *pop3Stub) { s.list = []string{"1 120", "2 98765 extra", "3 4000"} })
	doc := pop3Doc(t, pop3Collector(t, s, account, listing), s.Addr)
	if doc.LargestBytes == nil || *doc.LargestBytes != 98765 || *doc.Messages != 12 {
		t.Errorf("the largest message: %+v", doc)
	}
	if sent := strings.Join(s.sent(), "|"); !strings.HasSuffix(sent, "|STAT|LIST|QUIT") {
		t.Errorf("sent %s", sent)
	}
	if doc := pop3Doc(t, pop3Collector(t, s, account), s.Addr); doc.LargestBytes != nil || strings.Contains(strings.Join(s.sent()[8:], "|"), "LIST") {
		t.Errorf("a collector that does not ask: %+v, sent %v", doc, s.sent())
	}
	empty := newPOP3Stub(t, func(s *pop3Stub) { s.list, s.stat = []string{}, "+OK 0 0" })
	if doc := pop3Doc(t, pop3Collector(t, empty, account, listing), empty.Addr); doc.LargestBytes != nil || *doc.Messages != 0 {
		t.Errorf("an empty mailbox: %+v", doc)
	}
	for name, tc := range map[string]struct {
		list []string
		want string
	}{
		"refused":        {nil, "answered LIST with -ERR no list for you"},
		"no size":        {[]string{"1 120", "garbage here"}, `answered LIST with the line "garbage here", which is no message's number and size`},
		"a negative one": {[]string{"1 -5"}, "which is no message's number and size"},
	} {
		bad := newPOP3Stub(t, func(s *pop3Stub) { s.list = tc.list })
		_, err := FetchCollector(context.Background(), bad.Addr, pop3Collector(t, bad, account, listing), RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(model.CallerText(err), "garbage") || strings.Contains(model.CallerText(err), "no list for you") {
			t.Errorf("%s: err=%v, the caller is told %q", name, err, model.CallerText(err))
		}
	}
	// The lines count against the response limit, as every answer does.
	var many []string
	for i := 1; i <= 2000; i++ {
		many = append(many, fmt.Sprintf("%d 1000", i))
	}
	large := newPOP3Stub(t, func(s *pop3Stub) { s.list = many })
	_, err := FetchCollector(context.Background(), large.Addr, pop3Collector(t, large, account, listing, func(r *model.RequestConfig) { r.MaxResponseBytes = 4096 }), RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("a list past the response limit: err=%v", err)
	}
}

// A target is host, host:port, pop3://host:port or pop3s://host:port, port
// 110, and 995 for pop3s://, and is labelled with its scheme; a path, a
// user and another scheme are refused.
func TestAPOP3TargetIsAHostWithItsScheme(t *testing.T) {
	requester := pop3Requester{}
	for target, want := range map[string]string{
		"mail.example.com":           "pop3://mail.example.com:110",
		"mail.example.com:1110":      "pop3://mail.example.com:1110",
		"pop3://mail.example.com":    "pop3://mail.example.com:110",
		"pop3s://mail.example.com":   "pop3s://mail.example.com:995",
		"pop3s://[2001:db8::1]:9995": "pop3s://[2001:db8::1]:9995",
	} {
		if got, err := requester.Label(target, nil, RequestOverrides{}); err != nil || got != want {
			t.Errorf("the label of %s is %q, %v; want %s", target, got, err, want)
		}
	}
	for _, target := range []string{"imap://mail.example.com", "pop3://me@mail.example.com", "pop3://mail.example.com/inbox", ""} {
		if err := requester.CheckTarget(nil, target, false); err == nil {
			t.Errorf("the target %q was taken", target)
		}
	}
}

// What is no POP3 server fails the scrape at once and within bounds: a
// line without end, an answer that is neither +OK nor -ERR, a list of
// capabilities without end, and responses past the response limit, which
// is a limit exceeded.
func TestAPOP3ScrapeGivesUpOnWhatIsNoPOP3(t *testing.T) {
	endless := testutil.NewTCPStub(t, func(conn net.Conn) {
		chunk := []byte(strings.Repeat("+", 4096))
		for {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	_, err := FetchCollector(ctx, endless.Addr, pop3Collector(t, nil), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "a line of the response is longer than 4096 bytes") || time.Since(start) > 30*time.Second {
		t.Errorf("a line without end: err=%v after %s", err, time.Since(start))
	}
	answering := func(answer string) *testutil.TCPStub {
		return testutil.NewTCPStub(t, func(conn net.Conn) {
			_, _ = fmt.Fprint(conn, "+OK ready\r\n")
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = fmt.Fprint(conn, answer)
			_, _ = bufio.NewReader(conn).ReadString('\n')
		})
	}
	for name, tc := range map[string]struct{ answer, want string }{
		"an HTTP server":           {"HTTP/1.1 400 Bad Request\r\n\r\n", `the server answered "HTTP/1.1 400 Bad Request", which is no POP3 response; is the target a POP3 server?`},
		"capabilities without end": {"+OK\r\n" + strings.Repeat("X-CAPABILITY\r\n", 300), "the answer to CAPA has more than 200 lines"},
	} {
		stub := answering(tc.answer)
		if _, err := FetchCollector(ctx, stub.Addr, pop3Collector(t, nil), RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	large := answering("+OK\r\n" + strings.Repeat("X-"+strings.Repeat("A", 90)+"\r\n", 40) + ".\r\n")
	_, err = FetchCollector(ctx, large.Addr, pop3Collector(t, nil, func(r *model.RequestConfig) { r.MaxResponseBytes = 1024 }), RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("responses past the limit: err=%v, want a limit exceeded", err)
	}
}

// A server among the collector's denied targets is refused before it is
// connected to.
func TestAPOP3CollectorHoldsTheTargetPolicy(t *testing.T) {
	s := newPOP3Stub(t)
	c := pop3Collector(t, s, func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) || s.Accepted() != 0 {
		t.Errorf("err=%v, %d connections; want the target refused", err, s.Accepted())
	}
}

// The keys the opt-in pop3 type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestThePOP3TypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypePOP3)
}
