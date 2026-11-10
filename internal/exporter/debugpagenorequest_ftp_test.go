//go:build request_type_ftp

package exporter

import "testing"

// An ftp playground reads what was pasted as the file's content and
// connects to no server.
func TestAnFTPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: ftp, root: /exports, path: queue.json}\ntransform: {type: jq}\nmetrics: [{name: queue_depth, expression: .depth}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"depth": 7}`, series: "queue_depth 7\n",
	})
	requirePlaygroundRefusesFiles(t, "ftp")
}
