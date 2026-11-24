//go:build request_type_amqp

package repository

import "testing"

// The keys of an amqp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnAMQPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, amqpSchemaKeys())
}
