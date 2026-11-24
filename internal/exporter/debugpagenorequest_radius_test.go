//go:build request_type_radius

package exporter

import "testing"

// A radius playground reads a pasted reply, the JSON document the exporter
// makes of one, and sends no server a request; it needs neither the secret
// nor the password, whose files it may not name.
func TestARADIUSPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, source bool) string {
			files := ""
			if source {
				files = ", secret_file: /nonexistent/secret, password_file: /nonexistent/password"
			}
			return "name: " + name + "\nrequest: {type: radius, username: monitor" + files + "}\ntransform: {type: jq}\nmetrics: [{name: radius_accepted, expression: .accepted}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"reply": "access-accept", "accepted": true}`, series: "radius_accepted 1\n",
	})
	requirePlaygroundRefusesFiles(t, "radius")
}
