//go:build request_type_nats

package exporter

import "testing"

// A nats playground reads a pasted answer, the JSON document the exporter
// makes of one, and connects to no server.
func TestANATSPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: nats}\ntransform: {type: jq}\nmetrics: [{name: nats_rtt_seconds, expression: .rtt_seconds}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"rtt_seconds": 0.25}`, series: "nats_rtt_seconds 0.25\n",
	})
	requirePlaygroundRefusesFiles(t, "nats")
}
