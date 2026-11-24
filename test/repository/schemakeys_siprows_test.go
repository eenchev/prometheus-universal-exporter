//go:build !select_request_types || request_type_sip

package repository

// sipSchemaKeys are the keys of an sip collector that a schema holds to
// a rule: allowed values, a pattern or a length. TODO(sip): a row for
// each, as schemakeys_tcprows_test.go has, built: true for a key of an
// opt-in type, which the committed schemas, the default build's, leave out.
func sipSchemaKeys() []schemaKey {
	return nil
}
