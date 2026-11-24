//go:build request_type_radius

package exporter

import (
	"crypto/md5"
	"encoding/binary"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// radiusAccepting is a RADIUS server that accepts every request, its reply
// made with secret.
func radiusAccepting(t *testing.T, secret string) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		request := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFrom(request)
			if err != nil {
				return
			}
			if n < 20 {
				continue
			}
			// An Access-Accept with a Session-Timeout of an hour.
			reply := append([]byte{2, request[1], 0, 26}, request[4:20]...)
			reply = binary.BigEndian.AppendUint32(append(reply, 27, 6), 3600)
			digest := md5.Sum(append(reply[:len(reply):len(reply)], secret...))
			copy(reply[4:20], digest[:])
			_, _ = conn.WriteTo(reply, from)
		}
	}()
	return conn.LocalAddr().String()
}

// A radius collector's probe gives the series its jq rules read from the
// document of the reply, whether the account was accepted and the session
// the server grants, with a label value filled from the probe; the verbose
// self-metrics label the scrape with radius://host:port and the packet
// sent; the collectors page hints the target as a RADIUS server; and a
// python transform reads the document as data.
func TestARADIUSCollectorProbe(t *testing.T) {
	addr := radiusAccepting(t, "shared")
	secret := testutil.WriteFile(t, "secret", "shared\n")
	password := testutil.WriteFile(t, "password", "hunter2\n")
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: login
    request:
      type: radius
      secret_file: `+secret+`
      username: monitor
      password_file: `+password+`
    transform:
      type: jq
    metrics:
      - name: radius_login_accepted
        expression: .accepted
        labels:
          - name: reply
            expression: .reply
          - name: site
            value: "{{param_site}}"
      - name: radius_session_timeout_seconds
        expression: .session_timeout_seconds
  - name: alive
    request:
      type: radius
      mode: status-server
      secret_file: `+secret+`
    transform:
      type: python
      script: |
        metric(name="radius_alive", type="gauge", value=1 if data["accepted"] else 0, labels={"mode": data["mode"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=login&param_site=hq&target="+addr, nil)
	want := `radius_login_accepted{reply="access-accept",site="hq"} 1` + "\n" + `radius_session_timeout_seconds 3600`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=login&param_site=hq&target=radius://"+addr, nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target with a scheme: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="login",http_method="ACCESS-REQUEST",url="radius://` + addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "host:1812") {
		t.Errorf("the collectors page hints no radius target:\n%s", page)
	}
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=alive&target="+addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `radius_alive{mode="status-server"} 1` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
