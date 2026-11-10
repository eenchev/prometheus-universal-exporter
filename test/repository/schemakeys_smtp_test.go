//go:build request_type_smtp

package repository

import "testing"

// The keys of an smtp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSMTPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, smtpSchemaKeys())
}
