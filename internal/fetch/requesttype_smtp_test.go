//go:build request_type_smtp

package fetch

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An smtp collector's placeholders are in its addresses.
func init() {
	ownPlaceholders[RequestTypeSMTP] = func(c *model.Collector) {
		c.Request.Own = SMTPConfig{MailFrom: "{{param_from:a}}@example.com", RcptTo: []string{"{{param_to:b}}@example.com"}}
	}
}

// smtpStub is a mail server as the tests need one: it greets, answers EHLO
// with its extensions, begins TLS on STARTTLS, takes the account user and
// pass by PLAIN and LOGIN and the token by XOAUTH2, takes any sender and
// refuses the recipients of refuse, and keeps every line it was sent.
type smtpStub struct {
	*testutil.TCPStub
	mu    sync.Mutex
	lines []string
	// What it offers: STARTTLS before TLS, and the mechanisms of AUTH.
	startTLS   bool
	mechanisms string
	user, pass string
	token      string
	refuse     map[string]bool
	greeting   string
}

func (s *smtpStub) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *smtpStub) serve(conn net.Conn, upgrade func() (net.Conn, error)) {
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
	secure := false
	say(s.greeting)
	for {
		line, ok := read()
		if !ok {
			return
		}
		verb, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			lines := []string{"250-mail.test greets " + rest, "250-SIZE 35882577", "250-8BITMIME"}
			if s.startTLS && !secure {
				lines = append(lines, "250-STARTTLS")
			}
			if s.mechanisms != "" {
				lines = append(lines, "250-AUTH "+s.mechanisms)
			}
			say(strings.Join(lines, "\r\n") + "\r\n250 SMTPUTF8")
		case "STARTTLS":
			say("220 go ahead")
			upgraded, err := upgrade()
			if err != nil {
				return
			}
			conn, reader, secure = upgraded, bufio.NewReader(upgraded), true
		case "AUTH":
			mechanism, initial, _ := strings.Cut(rest, " ")
			accepted := false
			switch mechanism {
			case "PLAIN":
				accepted = initial == b64("\x00"+s.user+"\x00"+s.pass)
			case "LOGIN":
				say("334 " + b64("Username:"))
				user, _ := read()
				say("334 " + b64("Password:"))
				pass, _ := read()
				accepted = user == b64(s.user) && pass == b64(s.pass)
			case "XOAUTH2":
				accepted = initial == b64("user="+s.user+"\x01auth=Bearer "+s.token+"\x01\x01")
				if !accepted {
					say("334 " + b64(`{"status":"400"}`))
					_, _ = read()
				}
			}
			if accepted {
				say("235 2.7.0 Accepted")
			} else {
				say("535-5.7.8 Username and Password not accepted.\r\n535 5.7.8 see the help")
			}
		case "MAIL":
			say("250 2.1.0 OK")
		case "RCPT":
			address := strings.Trim(strings.TrimPrefix(strings.ToUpper(rest), "TO:"), "<>")
			if s.refuse[strings.ToLower(address)] {
				say("550 5.1.1 no such user")
			} else {
				say("250 2.1.5 OK")
			}
		case "RSET":
			say("250 flushed")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

// newSMTPStub starts the server, as configure changed it before it took a
// connection.
func newSMTPStub(t *testing.T, configure ...func(*smtpStub)) *smtpStub {
	t.Helper()
	s := &smtpStub{startTLS: true, mechanisms: "LOGIN PLAIN XOAUTH2", user: "me@example.com", pass: "app pass word", token: "ya29.token", greeting: "220 mail.test ESMTP ready", refuse: map[string]bool{}}
	for _, fn := range configure {
		fn(s)
	}
	s.TCPStub = testutil.NewStartTLSStub(t, s.serve)
	return s
}

// smtpCollector is a validated smtp collector of the stub, changed by
// change first, which verifies the stub's certificate.
func smtpCollector(t *testing.T, s *smtpStub, own SMTPConfig, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypeSMTP, Own: own}}
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

func smtpDoc(t *testing.T, c *model.Collector, target string, overrides RequestOverrides) smtpDocument {
	t.Helper()
	resp, err := FetchCollector(context.Background(), target, c, overrides, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc smtpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v: %s", err, resp.Body)
	}
	return doc
}

// A scrape greets the server, begins TLS where it is offered and says EHLO
// again within it: the document holds the greeting, that the session is
// over TLS by STARTTLS, the extensions of the second EHLO, the mechanisms
// AUTH offers, sorted, and SIZE's limit, and nothing of AUTH or of a
// transaction the collector did not ask for.
func TestAnSMTPScrapeReadsTheGreetingAndTheExtensions(t *testing.T) {
	s := newSMTPStub(t)
	doc := smtpDoc(t, smtpCollector(t, s, SMTPConfig{Hello: "probe.example"}), s.Addr, RequestOverrides{})
	if doc.Greeting.Code != 220 || doc.Greeting.Text != "mail.test ESMTP ready" || !doc.TLS || !doc.StartTLS {
		t.Errorf("the document is %+v", doc)
	}
	if doc.Extensions["8BITMIME"] != true || doc.Extensions["SIZE"] != "35882577" || doc.Size == nil || *doc.Size != 35882577 {
		t.Errorf("the extensions are %v, size %v", doc.Extensions, doc.Size)
	}
	if _, offered := doc.Extensions["STARTTLS"]; offered {
		t.Errorf("the extensions are those before TLS: %v", doc.Extensions)
	}
	if strings.Join(doc.AuthMechanisms, " ") != "LOGIN PLAIN XOAUTH2" || doc.Auth != nil || doc.MailFrom != nil || len(doc.RcptTo) != 0 {
		t.Errorf("the document is %+v", doc)
	}
	if sent := strings.Join(s.sent(), "|"); sent != "EHLO probe.example|STARTTLS|EHLO probe.example|QUIT" {
		t.Errorf("the server was sent %s", sent)
	}
}

// AUTH logs in with PLAIN, LOGIN and XOAUTH2, each as its RFC writes it,
// which the stub checks byte for byte: PLAIN where the collector names no
// mechanism and the server offers it, LOGIN where it offers that alone.
func TestAnSMTPScrapeLogsInWithPlainLoginAndXOAuth2(t *testing.T) {
	password := testutil.WriteFile(t, "password", "app pass word\n")
	token := testutil.WriteFile(t, "token", "ya29.token\n")
	for name, tc := range map[string]struct {
		own        SMTPConfig
		mechanisms string
		change     func(*model.RequestConfig)
		want       string
	}{
		"plain by default":      {SMTPConfig{}, "LOGIN PLAIN", withAccount("me@example.com", password), "PLAIN"},
		"login where it is all": {SMTPConfig{}, "LOGIN", withAccount("me@example.com", password), "LOGIN"},
		"login when named":      {SMTPConfig{Auth: "login"}, "LOGIN PLAIN", withAccount("me@example.com", password), "LOGIN"},
		"xoauth2 from a file":   {SMTPConfig{XOAuth2: &SMTPXOAuth2{TokenFile: token}}, "PLAIN XOAUTH2", withAccount("me@example.com", ""), "XOAUTH2"},
	} {
		s := newSMTPStub(t, func(s *smtpStub) { s.mechanisms = tc.mechanisms })
		doc := smtpDoc(t, smtpCollector(t, s, tc.own, tc.change), s.Addr, RequestOverrides{})
		if doc.Auth == nil || doc.Auth.Mechanism != tc.want || doc.Auth.Code != 235 {
			t.Errorf("%s: auth is %+v, want %s accepted", name, doc.Auth, tc.want)
		}
	}
}

// XOAUTH2's token comes from the refresh token grant: the endpoint is sent
// the client's id and secret and the refresh token, each read from its
// file, and the access token it answers is sent to the server and kept, so
// a second scrape asks the endpoint for none.
func TestAnSMTPScrapeTradesARefreshTokenForItsAccessToken(t *testing.T) {
	s := newSMTPStub(t)
	asked := 0
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "1//refresh" || r.PostForm.Get("client_id") != "client" || r.PostForm.Get("client_secret") != "secret" || r.PostForm.Get("scope") != "https://mail.google.com/" {
			http.Error(w, `{"error":"invalid_grant","error_description":"Bad Request"}`, http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `{"access_token":"ya29.token","expires_in":3599,"token_type":"Bearer"}`)
	}))
	defer endpoint.Close()
	saved := smtpHTTPClient
	smtpHTTPClient = endpoint.Client()
	t.Cleanup(func() { smtpHTTPClient = saved })
	grant := &SMTPXOAuth2{TokenURL: endpoint.URL + "/token", ClientID: "client", ClientSecretFile: testutil.WriteFile(t, "secret", "secret\n"), RefreshTokenFile: testutil.WriteFile(t, "refresh", "1//refresh\n"), Scopes: []string{"https://mail.google.com/"}}
	c := smtpCollector(t, s, SMTPConfig{XOAuth2: grant}, withAccount("me@example.com", ""))
	t.Cleanup(func() { smtpTokens.forget(c.Name, grant) })
	for range 2 {
		if doc := smtpDoc(t, c, s.Addr, RequestOverrides{}); doc.Auth == nil || doc.Auth.Mechanism != "XOAUTH2" {
			t.Fatalf("auth is %+v", doc.Auth)
		}
	}
	if asked != 1 {
		t.Errorf("the token endpoint was asked %d times, want once for two scrapes", asked)
	}
	// A refused grant fails the scrape with the endpoint's error code, and
	// nothing of the secrets.
	bad := *grant
	bad.RefreshTokenFile = testutil.WriteFile(t, "stale", "1//stale\n")
	_, err := FetchCollector(context.Background(), s.Addr, smtpCollector(t, s, SMTPConfig{XOAuth2: &bad}, withAccount("me@example.com", "")), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "answered HTTP status 400, invalid_grant: Bad Request") || strings.Contains(err.Error(), "1//stale") || strings.Contains(err.Error(), "secret") {
		t.Errorf("err=%v, want the endpoint's refusal without the secrets", err)
	}
}

// A refused AUTH fails the scrape, saying the mechanism, the account and
// what the server answered, and never the password or the token, which the
// lines traced for a debug report do not hold either.
func TestAnSMTPScrapeFailsOnARefusedLoginWithoutItsSecrets(t *testing.T) {
	s := newSMTPStub(t)
	wrong := testutil.WriteFile(t, "password", "hunter2-wrong\n")
	c := smtpCollector(t, s, SMTPConfig{}, withAccount("me@example.com", wrong))
	ctx, trace := WithRequestTrace(context.Background())
	_, err := FetchCollector(ctx, s.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "refused AUTH PLAIN for me@example.com with 535 5.7.8 Username and Password not accepted. 5.7.8 see the help") {
		t.Fatalf("err=%v, want the refusal", err)
	}
	// What the server said is the operator's to read, in the log: whoever
	// called the probe is told that it refused, and not its words.
	if told := model.CallerText(err); strings.Contains(told, "not accepted") || !strings.Contains(told, "refused AUTH PLAIN for me@example.com with 535 "+model.WithheldMark) {
		t.Errorf("a probe's caller is told %q", told)
	}
	shown := fmt.Sprint(err, trace.Requests())
	for _, secret := range []string{"hunter2", base64.StdEncoding.EncodeToString([]byte("\x00me@example.com\x00hunter2-wrong"))} {
		if strings.Contains(shown, secret) {
			t.Errorf("the error or the trace holds %s: %s", secret, shown)
		}
	}
	if !strings.Contains(shown, "AUTH PLAIN ***") {
		t.Errorf("the trace does not show the command masked: %s", shown)
	}
}

// MAIL FROM and each RCPT TO are asked and answered, and no DATA follows:
// a recipient the server refuses is in the document with its code, and
// the scrape succeeds. A placeholder of an address is filled from the
// probe, and a value that would end the command is refused before
// anything is sent.
func TestAnSMTPScrapeAsksAboutRecipientsAndSendsNothing(t *testing.T) {
	s := newSMTPStub(t, func(s *smtpStub) { s.refuse["nobody@example.com"] = true })
	c := smtpCollector(t, s, SMTPConfig{MailFrom: "probe@example.com", RcptTo: []string{"postmaster@example.com", "{{param_user}}@example.com"}})
	doc := smtpDoc(t, c, s.Addr, RequestOverrides{Params: map[string]string{"param_user": "nobody"}})
	if doc.MailFrom == nil || !doc.MailFrom.Accepted || doc.MailFrom.Code != 250 || len(doc.RcptTo) != 2 {
		t.Fatalf("the document is %+v", doc)
	}
	if first, second := doc.RcptTo[0], doc.RcptTo[1]; !first.Accepted || first.Address != "postmaster@example.com" || second.Accepted || second.Code != 550 || second.Text != "5.1.1 no such user" || second.Address != "nobody@example.com" {
		t.Errorf("the recipients are %+v", doc.RcptTo)
	}
	sent := strings.Join(s.sent(), "|")
	if !strings.Contains(sent, "MAIL FROM:<probe@example.com>|RCPT TO:<postmaster@example.com>|RCPT TO:<nobody@example.com>|RSET|QUIT") || strings.Contains(sent, "DATA") {
		t.Errorf("the server was sent %s", sent)
	}
	before := s.Accepted()
	_, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{Params: map[string]string{"param_user": "x>\r\nDATA"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "request.rcpt_to[1]: the value of param_user holds a blank, an angle bracket or a control character") || s.Accepted() != before {
		t.Errorf("err=%v, connections %d to %d; want the value refused before anything is sent", err, before, s.Accepted())
	}
}

// A password is not sent over a connection that is not TLS: a server that
// offers no STARTTLS fails the scrape before AUTH, unless starttls_mode: never
// says it has none; starttls_mode: required fails where it is not offered, with
// or without an account; and a greeting that is no 220 fails saying what
// the server said.
func TestAnSMTPScrapeHoldsTheSessionToTLS(t *testing.T) {
	password := testutil.WriteFile(t, "password", "app pass word\n")
	s := newSMTPStub(t, func(s *smtpStub) { s.startTLS = false })
	_, err := FetchCollector(context.Background(), s.Addr, smtpCollector(t, s, SMTPConfig{}, withAccount("me@example.com", password)), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "the account's credential is not sent over a connection that is not TLS") || strings.Contains(strings.Join(s.sent(), "|"), "AUTH") {
		t.Errorf("err=%v, sent %v; want no AUTH in plaintext", err, s.sent())
	}
	if doc := smtpDoc(t, smtpCollector(t, s, SMTPConfig{StartTLS: "never"}, withAccount("me@example.com", password)), s.Addr, RequestOverrides{}); doc.Auth == nil || doc.TLS {
		t.Errorf("starttls never: %+v", doc)
	}
	_, err = FetchCollector(context.Background(), s.Addr, smtpCollector(t, s, SMTPConfig{StartTLS: "required"}), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS, which request.starttls_mode: required asks for") {
		t.Errorf("required: err=%v", err)
	}
	busy := newSMTPStub(t, func(s *smtpStub) { s.greeting = "554 no service for you" })
	_, err = FetchCollector(context.Background(), busy.Addr, smtpCollector(t, busy, SMTPConfig{}), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "greeted with 554 no service for you, not 220") || strings.Contains(model.CallerText(err), "no service") {
		t.Errorf("greeting: err=%v", err)
	}
}

// An smtps:// target is TLS from the first byte, with no STARTTLS, and its
// certificate is what $tls describes.
func TestAnSMTPSTargetIsTLSFromTheFirstByte(t *testing.T) {
	s := newSMTPStub(t, func(s *smtpStub) { s.startTLS = false })
	s.TCPStub = testutil.NewTLSTCPStub(t, func(conn net.Conn) { s.serve(conn, nil) })
	c := smtpCollector(t, s, SMTPConfig{})
	resp, err := FetchCollector(context.Background(), "smtps://"+s.Addr, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc smtpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil || !doc.TLS || doc.StartTLS {
		t.Errorf("%v: %s", err, resp.Body)
	}
	if resp.Trip == nil || resp.Trip.TLSConnection == nil {
		t.Errorf("the answer has no TLS connection for $tls: %+v", resp.Trip)
	}
}

// A collector is refused at load for a mechanism without what it logs in
// with, a recipient without a sender, an address that is none, a token
// endpoint that is not https, and smtps:// beside starttls_mode: required.
func TestSMTPConfigurationRules(t *testing.T) {
	for name, tc := range map[string]struct {
		own    SMTPConfig
		change func(*model.RequestConfig)
		want   string
	}{
		"starttls":           {SMTPConfig{StartTLS: "maybe"}, nil, `request.starttls_mode "maybe" is not auto, required or never`},
		"auth":               {SMTPConfig{Auth: "cram-md5"}, nil, `request.auth "cram-md5" is not plain, login or xoauth2`},
		"no username":        {SMTPConfig{Auth: "plain"}, nil, "logs in without request.username"},
		"no password":        {SMTPConfig{}, withAccount("me@example.com", ""), "sets request.username without request.password_file"},
		"xoauth2 no source":  {SMTPConfig{Auth: "xoauth2"}, withAccount("me@example.com", ""), "request.auth is xoauth2 without request.xoauth2"},
		"xoauth2 both":       {SMTPConfig{XOAuth2: &SMTPXOAuth2{TokenFile: "/t", TokenURL: "https://x/t"}}, withAccount("me@example.com", ""), "token_file beside the keys of the refresh token grant"},
		"xoauth2 http":       {SMTPConfig{XOAuth2: &SMTPXOAuth2{TokenURL: "http://x/t", ClientID: "c", RefreshTokenFile: "/r"}}, withAccount("me@example.com", ""), "is not an https:// URL"},
		"xoauth2 no refresh": {SMTPConfig{XOAuth2: &SMTPXOAuth2{TokenURL: "https://x/t", ClientID: "c"}}, withAccount("me@example.com", ""), "without refresh_token_file"},
		"password beside":    {SMTPConfig{XOAuth2: &SMTPXOAuth2{TokenFile: "/t"}}, withAccount("me@example.com", "/p"), "request.password_file beside xoauth2"},
		"rcpt alone":         {SMTPConfig{RcptTo: []string{"a@example.com"}}, nil, "request.rcpt_to without request.mail_from"},
		"no address":         {SMTPConfig{MailFrom: "nobody"}, nil, `request.mail_from "nobody" is not an address`},
		"null recipient":     {SMTPConfig{MailFrom: "<>", RcptTo: []string{"<>"}}, nil, "request.rcpt_to[0] is <>"},
		"hello":              {SMTPConfig{Hello: "two words"}, nil, `request.hello "two words" is not a name`},
	} {
		c := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypeSMTP, Own: tc.own}}
		if tc.change != nil {
			tc.change(&c.Request)
		}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	c := smtpCollector(t, nil, SMTPConfig{StartTLS: "required"})
	if _, err := FetchCollector(context.Background(), "smtps://mail.example:465", c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "is smtps://, TLS from the first byte, but the collector sets request.starttls_mode: required") {
		t.Errorf("smtps beside required: err=%v", err)
	}
	for _, target := range []string{"imap://mail.example", "mail.example/inbox", "user@mail.example", "mail.example:0", ""} {
		if _, err := FetchCollector(context.Background(), target, smtpCollector(t, nil, SMTPConfig{}), RequestOverrides{}, nil); err == nil {
			t.Errorf("the target %q is taken", target)
		}
	}
}

// A dry run reads the files a scrape would: it warns of the files of
// request.xoauth2 that are not there or are empty, by their keys.
func TestAnSMTPCollectorsFilesAreCheckedForADryRun(t *testing.T) {
	grant := &SMTPXOAuth2{TokenURL: "https://oauth2.example/token", ClientID: "client", ClientSecretFile: testutil.WriteFile(t, "secret", "\n"), RefreshTokenFile: "/nonexistent/refresh"}
	c := smtpCollector(t, nil, SMTPConfig{XOAuth2: grant}, withAccount("me@example.com", ""))
	warnings := strings.Join(RequestTypes[RequestTypeSMTP].CheckFiles(c, nil), "\n")
	if !strings.Contains(warnings, `collector "mail": request.xoauth2.refresh_token_file:`) || !strings.Contains(warnings, "request.xoauth2.client_secret_file") || !strings.Contains(warnings, "is empty") {
		t.Errorf("the warnings are %q", warnings)
	}
	grant.ClientSecretFile, grant.RefreshTokenFile = "", testutil.WriteFile(t, "refresh", "1//r\n")
	if warnings := RequestTypes[RequestTypeSMTP].CheckFiles(smtpCollector(t, nil, SMTPConfig{XOAuth2: grant}, withAccount("me@example.com", "")), nil); len(warnings) != 0 {
		t.Errorf("files that are there are warned of: %q", warnings)
	}
}

// A server that sends a line without an end is given up on at the most a
// reply may be, long before the scrape's deadline, and is not read into
// memory until then.
func TestAnSMTPScrapeGivesUpOnALineWithoutEnd(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		chunk := []byte(strings.Repeat("2", 4096))
		for {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	_, err := FetchCollector(ctx, stub.Addr, smtpCollector(t, nil, SMTPConfig{}), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "a reply is longer than 65536 bytes") || time.Since(start) > 30*time.Second {
		t.Errorf("err=%v after %s, want the length refused at once", err, time.Since(start))
	}
}

// A server among the collector's denied targets is refused before it is
// connected to.
func TestAnSMTPCollectorHoldsTheTargetPolicy(t *testing.T) {
	s := newSMTPStub(t)
	c := smtpCollector(t, s, SMTPConfig{}, func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) || s.Accepted() != 0 {
		t.Errorf("err=%v, %d connections; want the target refused", err, s.Accepted())
	}
}

// The keys the opt-in smtp type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheSMTPTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeSMTP)
}
