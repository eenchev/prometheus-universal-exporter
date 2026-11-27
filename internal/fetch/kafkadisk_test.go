//go:build request_type_kafka

package fetch

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/kafkatest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// kafkaDiskCluster is kafkaCluster3 with the bytes each replica takes, a
// copy of orders' partition 0 that broker 3 is moving to another
// directory, and orders' and payments' settings, payments' refused.
func kafkaDiskCluster(t *testing.T) *kafkatest.Cluster {
	t.Helper()
	k := kafkaCluster3(t, false)
	secret, retention, policy := "hidden", "604800000", "delete"
	k.Edit(func(c *kafkatest.Cluster) {
		c.Brokers[0].LogDir = "/data/kafka"
		orders := c.Topics[0].Partitions
		orders[0].Sizes, orders[0].Future = map[int32]int64{1: 1000, 2: 900, 3: 950}, map[int32]int64{3: 300}
		orders[1].Sizes = map[int32]int64{2: 500, 3: 500, 1: 400}
		c.Topics[1].Partitions[0].Sizes = map[int32]int64{1: 70, 2: 70, 3: 70}
		c.Topics[2].Partitions[0].Sizes = map[int32]int64{1: 5}
		c.Topics[0].Configs = map[string]*string{"retention.ms": &retention, "cleanup.policy": &policy, "sasl.jaas.config": &secret, "segment.ms": nil}
		c.Topics[0].Sensitive = []string{"sasl.jaas.config"}
		c.Topics[1].ConfigError = kafkatest.TopicAuthorization
	})
	return k
}

// request.log_dirs asks every broker for its log directories: each broker
// reports what its directories hold, every topic, chosen or not, and the
// copies it is moving included; each chosen topic what all its replicas
// take; and each partition what its leader's replica takes. request.topic_
// configs asks one broker the settings it names of the chosen topics: a
// setting the broker does not show, a password or one it does not know, is
// null, and a topic it refuses has none, with an error.
func TestAKafkaScrapeReadsTheDiskAndTheTopicsSettings(t *testing.T) {
	k := kafkaDiskCluster(t)
	c := kafkaCollector(t, func(_ *model.Collector, r *KafkaConfig) {
		r.LogDirs, r.TopicConfigs = true, []string{"retention.ms", "cleanup.policy", "sasl.jaas.config", "segment.ms", "no.such.setting"}
	})
	doc, err := kafkaDoc(t, c, k.Brokers[0].Addr)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kafkaJSON(t, doc.Brokers[0].LogDirs), `[{"dir":"/data/kafka","bytes":1475,"error":""}]`; got != want {
		t.Errorf("broker 1's log directories %s, want %s", got, want)
	}
	for i, want := range []int64{1475, 1470, 1820} {
		if b := doc.Brokers[i]; b.DiskBytes == nil || *b.DiskBytes != want || len(b.LogDirs) != 1 {
			t.Errorf("broker %d: %s, want %d bytes", b.ID, kafkaJSON(t, b), want)
		}
	}
	orders, payments := doc.Topics[0], doc.Topics[1]
	if orders.DiskBytes == nil || *orders.DiskBytes != 4550 || payments.DiskBytes == nil || *payments.DiskBytes != 210 {
		t.Errorf("orders %v, payments %v bytes", kafkaJSON(t, orders.DiskBytes), kafkaJSON(t, payments.DiskBytes))
	}
	sizes := []string{}
	for _, p := range orders.Partitions {
		sizes = append(sizes, kafkaJSON(t, p.SizeBytes))
	}
	if !slices.Equal(sizes, []string{"1000", "500", "null"}) || *payments.Partitions[0].SizeBytes != 70 {
		t.Errorf("orders' partitions take %v", sizes)
	}
	if got, want := kafkaJSON(t, orders.Configs), `{"cleanup.policy":"delete","no.such.setting":null,"retention.ms":"604800000","sasl.jaas.config":null,"segment.ms":null}`; got != want {
		t.Errorf("orders' settings %s, want %s", got, want)
	}
	if payments.Configs != nil || !slices.Contains(doc.Errors, `describing the settings of topic "payments": refused by the stand-in`) {
		t.Errorf("payments' settings %v, errors %q", payments.Configs, doc.Errors)
	}
	asked := map[int16][]int32{}
	for _, a := range k.Asked() {
		asked[a.API] = append(asked[a.API], a.Broker)
	}
	slices.Sort(asked[kafkatest.DescribeLogDirs])
	if !slices.Equal(asked[kafkatest.DescribeLogDirs], []int32{1, 2, 3}) || !slices.Equal(asked[kafkatest.DescribeConfigs], []int32{1}) {
		t.Errorf("asked %v", asked)
	}
}

// Without request.log_dirs and request.topic_configs a scrape asks neither,
// nor needs a broker to speak them; with them, a broker that does not is
// refused. A broker that lists no directory, as Kafka answers a login
// without the permission, and one that cannot be reached have none, and a
// topic with a replica on one has no total; a directory with an error,
// such as a failed disk, says so and holds nothing.
func TestAKafkaScrapeOfTheDiskGoesOnWithoutABroker(t *testing.T) {
	k := kafkaDiskCluster(t)
	k.Edit(func(c *kafkatest.Cluster) {
		c.Versions = map[int16][2]int16{kafkatest.DescribeLogDirs: {0, 0}, kafkatest.DescribeConfigs: {0, 0}}
	})
	doc, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr)
	if err != nil || doc.Brokers[0].LogDirs != nil || doc.Brokers[0].DiskBytes != nil || doc.Topics[0].DiskBytes != nil || doc.Topics[0].Configs != nil {
		t.Fatalf("without the keys: %v, %s", err, kafkaJSON(t, doc))
	}
	for _, a := range k.Asked() {
		if a.API == kafkatest.DescribeLogDirs || a.API == kafkatest.DescribeConfigs {
			t.Errorf("asked API %d unasked", a.API)
		}
	}
	disk := kafkaCollector(t, func(_ *model.Collector, r *KafkaConfig) { r.LogDirs = true })
	if _, err := kafkaDoc(t, disk, k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "speaks DescribeLogDirs v0 to v0, not v1") {
		t.Errorf("a broker without DescribeLogDirs v1: %v", err)
	}
	settings := kafkaCollector(t, func(_ *model.Collector, r *KafkaConfig) { r.TopicConfigs = []string{"retention.ms"} })
	if _, err := kafkaDoc(t, settings, k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "speaks DescribeConfigs v0 to v0, not v2") {
		t.Errorf("a broker without DescribeConfigs v2: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) {
		c.Versions = nil
		c.Brokers[1].HideLogDirs = true
		c.Brokers[2].LogDirError = kafkatest.StorageError
	})
	doc, err = kafkaDoc(t, disk, k.Brokers[0].Addr)
	if err != nil {
		t.Fatal(err)
	}
	if b := doc.Brokers[1]; b.LogDirs != nil || b.DiskBytes != nil || !slices.ContainsFunc(doc.Errors, func(e string) bool {
		return strings.HasPrefix(e, "asking broker 2 for its log directories: ") && strings.Contains(e, "listed no log directories, as Kafka answers a login without the DESCRIBE permission on the cluster")
	}) {
		t.Errorf("broker 2: %s, errors %q", kafkaJSON(t, b), doc.Errors)
	}
	if got, want := kafkaJSON(t, doc.Brokers[2].LogDirs), `[{"dir":"/var/lib/kafka","bytes":null,"error":"KAFKA_STORAGE_ERROR"}]`; got != want || *doc.Brokers[2].DiskBytes != 0 {
		t.Errorf("broker 3's directories %s, want %s", got, want)
	}
	if doc.Topics[0].DiskBytes != nil || doc.Topics[0].Partitions[0].SizeBytes == nil || doc.Topics[0].Partitions[1].SizeBytes != nil {
		t.Errorf("orders %s", kafkaJSON(t, doc.Topics[0]))
	}
	k.Stop(3)
	if doc, err = kafkaDoc(t, disk, k.Brokers[0].Addr); err != nil || doc.Brokers[2].LogDirs != nil || !slices.ContainsFunc(doc.Errors, func(e string) bool { return strings.HasPrefix(e, "asking broker 3 for its log directories: ") }) {
		t.Errorf("broker 3 down: %v, %q", err, doc.Errors)
	}
}
