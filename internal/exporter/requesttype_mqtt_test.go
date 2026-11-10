//go:build request_type_mqtt

package exporter

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An mqtt collector's probe gives the series its jq rules read from the
// document of the messages, a topic's level filled from the probe; the
// verbose self-metrics label the scrape with mqtt://host:port and
// SUBSCRIBE; and the collectors page hints the target as a broker.
func TestAnMQTTCollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		// Each packet of the client's is short: a first byte, a length
		// and that many bytes.
		packet := func() (byte, []byte) {
			head := make([]byte, 2)
			if _, err := io.ReadFull(conn, head); err != nil {
				return 0, nil
			}
			rest := make([]byte, head[1])
			_, _ = io.ReadFull(conn, rest)
			return head[0] >> 4, rest
		}
		for {
			switch kind, rest := packet(); kind {
			case 1:
				_, _ = conn.Write([]byte{0x20, 2, 0, 0})
			case 8:
				// The filter subscribed to names the topic published.
				topic := strings.TrimSuffix(string(rest[4:len(rest)-1]), "#") + "temperature"
				_, _ = conn.Write([]byte{0x90, 3, rest[0], rest[1], 0})
				publish := append([]byte{0, byte(len(topic))}, topic...)
				publish = append(publish, `{"celsius":21.5}`...)
				_, _ = conn.Write(append([]byte{0x31, byte(len(publish))}, publish...))
			default:
				return
			}
		}
	})
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: sensors
    request:
      type: mqtt
      topics: ["sites/{{param_site}}/#"]
      max_messages: 1
      window: 20s
    transform:
      type: jq
    metrics:
      - name: sensor_temperature_celsius
        items: .messages[]
        expression: .json.celsius
        labels:
          - name: topic
            expression: .topic
  - name: sensorspy
    request:
      type: mqtt
      topics: [sites/berlin/#]
      max_messages: 1
      window: 20s
    transform:
      type: python
      script: |
        for m in data["messages"]:
            metric(name="sensor_celsius", type="gauge", value=m["json"]["celsius"], labels={"topic": m["topic"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=sensors&param_site=berlin&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `sensor_temperature_celsius{topic="sites/berlin/temperature"} 21.5` {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=sensors&param_site=%23&target="+stub.Addr, nil); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "a value is one level of a topic") {
		t.Errorf("a wildcard in a parameter: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="sensors",http_method="SUBSCRIBE",url="mqtt://` + stub.Addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "mqtts://host:8883") {
		t.Errorf("the collectors page hints no mqtt target:\n%s", page)
	}
	// A python transform reads the document as data.
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=sensorspy&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `sensor_celsius{topic="sites/berlin/temperature"} 21.5` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
