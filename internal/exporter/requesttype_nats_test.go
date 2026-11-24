//go:build request_type_nats

package exporter

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// natsTestServer is a NATS server with JetStream, one stream, ORDERS, and
// one consumer of it, billing.
func natsTestServer(t *testing.T) string {
	t.Helper()
	answers := map[string]string{
		"$JS.API.INFO":                         `{"streams":1,"consumers":1,"storage":2048}`,
		"$JS.API.STREAM.INFO.ORDERS":           `{"state":{"messages":120,"bytes":4096}}`,
		"$JS.API.CONSUMER.INFO.ORDERS.billing": `{"num_pending":7,"num_ack_pending":2}`,
	}
	return testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, `INFO {"server_name":"n1","version":"2.10.18","jetstream":true}`+"\r\n")
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			verb, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
			switch verb {
			case "PING":
				_, _ = io.WriteString(conn, "PONG\r\n")
			case "PUB":
				fields := strings.Fields(rest)
				_, _ = r.ReadString('\n')
				answer, known := answers[fields[0]]
				if !known {
					answer = `{"error":{"code":404,"description":"not found"}}`
				}
				_, _ = fmt.Fprintf(conn, "MSG %s 1 %d\r\n%s\r\n", fields[1], len(answer), answer)
			}
		}
	}).Addr
}

// A nats collector's probe gives the series its jq rules read from the
// document: the round trip, the server's version as a label, a stream's
// messages and a consumer's pending messages, with a label value filled
// from the probe; the verbose self-metrics label the scrape with nats://
// and the server; the collectors page hints the target; and a python
// transform reads the document as data.
func TestANATSCollectorProbe(t *testing.T) {
	addr := natsTestServer(t)
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: bus
    request:
      type: nats
      jetstream: true
      streams: [ORDERS, GONE]
      consumers: [ORDERS/billing]
    transform:
      type: jq
    metrics:
      - name: nats_up
        expression: 1
        labels:
          - name: version
            expression: .info.version
          - name: site
            value: "{{param_site}}"
      - name: nats_stream_messages
        items: .streams[] | select(.found)
        expression: .info.state.messages
        labels:
          - name: stream
            expression: .name
      - name: nats_consumer_pending
        items: .consumers[] | select(.found)
        expression: .info.num_pending
        labels:
          - name: consumer
            expression: .name
      - name: nats_streams_missing
        expression: '[.streams[] | select(.found | not)] | length'
  - name: buspy
    request:
      type: nats
    transform:
      type: python
      script: |
        metric(name="nats_jetstream_offered", type="gauge", value=1 if data["info"]["jetstream"] else 0, labels={"server": data["info"]["server_name"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=bus&param_site=lab&target="+addr, nil)
	want := `nats_consumer_pending{consumer="billing"} 7` + "\n" + `nats_stream_messages{stream="ORDERS"} 120` + "\n" + `nats_streams_missing 1` + "\n" + `nats_up{site="lab",version="2.10.18"} 1`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=bus&param_site=lab&target=http://nats.example.com", nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target that is no server: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="bus",http_method="PING",url="nats://` + addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "host:4222") {
		t.Errorf("the collectors page hints no nats target:\n%s", page)
	}
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=buspy&target="+addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `nats_jetstream_offered{server="n1"} 1` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
