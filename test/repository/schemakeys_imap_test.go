//go:build request_type_imap

package repository

import "testing"

// The keys of an imap collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnIMAPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, imapSchemaKeys())
}
