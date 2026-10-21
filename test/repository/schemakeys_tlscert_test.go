//go:build !select_request_types || request_type_tlscert

package repository

import "testing"

// tlscertCollector is a configuration of one tlscert collector, with a
// comment where a row writes a key of its request.
const tlscertCollector = `collectors:
  - name: demo
    request:
      type: tlscert
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .seconds_until_expiry
`

// tlscertSchemaKeys are the keys of a tlscert collector that a schema
// holds to a rule: an entry of allowed_targets and denied_targets, which
// is not "" and is written in ASCII, as for every type that reaches a host;
// and its tls block's insecure_skip_verify, which a tlscert collector has
// no use for, a chain that does not verify being reported and never
// refused: false is taken by both, true refused by both, which the schema
// took before it held a tlscert collector's tls block to it.
func tlscertSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.allowed_targets[]", of: "a tlscert collector's", document: tlscertCollector, at: request, setting: "      allowed_targets: [%s]\n", valid: "www.example.com", invalid: "bücher.example", absent: true, emptyWas: taken},
		{key: "collectors[].request.denied_targets[]", of: "a tlscert collector's", document: tlscertCollector, at: request, setting: "      denied_targets: [%s]\n", valid: "10.0.0.0/8", invalid: "bücher.example", absent: true, emptyWas: taken},
		{key: "collectors[].request.tls.insecure_skip_verify", document: tlscertCollector, at: request, setting: "      tls:\n        insecure_skip_verify: %s\n", valid: "false", invalid: "true", absent: true, invalidWas: taken},
	}
}

// The keys of a tlscert collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnTLSCertKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, tlscertSchemaKeys())
}
