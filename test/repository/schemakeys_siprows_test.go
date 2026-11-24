//go:build !select_request_types || request_type_sip

package repository

// sipCollector is a configuration of one sip collector, with a comment
// where a row writes a key of its request.
const sipCollector = `collectors:
  - name: demo
    request:
      type: sip
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .ok
`

// sipSchemaKeys are the keys of a sip collector that a schema holds to a
// rule: sip_transport, of three values; request_uri and from_uri, SIP URIs
// of at most 255 bytes; and user_agent, a line of at most 128.
func sipSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.sip_transport", of: "sip", document: sipCollector, at: request, setting: "      sip_transport: %s\n", valid: "tcp", invalid: "sctp", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.request_uri", of: "sip", document: sipCollector, at: request, setting: "      request_uri: %s\n", valid: "sip:1000@pbx.example.com", invalid: "pbx.example.com", absent: true, empty: true, built: true},
		{key: "collectors[].request.from_uri", of: "sip", document: sipCollector, at: request, setting: "      from_uri: %s\n", valid: "sips:probe@example.com", invalid: `"sip:a b@example.com"`, absent: true, empty: true, built: true},
		{key: "collectors[].request.user_agent", of: "sip", document: sipCollector, at: request, setting: "      user_agent: %s\n", valid: "probe/1", invalid: `"a\tb"`, absent: true, empty: true, built: true},
	}
}
