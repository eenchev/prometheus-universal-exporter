//go:build !select_request_types || request_type_dns

package repository

import "testing"

// dnsCollector is a configuration of one dns collector, with a comment
// where a row writes a key of its request.
const dnsCollector = `collectors:
  - name: demo
    request:
      type: dns
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .count
`

// dnsSchemaKeys are the keys of a dns collector that a schema holds to
// allowed values or a pattern: the record type, of the list in any case and
// with blanks around it or not; the server, not blanks alone; and the
// transport, udp or tcp. Each is "" the key left out, which the schema as it
// was, before "" came to be, refused of record_type and transport.
func dnsSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.record_type", document: dnsCollector, at: request, setting: "      record_type: %s\n", valid: "MX", invalid: "SOA", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.record_type", of: "in another case, with blanks around it", document: dnsCollector, at: request, setting: "      record_type: %s\n", valid: `" aaaa\t"`, invalid: `"m x"`, absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.server", document: dnsCollector, at: request, setting: "      server: %s\n", valid: "10.0.0.53:53", invalid: `" "`, absent: true, empty: true},
		{key: "collectors[].request.transport", document: dnsCollector, at: request, setting: "      transport: %s\n", valid: "tcp", invalid: "TCP", absent: true, empty: true, emptyWas: refused},
	}
}

// The keys of a dns collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnDNSKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, dnsSchemaKeys())
}
