//go:build request_type_websocket

package exporter

import "testing"

// A websocket playground reads what was pasted as the server's message and
// makes no handshake.
func TestAWebSocketPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: websocket}\ntransform: {type: jq}\nmetrics: [{name: queue_depth, expression: .depth}]\n"
		},
		target: func(addr string) string { return "ws://" + addr + "/feed" },
		body:   `{"depth": 7}`, series: "queue_depth 7\n",
	})
	requirePlaygroundRefusesFiles(t, "websocket")
}
