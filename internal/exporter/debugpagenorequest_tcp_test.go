//go:build !select_request_types || request_type_tcp

package exporter

import "testing"

// A tcp playground reads what was pasted as the service's answer and
// connects to no service.
func TestATCPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: tcp, send: \"stats\\r\\n\"}\ntransform: {type: regex}\nmetrics: [{name: connections, type: gauge, expression: 'conns (\\d+)'}]\n"
		},
		target: func(addr string) string { return addr },
		body:   "conns 9\n", series: "connections 9\n",
	})
	requirePlaygroundRefusesFiles(t, "tcp")
}
