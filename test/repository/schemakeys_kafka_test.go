//go:build request_type_kafka

package repository

import "testing"

// The keys of a kafka collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnKafkaKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, kafkaSchemaKeys())
}
