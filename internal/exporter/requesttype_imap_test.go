//go:build request_type_imap

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

// An imap collector's probe gives the series its jq rules read from the
// document of the session, a mailbox's name and a label value filled from
// the probe; the verbose self-metrics label the scrape with
// imap://host:port and how far the session went; and the collectors page
// hints the target as a mail server.
func TestAnIMAPCollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		_, _ = fmt.Fprint(conn, "* OK mail.test ready\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			tag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
			switch verb, args, _ := strings.Cut(rest, " "); verb {
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 IDLE\r\n%s OK done\r\n", tag)
			case "STATUS":
				name, _, _ := strings.Cut(args, " (")
				if name == `"INBOX"` {
					_, _ = fmt.Fprintf(conn, "* STATUS %s (MESSAGES 12 UNSEEN 3 RECENT 0 UIDNEXT 44 UIDVALIDITY 7)\r\n%s OK done\r\n", name, tag)
				} else {
					_, _ = fmt.Fprintf(conn, "%s NO no such mailbox\r\n", tag)
				}
			case "LOGOUT":
				_, _ = fmt.Fprintf(conn, "* BYE\r\n%s OK done\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s OK done\r\n", tag)
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
      type: imap
      username: monitor@example.com
      password_file: `+password+`
      allow_plaintext: true
      mailboxes: [INBOX, "{{param_box}}"]
    transform:
      type: jq
    metrics:
      - name: imap_mailbox_messages
        items: .mailboxes[] | select(.ok)
        expression: .messages
        labels:
          - name: mailbox
            expression: .name
          - name: asked
            value: "{{param_box}}"
      - name: imap_mailbox_found
        items: .mailboxes[]
        expression: .ok
        labels:
          - name: mailbox
            expression: .name
  - name: mailpy
    request:
      type: imap
      username: monitor@example.com
      password_file: `+password+`
      allow_plaintext: true
    transform:
      type: python
      script: |
        for box in data["mailboxes"]:
            metric(name="imap_mailbox_unseen", type="gauge", value=box["unseen"], labels={"mailbox": box["name"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=mail&param_box=Gone&target="+stub.Addr, nil)
	want := `imap_mailbox_found{mailbox="Gone"} 0` + "\n" + `imap_mailbox_found{mailbox="INBOX"} 1` + "\n" + `imap_mailbox_messages{asked="Gone",mailbox="INBOX"} 12`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=mail&param_box=Gone&target=smtp://"+stub.Addr, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target of another scheme: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="mail",http_method="STATUS",url="imap://` + stub.Addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "imaps://host:993") {
		t.Errorf("the collectors page hints no imap target:\n%s", page)
	}
	// A python transform reads the document as data.
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=mailpy&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `imap_mailbox_unseen{mailbox="INBOX"} 3` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
