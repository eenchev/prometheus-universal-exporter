//go:build !select_request_types || request_type_ntp

package repository

import "testing"

// ntpSchemaKeys are the keys of an ntp collector that a schema holds to
// a rule: none, since the type has no keys of its own and takes of the
// common part only the target lists, which http's rows hold.
func ntpSchemaKeys() []schemaKey {
	return nil
}

// The keys of an ntp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnNTPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, ntpSchemaKeys())
}
