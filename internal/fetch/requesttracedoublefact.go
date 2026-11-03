//go:build !select_request_types || request_type_ntp || request_type_tlscert

package fetch

// The request types whose spans tell a number that is no integer, such as
// a time in seconds, make it with doubleFact; a build of none of them has no
// use for it.

// doubleFact makes a TraceFact of a floating-point number.
func doubleFact(key string, value float64) TraceFact {
	return TraceFact{Key: key, Kind: TraceFactDouble, Double: value}
}
