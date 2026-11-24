//go:build request_type_sip

package repository

import "testing"

// The keys of an sip collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSIPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, sipSchemaKeys())
}
