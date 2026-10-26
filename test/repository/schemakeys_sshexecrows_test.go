//go:build !select_request_types || request_type_sshexec

package repository

// sshexecSchemaKeys are the keys of a sshexec collector that a schema holds to
// a rule: allowed values, a pattern or a length. TODO(sshexec): a row for
// each, as schemakeys_tcprows_test.go has, built: true for a key of an
// opt-in type, which the committed schemas, the default build's, leave out.
func sshexecSchemaKeys() []schemaKey {
	return nil
}
