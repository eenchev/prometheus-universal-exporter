//go:build request_type_tcp

package fetch

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// startTLSServer is a server that greets, answers each line the client
// sends in plaintext with the next of replies, and then, with upgrade,
// begins TLS, as a server that has said yes to STARTTLS does, and answers
// the line read over TLS with answer. What it saw is sent on seen, joined
// with '|': each line read in plaintext, "TLS" once the handshake is made,
// the line read over TLS, "closed" when the client closed instead of
// sending one, and "then" and what came after the replies without upgrade.
func startTLSServer(greeting string, replies []string, upgrade bool, answer string, seen chan<- string) func(net.Conn, func() (net.Conn, error)) {
	return func(conn net.Conn, begin func() (net.Conn, error)) {
		var got []string
		defer func() {
			if seen != nil {
				seen <- strings.Join(got, "|")
			}
		}()
		_, _ = io.WriteString(conn, greeting)
		for _, reply := range replies {
			line, err := testutil.ReadUntil(conn, "\n")
			if err != nil {
				got = append(got, "closed")
				return
			}
			got = append(got, line)
			_, _ = io.WriteString(conn, reply)
		}
		if !upgrade {
			if rest, _ := io.ReadAll(conn); len(rest) > 0 {
				got = append(got, "then "+string(rest))
			}
			return
		}
		secure, err := begin()
		if err != nil {
			got = append(got, "handshake: "+err.Error())
			return
		}
		got = append(got, "TLS")
		line, err := testutil.ReadUntil(secure, "\n")
		if err != nil {
			got = append(got, "closed")
			return
		}
		got = append(got, line)
		_, _ = io.WriteString(secure, answer)
	}
}

// The plaintext side of each protocol's STARTTLS, as a server that offers
// it speaks it, and what the client is to send.
var startTLSDialogues = map[string]struct {
	greeting string
	replies  []string
	sent     string
}{
	"smtp": {
		"220-mx.example ESMTP Postfix\r\n220 ready\r\n",
		[]string{"250-mx.example\r\n250-PIPELINING\r\n250-SIZE 10240000\r\n250-starttls\r\n250 8BITMIME\r\n", "220 2.0.0 Ready to start TLS\r\n"},
		"EHLO [127.0.0.1]\r\n|STARTTLS\r\n",
	},
	"imap": {
		"* OK [CAPABILITY IMAP4rev1 STARTTLS LOGINDISABLED] Dovecot ready.\r\n",
		[]string{"* CAPABILITY IMAP4rev1 STARTTLS\r\na1 OK Begin TLS negotiation now.\r\n"},
		"a1 STARTTLS\r\n",
	},
	"pop3": {"+OK Dovecot ready.\r\n", []string{"+OK Begin TLS negotiation now.\r\n"}, "STLS\r\n"},
	"ftp":  {"220-Welcome\r\n  to the archive\r\n220 ready\r\n", []string{"234 AUTH TLS successful\r\n"}, "AUTH TLS\r\n"},
}

// startTLSCollector is a tcp collector speaking protocol's STARTTLS with
// send, verifying the server with caFile.
func startTLSCollector(t *testing.T, protocol, send, caFile string, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	return tcpCollector(t, append([]func(*model.Collector){func(c *model.Collector) {
		c.Request.Own, c.Request.StartTLS, c.Request.TLS.CAFile = TCPConfig{Send: send}, protocol, caFile
	}}, edit...)...)
}

// request.starttls begins TLS on the plaintext connection as each protocol
// asks for it, after the greeting, which is read without request.greeting:
// SMTP's EHLO, naming the client by its address, over a greeting and an
// answer of several lines, STARTTLS listed in lower case, then STARTTLS;
// IMAP's tagged STARTTLS, answered after an untagged line; POP3's STLS;
// FTP's AUTH TLS. send is written over TLS alone, and its answer is the
// body; without send the scrape ends with the handshake, the greeting the
// body. The label is tls://, and the steps' seconds are headers.
func TestATCPCollectorBeginsTLSWithSTARTTLS(t *testing.T) {
	for protocol, d := range startTLSDialogues {
		for _, send := range []string{"QUIT\r\n", ""} {
			seen := make(chan string, 1)
			server := testutil.NewStartTLSStub(t, startTLSServer(d.greeting, d.replies, true, "221 Bye\r\n", seen))
			c := startTLSCollector(t, protocol, send, server.CAFile)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := FetchCollector(ctx, server.Addr, c, RequestOverrides{}, nil)
			cancel()
			if err != nil {
				t.Errorf("%s, send %q: %v", protocol, send, err)
				continue
			}
			want, body := d.sent+"|TLS|QUIT\r\n", "221 Bye\r\n"
			if send == "" {
				want, body = d.sent+"|TLS|closed", d.greeting
			}
			if got := <-seen; got != want {
				t.Errorf("%s, send %q: the server saw %q, want %q", protocol, send, got, want)
			}
			if string(resp.Body) != body || resp.Headers.Get("X-Tcp-Greeting") == "" {
				t.Errorf("%s, send %q: body %q, headers %v", protocol, send, resp.Body, resp.Headers)
			}
			seconds(t, resp, "X-Tcp-Starttls-Seconds")
			seconds(t, resp, "X-Tcp-Tls-Seconds")
			seconds(t, resp, "X-Tcp-Greeting-Seconds")
			if label, err := RequestLabelFor(server.Addr, c, RequestOverrides{}); err != nil || label != "tls://"+server.Addr {
				t.Errorf("%s: label %q, %v", protocol, label, err)
			}
		}
	}
}

// Without a tls block request.starttls verifies the server against the
// system's roots, which do not hold the stub's CA, so the handshake fails
// and send is never written; request.greeting, when set, is still matched
// against the greeting.
func TestATCPSTARTTLSVerifiesTheServer(t *testing.T) {
	d := startTLSDialogues["pop3"]
	seen := make(chan string, 1)
	server := testutil.NewStartTLSStub(t, startTLSServer(d.greeting, d.replies, true, "+OK bye\r\n", seen))
	_, err := tcpScrape(t, startTLSCollector(t, "pop3", "QUIT\r\n", ""), server.Addr, nil)
	if err == nil || !strings.Contains(err.Error(), "TLS handshake with "+server.Addr) {
		t.Errorf("an unknown authority: %v", err)
	}
	if got := <-seen; !strings.HasPrefix(got, "STLS\r\n|handshake: ") {
		t.Errorf("the server saw %q", got)
	}
	server = testutil.NewStartTLSStub(t, startTLSServer(d.greeting, d.replies, true, "+OK bye\r\n", nil))
	c := startTLSCollector(t, "pop3", "QUIT\r\n", server.CAFile, func(c *model.Collector) {
		c.Request.Own, c.Request.StartTLS = TCPConfig{Send: "QUIT\r\n", Greeting: "^\\+OK Courier"}, "pop3"
	})
	if _, err := tcpScrape(t, c, server.Addr, nil); err == nil || !strings.Contains(err.Error(), "does not match request.greeting") {
		t.Errorf("request.greeting: %v", err)
	}
	insecure := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.StartTLS, c.Request.TLS.InsecureSkipVerify = TCPConfig{Send: "QUIT\r\n"}, "pop3", true
	})
	if body, err := tcpScrape(t, insecure, server.Addr, nil); err != nil || body != "+OK bye\r\n" {
		t.Errorf("insecure_skip_verify: %q, %v", body, err)
	}
}

// A server that does not offer STARTTLS, refuses it, greets otherwise or
// answers what is not its protocol's fails the scrape naming the step, and
// nothing more is sent: never send, in plaintext or at all. The server's
// words are in the error and withheld from a probe's caller.
func TestATCPSTARTTLSThatIsNotBegunFailsTheScrape(t *testing.T) {
	smtpGreeting := "220 mx.example ESMTP\r\n"
	for name, tc := range map[string]struct {
		protocol, greeting string
		replies            []string
		seen, want, said   string
	}{
		"SMTP without STARTTLS": {
			"smtp", smtpGreeting, []string{"250-mx.example\r\n250-PIPELINING\r\n250 8BITMIME\r\n"}, "EHLO [127.0.0.1]\r\n",
			`does not offer STARTTLS in its answer to EHLO, "250-mx.example\r\n250-PIPELINING\r\n250 8BITMIME\r\n"; the scrape does not go on in plaintext`, "PIPELINING",
		},
		"SMTP's 454": {
			"smtp", smtpGreeting, []string{"250-mx.example\r\n250 STARTTLS\r\n", "454 4.7.0 TLS not available due to local problem\r\n"}, "EHLO [127.0.0.1]\r\n|STARTTLS\r\n",
			`did not begin TLS: it answered STARTTLS with "454 4.7.0 TLS not available due to local problem\r\n", not 220`, "local problem",
		},
		"IMAP's NO": {
			"imap", "* OK ready\r\n", []string{"a1 NO STARTTLS unavailable here\r\n"}, "a1 STARTTLS\r\n",
			`it answered STARTTLS with "a1 NO STARTTLS unavailable here\r\n", not a1 OK`, "unavailable here",
		},
		"IMAP's BAD": {"imap", "* OK ready\r\n", []string{"* BYE no\r\na1 BAD unknown command\r\n"}, "a1 STARTTLS\r\n", `not a1 OK`, "unknown command"},
		"POP3's -ERR": {
			"pop3", "+OK ready\r\n", []string{"-ERR command not supported here\r\n"}, "STLS\r\n",
			`it answered STLS with "-ERR command not supported here\r\n", not +OK`, "not supported here",
		},
		"FTP's 534": {
			"ftp", "220 ready\r\n", []string{"534 request denied for policy reasons\r\n"}, "AUTH TLS\r\n",
			`it answered AUTH TLS with "534 request denied for policy reasons\r\n", not 234`, "policy reasons",
		},
		"SMTP's 554 greeting": {
			"smtp", "554 mx.example no service for you\r\n", nil, "",
			`the greeting of 127.0.0.1:`, "no service for you",
		},
		"an IMAP server already authenticated": {"imap", "* PREAUTH welcome back\r\n", nil, "", `does not begin with * OK, which request.starttls imap needs before it asks for TLS`, "welcome back"},
		"garbage to EHLO": {
			"smtp", smtpGreeting, []string{"HTTP/1.1 400 Bad Request\r\n"}, "EHLO [127.0.0.1]\r\n",
			`answered EHLO with "HTTP/1.1 400 Bad Request\r\n", not 250`, "Bad Request",
		},
		"garbage to STLS":                       {"pop3", "+OK ready\r\n", []string{"OK sure\r\n"}, "STLS\r\n", `answered STLS with "OK sure\r\n", not +OK`, "sure"},
		"a POP3 server where SMTP is asked for": {"smtp", "+OK ready\r\n", nil, "", `the greeting of 127.0.0.1:`, "+OK ready"},
	} {
		seen := make(chan string, 1)
		server := testutil.NewStartTLSStub(t, startTLSServer(tc.greeting, tc.replies, false, "", seen))
		_, err := tcpScrape(t, startTLSCollector(t, tc.protocol, "SECRET {{param_key:x}}\r\n", server.CAFile), server.Addr, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.said) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
			continue
		}
		if caller := model.CallerText(err); strings.Contains(caller, tc.said) || !strings.Contains(caller, "[withheld; in the exporter's log]") {
			t.Errorf("%s: the caller is told %q", name, caller)
		}
		if got := <-seen; got != tc.seen {
			t.Errorf("%s: the server saw %q, want %q", name, got, tc.seen)
		}
	}
}

// A server that closes the connection, says nothing until the deadline, or
// writes an answer past the response limit while the exchange asks for TLS
// fails the scrape saying which answer it waited for; one that writes more
// after its yes, before the handshake, fails it too, since those bytes came
// in plaintext and would be read as the server's over TLS.
func TestATCPSTARTTLSAnswersAreBounded(t *testing.T) {
	closing := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, "+OK ready\r\n")
		_, _ = testutil.ReadUntil(conn, "\n")
	})
	if _, err := tcpScrape(t, startTLSCollector(t, "pop3", "", closing.CAFile), closing.Addr, nil); err == nil || err.Error() != "the server "+closing.Addr+" closed the connection without answering STLS, which request.starttls pop3 sends" {
		t.Errorf("a server that closes: %v", err)
	}
	silent := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, "220 mx.example ESMTP\r\n")
		_, _ = testutil.ReadUntil(conn, "\n")
		_, _ = io.WriteString(conn, "250-mx.example\r\n250 STARTTLS\r\n")
		_, _ = io.Copy(io.Discard, conn)
	})
	start := time.Now()
	_, err := FetchCollector(context.Background(), silent.Addr, startTLSCollector(t, "smtp", "QUIT\r\n", silent.CAFile), RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "waiting for the answer of "+silent.Addr+" to STARTTLS, which request.starttls smtp sends, 0 bytes read") {
		t.Errorf("a server that does not answer: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s", elapsed)
	}
	silentGreeting := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) { _, _ = io.Copy(io.Discard, conn) })
	_, err = FetchCollector(context.Background(), silentGreeting.Addr, startTLSCollector(t, "imap", "", silentGreeting.CAFile), RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "waiting for the greeting of "+silentGreeting.Addr+", as request.starttls expects") {
		t.Errorf("a server that does not greet: %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	big := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, "* OK ready\r\n")
		_, _ = testutil.ReadUntil(conn, "\n")
		_, _ = io.WriteString(conn, strings.Repeat("* CAPABILITY IMAP4rev1\r\n", 100))
		<-release
	})
	_, err = tcpScrape(t, startTLSCollector(t, "imap", "", big.CAFile, func(c *model.Collector) { c.Request.MaxResponseBytes = 1024 }), big.Addr, nil)
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the answer of "+big.Addr+" to STARTTLS is larger than the response limit of 1024 bytes") {
		t.Errorf("an answer past the limit: %v", err)
	}
	seen := make(chan string, 1)
	injected := testutil.NewStartTLSStub(t, startTLSServer("+OK ready\r\n", []string{"+OK begin TLS\r\n+OK forged\r\n"}, false, "", seen))
	_, err = tcpScrape(t, startTLSCollector(t, "pop3", "QUIT\r\n", injected.CAFile), injected.Addr, nil)
	if err == nil || err.Error() != "the server "+injected.Addr+" wrote 12 bytes after it said to begin TLS and before the handshake; they came in plaintext, where anyone on the way could have written them, so the scrape does not read them as the server's" {
		t.Errorf("bytes after the yes: %v", err)
	}
	if got := <-seen; got != "STLS\r\n" {
		t.Errorf("the server saw %q", got)
	}
}

// With request.document the document says which protocol began TLS and how
// long asking for it took, starttls_seconds, beside tls_seconds and the
// greeting's, and holds the greeting and the answer read over TLS.
func TestATCPSTARTTLSDocument(t *testing.T) {
	d := startTLSDialogues["smtp"]
	server := testutil.NewStartTLSStub(t, startTLSServer(d.greeting, d.replies, true, "221 Bye\r\n", nil))
	doc := tcpDocumentOf(t, server.Addr, TCPConfig{Send: "QUIT\r\n"}, func(c *model.Collector) { c.Request.StartTLS, c.Request.TLS.CAFile = "smtp", server.CAFile })
	if doc["starttls"] != "smtp" {
		t.Errorf("starttls %v", doc["starttls"])
	}
	delete(doc, "starttls")
	if keys := documentKeys(t, "STARTTLS", doc, map[string]string{"greeting": d.greeting, "answer": "221 Bye\r\n"}); keys != "answer answer_seconds connect_seconds duration_seconds greeting greeting_seconds starttls_seconds tls_seconds" {
		t.Errorf("STARTTLS: %s", keys)
	}
}

// A debug probe's request section names the protocol in Tcp-Starttls and
// shows, after the greeting, each command sent and the server's answer,
// quoted, and the TLS version begun, before how reading the answer stopped.
func TestATCPSTARTTLSIsTraced(t *testing.T) {
	d := startTLSDialogues["pop3"]
	server := testutil.NewStartTLSStub(t, startTLSServer(d.greeting, d.replies, true, "+OK bye\r\n", nil))
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.Addr, startTLSCollector(t, "pop3", "QUIT\r\n", server.CAFile), RequestOverrides{Timeout: 5 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	r := trace.Requests()
	want := `greeted with "+OK Dovecot ready.\r\n"; sent "STLS\r\n", answered "+OK Begin TLS negotiation now.\r\n"; began TLS 1.3; read 9 bytes, the server closed the connection`
	if len(r) != 1 || r[0].Header.Get("Tcp-Starttls") != "pop3" || r[0].URL != "tls://"+server.Addr || r[0].Outcome != want {
		t.Errorf("traced %+v\nwant %s", r, want)
	}
}

// request.starttls is checked when the collector loads: one of the four
// protocols, and no terminator without send, since nothing is read after
// the handshake then. It goes with a tls block, whose settings the
// handshake takes, with request.greeting and with request.document.
func TestATCPSTARTTLSIsCheckedAtLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		starttls string
		own      TCPConfig
		want     string
	}{
		"an unknown protocol":   {"ldap", TCPConfig{}, `collector "stats" request.starttls "ldap" is not a protocol whose STARTTLS a tcp collector speaks; want smtp, imap, pop3 or ftp`},
		"upper case":            {"SMTP", TCPConfig{}, `request.starttls "SMTP" is not a protocol`},
		"a terminator, no send": {"smtp", TCPConfig{Terminator: "\r\n"}, `collector "stats" sets request.starttls and request.terminator without request.send`},
	} {
		c := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP, Own: tc.own, StartTLS: tc.starttls}}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.StartTLS = TCPConfig{Greeting: `^\* OK`, Send: "a LOGOUT\r\n", Terminator: "a OK", Document: true}, "imap"
		c.Request.TLS = model.TLSConfig{CAFile: "/ca.pem", CertFile: "/c.pem", KeyFile: "/k.pem", ServerName: "mail.example.com"}
	})
	tcpCollector(t, func(c *model.Collector) { c.Request.StartTLS = "ftp" })
}
