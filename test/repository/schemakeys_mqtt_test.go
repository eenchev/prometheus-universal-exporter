//go:build request_type_mqtt

package repository

import "testing"

// The keys of an mqtt collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnMQTTKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, mqttSchemaKeys())
}
