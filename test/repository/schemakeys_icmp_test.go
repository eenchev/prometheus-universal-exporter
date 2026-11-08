//go:build !select_request_types || request_type_icmp

package repository

import "testing"

// The keys of an icmp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnICMPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, icmpSchemaKeys())
}

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
// default. The schema and the exporter give each one verdict
// (TestEveryConstrainedKeyOfTheSchemasIsInATable reads the table).
func icmpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.interval", of: "an icmp collector's", document: icmpCollector, at: request, setting: "      interval: %s\n", valid: "200ms", invalid: "often", absent: true, duration: true},
		{key: "collectors[].request.interval", of: "an icmp collector's under its least", document: icmpCollector, at: request, setting: "      interval: %s\n", valid: "10ms", invalid: "5ms", absent: true, invalidWas: taken},
		{key: "collectors[].request.reply_timeout", of: "an icmp collector's", document: icmpCollector, at: request, setting: "      reply_timeout: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true},
		{key: "collectors[].request.reply_timeout", of: "an icmp collector's under its least", document: icmpCollector, at: request, setting: "      reply_timeout: %s\n", valid: "10ms", invalid: "1ms", absent: true, invalidWas: taken},
	}
}
