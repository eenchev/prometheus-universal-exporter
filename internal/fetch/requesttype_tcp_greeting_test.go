//go:build request_type_tcp

package fetch

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// greeter is a server that speaks first, as SMTP, FTP, POP3, IMAP and SSH
// do: it waits for wait, notes whether the client wrote anything before it
// greeted, writes greeting, and then answers the line it reads with answer,
// closing the connection, or, with answer empty, keeps it open until the
// client closes it. What it saw is sent on seen: "early " and what came
// before the greeting, or what came after it.
func greeter(wait time.Duration, greeting, answer string, seen chan<- string) func(net.Conn) {
	return func(conn net.Conn) {
		time.Sleep(wait)
		_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		early := make([]byte, 64)
		if n, _ := conn.Read(early); n > 0 {
			if seen != nil {
				seen <- "early " + string(early[:n])
			}
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		_, _ = io.WriteString(conn, greeting)
		if answer == "" {
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		line, _ := testutil.ReadUntil(conn, "\n")
		if seen != nil {
			seen <- line
		}
		_, _ = io.WriteString(conn, answer)
	}
}

// greetingScrape is a scrape of target through a collector expecting
// greeting and sending send.
func greetingScrape(t *testing.T, target, greeting, send string, edit ...func(*model.Collector)) (*HTTPResponse, error) {
	t.Helper()
	c := tcpCollector(t, append([]func(*model.Collector){func(c *model.Collector) { c.Request.Own = TCPConfig{Greeting: greeting, Send: send} }}, edit...)...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return FetchCollector(ctx, target, c, RequestOverrides{}, nil)
}

// seconds is a header's number of seconds, failing the test when it is
// none.
func seconds(t *testing.T, resp *HTTPResponse, header string) float64 {
	t.Helper()
	text := resp.Headers.Get(header)
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value < 0 {
		t.Fatalf("%s %q: %v", header, text, err)
	}
	return value
}

// With request.greeting the server's greeting is read, and matched, before
// send is written: an SMTP server greeting on one line, one greeting over
// several "220-" lines as SMTP and FTP may, ending with "220 " or "220"
// alone, each sees QUIT only after it has greeted, and the answer to QUIT
// is the body; the greeting's lines are X-Tcp-Greeting.
func TestATCPGreetingIsReadBeforeSendIsWritten(t *testing.T) {
	for name, tc := range map[string]struct {
		greeting string
		lines    []string
	}{
		"one line":           {"220 mx.example ESMTP Postfix\r\n", []string{"220 mx.example ESMTP Postfix"}},
		"continued":          {"220-mx.example ESMTP\r\n220-no UCE\r\n220 ready\r\n", []string{"220-mx.example ESMTP", "220-no UCE", "220 ready"}},
		"FTP's free lines":   {"220-Welcome\r\n  to the archive\r\n230 not the end\r\n220 Ready\r\n", []string{"220-Welcome", "  to the archive", "230 not the end", "220 Ready"}},
		"the code alone":     {"220-first\r\n220\r\n", []string{"220-first", "220"}},
		"a line feed alone":  {"220-first\n220 ready\n", []string{"220-first", "220 ready"}},
		"no code, one line":  {"* OK [CAPABILITY IMAP4rev1] ready\r\n", []string{"* OK [CAPABILITY IMAP4rev1] ready"}},
		"four digits, a '-'": {"2200-x\r\n", []string{"2200-x"}},
	} {
		seen := make(chan string, 1)
		server := testutil.NewTCPStub(t, greeter(0, tc.greeting, "221 Bye\r\n", seen))
		resp, err := greetingScrape(t, server.Addr, `^(220|\* OK)`, "QUIT\r\n")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := <-seen; got != "QUIT\r\n" {
			t.Errorf("%s: the server saw %q", name, got)
		}
		if string(resp.Body) != "221 Bye\r\n" || strings.Join(resp.Headers.Values("X-Tcp-Greeting"), "|") != strings.Join(tc.lines, "|") {
			t.Errorf("%s: body %q, greeting %q", name, resp.Body, resp.Headers.Values("X-Tcp-Greeting"))
		}
	}
}

// What a server writes after its greeting, in the same write, is the start
// of the answer, not lost with the greeting's read.
func TestWhatFollowsATCPGreetingIsTheAnswers(t *testing.T) {
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "220 mx.example ESMTP\r\n250 early\r\n")
		if line, err := testutil.ReadUntil(conn, "\n"); err == nil && line == "QUIT\r\n" {
			_, _ = io.WriteString(conn, "221 Bye\r\n")
		}
	})
	resp, err := greetingScrape(t, server.Addr, "^220 ", "QUIT\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "250 early\r\n221 Bye\r\n" || resp.Headers.Get("X-Tcp-Greeting") != "220 mx.example ESMTP" {
		t.Errorf("body %q, headers %v", resp.Body, resp.Headers)
	}
}

// Without send the greeting is the answer, and nothing more is read, though
// the server, as an SSH server does after its banner, keeps the connection
// open waiting for the client.
func TestATCPGreetingWithoutSendIsTheAnswer(t *testing.T) {
	server := testutil.NewTCPStub(t, greeter(0, "SSH-2.0-OpenSSH_9.6p1 Ubuntu-3ubuntu13\r\n", "", nil))
	start := time.Now()
	resp, err := greetingScrape(t, server.Addr, `^SSH-2\.0-OpenSSH_(9|[1-9][0-9])\.`, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "SSH-2.0-OpenSSH_9.6p1 Ubuntu-3ubuntu13\r\n" || resp.Headers.Get("X-Tcp-Answer-Seconds") != "" {
		t.Errorf("body %q, headers %v", resp.Body, resp.Headers)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s", elapsed)
	}
	if method := RequestMethodFor(tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Greeting: "^SSH-"} }), RequestOverrides{}); method != "READ" {
		t.Errorf("method %q", method)
	}
}

// Every scrape's answer carries how long its steps took, in seconds:
// X-Tcp-Connect-Seconds and X-Tcp-Answer-Seconds always, X-Tcp-Tls-Seconds
// with TLS and X-Tcp-Greeting-Seconds with a greeting; a greeting that
// comes 300ms late takes as long, and the body is the answer alone, as a
// collector without a greeting had it.
func TestATCPScrapeReportsItsTimings(t *testing.T) {
	slow := testutil.NewTCPStub(t, greeter(300*time.Millisecond, "+OK POP3 ready\r\n", "+OK bye\r\n", nil))
	resp, err := greetingScrape(t, slow.Addr, `^\+OK`, "QUIT\r\n")
	if err != nil {
		t.Fatal(err)
	}
	connect, greet, answer := seconds(t, resp, "X-Tcp-Connect-Seconds"), seconds(t, resp, "X-Tcp-Greeting-Seconds"), seconds(t, resp, "X-Tcp-Answer-Seconds")
	if connect > 2 || greet < 0.29 || greet > 4 || answer > 2 || resp.Headers.Get("X-Tcp-Tls-Seconds") != "" || string(resp.Body) != "+OK bye\r\n" {
		t.Errorf("connect %g, greeting %g, answer %g, headers %v, body %q", connect, greet, answer, resp.Headers, resp.Body)
	}
	// A collector without a greeting: the answer as before, its timings
	// beside it.
	server := testutil.NewTCPStub(t, memcached(nil))
	plain := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"} })
	resp, err = FetchCollector(context.Background(), server.Addr, plain, RequestOverrides{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seconds(t, resp, "X-Tcp-Connect-Seconds")
	seconds(t, resp, "X-Tcp-Answer-Seconds")
	if len(resp.Headers) != 2 || string(resp.Body) != memcachedStats {
		t.Errorf("headers %v, body %q", resp.Headers, resp.Body)
	}
}

// A server that speaks TLS from the start, as SMTPS on port 465 does,
// greets after the handshake: the greeting is read over TLS, and the
// handshake's time is X-Tcp-Tls-Seconds.
func TestATCPGreetingComesAfterTheTLSHandshake(t *testing.T) {
	seen := make(chan string, 1)
	server := testutil.NewTLSTCPStub(t, greeter(0, "220 smtps.example ESMTP\r\n", "221 Bye\r\n", seen))
	resp, err := greetingScrape(t, server.Addr, `^220 `, "QUIT\r\n", func(c *model.Collector) { c.Request.TLS.CAFile = server.CAFile })
	if err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "QUIT\r\n" || string(resp.Body) != "221 Bye\r\n" || resp.Headers.Get("X-Tcp-Greeting") != "220 smtps.example ESMTP" {
		t.Errorf("seen %q, body %q, headers %v", got, resp.Body, resp.Headers)
	}
	seconds(t, resp, "X-Tcp-Tls-Seconds")
	seconds(t, resp, "X-Tcp-Greeting-Seconds")
}

// A greeting that does not match fails the scrape, quoting the greeting and
// the pattern, the greeting withheld from a probe's caller; send is never
// written. A server that closes without a greeting, one that closes in the
// middle of it, one that never greets and one whose greeting outgrows the
// response limit fail it too.
func TestATCPGreetingThatIsNotThereFailsTheScrape(t *testing.T) {
	seen := make(chan string, 1)
	refusing := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "554 mx.example no service for 192.0.2.7\r\n")
		after, _ := io.ReadAll(conn)
		seen <- string(after)
	})
	_, err := greetingScrape(t, refusing.Addr, `^220[ -]`, "QUIT\r\n")
	want := `the greeting of ` + refusing.Addr + `, "554 mx.example no service for 192.0.2.7\r\n", does not match request.greeting "^220[ -]"`
	if err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	if caller := model.CallerText(err); caller != `the greeting of `+refusing.Addr+`, [withheld; in the exporter's log], does not match request.greeting "^220[ -]"` {
		t.Errorf("the caller is told %q", caller)
	}
	if got := <-seen; got != "" {
		t.Errorf("send was written after a greeting that did not match: %q", got)
	}

	closing := testutil.NewTCPStub(t, func(net.Conn) {})
	if _, err := greetingScrape(t, closing.Addr, `^220 `, "QUIT\r\n"); err == nil || err.Error() != "the server "+closing.Addr+" closed the connection without a greeting; request.greeting expects one" {
		t.Errorf("no greeting: %v", err)
	}
	cut := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "220-mx.example\r\n220 rea") })
	_, err = greetingScrape(t, cut.Addr, `^220`, "QUIT\r\n")
	if err == nil || err.Error() != `the server `+cut.Addr+` closed the connection before its greeting ended, after 23 bytes: "220-mx.example\r\n220 rea"` || strings.Contains(model.CallerText(err), "mx.example") {
		t.Errorf("a greeting cut short: %v, the caller told %q", err, model.CallerText(err))
	}

	silent := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Greeting: "^220", Send: "QUIT\r\n"} })
	start := time.Now()
	_, err = FetchCollector(context.Background(), silent.Addr, c, RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "waiting for the greeting of "+silent.Addr+", as request.greeting expects, 0 bytes read") {
		t.Errorf("a server that never greets: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s", elapsed)
	}

	release := make(chan struct{})
	defer close(release)
	big := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, strings.Repeat("x", 2048))
		<-release
	})
	_, err = greetingScrape(t, big.Addr, "^x", "", func(c *model.Collector) { c.Request.MaxResponseBytes = 1024 })
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the greeting of "+big.Addr+" is larger than the response limit of 1024 bytes") {
		t.Errorf("a greeting past the limit: %v", err)
	}
	exact := testutil.NewTCPStub(t, greeter(0, strings.Repeat("x", 1023)+"\n", "", nil))
	if resp, err := greetingScrape(t, exact.Addr, "^x", "", func(c *model.Collector) { c.Request.MaxResponseBytes = 1024 }); err != nil || len(resp.Body) != 1024 {
		t.Errorf("a greeting of the limit: %v", err)
	}
}

// request.greeting is checked when the collector loads: a pattern Go reads,
// no placeholder, at most 64 KiB, and no terminator without send, since
// nothing is read after the greeting then.
func TestATCPGreetingIsCheckedAtLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		own  TCPConfig
		want string
	}{
		"not a pattern":         {TCPConfig{Greeting: "^220 (", Send: "QUIT\r\n"}, `collector "stats" request.greeting "^220 (" is not a regular expression Go reads: error parsing regexp: missing closing )`},
		"a backreference":       {TCPConfig{Greeting: `^(\d+) \1`}, "is not a regular expression Go reads"},
		"a placeholder":         {TCPConfig{Greeting: "^{{param_code}} "}, `request.greeting "^{{param_code}} " has a placeholder`},
		"too long":              {TCPConfig{Greeting: strings.Repeat("x", 64<<10+1)}, "request.greeting is 65537 bytes, more than the 65536"},
		"a terminator, no send": {TCPConfig{Greeting: "^220 ", Terminator: "\r\n"}, `collector "stats" sets request.greeting and request.terminator without request.send`},
	} {
		c := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP, Own: tc.own}}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{Greeting: "^220 ", Send: "QUIT\r\n", Terminator: "\r\n"}
	})
}

// A debug probe's request section shows the greeting step: the pattern in
// Tcp-Greeting, listed before Tcp-Send, and the greeting read, quoted,
// before how reading the answer stopped; a greeting that did not match is
// shown whole in the error.
func TestATCPGreetingIsTraced(t *testing.T) {
	server := testutil.NewTCPStub(t, greeter(0, "220 mx.example ESMTP\r\n", "221 Bye\r\n", nil))
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.Addr, tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Greeting: "^220 ", Send: "QUIT\r\n"} }), RequestOverrides{Timeout: 5 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	r := trace.Requests()
	if len(r) != 1 || r[0].Header.Get("Tcp-Greeting") != `"^220 "` || r[0].Header.Get("Tcp-Send") != `"QUIT\r\n"` ||
		r[0].Outcome != `greeted with "220 mx.example ESMTP\r\n", which matched request.greeting; read 9 bytes, the server closed the connection` {
		t.Errorf("traced %+v", r)
	}
	banner := testutil.NewTCPStub(t, greeter(0, "SSH-2.0-OpenSSH_9.6\r\n", "", nil))
	ctx, trace = WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, banner.Addr, tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Greeting: "^SSH-2"} }), RequestOverrides{Timeout: 5 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	if r := trace.Requests(); len(r) != 1 || r[0].Method != "READ" || r[0].Outcome != `greeted with "SSH-2.0-OpenSSH_9.6\r\n", which matched request.greeting; nothing sent, the greeting is the answer` {
		t.Errorf("traced %+v", r)
	}
	ctx, trace = WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, banner.Addr, tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Greeting: "^220 "} }), RequestOverrides{Timeout: 5 * time.Second}, nil); err == nil {
		t.Fatal("a greeting that does not match was taken")
	}
	if r := trace.Requests(); len(r) != 1 || r[0].Outcome != `error: the greeting of `+banner.Addr+`, "SSH-2.0-OpenSSH_9.6\r\n", does not match request.greeting "^220 "` {
		t.Errorf("traced %+v", r)
	}
}
