//go:build !select_request_types || request_type_mqtt

package repository

// mqttCollector is a configuration of one mqtt collector, with a comment
// where a row writes a key of its request.
const mqttCollector = `collectors:
  - name: demo
    request:
      type: mqtt
      topics: [sensors/#]
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .messages | length
`

// mqttSchemaKeys are the keys of an mqtt collector that a schema holds to
// a rule: its durations.
func mqttSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.window", document: mqttCollector, at: request, setting: "      window: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true, built: true},
		{key: "collectors[].request.keep_alive", document: mqttCollector, at: request, setting: "      keep_alive: %s\n", valid: "60s", invalid: "often", absent: true, duration: true, built: true},
	}
}
