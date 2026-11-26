//go:build request_type_kafka

package exporter

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/kafkatest"
)

// kafkaTestCluster is a cluster of two brokers with a topic, orders, of two
// partitions, and a group, billing, behind on one of them.
func kafkaTestCluster(t *testing.T) string {
	t.Helper()
	k := kafkatest.New(t, 2, false)
	k.Edit(func(c *kafkatest.Cluster) {
		c.Topics = []kafkatest.Topic{{Name: "orders", Partitions: []kafkatest.Partition{
			{Leader: 1, Replicas: []int32{1, 2}, ISR: []int32{1, 2}, Newest: 120, Oldest: 20},
			{Leader: 2, Replicas: []int32{2, 1}, ISR: []int32{2}, Newest: 30},
		}}}
		c.Groups = []kafkatest.Group{{Name: "billing", Protocol: "consumer", State: "Stable", Members: 3, Coordinator: 2,
			Offsets: map[string]map[int32]int64{"orders": {0: 113, 1: 30}}}}
	})
	return k.Brokers[0].Addr
}

// A kafka collector's probe gives the series its jq rules read from the
// document: the brokers, a topic's partitions and those under-replicated,
// a group's lag by topic and its members, with a label value filled from
// the probe; the verbose self-metrics label the scrape with kafka:// and
// the broker; the collectors page hints the target; and a python transform
// reads the document as data.
func TestAKafkaCollectorProbe(t *testing.T) {
	addr := kafkaTestCluster(t)
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: kafka
    request:
      type: kafka
    transform:
      type: jq
    metrics:
      - name: kafka_brokers
        expression: .brokers | length
        labels:
          - name: cluster
            value: "{{param_cluster}}"
      - name: kafka_topic_partitions
        items: .topics[]
        expression: .partition_count
        labels:
          - name: topic
            expression: .name
      - name: kafka_topic_under_replicated_partitions
        items: .topics[]
        expression: .under_replicated
        labels:
          - name: topic
            expression: .name
      - name: kafka_consumergroup_lag
        items: .groups[] | .topics[] | select(.lag != null)
        expression: .lag
        labels:
          - name: consumergroup
            expression: .group
          - name: topic
            expression: .topic
      - name: kafka_consumergroup_members
        items: .groups[] | select(.members != null)
        expression: .members
        labels:
          - name: consumergroup
            expression: .group
  - name: kafkapy
    request:
      type: kafka
    transform:
      type: python
      script: |
        metric(name="kafka_topics", type="gauge", value=len(data["topics"]), labels={"cluster": data["cluster_id"]})
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=kafka&param_cluster=main&target="+addr, nil)
	want := `kafka_brokers{cluster="main"} 2` + "\n" +
		`kafka_consumergroup_lag{consumergroup="billing",topic="orders"} 7` + "\n" +
		`kafka_consumergroup_members{consumergroup="billing"} 3` + "\n" +
		`kafka_topic_partitions{topic="orders"} 2` + "\n" +
		`kafka_topic_under_replicated_partitions{topic="orders"} 1`
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != want {
		t.Errorf("%d %v\n%s", recorder.Code, got, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=kafka&param_cluster=main&target=http://kafka.example.com", nil); recorder.Code != http.StatusBadRequest {
		t.Errorf("a target that is no broker: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="kafka",http_method="METADATA",url="kafka://` + addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, "host:9092") {
		t.Errorf("the collectors page hints no kafka target:\n%s", page)
	}
	requirePython(t)
	recorder = probeOnce(t, server, "/probe?collector=kafkapy&target="+addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != `kafka_topics{cluster="stand-in-cluster"} 1` {
		t.Errorf("python: %d %v\n%s", recorder.Code, got, recorder.Body)
	}
}
