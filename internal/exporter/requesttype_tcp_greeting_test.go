//go:build request_type_tcp

package exporter

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// greetingExporter is an exporter of a collector checking an SSH server's
// banner, with a regex rule reading the version from it, and one checking
// an SMTP server, with jq rules reading the answer to QUIT and the
// scrape's timings from request.document, and the stand-in server that
// greets as each, by its port: sshd's banner, or an SMTP greeting over two
// lines and 221 to QUIT. The logs are returned beside it.
func greetingExporter(t *testing.T, ssh, smtp string) (*Server, string, string, *strings.Builder) {
	t.Helper()
	banner := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, ssh)
		_, _ = io.Copy(io.Discard, conn)
	})
	mail := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, smtp)
		if line, err := testutil.ReadUntil(conn, "\n"); err == nil && line == "QUIT\r\n" {
			_, _ = io.WriteString(conn, "221 2.0.0 Bye\r\n")
		}
	})
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", `collectors:
  - name: ssh
    request:
      type: tcp
      greeting: '^SSH-2\.0-'
    transform:
      type: regex
    metrics:
      - name: ssh_openssh_version
        expression: '^SSH-2\.0-OpenSSH_(\d+\.\d+)'
  - name: smtp
    request:
      type: tcp
      greeting: '^220[ -]'
      send: "QUIT\r\n"
      document: true
    transform:
      type: jq
    metrics:
      - name: smtp_up
        expression: 'if (.answer | startswith("221")) then 1 else 0 end'
      - name: tcp_connect_seconds
        expression: .connect_seconds
      - name: tcp_greeting_seconds
        expression: .greeting_seconds
      - name: tcp_answer_seconds
        expression: .answer_seconds
      - name: tcp_duration_seconds
        expression: .duration_seconds
      - name: tcp_header_connect_seconds
        expression: '$headers["x-tcp-connect-seconds"] | tonumber'
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	return NewServer(config.NewManager(cfg, path, logger), "python3", logger), banner.Addr, mail.Addr, &logs
}

// Through the exporter, a greeting-only check answers what its regex rule
// reads from the greeting, which is the body; a greeting that does not
// match fails the probe in the tcp stage, its caller told the pattern with
// the greeting withheld, while the log and a debug probe's report show the
// greeting whole; and the report's request section shows the greeting
// step before the bytes sent.
func TestATCPGreetingThroughTheExporter(t *testing.T) {
	exp, ssh, smtp, logs := greetingExporter(t, "SSH-2.0-OpenSSH_9.6\r\n", "554 5.3.2 mx.internal.example has no service for you\r\n")
	recorder := probeOnce(t, exp, "/probe?collector=ssh&target="+ssh, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != "ssh_openssh_version 9.6" {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	recorder = probeOnce(t, exp, "/probe?collector=smtp&target="+smtp, nil)
	body := recorder.Body.String()
	if recorder.Code == http.StatusOK || !strings.Contains(body, "collector smtp tcp failed: the greeting of "+smtp+", [withheld; in the exporter's log], does not match request.greeting \"^220[ -]\"") || strings.Contains(body, "mx.internal.example") {
		t.Errorf("a greeting that does not match: %d %s", recorder.Code, body)
	}
	if !strings.Contains(logs.String(), "mx.internal.example has no service for you") {
		t.Errorf("the log does not show the greeting:\n%s", logs)
	}
	exp.SetDebug(DebugFull)
	report := probeOnce(t, exp, "/probe?debug=true&collector=smtp&target="+smtp, nil).Body.String()
	for _, want := range []string{`Tcp-Greeting: "^220[ -]"` + "\n" + `     Tcp-Send: "QUIT\r\n"`, `"554 5.3.2 mx.internal.example has no service for you\r\n", does not match request.greeting`} {
		if !strings.Contains(report, want) {
			t.Errorf("no %q in the report:\n%s", want, report)
		}
	}
}

// The timings of a scrape reach plain jq rules through request.document,
// with no script: a greeting over two lines, then QUIT, gives an up series
// read from the answer and the connection's, the greeting's, the answer's
// and the whole exchange's seconds, and $headers still has them; a debug
// probe's report shows the greeting step and the timings among the
// answer's headers.
func TestATCPScrapesTimingsReachTheRules(t *testing.T) {
	exp, _, smtp, _ := greetingExporter(t, "", "220-mx.example ESMTP\r\n220 ready\r\n")
	recorder := probeOnce(t, exp, "/probe?collector=smtp&target="+smtp, nil)
	got := seriesLines(recorder.Body.String())
	if recorder.Code != http.StatusOK || len(got) != 6 {
		t.Fatalf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	for i, prefix := range []string{"smtp_up 1", "tcp_answer_seconds ", "tcp_connect_seconds ", "tcp_duration_seconds ", "tcp_greeting_seconds ", "tcp_header_connect_seconds "} {
		if !strings.HasPrefix(got[i], prefix) {
			t.Errorf("series %q, want %q", got[i], prefix)
		}
	}
	exp.SetDebug(DebugFull)
	report := probeOnce(t, exp, "/probe?debug=true&collector=smtp&target="+smtp, nil).Body.String()
	for _, want := range []string{`greeted with "220-mx.example ESMTP\r\n220 ready\r\n", which matched request.greeting; read 15 bytes, the server closed the connection`, "X-Tcp-Greeting: 220-mx.example ESMTP", "X-Tcp-Greeting: 220 ready", "X-Tcp-Connect-Seconds: "} {
		if !strings.Contains(report, want) {
			t.Errorf("no %q in the report:\n%s", want, report)
		}
	}
}

// A python transform reads the timings and the greeting from
// response.headers, the greeting a value for each of its lines.
func TestATCPScrapesTimingsReachPython(t *testing.T) {
	requirePython(t)
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		if line, err := testutil.ReadUntil(conn, "\n"); err == nil && line == "a LOGOUT\r\n" {
			_, _ = io.WriteString(conn, "* BYE\r\na OK LOGOUT completed\r\n")
		}
	})
	exp := labelServer(t, `collectors:
  - name: imap
    request:
      type: tcp
      greeting: '^\* OK'
      send: "a LOGOUT\r\n"
    transform:
      type: python
      script: |
        metric(name="imap_greeting_lines", value=len(response.headers["X-Tcp-Greeting"]))
        metric(name="imap_connect_ok", value=1 if float(response.header("x-tcp-connect-seconds")) >= 0 else 0)
        metric(name="imap_logout_ok", value=1 if "a OK" in response.text else 0)
    metrics: []
`)
	recorder := probeOnce(t, exp, "/probe?collector=imap&target="+server.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != "imap_connect_ok 1\nimap_greeting_lines 1\nimap_logout_ok 1" {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
}

// A greeting holding control characters reaches a debug probe's report
// through X-Tcp-Greeting escaped, a carriage return as \r and an escape as
// \x1b, so it neither rewrites the report's line on a terminal nor acts on
// it; a header without one, a tab in it included, is shown as it is.
func TestATCPGreetingsControlCharactersAreEscapedInTheReport(t *testing.T) {
	exp, _, smtp, _ := greetingExporter(t, "", "220 mx.example\r250 forged\x1b[2J\x07\r\n")
	exp.SetDebug(DebugFull)
	report := probeOnce(t, exp, "/probe?debug=true&collector=smtp&target="+smtp, nil).Body.String()
	if !strings.Contains(report, `X-Tcp-Greeting: 220 mx.example\r250 forged\x1b[2J\a`) || strings.ContainsAny(report, "\r\x1b\x07") {
		t.Errorf("the greeting's control characters reach the report:\n%q", report)
	}
	if lines := headerLines(http.Header{"X-Plain": {"a\tb é"}, "X-Next": {"c\u0085d\x7f"}}); strings.Join(lines, "|") != `X-Next: c\u0085d\x7f|X-Plain: a`+"\t"+`b é` {
		t.Errorf("lines %q", lines)
	}
}

// A probe whose send would come out empty, {{param_cmd:}} without
// param_cmd, is answered 400 naming the parameter, and the server is never
// connected to.
func TestATCPProbeWhoseSendComesOutEmptyIsRefused(t *testing.T) {
	server := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	exp := labelServer(t, `collectors:
  - name: memcached
    request:
      type: tcp
      send: "{{param_cmd:}}"
      terminator: "END\r\n"
    transform:
      type: regex
    metrics:
      - name: memcached_up
        expression: '^STAT pid (\d+)'
`)
	recorder := probeOnce(t, exp, "/probe?collector=memcached&target="+server.Addr, nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "request.send is empty once param_cmd is filled in") {
		t.Errorf("%d %s", recorder.Code, recorder.Body)
	}
	if n := server.Accepted(); n != 0 {
		t.Errorf("the server was connected to %d times", n)
	}
}
