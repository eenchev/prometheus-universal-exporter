//go:build !select_request_types || request_type_tlscert

package fetch

import (
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A tlscert collector's request has the connection and the handshake it
// makes as its phases.
func TestATLSCertRequestHasItsConnectionsPhases(t *testing.T) {
	pki := newTestPKI(t)
	leaf := makeCert(t, certSpec{cn: "www.example.test", dns: []string{"www.example.test"}, ips: []string{"127.0.0.1"}, notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(24 * time.Hour), serial: 7}, pki.intermediate)
	addr, _ := tlsServer(t, []*testCert{leaf, pki.intermediate}, nil, true)
	requests := spanTracedRequests(t, tlsCertCollector(t, model.TLSConfig{}), addr)
	if len(requests) != 1 || phaseNames(t, requests[0]) != "connection.connect,connection.tls" {
		t.Errorf("requests %+v", requests)
	}
}
