//go:build !select_request_types || request_type_icmp

package exporter

import (
	"strings"
	"testing"
)

// An icmp playground reads a pasted document of replies, as the exporter
// makes one, and pings nothing.
func TestAnICMPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: icmp}\ntransform: {type: jq}\nmetrics: [{name: ping_received, expression: .received}]\n"
		},
		target: func(addr string) string { host, _, _ := strings.Cut(addr, ":"); return host },
		body:   `{"sent": 3, "received": 3, "loss_ratio": 0}`, series: "ping_received 3\n",
	})
	requirePlaygroundRefusesFiles(t, "icmp")
}
