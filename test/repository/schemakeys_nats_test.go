//go:build request_type_nats

package repository

import "testing"

// The keys of an nats collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnNATSKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, natsSchemaKeys())
}
