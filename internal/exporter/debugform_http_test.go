//go:build !select_request_types || request_type_http

package exporter

import (
	"testing"
)

// The debug page asks a run of an http collector for a URL, its own probe
// parameters with their defaults and the others its type takes, and, of a
// pasted response, the body with its Content-Type, status and headers; so
// does the playground, an http collector, of a pasted response.
func TestTheDebugPageAsksAnHTTPCollectorForAURLAndItsParameters(t *testing.T) {
	server := debugFormServer(t, `  - name: weather
    request:
      type: http
      path: /forecast/{{param_city:sofia}}
      query: {days: "{{param_days}}"}
      forward_headers: [X-Tenant]
    decoder: {type: json}
    transform: {type: jq}
    metrics: [{name: t, expression: .t}]
`)
	page, form := debugFormPage(t, server, "weather")
	want := debugForm{
		RequestType: "http", Answer: "Paste what the target answers: the body, with its Content-Type, status and headers",
		ContentType: true, HTTPAnswer: true, TargetLabel: "URL", TargetHint: "https://host:port",
		Params: "param_city=sofia\nparam_days=\nalso: method, path, body, insecure_skip_verify, follow_redirects, enable_http2, retry_attempts, retry_backoff, header_X-Tenant",
	}
	if form != want {
		t.Errorf("the form is %+v, want %+v", form, want)
	}
	requireDebugForm(t, page, form)
	requireInPage(t, page, `<p class="note" id="fetched-through">Probed through weather, the collector chosen.</p>`,
		`The target is probed as <code>/probe?debug=true</code> probes it`)
	playground, _ := debugFormPage(t, server, "")
	requireInPage(t, playground, `<div id="content-type-part">`, `<div class="narrow" id="status-part">`, `<div id="headers-part">`)
}
