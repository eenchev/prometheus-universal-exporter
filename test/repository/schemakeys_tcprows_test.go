//go:build !select_request_types || request_type_tcp

package repository

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

// tcpSchemaKeys are the keys of a tcp collector that a schema holds to a
// rule: an entry of allowed_targets and denied_targets, which is not "" and
// is written in ASCII, as for every type that reaches a host, and
// starttls, one of four protocols, exactly, "" the key left out. send and
// terminator are free text, "" the key left out. They are held in every
// build's schema, the default build's among them, which does not carry the
// opt-in type (TestEveryConstrainedKeyOfTheSchemasIsInATable), and the two
// verdicts are compared in a build with it (schemakeys_tcp_test.go).
func tcpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.allowed_targets[]", of: "a tcp collector's", document: tcpCollector, at: request, setting: "      allowed_targets: [%s]\n", valid: "memcached.cache", invalid: "bücher.example", absent: true, emptyWas: taken, built: true},
		{key: "collectors[].request.starttls", of: "a tcp collector's", document: tcpCollector, at: request, setting: "      starttls: %s\n", valid: "smtp", invalid: "SMTP", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.denied_targets[]", of: "a tcp collector's", document: tcpCollector, at: request, setting: "      denied_targets: [%s]\n", valid: "10.0.0.0/8", invalid: "bücher.example", absent: true, emptyWas: taken, built: true},
	}
}
