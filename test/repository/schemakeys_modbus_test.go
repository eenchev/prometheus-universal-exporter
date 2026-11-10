//go:build request_type_modbus

package repository

import "testing"

// The keys of a modbus collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnModbusKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, modbusSchemaKeys())
}
