//go:build !select_request_types || request_type_ntp

package exporter

import (
	"encoding/binary"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// ntpCollectors is an ntp collector exporting the clock's offset from the
// server, the round trip's delay and the server's stratum.
const ntpCollectors = `web:
  self_metrics:
    verbose: true
collectors:
  - name: clock
    request:
      type: ntp
      allowed_targets: [127.0.0.0/8]
    transform:
      type: jq
    metrics:
      - name: ntp_offset_seconds
        expression: .offset_seconds
      - name: ntp_delay_seconds
        expression: .delay_seconds
      - name: ntp_stratum
        expression: .stratum
        labels:
          - name: reference_id
            expression: .reference_id
`

// ntpResponder is a stand-in NTP server on 127.0.0.1 that answers every
// request at once as a stratum 2 server 3 seconds ahead of this clock, or
// as a Kiss-o'-Death RATE with kiss, and returns its address.
func ntpResponder(t *testing.T, kiss bool) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}
			ahead := time.Now().Add(3 * time.Second)
			now := uint64(ahead.Unix()+2208988800)<<32 | (uint64(ahead.Nanosecond())<<32)/uint64(time.Second)
			reply := make([]byte, 48)
			reply[0], reply[1], reply[2], reply[3] = 4<<3|4, 2, 6, 0xec
			copy(reply[12:16], []byte{192, 0, 2, 7})
			if kiss {
				reply[1] = 0
				copy(reply[12:16], "RATE")
			}
			binary.BigEndian.PutUint64(reply[16:], now-64<<32)
			copy(reply[24:32], buf[40:48])
			binary.BigEndian.PutUint64(reply[32:], now)
			binary.BigEndian.PutUint64(reply[40:], now)
			_, _ = conn.WriteTo(reply, from)
		}
	}()
	return conn.LocalAddr().String()
}

func ntpExporter(t *testing.T) *Server {
	t.Helper()
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", ntpCollectors)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
}

// An ntp collector's probe gives the series its jq rules read from the
// document: an offset of about the 3 seconds the server is ahead, a delay,
// and the stratum labelled with the reference id. The verbose self-metrics
// label the request ntp://host:port and QUERY.
func TestAnNTPCollectorProbe(t *testing.T) {
	server, address := ntpExporter(t), ntpResponder(t, false)
	recorder := probeOnce(t, server, "/probe?collector=clock&target="+address, nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `ntp_stratum{reference_id="192.0.2.7"} 2`) || !strings.Contains(body, "ntp_delay_seconds ") {
		t.Fatalf("%d %s", recorder.Code, body)
	}
	offset := -1.0
	for _, line := range seriesLines(body) {
		if value, ok := strings.CutPrefix(line, "ntp_offset_seconds "); ok {
			offset, _ = strconv.ParseFloat(value, 64)
		}
	}
	if offset < 2.5 || offset > 3.5 {
		t.Errorf("offset %g in %s", offset, body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="clock",http_method="QUERY",url="ntp://` + address + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
}

// A debug probe shows the request and the document the rules read. A
// target that is no host:port is answered 400, and one the target lists
// refuse 403 and counted in http_exporter_targets_refused_total, before
// anything is sent. A Kiss-o'-Death fails the probe in the ntp stage,
// naming its code to the caller.
func TestADebugProbeOfAnNTPCollector(t *testing.T) {
	server, address := ntpExporter(t), ntpResponder(t, false)
	server.SetDebug(DebugFull)
	body := probeOnce(t, server, "/probe?debug=true&collector=clock&target="+address, nil).Body.String()
	for _, want := range []string{"1. QUERY ntp://" + address + " -> stratum 2, offset ", "Ntp-Version: 4", `"reference_id":"192.0.2.7"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	for query, want := range map[string]int{
		"/probe?collector=clock":                         http.StatusBadRequest,
		"/probe?collector=clock&target=ntp://" + address: http.StatusBadRequest,
		"/probe?collector=clock&target=192.0.2.1:123":    http.StatusForbidden,
	} {
		if recorder := probeOnce(t, server, query, nil); recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
	}
	if metrics := probeOnce(t, server, "/self-metrics", nil).Body.String(); !strings.Contains(metrics, `http_exporter_targets_refused_total{collector="clock"} 1`) {
		t.Errorf("the refusal is not counted:\n%s", metrics)
	}
	kiss := ntpResponder(t, true)
	recorder := probeOnce(t, server, "/probe?collector=clock&target="+kiss, nil)
	if want := "collector clock ntp failed: " + kiss + " answered a Kiss-o'-Death, RATE: rate exceeded"; recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), want) {
		t.Errorf("%d %s, want %s", recorder.Code, recorder.Body, want)
	}
}

// An ntp collector's label values take the probe's param_<name>
// parameters, as every type's do, and a python transform reads its
// document as data.
func TestAnNTPCollectorsLabelValuesAndAPythonTransform(t *testing.T) {
	address := ntpResponder(t, false)
	server := labelServer(t, `collectors:
  - name: clock
    request:
      type: ntp
    transform:
      type: jq
    metrics:
      - name: ntp_stratum
        expression: .stratum
        labels:
          - name: env
            value: "{{param_env:dev}}"
`)
	for query, want := range map[string]string{
		"/probe?collector=clock&target=" + address + "&param_env=prod": `ntp_stratum{env="prod"} 2`,
		"/probe?collector=clock&target=" + address:                     `ntp_stratum{env="dev"} 2`,
	} {
		recorder := probeOnce(t, server, query, nil)
		if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || len(got) != 1 || got[0] != want {
			t.Errorf("%s: %d %v, want %s", query, recorder.Code, got, want)
		}
	}
	requirePython(t)
	server = labelServer(t, `collectors:
  - name: clock
    request:
      type: ntp
    transform:
      type: python
      script: |
        metric(name="ntp_in_step", type="gauge", value=1 if abs(data["offset_seconds"]) > 1 else 0, labels={"stratum": str(data["stratum"])})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=clock&target="+address, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || len(got) != 1 || got[0] != `ntp_in_step{stratum="2"} 1` {
		t.Errorf("%d %v", recorder.Code, got)
	}
}
