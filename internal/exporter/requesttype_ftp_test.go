//go:build request_type_ftp

package exporter

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An ftp collector's probe gives the series its rules read from the file,
// decoded as its extension says, the file named by the probe; the verbose
// self-metrics label the scrape with the file's URL and RETR; a path
// parameter that leads outside request.root is answered 400; and the
// collectors page hints the target as a server.
func TestAnFTPCollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		say := func(text string) { _, _ = fmt.Fprint(conn, text+"\r\n") }
		say("220 ready")
		var data net.Listener
		defer func() {
			if data != nil {
				_ = data.Close()
			}
		}()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch verb, arg, _ := strings.Cut(strings.TrimSpace(line), " "); verb {
			case "USER":
				say("230 welcome")
			case "TYPE":
				say("200 binary")
			case "EPSV":
				var listen net.ListenConfig
				if data, err = listen.Listen(context.Background(), "tcp", "127.0.0.1:0"); err != nil {
					return
				}
				say(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", data.Addr().(*net.TCPAddr).Port))
			case "RETR":
				if arg != "/exports/berlin/queue.json" {
					say("550 no such file")
					continue
				}
				say("150 here it comes")
				transfer, err := data.Accept()
				if err != nil {
					return
				}
				_, _ = fmt.Fprint(transfer, `{"depth": 12}`)
				_ = transfer.Close()
				say("226 done")
			case "QUIT":
				say("221 bye")
				return
			default:
				say("502 not implemented")
			}
		}
	})
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: drop
    request:
      type: ftp
      root: /exports
      path: "{{param_site}}/queue.json"
    transform:
      type: jq
    metrics:
      - name: queue_depth
        expression: .depth
        labels:
          - name: site
            value: "{{param_site}}"
  - name: droppy
    request:
      type: ftp
      root: /exports
      path: berlin/queue.json
    transform:
      type: python
      script: |
        metric(name="queue_depth_doubled", type="gauge", value=data["depth"] * 2)
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=drop&param_site=berlin&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `queue_depth{site="berlin"} 12` {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=drop&path=../etc/passwd&target="+stub.Addr, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a path outside the root: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="drop",http_method="RETR",url="ftp://` + stub.Addr + `/exports/{{param_site}}/queue.json"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "ftpes://host:21") {
		t.Errorf("the collectors page hints no ftp target:\n%s", page)
	}
	// A python transform reads the file, decoded, as data.
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=droppy&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != "queue_depth_doubled 24" {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
