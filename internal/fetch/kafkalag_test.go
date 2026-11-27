//go:build request_type_kafka

package fetch

import (
	"fmt"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/kafkatest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// kafkaClock sets the clock the offsets are read by, and empties the
// histories, for the test.
func kafkaClock(t *testing.T) *time.Time {
	t.Helper()
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	kafkaHistories.Lock()
	kafkaHistories.clusters = map[string]*kafkaClusterHistory{}
	kafkaHistories.Unlock()
	kafkaNow = func() time.Time { return at }
	t.Cleanup(func() {
		kafkaNow = time.Now
		kafkaHistories.Lock()
		kafkaHistories.clusters = map[string]*kafkaClusterHistory{}
		kafkaHistories.Unlock()
	})
	return &at
}

// kafkaSeconds is a lag in seconds as the document writes it.
func kafkaSeconds(v *float64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

// A group's lag in seconds is how long ago the first message it has not
// read was written, as the scrapes of the partition say: null at the first
// scrape of a partition that lags; interpolated between the two scrapes
// around the committed offset; at the partition's rate before the oldest
// scrape kept; 0 for a partition read to the end; and a topic's the
// longest of its partitions', null while one that lags has none. Another
// collector keeps its own history, and a partition whose offset goes back
// starts again.
func TestAKafkaGroupLagsInSeconds(t *testing.T) {
	at := kafkaClock(t)
	k := kafkaCluster3(t, false)
	scrape := func(c *model.Collector, newest, committed int64) (partition, topic string) {
		t.Helper()
		k.Edit(func(c *kafkatest.Cluster) {
			c.Topics[0].Partitions[0].Newest = newest
			c.Groups[0].Offsets["orders"][0] = committed
		})
		doc, err := kafkaDoc(t, c, k.Brokers[0].Addr)
		if err != nil {
			t.Fatal(err)
		}
		orders := doc.Groups[1].Topics[0]
		if doc.Groups[1].Group != "billing" || orders.Partitions[1].LagSeconds == nil || *orders.Partitions[1].LagSeconds != 0 || orders.Partitions[2].LagSeconds != nil {
			t.Fatalf("billing %s", kafkaJSON(t, doc.Groups[1]))
		}
		return kafkaSeconds(orders.Partitions[0].LagSeconds), kafkaSeconds(orders.LagSeconds)
	}
	c := kafkaCollector(t, nil)
	for i, step := range []struct {
		after             time.Duration
		newest, committed int64
		partition, topic  string
	}{
		{0, 100, 90, "null", "null"},
		// Offsets 100 to 159 were written over the minute: 120 at 21s.
		{time.Minute, 160, 120, "39", "39"},
		// Not written to for a minute: 95 is before the oldest scrape, at the
		// rate of 60 messages in 120s, 4 messages before offset 99.
		{time.Minute, 160, 95, "128", "128"},
		{0, 160, 160, "0", "0"},
		// The partition was made again: its history starts again.
		{time.Minute, 5, 2, "null", "null"},
	} {
		*at = at.Add(step.after)
		if partition, topic := scrape(c, step.newest, step.committed); partition != step.partition || topic != step.topic {
			t.Errorf("step %d: partition %s, topic %s; want %s, %s", i, partition, topic, step.partition, step.topic)
		}
	}
	other := kafkaCollector(t, nil)
	other.Name = "other"
	*at = at.Add(time.Minute)
	if partition, _ := scrape(other, 50, 10); partition != "null" {
		t.Errorf("another collector's first scrape: %s", partition)
	}
}

// What is kept of a partition is bounded: past kafkaHistoryPoints, every
// other point of the older half goes, the oldest and the newest kept; a
// partition not written to keeps the first and the last scrape at its
// offset; a scrape no later than the last is not kept. A partition or a
// cluster not read for kafkaHistoryIdle is dropped, and past
// kafkaMaxHistories clusters the least recently read goes.
func TestTheKafkaOffsetHistoryIsBounded(t *testing.T) {
	kafkaClock(t)
	start := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	p := &kafkaPartitionHistory{}
	for i := range 1000 {
		p.add(int64(i*10), start.Add(time.Duration(i)*time.Second))
		if len(p.points) > kafkaHistoryPoints {
			t.Fatalf("%d points after %d scrapes", len(p.points), i+1)
		}
	}
	if p.points[0].offset != 0 || p.points[len(p.points)-1].offset != 9990 {
		t.Errorf("kept %v to %v", p.points[0], p.points[len(p.points)-1])
	}
	for i := 1; i < len(p.points); i++ {
		if !p.points[i].at.After(p.points[i-1].at) || p.points[i].offset <= p.points[i-1].offset {
			t.Errorf("points %d and %d out of order", i-1, i)
		}
	}
	idle := &kafkaPartitionHistory{}
	for i := range 5 {
		idle.add(7, start.Add(time.Duration(i)*time.Minute))
	}
	idle.add(7, start)
	if len(idle.points) != 2 || !idle.points[0].at.Equal(start) || !idle.points[1].at.Equal(start.Add(4*time.Minute)) {
		t.Errorf("a partition not written to: %v", idle.points)
	}
	key := kafkaPartitionKey{"orders", 0}
	kafkaRecord("a", map[kafkaPartitionKey]int64{key: 1, {"gone", 0}: 1}, start)
	kafkaRecord("a", map[kafkaPartitionKey]int64{key: 2}, start.Add(kafkaHistoryIdle/2))
	kafkaRecord("a", map[kafkaPartitionKey]int64{key: 3}, start.Add(kafkaHistoryIdle+time.Second))
	kafkaHistories.Lock()
	partitions := len(kafkaHistories.clusters["a"].partitions)
	kafkaHistories.Unlock()
	if partitions != 1 {
		t.Errorf("%d partitions kept, want the one read again", partitions)
	}
	later := start.Add(3 * kafkaHistoryIdle)
	for i := range kafkaMaxHistories + 5 {
		kafkaRecord(fmt.Sprint("cluster ", i), map[kafkaPartitionKey]int64{key: 1}, later.Add(time.Duration(i)*time.Second))
	}
	kafkaHistories.Lock()
	_, first := kafkaHistories.clusters["cluster 0"]
	_, last := kafkaHistories.clusters[fmt.Sprint("cluster ", kafkaMaxHistories+4)]
	_, stale := kafkaHistories.clusters["a"]
	clusters := len(kafkaHistories.clusters)
	kafkaHistories.Unlock()
	if clusters != kafkaMaxHistories || first || !last || stale {
		t.Errorf("%d clusters kept, the first %v, the last %v, the idle one %v", clusters, first, last, stale)
	}
}
