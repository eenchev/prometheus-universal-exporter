//go:build request_type_tcp

package exporter

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// One caching tcp collector, speaking plaintext, serves a service behind
// TLS through a static target's own tls block: the target with the
// service's CA reads it, the collector's plaintext does not, and a target
// whose own block, tls: {}, verifies with the system roots alone is refused
// by the handshake rather than answered from the cache entry of the target
// beside it. A probe cannot ask for TLS: tls is not the probe's to set.
func TestTCPStaticTargetsSpeakTLSWithTheirOwnBlocks(t *testing.T) {
	server := testutil.NewTLSTCPStub(t, func(conn net.Conn) {
		if first, err := testutil.ReadUntil(conn, "\n"); err == nil && first == "stats\r\n" {
			_, _ = io.WriteString(conn, "STAT curr_connections 10\r\nEND\r\n")
		}
	})
	dir := t.TempDir()
	exp := staticServerFromFiles(t, dir, `collectors:
  - name: memcached
    request:
      type: tcp
      send: "stats\r\n"
      terminator: "END\r\n"
    cache:
      ttl: 1h
    transform:
      type: regex
    metrics:
      - name: memcached_curr_connections
        expression: '(?m)^STAT curr_connections (\d+)\r$'
`, `interval: 1m
concurrency: 1
targets:
  - name: own_ca
    collector: memcached
    target: `+server.Addr+`
    request:
      tls: {ca_file: `+server.CAFile+`}
  - name: system_roots
    collector: memcached
    target: `+server.Addr+`
    request:
      tls: {}
  - name: plaintext
    collector: memcached
    target: `+server.Addr+`
`)
	exp.scrapeStaticTargets(t.Context(), 0)
	body := probeOnce(t, exp, "/static-targets", nil).Body.String()
	if want := `memcached_curr_connections{static_target="own_ca"} 10`; !strings.Contains(body, want) {
		t.Errorf("no %s in\n%s", want, body)
	}
	checkTargetsUp(t, body, map[string]string{"own_ca": "1", "system_roots": "0", "plaintext": "0"})
	query := url.Values{"collector": {"memcached"}, "target": {server.Addr}, "tls": {"true"}, "ca_file": {server.CAFile}}
	if recorder := probeOnce(t, exp, "/probe?"+query.Encode(), nil); recorder.Code == http.StatusOK {
		t.Errorf("a probe asking for TLS was answered, from a server the collector reaches in plaintext: %s", recorder.Body)
	}
}
