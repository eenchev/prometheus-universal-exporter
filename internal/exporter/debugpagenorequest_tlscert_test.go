//go:build !select_request_types || request_type_tlscert

package exporter

import "testing"

// A tlscert playground reads a pasted handshake, the JSON document the
// exporter makes of one, and makes none of its own.
func TestATLSCertPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: tlscert}\ntransform: {type: jq}\nmetrics: [{name: tls_expiry_seconds, expression: .seconds_until_expiry}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"verified": true, "seconds_until_expiry": 86400, "certificates": []}`, series: "tls_expiry_seconds 86400\n",
	})
	requirePlaygroundRefusesFiles(t, "tlscert")
}
