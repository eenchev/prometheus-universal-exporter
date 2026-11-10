//go:build request_type_smtp

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

// An smtp collector's probe gives the series its jq rules read from the
// document of the session, its label values filled from the probe; the
// verbose self-metrics label the scrape with smtp://host:port and how far
// the session went; and the collectors page hints the target as a mail
// server.
func TestAnSMTPCollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = fmt.Fprint(conn, "220 mail.test ESMTP\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch verb, _, _ := strings.Cut(strings.TrimSpace(line), " "); verb {
			case "EHLO":
				_, _ = fmt.Fprint(conn, "250-mail.test\r\n250-SIZE 1000\r\n250 8BITMIME\r\n")
			case "RCPT":
				_, _ = fmt.Fprint(conn, "550 5.1.1 no such user\r\n")
			case "QUIT":
				_, _ = fmt.Fprint(conn, "221 bye\r\n")
				return
			default:
				_, _ = fmt.Fprint(conn, "250 OK\r\n")
			}
		}
	})
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: mail
    request:
      type: smtp
      mail_from: probe@example.com
      rcpt_to: ["{{param_user}}@example.com"]
    transform:
      type: jq
    metrics:
      - name: smtp_size_limit_bytes
        expression: .size
      - name: smtp_recipient_accepted
        expression: .rcpt_to[0].accepted
        labels:
          - name: user
            value: "{{param_user}}"
          - name: code
            expression: .rcpt_to[0].code
  - name: mailpy
    request:
      type: smtp
    transform:
      type: python
      script: |
        metric(name="smtp_extensions", type="gauge", value=len(data["extensions"]), labels={"greeting": str(data["greeting"]["code"])})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=mail&param_user=nobody&target="+stub.Addr, nil)
	want := `smtp_recipient_accepted{code="550",user="nobody"} 0` + "\n" + "smtp_size_limit_bytes 1000"
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=mail&param_user=nobody&target=imap://"+stub.Addr, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target of another scheme: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="mail",http_method="RCPT",url="smtp://` + stub.Addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "smtps://host:465") {
		t.Errorf("the collectors page hints no smtp target:\n%s", page)
	}
	// A python transform reads the document as data.
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=mailpy&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `smtp_extensions{greeting="220"} 2` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
