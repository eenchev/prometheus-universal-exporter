//go:build !select_request_types || request_type_ntp

package exporter

import "testing"

// An ntp playground reads a pasted reply, the JSON document the exporter
// makes of one, and asks no server.
func TestAnNTPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: ntp}\ntransform: {type: jq}\nmetrics: [{name: ntp_stratum, expression: .stratum}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"stratum": 2, "offset_seconds": 0.001}`, series: "ntp_stratum 2\n",
	})
	requirePlaygroundRefusesFiles(t, "ntp")
}
