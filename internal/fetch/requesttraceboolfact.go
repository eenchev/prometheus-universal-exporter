//go:build !select_request_types || request_type_dns || request_type_sql || request_type_tcp || request_type_tlscert

package fetch

// The request types whose spans tell a yes or a no make it with boolFact;
// a build of none of them has no use for it.

// boolFact makes a TraceFact of a yes or a no.
func boolFact(key string, value bool) TraceFact {
	return TraceFact{Key: key, Kind: TraceFactBool, Bool: value}
}
