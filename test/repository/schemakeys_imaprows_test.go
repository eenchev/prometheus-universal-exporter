//go:build !select_request_types || request_type_imap

package repository

// imapSchemaKeys are the keys of an imap collector that a schema holds to
// a rule of allowed values, a pattern or a length: none. Its mailboxes are
// held to their number, which the loader counts as the schema does.
func imapSchemaKeys() []schemaKey {
	return nil
}
