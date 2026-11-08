//go:build !select_request_types || request_type_dns || request_type_redis || request_type_tcp || request_type_tlscert

package fetch

// The request types whose spans tell a name of the exporter's vocabulary —
// a record type, a reply's type, a TLS version, why a port is not open —
// make it with nameFact; a build of none of them has no use for it.

// nameFact makes a TraceFact of a name the exporter chose, never of text
// the target sent.
func nameFact(key, name string) TraceFact {
	return TraceFact{Key: key, Kind: TraceFactName, Name: name}
}
