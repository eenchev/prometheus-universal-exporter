//go:build request_type_smtp || request_type_imap

package fetch

// netAuthSchemaRule is what the configuration schema holds request.auth to
// for a type that speaks netAuthMechanisms and no other: the common key's
// own rule allows every mechanism any type speaks.
func netAuthSchemaRule() map[string]any {
	return map[string]any{"properties": map[string]any{"auth": map[string]any{"enum": SchemaOptionalEnum(netAuthMechanisms)}}}
}
