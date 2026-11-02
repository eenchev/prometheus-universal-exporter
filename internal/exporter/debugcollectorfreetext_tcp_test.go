//go:build request_type_tcp

package exporter

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A tcp collector's send holding a credential is withheld whole: a field
// named like one, and a protocol's login command, whose argument reads as
// a credential by the command alone — at a line's start or after a ;,
// IMAP's LOGIN, Redis's HELLO with AUTH, and AUTH framed as RESP. A send
// that only has a word beginning with one, author, is shown.
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
  - name: inline
    request:
      type: tcp
      send: "PING; AUTH s3cret-inline"
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
  - name: imap
    request:
      type: tcp
      send: "a1 LOGIN monitor s3cret-imap\r\na2 SELECT INBOX\r\n"
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
  - name: hello
    request:
      type: tcp
      send: "HELLO 3 AUTH default s3cret-hello\r\nINFO\r\n"
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
  - name: resp
    request:
      type: tcp
      send: "*2\r\n$4\r\nAUTH\r\n$12\r\ns3cret-resp1\r\n"
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
`)
	cfg, err := config.Load(testutil.WriteFile(t, "plain.yaml", `collectors:
  - name: plain
    request:
      type: tcp
      send: "author Bob\r\nGET /authors HTTP/1.0\r\n\r\n"
    transform: {type: jq}
    metrics: [{name: up, expression: '1'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	shown, err := shownCollectorYAML(&cfg.Collectors[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shown, fetch.Redacted) || !strings.Contains(shown, "author Bob") {
		t.Errorf("a send without a credential is withheld:\n%s", shown)
	}
}
