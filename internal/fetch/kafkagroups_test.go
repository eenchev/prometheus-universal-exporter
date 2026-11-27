//go:build request_type_kafka

package fetch

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/kafkatest"
)

// kafkaGroup is the group named name of doc.
func kafkaGroup(t *testing.T, doc kafkaDocument, name string) kafkaGroupDoc {
	t.Helper()
	for _, g := range doc.Groups {
		if g.Group == name {
			return g
		}
	}
	t.Fatalf("no group %q in %s", name, kafkaJSON(t, doc.Groups))
	return kafkaGroupDoc{}
}

// A group of Kafka 4's consumer protocol (KIP-848), which DescribeGroups
// describes as dead, is described by ConsumerGroupDescribe, a flexible
// version, of the coordinator that speaks it: its state and members, with
// its lag as any group's. A classic group keeps DescribeGroups' answer, a
// dead one too, which ConsumerGroupDescribe does not find. A group that
// ConsumerGroupDescribe refuses has its error; one it cannot answer fails
// only the groups it was asked about. A coordinator that does not speak it
// is not sent it, and its groups are the classic protocol's.
func TestAKafkaScrapeDescribesKafka4sConsumerGroups(t *testing.T) {
	k := kafkaCluster3(t, false)
	k.Edit(func(c *kafkatest.Cluster) {
		c.Groups = append(c.Groups,
			kafkatest.Group{Name: "streams", Protocol: "consumer", State: "Stable", Members: 3, Coordinator: 2, Consumer: true, Offsets: map[string]map[int32]int64{"orders": {0: 95}}},
			kafkatest.Group{Name: "hidden", Protocol: "consumer", State: "Stable", Coordinator: 2, Consumer: true, ConsumerError: kafkatest.GroupAuthorization},
			kafkatest.Group{Name: "gone", Protocol: "consumer", State: "Dead", Coordinator: 2},
		)
	})
	doc, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr)
	if err != nil {
		t.Fatal(err)
	}
	streams := kafkaGroup(t, doc, "streams")
	if streams.GroupProtocol != "consumer" || *streams.State != "Stable" || *streams.Members != 3 || streams.Error != "" || *streams.Topics[0].Lag != 5 {
		t.Errorf("streams %s", kafkaJSON(t, streams))
	}
	if hidden := kafkaGroup(t, doc, "hidden"); hidden.GroupProtocol != "consumer" || hidden.State != nil || hidden.Error != `describing group "hidden": GROUP_AUTHORIZATION_FAILED` || !slices.Contains(doc.Errors, hidden.Error) {
		t.Errorf("hidden %s, errors %q", kafkaJSON(t, hidden), doc.Errors)
	}
	if gone := kafkaGroup(t, doc, "gone"); gone.GroupProtocol != "classic" || *gone.State != "Dead" || gone.Error != "" {
		t.Errorf("gone %s", kafkaJSON(t, gone))
	}
	if billing := kafkaGroup(t, doc, "billing"); billing.GroupProtocol != "classic" || *billing.State != "Stable" {
		t.Errorf("billing %s", kafkaJSON(t, billing))
	}
	described := map[int32]bool{}
	for _, a := range k.Asked() {
		if a.API == kafkatest.ConsumerGroupDescribe {
			described[a.Broker] = true
		}
	}
	if len(described) != 1 || !described[2] {
		t.Errorf("ConsumerGroupDescribe sent to %v, want broker 2 alone, the one with groups DescribeGroups said were dead", described)
	}

	k.Edit(func(c *kafkatest.Cluster) {
		c.Answer = func(_ int32, api int16) []byte {
			if api == kafkatest.ConsumerGroupDescribe {
				return []byte{0, 0, 0, 0, 0, 9}
			}
			return nil
		}
	})
	if doc, err = kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr); err != nil {
		t.Fatal(err)
	}
	if streams := kafkaGroup(t, doc, "streams"); !strings.Contains(streams.Error, `describing group "streams": `) || !strings.Contains(streams.Error, "answered ConsumerGroupDescribe with what does not decode") || *streams.Topics[0].Lag != 5 {
		t.Errorf("an answer that does not decode: %s", kafkaJSON(t, streams))
	}
	if billing := kafkaGroup(t, doc, "billing"); billing.Error != "" || *billing.Topics[0].Lag != 10 {
		t.Errorf("billing beside it %s", kafkaJSON(t, billing))
	}

	k.Edit(func(c *kafkatest.Cluster) {
		c.Answer = nil
		c.Versions = map[int16][2]int16{kafkatest.ConsumerGroupDescribe: {-1, -1}}
	})
	before := len(k.Asked())
	if doc, err = kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr); err != nil {
		t.Fatal(err)
	}
	if streams := kafkaGroup(t, doc, "streams"); streams.GroupProtocol != "classic" || *streams.State != "Dead" || *streams.Members != 0 {
		t.Errorf("without ConsumerGroupDescribe: %s", kafkaJSON(t, streams))
	}
	for _, a := range k.Asked()[before:] {
		if a.API == kafkatest.ConsumerGroupDescribe {
			t.Errorf("ConsumerGroupDescribe sent to broker %d, which does not speak it", a.Broker)
		}
	}
}
