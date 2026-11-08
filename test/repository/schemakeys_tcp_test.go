//go:build !select_request_types || request_type_tcp

package repository

import "testing"

// The keys of a tcp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnTCPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, tcpSchemaKeys())
}

// tcpCollector is a configuration of one tcp collector, with a comment
// where a row writes a key of its request.
const tcpCollector = `collectors:
  - name: demo
    request:
      type: tcp
      send: "stats\r\n"
      # request
    transform:
      type: regex
    metrics:
      - name: v
        expression: 'STAT curr_connections (\d+)'
`

// tcpSteps is a configuration of one tcp collector of two steps, the
// first's send where a row writes it, a line of its own after its until, so
// that it may be left out.
const tcpSteps = `collectors:
  - name: demo
    request:
      type: tcp
      steps:
        - until: '^250 '
          send: "EHLO x\r\n"
        - send: "QUIT\r\n"
    transform:
      type: regex
    metrics:
      - name: v
        expression: '^(\d+)'
`

// tcpConnect is a configuration of one tcp collector of connect_only, with
// a comment where a row writes a key of its request.
const tcpConnect = `collectors:
  - name: demo
    request:
      type: tcp
      connect_only: true
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: 'if .open then 1 else 0 end'
`

// tcpSchemaKeys are the keys of a tcp collector that a schema holds to a
// rule: an entry of allowed_targets and denied_targets, which is not "" and
// is written in ASCII, as for every type that reaches a host, and
// starttls, one of four protocols, exactly, "" the key left out; a
// step's send, required and not ""; and connect_timeout, a duration of at
// least 10ms. send and
// terminator are free text, "" the key left out. The schema and the exporter give each one verdict
// (TestEveryConstrainedKeyOfTheSchemasIsInATable reads the table).
func tcpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.allowed_targets[]", of: "a tcp collector's", document: tcpCollector, at: request, setting: "      allowed_targets: [%s]\n", valid: "memcached.cache", invalid: "bücher.example", absent: true, emptyWas: taken},
		{key: "collectors[].request.starttls", of: "a tcp collector's", document: tcpCollector, at: request, setting: "      starttls: %s\n", valid: "smtp", invalid: "SMTP", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.steps[].send", document: tcpSteps, at: "          send: \"EHLO x\\r\\n\"\n", setting: "          send: %s\n", valid: `"HELO x\r\n"`},
		{key: "collectors[].request.connect_timeout", of: "a tcp collector's", document: tcpConnect, at: request, setting: "      connect_timeout: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].request.connect_timeout", of: "a tcp collector's under its least", document: tcpConnect, at: request, setting: "      connect_timeout: %s\n", valid: "10ms", invalid: "1ms", absent: true, invalidWas: taken},
		{key: "collectors[].request.denied_targets[]", of: "a tcp collector's", document: tcpCollector, at: request, setting: "      denied_targets: [%s]\n", valid: "10.0.0.0/8", invalid: "bücher.example", absent: true, emptyWas: taken},
	}
}
