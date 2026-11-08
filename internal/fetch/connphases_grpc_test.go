//go:build !select_request_types || request_type_grpc

package fetch

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A gRPC call that had grpc-go connect has the connection's phases, its
// dial and TLS handshake; the next call, on the connection kept, has none.
// With target lists the exporter's own dialer is timed as well.
func TestAGRPCCallsConnectionPhasesAreTheCallThatMadeIt(t *testing.T) {
	for name, policy := range map[string][]string{"without target lists": nil, "with target lists": {"127.0.0.0/8"}} {
		server := grpctest.Start(t, grpctest.Options{TLS: true, Reflection: "v1", Answer: statsAnswer})
		c := grpcCollector()
		c.Request.TLS = model.TLSConfig{CAFile: server.CAFile}
		c.Request.AllowedTargets = policy
		checked := validGRPC(t, c)
		first := spanTracedRequests(t, checked, server.Addr)
		if len(first) != 1 || phaseNames(t, first[0]) != "connection.connect,connection.tls" {
			t.Errorf("%s: the first call: %+v", name, first)
		}
		again := spanTracedRequests(t, checked, server.Addr)
		if len(again) != 1 || len(again[0].Phases) != 0 {
			t.Errorf("%s: the next call: %+v", name, again)
		}
	}
}
