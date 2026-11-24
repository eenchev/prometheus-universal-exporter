//go:build !select_request_types || request_type_nats

package repository

// natsCollector is a configuration of one nats collector that asks
// JetStream, with a comment where a row writes a key of its request.
const natsCollector = `collectors:
  - name: demo
    request:
      type: nats
      jetstream: true
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .rtt_seconds
`

// natsSchemaKeys are the keys of a nats collector that a schema holds to a
// rule: streams, each a name, and consumers, each a stream and a name.
func natsSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.streams[]", of: "nats", document: natsCollector, at: request, setting: "      streams: [%s]\n", valid: "ORDERS", invalid: "A.B", absent: true, built: true},
		{key: "collectors[].request.consumers[]", of: "nats", document: natsCollector, at: request, setting: "      consumers: [%s]\n", valid: "ORDERS/billing", invalid: "billing", absent: true, built: true},
	}
}
