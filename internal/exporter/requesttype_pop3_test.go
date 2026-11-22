//go:build request_type_pop3

package exporter

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A pop3 collector's probe gives the series its jq rules read from the
// document of the session, the mailbox's messages and their size, with a
// label value filled from the probe and the mechanism the configuration
// names; the verbose self-metrics label the scrape with pop3://host:port
// and how far the session went; the collectors page hints the target as a
// mail server; and a python transform reads the document as data.
func TestAPOP3CollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = fmt.Fprint(conn, "+OK mail.test ready\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch verb, args, _ := strings.Cut(strings.TrimSpace(line), " "); verb {
			case "CAPA":
				_, _ = fmt.Fprint(conn, "+OK\r\nUSER\r\nSASL PLAIN\r\n.\r\n")
			case "AUTH":
				if args != "PLAIN" {
					_, _ = fmt.Fprint(conn, "-ERR no such mechanism\r\n")
					break
				}
				_, _ = fmt.Fprint(conn, "+ \r\n")
				if answer, _ := reader.ReadString('\n'); strings.TrimSpace(answer) != "AG1vbml0b3JAZXhhbXBsZS5jb20Ac2VjcmV0" {
					_, _ = fmt.Fprint(conn, "-ERR [AUTH] no\r\n")
					break
				}
				_, _ = fmt.Fprint(conn, "+OK welcome\r\n")
			case "STAT":
				_, _ = fmt.Fprint(conn, "+OK 12 123456\r\n")
			case "LIST":
				_, _ = fmt.Fprint(conn, "+OK\r\n1 120\r\n2 98765\r\n.\r\n")
			case "QUIT":
				_, _ = fmt.Fprint(conn, "+OK bye\r\n")
				return
			default:
				_, _ = fmt.Fprint(conn, "+OK\r\n")
			}
		}
	})
	password := testutil.WriteFile(t, "password", "secret\n")
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: mail
    request:
      type: pop3
      username: monitor@example.com
      password_file: `+password+`
      auth: plain
      allow_plaintext: true
      list: true
    transform:
      type: jq
    metrics:
      - name: pop3_mailbox_messages
        expression: .messages
        labels:
          - name: auth
            expression: .auth
          - name: asked
            value: "{{param_box}}"
      - name: pop3_mailbox_size_bytes
        expression: .size_bytes
      - name: pop3_mailbox_largest_bytes
        expression: .largest_bytes
  - name: mailpy
    request:
      type: pop3
      username: monitor@example.com
      password_file: `+password+`
      auth: plain
      allow_plaintext: true
    transform:
      type: python
      script: |
        metric(name="pop3_mailbox_messages", type="gauge", value=data["messages"], labels={"tls": data["tls"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=mail&param_box=orders&target="+stub.Addr, nil)
	want := `pop3_mailbox_largest_bytes 98765` + "\n" + `pop3_mailbox_messages{asked="orders",auth="plain"} 12` + "\n" + `pop3_mailbox_size_bytes 123456`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=mail&param_box=orders&target=imap://"+stub.Addr, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target of another scheme: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="mail",http_method="STAT",url="pop3://` + stub.Addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "pop3s://host:995") {
		t.Errorf("the collectors page hints no pop3 target:\n%s", page)
	}
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=mailpy&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `pop3_mailbox_messages{tls="false"} 12` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
