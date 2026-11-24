//go:build request_type_radius

package repository

import "testing"

// The keys of a radius collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnRADIUSKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, radiusSchemaKeys())
}
