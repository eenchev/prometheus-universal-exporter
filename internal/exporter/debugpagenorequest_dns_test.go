//go:build !select_request_types || request_type_dns

package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// A dns playground's request.server is never asked: it reads a pasted
// lookup, the JSON document the exporter makes of one.
func TestADNSPlaygroundMakesNoRequest(t *testing.T) {
	tier.Slow(t)
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, addr string, _ bool) string {
			return "name: " + name + "\nrequest: {type: dns, record_type: A, server: '" + addr + "'}\ntransform: {type: jq}\nmetrics: [{name: dns_records, expression: .count}]\n"
		},
		target: func(string) string { return "example.com" },
		body:   `{"name": "example.com", "count": 2, "answers": []}`, series: "dns_records 2\n",
	})
	requirePlaygroundRefusesFiles(t, "dns")
}
