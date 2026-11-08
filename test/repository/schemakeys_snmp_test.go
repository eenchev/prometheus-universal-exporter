//go:build request_type_snmp

package repository

import "testing"

// The keys of an snmp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSNMPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, snmpSchemaKeys())
}
