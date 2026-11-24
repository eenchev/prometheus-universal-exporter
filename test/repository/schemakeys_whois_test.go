//go:build request_type_whois

package repository

import "testing"

// The keys of a whois collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnWhoisKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, whoisSchemaKeys())
}
