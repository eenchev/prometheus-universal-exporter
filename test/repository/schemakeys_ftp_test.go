//go:build request_type_ftp

package repository

import "testing"

// The keys of an ftp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnFTPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, ftpSchemaKeys())
}
