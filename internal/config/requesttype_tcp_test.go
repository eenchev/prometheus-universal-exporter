//go:build request_type_tcp

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A tcp collector's answer is the server's text, handed to the decoder as
// a body is: a regex transform reads it as text, a csv one as CSV, and a
// decoder written out is kept, since the answer may be JSON, YAML or
// anything else a body is. send and terminator are read as YAML writes
// them, \r\n in double quotes a carriage return and a line feed.
func TestATCPCollectorDecodesAsItsTransformReads(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: memcached
    request:
      type: tcp
      send: "stats\r\n"
      terminator: "END\r\n"
    transform:
      type: regex
    metrics:
      - name: memcached_curr_connections
        expression: 'STAT curr_connections (\d+)'
  - name: haproxy
    request:
      type: tcp
      send: "show stat\n"
    transform:
      type: csv
    metrics:
      - name: haproxy_current_sessions
        expression: scur
  - name: status
    request:
      type: tcp
    decoder:
      type: json
    transform:
      type: jq
    metrics:
      - name: up
        expression: .up
`))
	if err != nil {
		t.Fatal(err)
	}
	var decoders []string
	for _, c := range cfg.Collectors {
		decoders = append(decoders, c.Decoder.Type)
	}
	if got := strings.Join(decoders, " "); got != "text csv json" {
		t.Errorf("decoders %s", got)
	}
	if r := cfg.Collectors[0].Request; r.Send != "stats\r\n" || r.Terminator != "END\r\n" {
		t.Errorf("send %q, terminator %q", r.Send, r.Terminator)
	}
}
