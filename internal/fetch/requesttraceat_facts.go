//go:build !select_request_types || request_type_icmp || request_type_ntp

package fetch

// facts adds facts to those of the request (traceFacts), for the types that
// tell facts of requests recorded at their own places: icmp and ntp.
func (a tracedAt) facts(facts ...TraceFact) {
	a.edit(func(r *TracedRequest) { r.Facts = append(r.Facts, facts...) })
}
