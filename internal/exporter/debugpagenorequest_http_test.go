//go:build !select_request_types || request_type_http

package exporter

import "testing"

// An http playground's request is never made: one sent through its proxy
// would connect to it, and a login of its would be refused for reading files.
func TestAnHTTPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, addr string, source bool) string {
			proxy := "  proxy_url: http://" + addr + "\n"
			if source {
				proxy = ""
			}
			return "name: " + name + "\nrequest:\n  type: http\n" + proxy + "decoder: {type: json}\ntransform: {type: jq}\nmetrics: [{name: up, expression: .up}]\n"
		},
		target: func(addr string) string { return "http://" + addr + "/metrics" },
		body:   `{"up": 1}`, series: "up 1\n",
	})
	requirePlaygroundRefusesFiles(t, "http")
}
