//go:build request_type_imap

package fetch

import (
	"bufio"
	"context"
	"encoding/base64"
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

// An imap collector's placeholders are in its mailboxes.
func init() {
	ownPlaceholders[RequestTypeIMAP] = func(c *model.Collector) {
		c.Request.Username, c.Request.PasswordFile = "me", "/password"
		c.Request.Own = IMAPConfig{Mailboxes: []string{"Archive/{{param_year:2026}}"}}
	}
}

// imapStub is a mail server as the tests need one: it greets, answers
// CAPABILITY, begins TLS on STARTTLS, takes the account user and pass by
// LOGIN and by AUTHENTICATE PLAIN, answers STATUS and LIST from boxes and
// GETQUOTAROOT with a quota, and keeps every line it was sent.
type imapStub struct {
	*testutil.TCPStub
	mu    sync.Mutex
	lines []string
	// What it offers: STARTTLS before TLS, and these beside IMAP4rev1,
	// before a login and after it.
	startTLS    bool
	offers      string
	offersAfter string
	user, pass  string
	greeting    string
	// boxes are its mailboxes in the order LIST gives them: the name as
	// the server writes it, its flags, and the counts of its STATUS, ""
	// for one STATUS is refused for.
	boxes []imapStubBox
}

type imapStubBox struct{ written, flags, status string }

func (s *imapStub) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *imapStub) serve(conn net.Conn, upgrade func() (net.Conn, error)) {
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
	secure, in := false, false
	say(s.greeting)
	for {
		line, ok := read()
		if !ok {
			return
		}
		tag, rest, _ := strings.Cut(line, " ")
		verb, args, _ := strings.Cut(rest, " ")
		login := func(accepted bool) {
			if in = accepted; accepted {
				say(tag + " OK [CAPABILITY IMAP4rev1] Logged in")
			} else {
				say(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)")
			}
		}
		switch strings.ToUpper(verb) {
		case "CAPABILITY":
			offered := "IMAP4rev1 " + s.offers
			if in {
				offered = "IMAP4rev1 " + s.offersAfter
			}
			if s.startTLS && !secure {
				offered += " STARTTLS"
			}
			say("* CAPABILITY " + strings.TrimSpace(offered))
			say(tag + " OK done")
		case "STARTTLS":
			say(tag + " OK begin TLS")
			upgraded, err := upgrade()
			if err != nil {
				return
			}
			conn, reader, secure = upgraded, bufio.NewReader(upgraded), true
		case "LOGIN":
			login(args == `"`+s.user+`" "`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s.pass)+`"`)
		case "AUTHENTICATE":
			mechanism, initial, _ := strings.Cut(args, " ")
			if mechanism == "PLAIN" && initial == "" {
				say("+ ")
				initial, _ = read()
			}
			login(mechanism == "PLAIN" && initial == base64.StdEncoding.EncodeToString([]byte("\x00"+s.user+"\x00"+s.pass)))
		case "STATUS":
			name := args[:strings.LastIndex(args, " (")]
			answered := false
			for _, box := range s.boxes {
				if `"`+box.written+`"` == name && box.status != "" {
					say("* STATUS " + name + " (" + box.status + ")")
					answered = true
				}
			}
			if answered {
				say(tag + " OK STATUS completed")
			} else {
				say(tag + " NO [NONEXISTENT] Unknown Mailbox: " + name + " (Failure)")
			}
		case "LIST":
			for i, box := range s.boxes {
				if i == 1 {
					// One name as a literal, as some servers send them.
					say(fmt.Sprintf(`* LIST (%s) "/" {%d}`, box.flags, len(box.written)))
					_, _ = fmt.Fprint(conn, box.written)
					say("")
					continue
				}
				say(`* LIST (` + box.flags + `) "/" "` + box.written + `"`)
			}
			say(tag + " OK LIST completed")
		case "GETQUOTAROOT":
			say(`* QUOTAROOT ` + args + ` ""`)
			say(`* QUOTA "" (STORAGE 1024 15728640 MESSAGE 12 1000)`)
			say(tag + " OK done")
		case "LOGOUT":
			say("* BYE bye")
			say(tag + " OK done")
			return
		default:
			say(tag + " BAD unknown command")
		}
	}
}

// newIMAPStub starts the server, as configure changed it before it took a
// connection.
func newIMAPStub(t *testing.T, configure ...func(*imapStub)) *imapStub {
	t.Helper()
	s := &imapStub{startTLS: true, offers: "SASL-IR AUTH=PLAIN", offersAfter: "QUOTA STATUS=SIZE", user: "me@example.com", pass: `app "pass" word`, greeting: "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] mail.test ready",
		boxes: []imapStubBox{
			{"INBOX", `\HasNoChildren`, "MESSAGES 12 UNSEEN 3 RECENT 1 UIDNEXT 44 UIDVALIDITY 7 SIZE 123456"},
			{"Sent Mail", `\Sent \HasNoChildren`, "MESSAGES 5 UNSEEN 0 RECENT 0 UIDNEXT 6 UIDVALIDITY 8 SIZE 50"},
			{"[Gmail]", `\Noselect \HasChildren`, ""},
			{"Entw&APw-rfe", `\Drafts`, "MESSAGES 1 UNSEEN 1 RECENT 0 UIDNEXT 2 UIDVALIDITY 9"},
			{"Gone", ``, ""},
		}}
	for _, fn := range configure {
		fn(s)
	}
	s.TCPStub = testutil.NewStartTLSStub(t, s.serve)
	return s
}

// imapCollector is a validated imap collector of the stub, changed by
// change first, which verifies the stub's certificate.
func imapCollector(t *testing.T, s *imapStub, own IMAPConfig, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypeIMAP, Own: own}}
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

func imapDoc(t *testing.T, c *model.Collector, target string, overrides RequestOverrides) imapDocument {
	t.Helper()
	resp, err := FetchCollector(context.Background(), target, c, overrides, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc imapDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v: %s", err, resp.Body)
	}
	return doc
}

func imapPassword(t *testing.T) string {
	t.Helper()
	return testutil.WriteFile(t, "password", `app "pass" word`+"\n")
}

// Without an account a scrape reads the greeting, begins TLS where it is
// offered and asks for the capabilities again within it: the document
// holds the greeting's text, that the session is over TLS by STARTTLS, and
// the capabilities, in upper case and sorted, and no mailbox.
func TestAnIMAPScrapeReadsTheGreetingAndTheCapabilities(t *testing.T) {
	s := newIMAPStub(t)
	doc := imapDoc(t, imapCollector(t, s, IMAPConfig{}), s.Addr, RequestOverrides{})
	if doc.Greeting != "[CAPABILITY IMAP4rev1 AUTH=PLAIN] mail.test ready" || !doc.TLS || !doc.StartTLS || doc.Authenticated || len(doc.Mailboxes) != 0 || len(doc.Quota) != 0 {
		t.Errorf("the document is %+v", doc)
	}
	if got := strings.Join(doc.Capabilities, " "); got != "AUTH=PLAIN IMAP4REV1 SASL-IR" {
		t.Errorf("the capabilities are %s", got)
	}
	if sent := strings.Join(s.sent(), "|"); sent != "a1 CAPABILITY|a2 STARTTLS|a3 CAPABILITY|a4 LOGOUT" {
		t.Errorf("the server was sent %s", sent)
	}
}

// A scrape with an account logs in, by AUTHENTICATE PLAIN with its
// credential in the command where the server takes that, in a line of its
// own where it does not, and by LOGIN where the server offers no PLAIN,
// each as the stub checks it byte for byte; asks for the capabilities
// again; and asks STATUS of INBOX when the collector names no mailbox. The
// counts are in the document, the size where the server counts one.
func TestAnIMAPScrapeLogsInAndCountsTheInbox(t *testing.T) {
	password := imapPassword(t)
	for name, tc := range map[string]struct{ offers, how string }{
		"plain in the command": {"SASL-IR AUTH=PLAIN", "a4 AUTHENTICATE PLAIN "},
		"plain in a line":      {"AUTH=PLAIN", "a4 AUTHENTICATE PLAIN|"},
		"login":                {"AUTH=XOAUTH2", `a4 LOGIN "me@example.com" "app \"pass\" word"`},
	} {
		s := newIMAPStub(t, func(s *imapStub) { s.offers = tc.offers })
		doc := imapDoc(t, imapCollector(t, s, IMAPConfig{}, withAccount("me@example.com", password)), s.Addr, RequestOverrides{})
		sent := strings.Join(s.sent(), "|")
		if !doc.Authenticated || !strings.Contains(sent, tc.how) || !strings.HasSuffix(sent, `CAPABILITY|a6 STATUS "INBOX" (MESSAGES UNSEEN RECENT UIDNEXT UIDVALIDITY SIZE)|a7 LOGOUT`) {
			t.Errorf("%s: authenticated %v, sent %s", name, doc.Authenticated, sent)
		}
		if got := strings.Join(doc.Capabilities, " "); got != "IMAP4REV1 QUOTA STATUS=SIZE" {
			t.Errorf("%s: the capabilities after the login are %s", name, got)
		}
		if len(doc.Mailboxes) != 1 {
			t.Fatalf("%s: the mailboxes are %+v", name, doc.Mailboxes)
		}
		box := doc.Mailboxes[0]
		if box.Name != "INBOX" || !box.OK || *box.Messages != 12 || *box.Unseen != 3 || *box.Recent != 1 || *box.UIDNext != 44 || *box.UIDValidity != 7 || *box.Size != 123456 {
			t.Errorf("%s: INBOX is %+v", name, box)
		}
	}
}

// A mailbox named by a pattern is listed, and each mailbox of the list
// that can be selected is asked about, with its flags: a name that came as
// a literal, one in modified UTF-7, which is decoded for the document and
// encoded for STATUS, and one the server refuses STATUS for, which is in
// the document with what the server said and fails nothing. A mailbox is
// asked about once, however often it is named, and a placeholder of a name
// is filled from the probe. The quota is read where the collector asks.
func TestAnIMAPScrapeListsAndCountsMailboxes(t *testing.T) {
	s := newIMAPStub(t)
	c := imapCollector(t, s, IMAPConfig{Mailboxes: []string{"INBOX", "*", "{{param_box}}"}, Quota: true}, withAccount("me@example.com", imapPassword(t)))
	doc := imapDoc(t, c, s.Addr, RequestOverrides{Params: map[string]string{"param_box": "Entwürfe"}})
	var names []string
	for _, box := range doc.Mailboxes {
		names = append(names, box.Name)
	}
	if got := strings.Join(names, "|"); got != "INBOX|Sent Mail|Entwürfe|Gone" {
		t.Fatalf("the mailboxes are %s: %+v", got, doc.Mailboxes)
	}
	sent, drafts, gone := doc.Mailboxes[1], doc.Mailboxes[2], doc.Mailboxes[3]
	if !sent.OK || *sent.Messages != 5 || strings.Join(sent.Flags, " ") != `\Sent \HasNoChildren` {
		t.Errorf("Sent Mail is %+v", sent)
	}
	if !drafts.OK || *drafts.Unseen != 1 || drafts.Size != nil {
		t.Errorf("Entwürfe is %+v", drafts)
	}
	if gone.OK || gone.Messages != nil || gone.Text != `[NONEXISTENT] Unknown Mailbox: "Gone" (Failure)` {
		t.Errorf("Gone is %+v", gone)
	}
	asked := strings.Join(s.sent(), "|")
	if !strings.Contains(asked, `LIST "" "*"`) || !strings.Contains(asked, `STATUS "Entw&APw-rfe" (`) || strings.Count(asked, `STATUS "INBOX"`) != 1 || strings.Contains(asked, `STATUS "[Gmail]"`) {
		t.Errorf("the server was sent %s", asked)
	}
	if len(doc.Quota) != 2 || doc.Quota[0].Resource != "STORAGE" || doc.Quota[0].Usage != 1024 || *doc.Quota[0].UsageBytes != 1048576 || *doc.Quota[0].LimitBytes != 16106127360 ||
		doc.Quota[1].Resource != "MESSAGE" || doc.Quota[1].Limit != 1000 || doc.Quota[1].LimitBytes != nil || !strings.Contains(asked, `GETQUOTAROOT "INBOX"`) {
		t.Errorf("the quota is %+v", doc.Quota)
	}
	before := s.Accepted()
	_, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{Params: map[string]string{"param_box": "x\"\r\na9 DELETE INBOX"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "request.mailboxes[2]: the value of param_box holds a control character") || s.Accepted() != before {
		t.Errorf("err=%v, connections %d to %d; want the value refused before anything is sent", err, before, s.Accepted())
	}
}

// A refused login fails the scrape, saying the account and what the server
// answered, and never the password, which the lines traced for a debug
// report do not hold either; whoever called the probe is not told the
// server's words.
func TestAnIMAPScrapeFailsOnARefusedLoginWithoutItsSecrets(t *testing.T) {
	wrong := testutil.WriteFile(t, "password", "hunter2-wrong\n")
	for name, offers := range map[string]string{"AUTHENTICATE PLAIN": "SASL-IR AUTH=PLAIN", "LOGIN": ""} {
		s := newIMAPStub(t, func(s *imapStub) { s.offers = offers })
		ctx, trace := WithRequestTrace(context.Background())
		_, err := FetchCollector(ctx, s.Addr, imapCollector(t, s, IMAPConfig{}, withAccount("me@example.com", wrong)), RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), "refused "+name+" for me@example.com with NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)") {
			t.Fatalf("%s: err=%v, want the refusal", name, err)
		}
		if told := model.CallerText(err); strings.Contains(told, "Invalid credentials") || !strings.Contains(told, "refused "+name+" for me@example.com with NO "+model.WithheldMark) {
			t.Errorf("%s: a probe's caller is told %q", name, told)
		}
		shown := fmt.Sprint(err, trace.Requests())
		for _, secret := range []string{"hunter2", base64.StdEncoding.EncodeToString([]byte("\x00me@example.com\x00hunter2-wrong"))} {
			if strings.Contains(shown, secret) {
				t.Errorf("%s: the error or the trace holds %s: %s", name, secret, shown)
			}
		}
		if !strings.Contains(shown, name+" ") || !strings.Contains(shown, "***") {
			t.Errorf("%s: the trace does not show the command masked: %s", name, shown)
		}
		if strings.Contains(strings.Join(s.sent(), "|"), "STATUS") {
			t.Errorf("%s: the session went on after the refusal: %v", name, s.sent())
		}
	}
}

// A password is not sent over a connection that is not TLS: a server that
// offers no STARTTLS fails the scrape before the login, unless
// allow_plaintext says it has none. An imaps:// target is TLS from the
// first byte, and its connection is there for $tls. A greeting that is no
// OK, and a server that takes no login this type speaks, fail the scrape
// saying so.
func TestAnIMAPScrapeHoldsTheSessionToTLS(t *testing.T) {
	password := imapPassword(t)
	s := newIMAPStub(t, func(s *imapStub) { s.startTLS = false })
	_, err := FetchCollector(context.Background(), s.Addr, imapCollector(t, s, IMAPConfig{}, withAccount("me@example.com", password)), RequestOverrides{}, nil)
	if sent := strings.Join(s.sent(), "|"); err == nil || !strings.Contains(err.Error(), "the account's password is not sent over a connection that is not TLS") || strings.Contains(sent, "AUTHENTICATE") || strings.Contains(sent, "LOGIN") {
		t.Errorf("err=%v, sent %v; want no login in plaintext", err, sent)
	}
	if doc := imapDoc(t, imapCollector(t, s, IMAPConfig{AllowPlaintext: true}, withAccount("me@example.com", password)), s.Addr, RequestOverrides{}); !doc.Authenticated || doc.TLS || len(doc.Mailboxes) != 1 {
		t.Errorf("allow_plaintext: %+v", doc)
	}

	implicit := newIMAPStub(t, func(s *imapStub) { s.startTLS = false })
	implicit.TCPStub = testutil.NewTLSTCPStub(t, func(conn net.Conn) { implicit.serve(conn, nil) })
	resp, err := FetchCollector(context.Background(), "imaps://"+implicit.Addr, imapCollector(t, implicit, IMAPConfig{}, withAccount("me@example.com", password)), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc imapDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil || !doc.TLS || doc.StartTLS || !doc.Authenticated || resp.Trip == nil || resp.Trip.TLSConnection == nil {
		t.Errorf("imaps: %v: %s, trip %+v", err, resp.Body, resp.Trip)
	}

	busy := newIMAPStub(t, func(s *imapStub) { s.greeting = "* BYE no service for you" })
	_, err = FetchCollector(context.Background(), busy.Addr, imapCollector(t, busy, IMAPConfig{}), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), `greeted with "* BYE no service for you", not * OK`) || strings.Contains(model.CallerText(err), "no service") {
		t.Errorf("a greeting that is no OK: err=%v, the caller is told %q", err, model.CallerText(err))
	}
	disabled := newIMAPStub(t, func(s *imapStub) { s.offers = "LOGINDISABLED AUTH=GSSAPI" })
	_, err = FetchCollector(context.Background(), disabled.Addr, imapCollector(t, disabled, IMAPConfig{}, withAccount("me@example.com", password)), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "takes no LOGIN (LOGINDISABLED) and offers no AUTH=PLAIN") {
		t.Errorf("LOGINDISABLED: err=%v", err)
	}
}

// A collector is refused at load for an account without its password, for
// mailboxes or a quota without an account, for an empty mailbox and one
// with a control character, and for more mailboxes than it may name; a
// target that is no mail server's is refused before anything is sent.
func TestIMAPConfigurationRules(t *testing.T) {
	many := make([]string, imapMaxNamed+1)
	for i := range many {
		many[i] = fmt.Sprint("box", i)
	}
	for name, tc := range map[string]struct {
		own    IMAPConfig
		change func(*model.RequestConfig)
		want   string
	}{
		"a name without a password":    {IMAPConfig{}, withAccount("me", ""), "sets request.username without request.password_file"},
		"a password without a name":    {IMAPConfig{}, withAccount("", "/password"), "sets request.password_file without request.username"},
		"mailboxes without an account": {IMAPConfig{Mailboxes: []string{"INBOX"}}, nil, "asks about mailboxes without an account"},
		"a quota without an account":   {IMAPConfig{Quota: true}, nil, "asks about mailboxes without an account"},
		"an empty mailbox":             {IMAPConfig{Mailboxes: []string{"INBOX", " "}}, withAccount("me", "/password"), "request.mailboxes[1] is empty"},
		"a control character":          {IMAPConfig{Mailboxes: []string{"a\r\nb"}}, withAccount("me", "/password"), "request.mailboxes[0] holds a control character"},
		"too many":                     {IMAPConfig{Mailboxes: many}, withAccount("me", "/password"), "request.mailboxes has 51 entries, more than 50"},
		"a placeholder in the account": {IMAPConfig{}, withAccount("{{param_user}}", "/password"), "has a placeholder; the account is used as it is written"},
	} {
		c := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: RequestTypeIMAP, Own: tc.own}}
		if tc.change != nil {
			tc.change(&c.Request)
		}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	c := imapCollector(t, nil, IMAPConfig{})
	for target, want := range map[string]string{
		"":                       "target is required",
		"pop3://mail.example":    "an imap target is",
		"imap://mail.example/in": "an imap target is",
		"imap://me@mail.example": "has a user in it; the account is request.username",
	} {
		if err := CheckTarget(c, target, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("target %q: err=%v, want %q", target, err, want)
		}
	}
	for target, want := range map[string]string{"mail.example": "imap://mail.example:143", "imaps://mail.example": "imaps://mail.example:993", "imap://mail.example:1143": "imap://mail.example:1143"} {
		if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != want {
			t.Errorf("the label of %s is %q, %v; want %s", target, got, err, want)
		}
	}
}

// What is no IMAP server fails the scrape at once and within bounds: a
// line without end, a literal longer than a line may be, an answer under
// another tag, and responses past the response limit, which is a limit
// exceeded.
func TestAnIMAPScrapeGivesUpOnWhatIsNoIMAP(t *testing.T) {
	endless := testutil.NewTCPStub(t, func(conn net.Conn) {
		chunk := []byte(strings.Repeat("*", 4096))
		for {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	_, err := FetchCollector(ctx, endless.Addr, imapCollector(t, nil, IMAPConfig{}), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "a line of the response is longer than 65536 bytes") || time.Since(start) > 30*time.Second {
		t.Errorf("a line without end: err=%v after %s", err, time.Since(start))
	}
	answering := func(answer string) *testutil.TCPStub {
		return testutil.NewTCPStub(t, func(conn net.Conn) {
			_, _ = fmt.Fprint(conn, "* OK ready\r\n")
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = fmt.Fprint(conn, answer)
			_, _ = bufio.NewReader(conn).ReadString('\n')
		})
	}
	for name, tc := range map[string]struct{ answer, want string }{
		"a literal too long": {"* CAPABILITY {99999999}\r\n", "a line of the response is longer than 65536 bytes"},
		"another tag":        {"zz OK done\r\n", `the server answered "zz OK done", which is no IMAP response to the command`},
		"an HTTP server":     {"HTTP/1.1 400 Bad Request\r\n\r\n", `which is no IMAP response to the command; is the target an IMAP server?`},
	} {
		stub := answering(tc.answer)
		if _, err := FetchCollector(ctx, stub.Addr, imapCollector(t, nil, IMAPConfig{}), RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	large := answering(strings.Repeat("* CAPABILITY IMAP4rev1 "+strings.Repeat("X", 1000)+"\r\n", 4) + "a1 OK done\r\n")
	limited := imapCollector(t, nil, IMAPConfig{}, func(r *model.RequestConfig) { r.MaxResponseBytes = 2048 })
	if _, err := FetchCollector(ctx, large.Addr, limited, RequestOverrides{}, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("responses past the limit: err=%v", err)
	}
}

// A mailbox's name is written and read in modified UTF-7, and a name that
// is not that comes through as it came.
func TestIMAPMailboxNamesAreModifiedUTF7(t *testing.T) {
	for name, encoded := range map[string]string{
		"INBOX":          "INBOX",
		"Entwürfe":       "Entw&APw-rfe",
		"Q&A":            "Q&-A",
		"日本語":            "&ZeVnLIqe-",
		"~peter/mail/台北": "~peter/mail/&U,BTFw-",
		"a😀b":            "a&2D3eAA-b",
		"":               "",
	} {
		if got := imapEncodeName(name); got != encoded {
			t.Errorf("%q is written %q, want %q", name, got, encoded)
		}
		if got := imapDecodeName(encoded); got != name {
			t.Errorf("%q is read %q, want %q", encoded, got, name)
		}
	}
	for _, raw := range []string{"R&D", "a&!!-b", "Entwürfe", "odd&AP-x"} {
		if got := imapDecodeName(raw); got != raw {
			t.Errorf("%q, which is not modified UTF-7, is read %q", raw, got)
		}
	}
}

// A server among the collector's denied targets is refused before it is
// connected to.
func TestAnIMAPCollectorHoldsTheTargetPolicy(t *testing.T) {
	s := newIMAPStub(t)
	c := imapCollector(t, s, IMAPConfig{}, func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) || s.Accepted() != 0 {
		t.Errorf("err=%v, %d connections; want the target refused", err, s.Accepted())
	}
}

// The keys the opt-in imap type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheIMAPTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeIMAP)
}
