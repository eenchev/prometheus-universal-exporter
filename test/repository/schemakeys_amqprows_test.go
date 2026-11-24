//go:build !select_request_types || request_type_amqp

package repository

// amqpCollector is a configuration of one amqp collector, with a comment
// where a row writes a key of its request.
const amqpCollector = `collectors:
  - name: demo
    request:
      type: amqp
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .version
`

// amqpSchemaKeys are the keys of an amqp collector that a schema holds to
// a rule: vhost, a name of at most 255 bytes, and queues, at most 100 names.
func amqpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.vhost", of: "amqp", document: amqpCollector, at: request, setting: "      vhost: %s\n", valid: "/prod", invalid: `"a\tb"`, absent: true, empty: true, built: true},
		{key: "collectors[].request.queues[]", of: "amqp", document: amqpCollector, at: request, setting: "      queues: [%s]\n", valid: "orders", invalid: `"a\tb"`, absent: true, built: true},
	}
}
