//go:build request_type_modbus

package exporter

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A modbus collector's probe gives the series its jq rules read from the
// document of the values, its label values filled from the probe; the
// verbose self-metrics label the scrape with modbus://host:port/unit and
// READ; and the collectors page hints the target as a device.
func TestAModbusCollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		request := make([]byte, 12)
		for {
			if _, err := io.ReadFull(conn, request); err != nil {
				return
			}
			// Two registers, 0x0001 0x86A0: 100000 as a u32.
			_, _ = conn.Write([]byte{request[0], request[1], 0, 0, 0, 7, request[6], request[7], 4, 0x00, 0x01, 0x86, 0xA0})
		}
	})
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: meter
    request:
      type: modbus
      read:
        - name: energy
          table: input
          address: 72
          type: u32
    transform:
      type: jq
    metrics:
      - name: meter_energy_watt_hours_total
        type: counter
        expression: .energy
        labels:
          - name: site
            value: "{{param_site}}"
  - name: meterpy
    request:
      type: modbus
      read:
        - name: energy
          type: u32
    transform:
      type: python
      script: |
        metric(name="meter_energy_kwh", type="gauge", value=data["energy"] / 1000)
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=meter&param_site=berlin&target=modbus://"+stub.Addr+"/3", nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `meter_energy_watt_hours_total{site="berlin"} 100000` {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=meter&param_site=berlin&target=modbus://"+stub.Addr+"/300", nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a unit that is none: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="meter",http_method="READ",url="modbus://` + stub.Addr + `/3"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "modbus://host:502/&lt;unit&gt;") {
		t.Errorf("the collectors page hints no modbus target:\n%s", page)
	}
	// A python transform reads the document as data.
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=meterpy&target="+stub.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != "meter_energy_kwh 100" {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
