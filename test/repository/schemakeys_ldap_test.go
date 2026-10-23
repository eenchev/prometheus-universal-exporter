//go:build request_type_ldap

package repository

import "testing"

// The keys of an ldap collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnLDAPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, ldapSchemaKeys())
}
