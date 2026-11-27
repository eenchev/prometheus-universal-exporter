//go:build request_type_kafka

package fetch

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// The disk a cluster takes and its topics' settings, each asked only when
// the collector says so, as each is a request more to every broker or to
// one:
//
//   - request.log_dirs asks every broker for its log directories and the
//     size of each replica in them (DescribeLogDirs v1, Kafka 1.1 on). A
//     broker lists only its own, so each is asked. Kafka answers a login
//     without the DESCRIBE permission on the cluster with no directories,
//     which is reported as an error.
//   - request.topic_configs asks one broker the settings it names of every
//     chosen topic (DescribeConfigs v2, Kafka 2.0 on), as Kafka works them
//     out: the topic's own, else the broker's, else the default.

// kafkaReplica names a replica: a partition and the broker holding it.
type kafkaReplica struct {
	kafkaPartitionKey
	broker int32
}

// logDirs asks every broker for its log directories and its replicas'
// sizes, into doc.
func (k *kafkaCluster) logDirs(doc *kafkaDocument, failed func(error) bool) error {
	if !kafkaRequest(k.c).LogDirs {
		return nil
	}
	ids := make([]int32, 0, len(doc.Brokers))
	for _, b := range doc.Brokers {
		ids = append(ids, b.ID)
	}
	var mu sync.Mutex
	dirs := map[int32][]kafkaLogDirDoc{}
	// sizes are the replicas' sizes, futures, the copies a broker is
	// moving to another directory, apart; topics the bytes by topic.
	sizes, topics := map[kafkaReplica]int64{}, map[string]int64{}
	answered := map[int32]bool{}
	for id, err := range k.each(ids, func(id int32, conn *kafkaConn) error {
		w := kafkaWriter{}
		w.array(-1) // every topic
		r, err := conn.roundTrip(kafkaDescribeLogDirs, kafkaDescribeLogDirs.version, w.b)
		if err != nil {
			return err
		}
		r.int32() // throttle_time_ms
		mine := []kafkaLogDirDoc{}
		own, byTopic := map[kafkaReplica]int64{}, map[string]int64{}
		for n := r.array(10); n > 0; n-- {
			code := r.int16()
			d := kafkaLogDirDoc{Dir: r.string()}
			var bytes int64
			for m := r.array(6); m > 0; m-- {
				topic := r.string()
				for p := r.array(21); p > 0; p-- {
					index, size := r.int32(), r.int64()
					r.int64() // offset_lag
					future := r.bool()
					// A directory with an error, a disk that failed, holds no
					// replica the broker serves, as Kafka lists none in it.
					if code != 0 {
						continue
					}
					bytes += size
					byTopic[topic] += size
					if !future {
						own[kafkaReplica{kafkaPartitionKey{topic, index}, id}] = size
					}
				}
			}
			if code != 0 {
				d.Error = kafkaError(code)
			} else {
				d.Bytes = &bytes
			}
			mine = append(mine, d)
		}
		if err := conn.decoded(kafkaDescribeLogDirs, r); err != nil {
			return err
		}
		if len(mine) == 0 {
			return fmt.Errorf("%s listed no log directories, as Kafka answers a login without the DESCRIBE permission on the cluster", conn.broker)
		}
		slices.SortFunc(mine, func(a, b kafkaLogDirDoc) int { return strings.Compare(a.Dir, b.Dir) })
		mu.Lock()
		defer mu.Unlock()
		dirs[id], answered[id] = mine, true
		for replica, size := range own {
			sizes[replica] = size
		}
		for topic, size := range byTopic {
			topics[topic] += size
		}
		return nil
	}) {
		if failed(fmt.Errorf("asking broker %d for its log directories: %w", id, err)) {
			return err
		}
	}
	for i := range doc.Brokers {
		b := &doc.Brokers[i]
		if !answered[b.ID] {
			continue
		}
		b.LogDirs = dirs[b.ID]
		var total int64
		for _, d := range b.LogDirs {
			if d.Bytes != nil {
				total += *d.Bytes
			}
		}
		b.DiskBytes = &total
	}
	for i := range doc.Topics {
		t := &doc.Topics[i]
		whole := true
		for j := range t.Partitions {
			p := &t.Partitions[j]
			if size, ok := sizes[kafkaReplica{kafkaPartitionKey{t.Name, p.Partition}, p.Leader}]; ok {
				p.SizeBytes = &size
			}
			for _, replica := range p.Replicas {
				whole = whole && answered[replica]
			}
		}
		if whole {
			bytes := topics[t.Name]
			t.DiskBytes = &bytes
		}
	}
	return nil
}

// configs asks boot's broker the settings request.topic_configs names of
// every chosen topic, into doc.
func (k *kafkaCluster) configs(boot *kafkaConn, doc *kafkaDocument, failed func(error) bool) error {
	names := kafkaRequest(k.c).TopicConfigs
	if len(names) == 0 || len(doc.Topics) == 0 {
		return nil
	}
	w := kafkaWriter{}
	w.array(len(doc.Topics))
	for _, t := range doc.Topics {
		w.int8(2) // resource_type: a topic
		w.string(t.Name)
		w.array(len(names))
		for _, name := range names {
			w.string(name)
		}
	}
	w.bool(false) // include_synonyms
	r, err := boot.roundTrip(kafkaDescribeConfigs, kafkaDescribeConfigs.version, w.b)
	if err != nil {
		if failed(fmt.Errorf("asking %s for the topics' settings: %w", boot.broker, err)) {
			return err
		}
		return nil
	}
	r.int32() // throttle_time_ms
	at := map[string]int{}
	for i, t := range doc.Topics {
		at[t.Name] = i
	}
	refused := []string{}
	for n := r.array(10); n > 0; n-- {
		code := r.int16()
		said, _ := r.nullableString()
		r.int8() // resource_type
		topic := r.string()
		configs := map[string]*string{}
		for m := r.array(9); m > 0; m-- {
			name := r.string()
			value, shown := r.nullableString()
			r.bool() // read_only
			r.int8() // config_source
			sensitive := r.bool()
			for s := r.array(5); s > 0; s-- {
				r.string()         // name
				r.nullableString() // value
				r.int8()           // source
			}
			if shown && !sensitive && slices.Contains(names, name) {
				configs[name] = &value
			}
		}
		i, ok := at[topic]
		if !ok || r.err != nil {
			continue
		}
		if code != 0 {
			if said == "" {
				said = kafkaError(code)
			}
			refused = append(refused, fmt.Sprintf("describing the settings of topic %q: %s", topic, brokerSaid(said)))
			continue
		}
		t := &doc.Topics[i]
		t.Configs = make(map[string]*string, len(names))
		for _, name := range names {
			t.Configs[name] = configs[name]
		}
	}
	if err := boot.decoded(kafkaDescribeConfigs, r); err != nil {
		if failed(err) {
			return err
		}
		return nil
	}
	for _, e := range refused {
		failed(errors.New(e))
	}
	return nil
}
