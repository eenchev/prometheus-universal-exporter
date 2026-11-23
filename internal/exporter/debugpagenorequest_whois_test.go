//go:build request_type_whois

package exporter

import "testing"

// A whois playground reads a pasted registration, the JSON document the
// exporter makes of an answer, and asks no server, neither the one its
// request names nor IANA's.
func TestAWhoisPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, addr string, _ bool) string {
			return "name: " + name + "\nrequest: {type: whois, whois_server: '" + addr + "'}\ntransform: {type: jq}\nmetrics: [{name: domain_registered, expression: .registered}]\n"
		},
		target: func(string) string { return "example.test" },
		body:   `{"domain": "example.test", "registered": true}`, series: "domain_registered 1\n",
	})
	requirePlaygroundRefusesFiles(t, "whois")
}
