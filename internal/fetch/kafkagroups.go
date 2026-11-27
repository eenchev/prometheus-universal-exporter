//go:build request_type_kafka

package fetch

import "fmt"

// consumerGroups asks a coordinator, by ConsumerGroupDescribe v0 (Kafka 3.7
// on), the state and members of the groups at indexes that DescribeGroups
// described as dead or did not find, which are those of Kafka 4's consumer
// protocol (KIP-848). A group it does not find either is one of the
// classic protocol that is gone, and keeps what DescribeGroups said.
func consumerGroups(conn *kafkaConn, groups []kafkaGroupDoc, indexes []int) error {
	w := kafkaWriter{}
	w.compactArray(len(indexes))
	at := map[string]int{}
	for _, i := range indexes {
		w.compactString(groups[i].Group)
		at[groups[i].Group] = i
	}
	w.bool(false) // include_authorized_operations
	w.noTags()
	r, err := conn.roundTrip(kafkaConsumerGroupDescribe, kafkaConsumerGroupDescribe.version, w.b)
	if err != nil {
		return err
	}
	r.int32() // throttle_time_ms
	for n := r.compactArray(20); n > 0; n-- {
		code := r.int16()
		said, _ := r.compactNullableString()
		name, state := r.compactString(), r.compactString()
		r.int32()         // group_epoch
		r.int32()         // assignment_epoch
		r.compactString() // assignor_name
		members := r.compactArray(30)
		for range members {
			r.compactString()         // member_id
			r.compactNullableString() // instance_id
			r.compactNullableString() // rack_id
			r.int32()                 // member_epoch
			r.compactString()         // client_id
			r.compactString()         // client_host
			for t := r.compactArray(1); t > 0; t-- {
				r.compactString() // subscribed_topic_names
			}
			r.compactNullableString() // subscribed_topic_regex
			for range 2 {             // assignment, target_assignment
				for t := r.compactArray(19); t > 0; t-- {
					r.take(16)        // topic_id
					r.compactString() // topic_name
					for p := r.compactArray(4); p > 0; p-- {
						r.int32()
					}
					r.skipTags()
				}
				r.skipTags()
			}
			r.skipTags()
		}
		r.int32() // authorized_operations
		r.skipTags()
		i, ok := at[name]
		if !ok || r.err != nil || code == 69 { // GROUP_ID_NOT_FOUND: not of the consumer protocol
			continue
		}
		g := &groups[i]
		g.GroupProtocol, g.State, g.Members, g.Error = "consumer", nil, nil, ""
		if code != 0 {
			if said == "" {
				said = kafkaError(code)
			}
			g.Error = fmt.Sprintf("describing group %q: %s", name, brokerSaid(said))
			continue
		}
		g.State, g.Members = &state, &members
	}
	r.skipTags()
	return conn.decoded(kafkaConsumerGroupDescribe, r)
}
