//go:build request_type_websocket

package repository

import "testing"

// The keys of a websocket collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnWebSocketKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, websocketSchemaKeys())
}
