//go:build request_type_imap

package exporter

import "testing"

// An imap playground reads a pasted document, as the exporter makes one of
// a mail server's answers, and connects to no server.
func TestAnIMAPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: imap}\ntransform: {type: jq}\nmetrics: [{name: imap_tls, expression: .tls}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"greeting": "ready", "tls": true}`, series: "imap_tls 1\n",
	})
	requirePlaygroundRefusesFiles(t, "imap")
}
