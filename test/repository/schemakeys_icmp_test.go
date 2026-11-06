//go:build request_type_icmp

package repository

import "testing"

// The keys of an icmp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnICMPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, icmpSchemaKeys())
}
