//go:build !select_request_types || request_type_pop3

package repository

// pop3Collector is a configuration of one pop3 collector with an account,
// with a comment where a row writes a key of its request.
const pop3Collector = `collectors:
  - name: demo
    request:
      type: pop3
      username: monitor@example.com
      password_file: /etc/exporter/password
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .messages
`

// pop3SchemaKeys are the keys of a pop3 collector that a schema holds to
// a rule of allowed values, a pattern or a length: auth, which takes apop
// and user beside the mechanisms the other mail types speak.
func pop3SchemaKeys() []schemaKey {
	return []schemaKey{
		{key: "collectors[].request.auth", of: "pop3", document: pop3Collector, at: "      # request\n", setting: "      auth: %s\n", valid: "apop", invalid: "ntlm", absent: true, empty: true, emptyWas: refused, built: true},
	}
}
