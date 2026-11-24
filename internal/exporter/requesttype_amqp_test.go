//go:build request_type_amqp

package exporter

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// amqpTestBroker is an AMQP 0-9-1 broker that takes any login and has one
// queue, orders, of 42 ready messages and 3 consumers.
func amqpTestBroker(t *testing.T) string {
	t.Helper()
	frame := func(channel uint16, class, method uint16, arguments []byte) []byte {
		b := binary.BigEndian.AppendUint16([]byte{1}, channel)
		b = binary.BigEndian.AppendUint32(b, uint32(4+len(arguments)))
		b = binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(b, class), method)
		return append(append(b, arguments...), 0xCE)
	}
	short := func(b []byte, text string) []byte { return append(append(b, byte(len(text))), text...) }
	long := func(b []byte, text string) []byte {
		return append(binary.BigEndian.AppendUint32(b, uint32(len(text))), text...)
	}
	return testutil.NewTCPStub(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		if _, err := io.ReadFull(r, make([]byte, 8)); err != nil {
			return
		}
		properties := long(append(short(nil, "product"), 'S'), "RabbitMQ")
		properties = long(append(short(properties, "version"), 'S'), "4.0.2")
		_, _ = conn.Write(frame(0, 10, 10, long(long(long([]byte{0, 9}, string(properties)), "PLAIN"), "en_US")))
		for {
			head := make([]byte, 7)
			if _, err := io.ReadFull(r, head); err != nil {
				return
			}
			payload := make([]byte, binary.BigEndian.Uint32(head[3:])+1)
			if _, err := io.ReadFull(r, payload); err != nil {
				return
			}
			channel, class, method := binary.BigEndian.Uint16(head[1:]), binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:])
			switch {
			case class == 10 && method == 11:
				_, _ = conn.Write(frame(0, 10, 30, []byte{0, 0, 0, 2, 0, 0, 0, 0}))
			case class == 10 && method == 40:
				_, _ = conn.Write(frame(0, 10, 41, short(nil, "")))
			case class == 20 && method == 10:
				_, _ = conn.Write(frame(channel, 20, 11, long(nil, "")))
			case class == 50 && method == 10:
				queue := string(payload[7 : 7+int(payload[6])])
				if queue != "orders" {
					_, _ = conn.Write(frame(channel, 20, 40, append(short(binary.BigEndian.AppendUint16(nil, 404), "NOT_FOUND"), 0, 50, 0, 10)))
					continue
				}
				_, _ = conn.Write(frame(channel, 50, 11, binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(short(nil, queue), 42), 3)))
			case class == 10 && method == 50:
				_, _ = conn.Write(frame(0, 10, 51, nil))
				return
			}
		}
	}).Addr
}

// An amqp collector's probe gives the series its jq rules read from the
// document: each queue's ready messages and consumers by its name, whether
// it exists, and the broker's version as a label, with a label value
// filled from the probe; the verbose self-metrics label the scrape with
// amqp:// and the broker; the collectors page hints the target; and a
// python transform reads the document as data.
func TestAnAMQPCollectorProbe(t *testing.T) {
	addr := amqpTestBroker(t)
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: broker
    request:
      type: amqp
      queues: [orders, gone]
    transform:
      type: jq
    metrics:
      - name: amqp_queue_messages
        items: .queues[] | select(.exists)
        expression: .messages
        labels:
          - name: queue
            expression: .name
          - name: site
            value: "{{param_site}}"
      - name: amqp_queue_consumers
        items: .queues[] | select(.exists)
        expression: .consumers
        labels:
          - name: queue
            expression: .name
      - name: amqp_queue_exists
        items: .queues[]
        expression: .exists
        labels:
          - name: queue
            expression: .name
  - name: brokerpy
    request:
      type: amqp
    transform:
      type: python
      script: |
        metric(name="amqp_up", type="gauge", value=1, labels={"version": data["version"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=broker&param_site=lab&target="+addr, nil)
	want := `amqp_queue_consumers{queue="orders"} 3` + "\n" + `amqp_queue_exists{queue="gone"} 0` + "\n" + `amqp_queue_exists{queue="orders"} 1` + "\n" + `amqp_queue_messages{queue="orders",site="lab"} 42`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=broker&param_site=lab&target=http://broker.example.com", nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target that is no broker: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="broker",http_method="DECLARE-PASSIVE",url="amqp://` + addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "amqps://host") {
		t.Errorf("the collectors page hints no amqp target:\n%s", page)
	}
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=brokerpy&target="+addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `amqp_up{version="4.0.2"} 1` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
