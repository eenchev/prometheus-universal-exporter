//go:build !select_request_types || request_type_pop3

package repository

// pop3SchemaKeys are the keys of a pop3 collector that a schema holds to
// a rule of allowed values, a pattern or a length: none of its own. Its
// auth is a key of the common part, whose row is smtp's.
func pop3SchemaKeys() []schemaKey {
	return nil
}
