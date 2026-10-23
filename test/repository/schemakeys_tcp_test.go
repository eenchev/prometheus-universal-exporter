//go:build request_type_tcp

package repository

import "testing"

// The keys of a tcp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnTCPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, tcpSchemaKeys())
}
