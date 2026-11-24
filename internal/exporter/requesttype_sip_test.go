//go:build request_type_sip

package exporter

import (
	"net"
	"net/http"
	"strings"
	"testing"
)

// sipTestServer is a SIP server over UDP that answers every OPTIONS with
// status, echoing what a response echoes of its request.
func sipTestServer(t *testing.T, status string) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			reply := []string{"SIP/2.0 " + status}
			for _, line := range strings.Split(string(buffer[:n]), "\r\n") {
				for _, echoed := range []string{"Via:", "From:", "To:", "Call-ID:", "CSeq:"} {
					if strings.HasPrefix(line, echoed) {
						reply = append(reply, line)
					}
				}
			}
			reply = append(reply, "Server: stand-in/1.0", "Allow: INVITE, OPTIONS", "Content-Length: 0", "", "")
			_, _ = conn.WriteTo([]byte(strings.Join(reply, "\r\n")), from)
		}
	}()
	return conn.LocalAddr().String()
}

// A sip collector's probe gives the series its jq rules read from the
// document of the response, the status with the server as a label and a
// label value filled from the probe; a response that is no success is a
// probe that succeeded; the verbose self-metrics label the scrape with
// sip:// and the server; the collectors page hints the target as a server;
// and a python transform reads the document as data.
func TestASIPCollectorProbe(t *testing.T) {
	up, missing := sipTestServer(t, "200 OK"), sipTestServer(t, "404 Not Found")
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: pbx
    request:
      type: sip
    transform:
      type: jq
    metrics:
      - name: sip_up
        expression: .ok
        labels:
          - name: server
            expression: .server
          - name: site
            value: "{{param_site}}"
      - name: sip_status_code
        expression: .status_code
      - name: sip_methods_allowed
        expression: .allow | length
  - name: pbxpy
    request:
      type: sip
    transform:
      type: python
      script: |
        metric(name="sip_status_code", type="gauge", value=data["status_code"], labels={"reason": data["reason"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=pbx&param_site=lab&target="+up, nil)
	want := `sip_methods_allowed 2` + "\n" + `sip_status_code 200` + "\n" + `sip_up{server="stand-in/1.0",site="lab"} 1`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	recorder = probeOnce(t, server, "/probe?collector=pbx&param_site=lab&target="+missing, nil)
	if got := strings.Join(seriesLines(recorder.Body.String()), "\n"); recorder.Code != http.StatusOK || !strings.Contains(got, "sip_status_code 404") || !strings.Contains(got, `sip_up{server="stand-in/1.0",site="lab"} 0`) {
		t.Errorf("a 404: %d %v", recorder.Code, got)
	}
	if recorder := probeOnce(t, server, "/probe?collector=pbx&param_site=lab&target=sip:pbx.example.com", nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target that is no server: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="pbx",http_method="OPTIONS",url="sip://` + up + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "host or host:5060") {
		t.Errorf("the collectors page hints no sip target:\n%s", page)
	}
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=pbxpy&target="+missing, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `sip_status_code{reason="Not Found"} 404` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
