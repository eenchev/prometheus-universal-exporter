//go:build request_type_kafka

package repository

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/kafkatest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/kafka writes a cluster's series as kafka_exporter does, in a
// build with the type: the brokers and each one's address and ID; each
// topic's partitions, and each partition's newest and oldest offsets, its
// replicas in sync and in all, its leader and whether it is the preferred
// one, and whether it is under-replicated; each group's committed offset
// and lag by partition, -1 for a partition it committed none of, and by
// topic, and its members. Kafka's own topics and the console consumers'
// groups are left out, and no rule of it fails.
func TestTheKafkaExampleWritesKafkaExportersSeries(t *testing.T) {
	const path = "examples/kafka/config.yaml"
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	k := kafkatest.New(t, 2, false)
	k.Edit(func(c *kafkatest.Cluster) {
		c.Topics = []kafkatest.Topic{
			{Name: "orders", Partitions: []kafkatest.Partition{
				{Leader: 1, Replicas: []int32{1, 2}, ISR: []int32{1, 2}, Newest: 120, Oldest: 20},
				{Leader: 1, Replicas: []int32{2, 1}, ISR: []int32{1}, Newest: 30},
			}},
			{Name: "__consumer_offsets", Internal: true, Partitions: []kafkatest.Partition{{Leader: 1, Replicas: []int32{1}, ISR: []int32{1}}}},
		}
		c.Groups = []kafkatest.Group{
			{Name: "billing", Protocol: "consumer", State: "Stable", Members: 3, Coordinator: 2, Offsets: map[string]map[int32]int64{"orders": {0: 113, 1: -1}}},
			{Name: "console-consumer-4711", Protocol: "consumer", State: "Empty", Coordinator: 1, Offsets: map[string]map[int32]int64{"orders": {0: 1}}},
		}
	})
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	recorder := httptest.NewRecorder()
	exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector=kafka&target="+k.Brokers[0].Addr, nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%d %s", recorder.Code, body)
	}
	_, port1, _ := net.SplitHostPort(k.Brokers[0].Addr)
	p0, p1 := `{partition="0",topic="orders"}`, `{partition="1",topic="orders"}`
	g0, g1 := `{consumergroup="billing",partition="0",topic="orders"}`, `{consumergroup="billing",partition="1",topic="orders"}`
	for _, want := range []string{
		"kafka_brokers 2", `kafka_broker_info{address="127.0.0.1:` + port1 + `",id="1"} 1`,
		`kafka_topic_partitions{topic="orders"} 2`,
		"kafka_topic_partition_current_offset" + p0 + " 120", "kafka_topic_partition_oldest_offset" + p0 + " 20",
		"kafka_topic_partition_current_offset" + p1 + " 30", "kafka_topic_partition_oldest_offset" + p1 + " 0",
		"kafka_topic_partition_in_sync_replica" + p1 + " 1", "kafka_topic_partition_replicas" + p1 + " 2",
		"kafka_topic_partition_leader" + p1 + " 1", "kafka_topic_partition_leader_is_preferred" + p0 + " 1", "kafka_topic_partition_leader_is_preferred" + p1 + " 0",
		"kafka_topic_partition_under_replicated_partition" + p0 + " 0", "kafka_topic_partition_under_replicated_partition" + p1 + " 1",
		"kafka_consumergroup_current_offset" + g0 + " 113", "kafka_consumergroup_current_offset" + g1 + " -1",
		"kafka_consumergroup_lag" + g0 + " 7", "kafka_consumergroup_lag" + g1 + " -1",
		`kafka_consumergroup_current_offset_sum{consumergroup="billing",topic="orders"} 113`,
		`kafka_consumergroup_lag_sum{consumergroup="billing",topic="orders"} 7`,
		`kafka_consumergroup_members{consumergroup="billing"} 3`,
		"kafka_scrape_errors 0",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	for _, absent := range []string{"__consumer_offsets", "console-consumer", "rule_failures"} {
		if strings.Contains(body, absent) {
			t.Errorf("%q in\n%s", absent, body)
		}
	}
}
