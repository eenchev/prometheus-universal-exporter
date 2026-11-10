//go:build !select_request_types || request_type_websocket

package repository

// websocketCollector is a configuration of one websocket collector, with a
// comment where a row writes a key of its request.
const websocketCollector = `collectors:
  - name: demo
    request:
      type: websocket
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .depth
`

// websocketSchemaKeys are the keys of a websocket collector that a schema
// holds to a rule: its duration.
func websocketSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.read_window", document: websocketCollector, at: request, setting: "      read_window: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true, built: true},
	}
}
