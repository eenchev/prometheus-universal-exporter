//go:build !select_request_types || request_type_dns

package exporter

import "testing"

// The debug page asks a run of a dns collector for the name to resolve, or
// the address of a PTR lookup, the server being the collector's own; one
// with no placeholder takes no probe parameters, which the page says in
// place of the field; and a pasted response is the lookup's JSON document.
func TestTheDebugPageAsksADNSCollectorForTheNameToResolve(t *testing.T) {
	server := debugFormServer(t, `  - name: names
    request:
      type: dns
      record_type: A
      server: 127.0.0.1:53
    transform: {type: jq}
    metrics: [{name: dns_records, expression: .count}]
  - name: addresses
    request:
      type: dns
      record_type: PTR
    transform: {type: jq}
    metrics: [{name: dns_records, expression: .count}]
`)
	answer := "Paste the lookup's answer as the JSON document the exporter makes of it, as the Response of a run shows it"
	for name, want := range map[string]debugForm{
		"names":     {RequestType: "dns", Answer: answer, TargetLabel: "Name to resolve", TargetHint: "example.com"},
		"addresses": {RequestType: "dns", Answer: answer, TargetLabel: "Address to resolve", TargetHint: "192.0.2.1 or 2001:db8::1"},
	} {
		page, form := debugFormPage(t, server, name)
		if form != want {
			t.Errorf("%s: the form is %+v, want %+v", name, form, want)
		}
		requireDebugForm(t, page, form)
	}
}
