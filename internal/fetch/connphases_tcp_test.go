//go:build !select_request_types || request_type_tcp

package fetch

import (
	"net"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A tcp collector's request has its connection's phases: the name's
// lookup, the connection, and the TLS handshake over TLS.
func TestATCPRequestHasItsConnectionsPhases(t *testing.T) {
	plain := testutil.NewTCPStub(t, memcached(nil))
	_, port, _ := net.SplitHostPort(plain.Addr)
	stats := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"} })
	if got := phaseNames(t, spanTracedRequests(t, stats, "localhost:"+port)[0]); got != "connection.dns,connection.connect" {
		t.Errorf("plaintext to localhost: %s", got)
	}
	secure := testutil.NewTLSTCPStub(t, memcached(nil))
	overTLS := tcpCollector(t, func(c *model.Collector) {
		c.Request.TLS = model.TLSConfig{CAFile: secure.CAFile}
		c.Request.Own = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"}
	})
	if got := phaseNames(t, spanTracedRequests(t, overTLS, secure.Addr)[0]); got != "connection.connect,connection.tls" {
		t.Errorf("TLS to an address: %s", got)
	}
}
