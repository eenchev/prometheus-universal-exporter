//go:build request_type_pop3

package repository

import "testing"

// The keys of a pop3 collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnPOP3KeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, pop3SchemaKeys())
}
