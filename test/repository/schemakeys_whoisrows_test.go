//go:build !select_request_types || request_type_whois

package repository

// whoisCollector is a configuration of one whois collector, with a comment
// where a row writes a key of its request.
const whoisCollector = `collectors:
  - name: demo
    request:
      type: whois
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .expires_timestamp_seconds
`

// whoisSchemaKeys are the keys of a whois collector that a schema holds to
// a rule: query_prefix, a line of at most 64 characters.
func whoisSchemaKeys() []schemaKey {
	return []schemaKey{
		{key: "collectors[].request.query_prefix", of: "whois", document: whoisCollector, at: "      # request\n", setting: "      query_prefix: %s\n", valid: `"-T dn,ace "`, invalid: `"a\nb"`, absent: true, empty: true, built: true},
	}
}
