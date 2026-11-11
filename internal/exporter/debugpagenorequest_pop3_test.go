//go:build request_type_pop3

package exporter

import "testing"

// A pop3 playground reads a pasted document, as the exporter makes one of
// a mail server's answers, and connects to no server.
func TestAPOP3PlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: pop3}\ntransform: {type: jq}\nmetrics: [{name: pop3_tls, expression: .tls}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"greeting": "ready", "tls": true}`, series: "pop3_tls 1\n",
	})
	requirePlaygroundRefusesFiles(t, "pop3")
}
