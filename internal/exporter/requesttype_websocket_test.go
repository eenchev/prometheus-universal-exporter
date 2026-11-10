//go:build request_type_websocket

package exporter

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // RFC 6455's handshake
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A websocket collector's probe gives the series its jq rules read from
// the message the server sent, its label values filled from the probe; the
// verbose self-metrics label the scrape with the endpoint's URL, without
// its query, and READ; and the collectors page hints the target as an
// endpoint.
func TestAWebSocketCollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // as above
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		message := `{"path":"` + request.URL.Path + `","queue":7}`
		_, _ = conn.Write(append([]byte{0x81, byte(len(message))}, message...))
		// Until the client's close frame.
		_, _ = conn.Read(make([]byte, 64))
	})
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: feed
    request:
      type: websocket
    transform:
      type: jq
    metrics:
      - name: feed_queue_length
        expression: .queue
        labels:
          - name: path
            expression: .path
          - name: site
            value: "{{param_site}}"
  - name: feedpy
    request:
      type: websocket
    decoder:
      type: json
    transform:
      type: python
      script: |
        metric(name="feed_queue", type="gauge", value=data["queue"], labels={"path": data["path"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=feed&param_site=berlin&target=ws://"+stub.Addr+"/status%3Ftoken=secret", nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `feed_queue_length{path="/status",site="berlin"} 7` {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=feed&param_site=berlin&target=http://"+stub.Addr, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target of another scheme: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="feed",http_method="READ",url="ws://` + stub.Addr + `/status"} 1`; !strings.Contains(metrics, want) || strings.Contains(metrics, "secret") {
		t.Errorf("no %s, or the query, in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "wss://host/path") {
		t.Errorf("the collectors page hints no websocket target:\n%s", page)
	}
	// A python transform reads the message, decoded, as data.
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=feedpy&target=ws://"+stub.Addr+"/py", nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `feed_queue{path="/py"} 7` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
