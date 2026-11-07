//go:build request_type_icmp

package fetch

// facts adds facts to those of the request (traceFacts), for the types that
// tell facts of requests sent at once: icmp's all_addresses.
func (a tracedAt) facts(facts ...TraceFact) {
	a.edit(func(r *TracedRequest) { r.Facts = append(r.Facts, facts...) })
}
