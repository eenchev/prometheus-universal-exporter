//go:build request_type_sip

package exporter

import "testing"

// A sip playground reads a pasted response, the JSON document the exporter
// makes of one, and asks no server.
func TestASIPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: sip, sip_transport: tcp}\ntransform: {type: jq}\nmetrics: [{name: sip_status_code, expression: .status_code}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"status_code": 200, "ok": true}`, series: "sip_status_code 200\n",
	})
	requirePlaygroundRefusesFiles(t, "sip")
}
