//go:build !select_request_types || request_type_whois

package repository

// whoisSchemaKeys are the keys of a whois collector that a schema holds to
// a rule: allowed values, a pattern or a length. TODO(whois): a row for
// each, as schemakeys_tcprows_test.go has, built: true for a key of an
// opt-in type, which the committed schemas, the default build's, leave out.
func whoisSchemaKeys() []schemaKey {
	return nil
}
