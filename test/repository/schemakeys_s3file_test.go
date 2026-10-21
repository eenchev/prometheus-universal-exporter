//go:build request_type_s3file

package repository

import "testing"

// The keys of an s3file collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnS3FileKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, s3fileSchemaKeys())
}
