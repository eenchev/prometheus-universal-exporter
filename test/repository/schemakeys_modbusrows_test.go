//go:build !select_request_types || request_type_modbus

package repository

// modbusCollector is a configuration of one modbus collector of one read,
// its name on a line of its own and a comment where a row writes another
// key of the read.
const modbusCollector = `collectors:
  - name: demo
    request:
      type: modbus
      read:
        - address: 0
          name: volts
          # read
    transform:
      type: jq
    metrics:
      - name: v
        expression: .volts
`

// modbusSchemaKeys are the keys of a modbus collector that a schema holds
// to a rule: a read's name, required and of a pattern, and its table, type
// and order, each one of a few values, "" the key left out.
func modbusSchemaKeys() []schemaKey {
	read := "          # read\n"
	return []schemaKey{
		{key: "collectors[].request.read[].name", document: modbusCollector, at: "          name: volts\n", setting: "          name: %s\n", valid: "amps", invalid: "1a", built: true},
		{key: "collectors[].request.read[].table", document: modbusCollector, at: read, setting: "          table: %s\n", valid: "input", invalid: "inputs", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.read[].type", document: modbusCollector, at: read, setting: "          type: %s\n", valid: "i16", invalid: "u8", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.read[].order", document: modbusCollector, at: read, setting: "          order: %s\n", valid: "cdab", invalid: "little", absent: true, empty: true, emptyWas: refused, built: true},
	}
}
