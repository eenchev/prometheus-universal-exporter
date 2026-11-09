//go:build !select_request_types || request_type_grpc

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// A grpc playground reads a pasted response message, as JSON, and calls no
// server.
func TestAGRPCPlaygroundMakesNoRequest(t *testing.T) {
	tier.Slow(t)
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: grpc, rpc: acme.v1.Queue/Stats, descriptors: reflection}\ntransform: {type: jq}\nmetrics: [{name: queue_depth, expression: .depth}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"depth": 4}`, series: "queue_depth 4\n",
	})
	requirePlaygroundRefusesFiles(t, "grpc")
}
