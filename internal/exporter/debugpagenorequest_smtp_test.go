//go:build request_type_smtp

package exporter

import "testing"

// An smtp playground reads a pasted document, as the exporter makes one of
// a mail server's answers, and connects to no server.
func TestAnSMTPPlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, _ string, _ bool) string {
			return "name: " + name + "\nrequest: {type: smtp}\ntransform: {type: jq}\nmetrics: [{name: smtp_greeting_code, expression: .greeting.code}]\n"
		},
		target: func(addr string) string { return addr },
		body:   `{"greeting": {"code": 220, "text": "ready"}}`, series: "smtp_greeting_code 220\n",
	})
	requirePlaygroundRefusesFiles(t, "smtp")
}
