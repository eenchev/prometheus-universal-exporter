//go:build request_type_icmp

package exporter

import (
	"net/http"
	"strings"
	"testing"
)

// A probe of an icmp collector pings its target, and the rules read the
// document of the replies: here loopback's, whose every echo comes back.
// A system that opens no ICMP datagram socket for the test's group skips
// it.
func TestAProbeOfAnICMPCollectorReadsTheReplies(t *testing.T) {
	server := labelServer(t, `collectors:
  - name: ping
    request:
      type: icmp
      count: 2
      interval: 10ms
    decoder:
      type: json
    transform:
      type: jq
    metrics:
      - name: ping_received
        expression: .received
      - name: ping_loss_ratio
        expression: .loss_ratio
`)
	recorder := probeOnce(t, server, "/probe?collector=ping&target=127.0.0.1", nil)
	body := recorder.Body.String()
	if strings.Contains(body, "ping_group_range") || strings.Contains(body, "address family not supported") {
		t.Skipf("the system opens no ICMP datagram socket here: %s", body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, "ping_received 2\n") || !strings.Contains(body, "ping_loss_ratio 0\n") {
		t.Errorf("%d %v", recorder.Code, seriesLines(body))
	}
}

// A python transform reads the document of the replies as data.
func TestAPythonTransformReadsAnICMPCollectorsReplies(t *testing.T) {
	requirePython(t)
	server := labelServer(t, `collectors:
  - name: ping
    request:
      type: icmp
      count: 1
    transform:
      type: python
      script: |
        metric(name="ping_sent", type="gauge", value=data["sent"])
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=ping&target=127.0.0.1", nil)
	body := recorder.Body.String()
	if strings.Contains(body, "ping_group_range") || strings.Contains(body, "address family not supported") {
		t.Skipf("the system opens no ICMP datagram socket here: %s", body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, "ping_sent 1\n") {
		t.Errorf("%d %v", recorder.Code, seriesLines(body))
	}
}
