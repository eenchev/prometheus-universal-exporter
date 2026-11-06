//go:build !select_request_types || request_type_icmp

package repository

// icmpCollector is a configuration of one icmp collector, with a comment
// where a row writes a key of its request.
const icmpCollector = `collectors:
  - name: ping
    request:
      type: icmp
      # request
    transform:
      type: jq
    metrics:
      - name: received
        expression: .received
`

// icmpSchemaKeys are the keys of an icmp collector that a schema holds to
// a rule: interval and reply_timeout, durations of at least 10ms, 0 the
// default. They are held in every build's schema, the default build's
// among them, which does not carry the opt-in type, and the two verdicts
// are compared in a build with it (schemakeys_icmp_test.go).
func icmpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.interval", of: "an icmp collector's", document: icmpCollector, at: request, setting: "      interval: %s\n", valid: "200ms", invalid: "often", absent: true, duration: true, built: true},
		{key: "collectors[].request.interval", of: "an icmp collector's under its least", document: icmpCollector, at: request, setting: "      interval: %s\n", valid: "10ms", invalid: "5ms", absent: true, invalidWas: taken, built: true},
		{key: "collectors[].request.reply_timeout", of: "an icmp collector's", document: icmpCollector, at: request, setting: "      reply_timeout: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true, built: true},
		{key: "collectors[].request.reply_timeout", of: "an icmp collector's under its least", document: icmpCollector, at: request, setting: "      reply_timeout: %s\n", valid: "10ms", invalid: "1ms", absent: true, invalidWas: taken, built: true},
	}
}
