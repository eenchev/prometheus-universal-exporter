//go:build request_type_tcp

package exporter

import "testing"

// A tcp collector's send holding a credential is withheld whole: a field
// named like one, and a protocol's login command, whose argument reads as
// a credential by the command alone.
func TestTheDebugPageWithholdsATCPSendHoldingACredential(t *testing.T) {
	requireFreeTextWithheld(t, `  - name: auth
    request:
      type: tcp
      send: "AUTH s3cret-auth\r\nINFO\r\n"
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
  - name: pass
    request:
      type: tcp
      send: "USER monitor\r\npass s3cret-pass\r\nSTAT\r\n"
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
  - name: folded
    request:
      type: tcp
      send: >-
        stats
        token=s3cret-folded
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
`)
}
