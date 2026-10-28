//go:build !select_request_types || request_type_tlscert

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

// certStartTLSServer is a server that greets, answers each line the client
// sends in plaintext with the next of replies, and then, with upgrade,
// makes the server's side of the handshake, as a mail or FTP server that
// has said yes to STARTTLS does. What it saw is sent on seen, joined with
// '|': each line read in plaintext, "TLS" once the handshake is made, or
// "handshake: " and why it failed, and "closed" when the client closed
// before a line; without upgrade, "then" and anything that came after the
// replies.
func certStartTLSServer(greeting string, replies []string, upgrade bool, seen chan<- string) func(net.Conn, func() (net.Conn, error)) {
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
		if _, err := begin(); err != nil {
			got = append(got, "handshake: "+err.Error())
			return
		}
		got = append(got, "TLS")
	}
}

// certStartTLSDialogues are the plaintext side of each protocol's
// STARTTLS, as a server that offers it speaks it, and what the client is to
// send.
var certStartTLSDialogues = map[string]struct {
	greeting string
	replies  []string
	sent     string
}{
	"smtp": {
		"220-mx.example ESMTP Postfix\r\n220 ready\r\n",
		[]string{"250-mx.example\r\n250-PIPELINING\r\n250-STARTTLS\r\n250 8BITMIME\r\n", "220 2.0.0 Ready to start TLS\r\n"},
		"EHLO [127.0.0.1]\r\n|STARTTLS\r\n",
	},
	"imap": {
		"* OK [CAPABILITY IMAP4rev1 STARTTLS LOGINDISABLED] Dovecot ready.\r\n",
		[]string{"* CAPABILITY IMAP4rev1 STARTTLS\r\na1 OK Begin TLS negotiation now.\r\n"},
		"a1 STARTTLS\r\n",
	},
	"pop3": {"+OK Dovecot ready.\r\n", []string{"+OK Begin TLS negotiation now.\r\n"}, "STLS\r\n"},
	"ftp":  {"220 ProFTPD ready\r\n", []string{"234 AUTH TLS successful\r\n"}, "AUTH TLS\r\n"},
}

// certStartTLSCollector is a tlscert collector speaking protocol's
// STARTTLS, verifying the server with caFile.
func certStartTLSCollector(t *testing.T, protocol, caFile string, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	return tlsCertCollector(t, model.TLSConfig{CAFile: caFile}, append([]func(*model.Collector){func(c *model.Collector) { c.Request.StartTLS = protocol }}, edit...)...)
}

// With request.starttls a tlscert collector reads the greeting, asks for
// TLS as each protocol does, SMTP's EHLO naming the client by its address,
// and makes the handshake once the server has said yes: the document is
// the certificate the server presents after it, verified against ca_file,
// and says which protocol began TLS; nothing is sent over TLS. The label is
// tls://, as without it.
func TestATLSCertCollectorBeginsTLSWithSTARTTLS(t *testing.T) {
	for protocol, d := range certStartTLSDialogues {
		seen := make(chan string, 1)
		server := testutil.NewStartTLSStub(t, certStartTLSServer(d.greeting, d.replies, true, seen))
		c := certStartTLSCollector(t, protocol, server.CAFile)
		doc, err := handshakeDoc(t, c, server.Addr)
		if err != nil {
			t.Errorf("%s: %v", protocol, err)
			continue
		}
		if got, want := <-seen, d.sent+"|TLS"; got != want {
			t.Errorf("%s: the server saw %q, want %q", protocol, got, want)
		}
		if !doc.Verified || doc.StartTLS != protocol || doc.Host != "127.0.0.1" || doc.TLSVersion != "TLS 1.3" || len(doc.Certificates) == 0 || doc.Chain == nil || doc.Certificates[0].SecondsUntilExpiry <= 0 {
			t.Errorf("%s: document %+v", protocol, doc)
		}
		if label, err := RequestLabelFor(server.Addr, c, RequestOverrides{}); err != nil || label != "tls://"+server.Addr {
			t.Errorf("%s: label %q, %v", protocol, label, err)
		}
	}
	// Without it the document says so with "".
	// The stub's handshake is made on its first read.
	plain := testutil.NewTLSTCPStub(t, func(conn net.Conn) { _, _ = conn.Read(make([]byte, 1)) })
	doc, err := handshakeDoc(t, tlsCertCollector(t, model.TLSConfig{CAFile: plain.CAFile}), plain.Addr)
	if err != nil || doc.StartTLS != "" || !doc.Verified {
		t.Errorf("without starttls: %+v, %v", doc, err)
	}
}

// A target without a port is at the protocol's own with request.starttls:
// SMTP's 25, IMAP's 143, POP3's 110 and FTP's 21; 443 without it. A port
// written is the one dialed, as :587 for SMTP submission.
func TestATLSCertSTARTTLSTargetDefaultsToTheProtocolsPort(t *testing.T) {
	for protocol, port := range map[string]string{"smtp": "25", "imap": "143", "pop3": "110", "ftp": "21", "": "443"} {
		c := tlsCertCollector(t, model.TLSConfig{}, func(c *model.Collector) { c.Request.StartTLS = protocol })
		for target, label := range map[string]string{"mail.example.test": "tls://mail.example.test:" + port, "[2001:db8::25]": "tls://[2001:db8::25]:" + port, "mail.example.test:587": "tls://mail.example.test:587"} {
			if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != label {
				t.Errorf("%q, %s: label %q, %v, want %s", protocol, target, got, err, label)
			}
		}
	}
	// The port dialed is the label's: a server listening on the default
	// port is not reached when the target names another.
	address, err := parseTLSCertTarget("127.0.0.1", "smtp")
	if err != nil || address.hostPort != "127.0.0.1:25" || address.port != 25 {
		t.Errorf("dialed %+v, %v", address, err)
	}
}

// A server that does not offer STARTTLS, refuses it, greets otherwise or
// answers what is not its protocol's fails the scrape naming the step, and
// nothing more is sent, no handshake begun. The server's words are in the
// error and withheld from a probe's caller.
func TestATLSCertSTARTTLSThatIsNotBegunFailsTheScrape(t *testing.T) {
	for name, tc := range map[string]struct {
		protocol, greeting string
		replies            []string
		seen, want, said   string
	}{
		"SMTP without STARTTLS": {
			"smtp", "220 mx.example ESMTP\r\n", []string{"250-mx.example\r\n250 PIPELINING\r\n"}, "EHLO [127.0.0.1]\r\n",
			`does not offer STARTTLS in its answer to EHLO, "250-mx.example\r\n250 PIPELINING\r\n"; the scrape does not go on in plaintext`, "PIPELINING",
		},
		"SMTP's 454": {
			"smtp", "220 mx.example ESMTP\r\n", []string{"250-mx.example\r\n250 STARTTLS\r\n", "454 4.7.0 TLS not available due to local problem\r\n"}, "EHLO [127.0.0.1]\r\n|STARTTLS\r\n",
			`did not begin TLS: it answered STARTTLS with "454 4.7.0 TLS not available due to local problem\r\n", not 220`, "local problem",
		},
		"IMAP's NO":    {"imap", "* OK ready\r\n", []string{"a1 NO STARTTLS unavailable here\r\n"}, "a1 STARTTLS\r\n", `not a1 OK`, "unavailable here"},
		"POP3's -ERR":  {"pop3", "+OK ready\r\n", []string{"-ERR command not supported here\r\n"}, "STLS\r\n", `answered STLS with "-ERR command not supported here\r\n", not +OK`, "not supported here"},
		"FTP's 534":    {"ftp", "220 ready\r\n", []string{"534 request denied for policy reasons\r\n"}, "AUTH TLS\r\n", `answered AUTH TLS with "534 request denied for policy reasons\r\n", not 234`, "policy reasons"},
		"a 554 banner": {"smtp", "554 mx.example no service for you\r\n", nil, "", `does not begin with 220, which request.starttls smtp needs before it asks for TLS`, "no service for you"},
		"garbage to EHLO": {
			"smtp", "220 mx.example ESMTP\r\n", []string{"HTTP/1.1 400 Bad Request\r\n"}, "EHLO [127.0.0.1]\r\n",
			`answered EHLO with "HTTP/1.1 400 Bad Request\r\n", not 250`, "Bad Request",
		},
		"an HTTPS server's silence turned to garbage": {"imap", "SSH-2.0-OpenSSH_9.6\r\n", nil, "", `does not begin with * OK`, "OpenSSH"},
	} {
		seen := make(chan string, 1)
		server := testutil.NewStartTLSStub(t, certStartTLSServer(tc.greeting, tc.replies, false, seen))
		_, err := handshakeDoc(t, certStartTLSCollector(t, tc.protocol, server.CAFile), server.Addr)
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
// writes an answer past the response limit fails the scrape saying which
// answer it waited for, the limit being limits.max_response_bytes, since a
// tlscert collector has no request.max_response_bytes; one that writes more
// after its yes, before the handshake, fails it, since those bytes came in
// plaintext.
func TestATLSCertSTARTTLSAnswersAreBounded(t *testing.T) {
	closing := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, "+OK ready\r\n")
		_, _ = testutil.ReadUntil(conn, "\n")
	})
	if _, err := handshakeDoc(t, certStartTLSCollector(t, "pop3", closing.CAFile), closing.Addr); err == nil || err.Error() != "the server "+closing.Addr+" closed the connection without answering STLS, which request.starttls pop3 sends" {
		t.Errorf("a server that closes: %v", err)
	}
	silent := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, "220 mx.example ESMTP\r\n")
		_, _ = testutil.ReadUntil(conn, "\n")
		_, _ = io.WriteString(conn, "250-mx.example\r\n250 STARTTLS\r\n")
		_, _ = io.Copy(io.Discard, conn)
	})
	start := time.Now()
	_, err := FetchCollector(context.Background(), silent.Addr, certStartTLSCollector(t, "smtp", silent.CAFile), RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "waiting for the answer of "+silent.Addr+" to STARTTLS, which request.starttls smtp sends, 0 bytes read") {
		t.Errorf("a server that does not answer: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s", elapsed)
	}
	silentGreeting := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) { _, _ = io.Copy(io.Discard, conn) })
	_, err = FetchCollector(context.Background(), silentGreeting.Addr, certStartTLSCollector(t, "imap", silentGreeting.CAFile), RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "waiting for the greeting of "+silentGreeting.Addr+", as request.starttls expects") {
		t.Errorf("a server that does not greet: %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	big := testutil.NewStartTLSStub(t, func(conn net.Conn, _ func() (net.Conn, error)) {
		_, _ = io.WriteString(conn, strings.Repeat("220-banner\r\n", 200))
		<-release
	})
	_, err = handshakeDoc(t, certStartTLSCollector(t, "smtp", big.CAFile, func(c *model.Collector) { c.Limits.MaxResponseBytes = 1024 }), big.Addr)
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the greeting of "+big.Addr+" is larger than the response limit of 1024 bytes; raise limits.max_response_bytes") {
		t.Errorf("a greeting past the limit: %v", err)
	}
	seen := make(chan string, 1)
	injected := testutil.NewStartTLSStub(t, certStartTLSServer("+OK ready\r\n", []string{"+OK begin TLS\r\n+OK forged\r\n"}, false, seen))
	_, err = handshakeDoc(t, certStartTLSCollector(t, "pop3", injected.CAFile), injected.Addr)
	if err == nil || !strings.HasPrefix(err.Error(), "the server "+injected.Addr+" wrote 12 bytes after it said to begin TLS and before the handshake") {
		t.Errorf("bytes after the yes: %v", err)
	}
	if got := <-seen; got != "STLS\r\n" {
		t.Errorf("the server saw %q", got)
	}
}

// A debug probe's request section names the protocol in Tls-Starttls and
// shows the greeting, each command sent and the server's answer, quoted,
// before the handshake's outcome; a failure is the step's error.
func TestATLSCertSTARTTLSIsTraced(t *testing.T) {
	d := certStartTLSDialogues["pop3"]
	server := testutil.NewStartTLSStub(t, certStartTLSServer(d.greeting, d.replies, true, nil))
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.Addr, certStartTLSCollector(t, "pop3", server.CAFile), RequestOverrides{Timeout: 5 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	r := trace.Requests()
	want := `greeted with "+OK Dovecot ready.\r\n"; sent "STLS\r\n", answered "+OK Begin TLS negotiation now.\r\n"; TLS 1.3, 1 certificate, verified`
	if len(r) != 1 || r[0].Method != "HANDSHAKE" || r[0].Header.Get("Tls-Starttls") != "pop3" || r[0].URL != "tls://"+server.Addr || r[0].Outcome != want {
		t.Errorf("traced %+v\nwant %s", r, want)
	}
	refusing := testutil.NewStartTLSStub(t, certStartTLSServer("+OK ready\r\n", []string{"-ERR no\r\n"}, false, nil))
	ctx, trace = WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, refusing.Addr, certStartTLSCollector(t, "pop3", refusing.CAFile), RequestOverrides{Timeout: 5 * time.Second}, nil); err == nil {
		t.Fatal("a refusal was taken")
	}
	if r := trace.Requests(); len(r) != 1 || r[0].Outcome != `error: the server `+refusing.Addr+` did not begin TLS: it answered STLS with "-ERR no\r\n", not +OK` {
		t.Errorf("traced %+v", r)
	}
}

// request.starttls is checked when the collector loads: one of the four
// protocols. It goes with a tls block, whose settings the handshake takes.
func TestATLSCertSTARTTLSIsCheckedAtLoad(t *testing.T) {
	for _, protocol := range []string{"ldap", "SMTP", "starttls"} {
		c := &model.Collector{Name: "certs", Request: model.RequestConfig{Type: RequestTypeTLSCert, StartTLS: protocol}}
		want := `collector "certs" request.starttls "` + protocol + `" is not a protocol whose STARTTLS a tlscert collector speaks; want smtp, imap, pop3 or ftp`
		if err := ValidateRequest(c); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", protocol, err, want)
		}
	}
	tlsCertCollector(t, model.TLSConfig{CAFile: "/ca.pem", CertFile: "/c.pem", KeyFile: "/k.pem", ServerName: "mail.example.com"}, func(c *model.Collector) { c.Request.StartTLS = "imap" })
}
