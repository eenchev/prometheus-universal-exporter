//go:build request_type_sshexec

package repository

import "testing"

// The keys of a sshexec collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSshexecKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, sshexecSchemaKeys())
}
