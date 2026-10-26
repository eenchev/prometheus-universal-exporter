//go:build request_type_sshexec

package repository

import "testing"

// The keys of an sshexec collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSSHExecKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, sshexecSchemaKeys())
}
