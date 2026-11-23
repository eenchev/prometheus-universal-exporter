//go:build request_type_whois

package exporter

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A whois collector's probe gives the series its jq rules read from the
// document of the registration, the expiry as a timestamp with the
// registrar as a label and a label value filled from the probe; the
// verbose self-metrics label the scrape with whois:// and the domain; the
// collectors page hints the target as a domain; and a python transform
// reads the document as data.
func TestAWhoisCollectorProbe(t *testing.T) {
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		question, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || strings.TrimSpace(question) != "example.test" {
			_, _ = fmt.Fprint(conn, "No match for domain.\r\n")
			return
		}
		_, _ = fmt.Fprint(conn, "Domain Name: EXAMPLE.TEST\r\nRegistrar: Example Registrar\r\nRegistry Expiry Date: 2027-08-13T04:00:00Z\r\nName Server: NS1.EXAMPLE.TEST\r\nName Server: NS2.EXAMPLE.TEST\r\n")
	})
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: domain
    request:
      type: whois
      whois_server: `+stub.Addr+`
    transform:
      type: jq
    metrics:
      - name: domain_expiry_timestamp_seconds
        expression: .expires_timestamp_seconds
        labels:
          - name: registrar
            expression: .registrar
          - name: owner
            value: "{{param_owner}}"
      - name: domain_registered
        expression: .registered
      - name: domain_name_servers
        expression: .name_servers | length
  - name: domainpy
    request:
      type: whois
      whois_server: `+stub.Addr+`
    transform:
      type: python
      script: |
        metric(name="domain_registered", type="gauge", value=1 if data["registered"] else 0, labels={"domain": data["domain"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=domain&param_owner=web&target=example.test", nil)
	want := `domain_expiry_timestamp_seconds{owner="web",registrar="Example Registrar"} 1.8181296e+09` + "\n" + `domain_name_servers 2` + "\n" + `domain_registered 1`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=domain&param_owner=web&target=https://example.test", nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target that is no domain: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="domain",http_method="QUERY",url="whois://example.test"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "example.com") {
		t.Errorf("the collectors page hints no whois target:\n%s", page)
	}
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=domainpy&target=other.test", nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `domain_registered{domain="other.test"} 0` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
