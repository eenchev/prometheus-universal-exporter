//go:build !select_request_types || request_type_ftp

package repository

// ftpSchemaKeys are the keys of an ftp collector that a schema holds to a
// rule: none of its own, the type having no key that is not the common
// part's, whose rows are the types' that take them in every build.
func ftpSchemaKeys() []schemaKey {
	return nil
}
