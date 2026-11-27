//go:build request_type_kafka

package fetch

import (
	"math"
	"slices"
	"sync"
	"time"
)

// A consumer group's lag in seconds: how long ago the oldest message it
// has not read was written. Kafka says no such thing outright: ListOffsets
// gives a partition's newest offset now, not when each offset was written,
// and reading the message for its timestamp would read what a scrape does
// not. So each scrape keeps, by cluster, the newest offset of each partition
// and when it was read, and a lag of messages reads as the time since the
// newest offset passed the group's committed one: interpolated between the
// two scrapes around it, or, before the oldest one kept, at the rate the
// partition was written over what is kept.
//
// The first scrape of a partition that lags knows no time yet, and a
// partition not written to while it is kept has no rate, so their lag in
// seconds is null; a partition the group has read to the end lags 0
// seconds.

// kafkaOffsetPoint is a partition's newest offset and when it was read, in
// nanoseconds since the Unix epoch, which keeps a point to 16 bytes.
type kafkaOffsetPoint struct {
	offset int64
	at     int64
}

// kafkaPartitionHistory is what is kept of a partition: its points, in the
// order read, each newer and its offset no lower, and when it was last read.
type kafkaPartitionHistory struct {
	points []kafkaOffsetPoint
	used   time.Time
}

// kafkaClusterHistory is what is kept of a cluster, by partition.
type kafkaClusterHistory struct {
	partitions map[kafkaPartitionKey]*kafkaPartitionHistory
	used       time.Time
}

const (
	// kafkaHistoryPoints bounds the points kept of a partition. Past it, every
	// other point of the older half is dropped, so what is kept reaches
	// further back the longer it is kept, with fewer points.
	kafkaHistoryPoints = 64
	// kafkaHistoryIdle is how long a partition or a cluster not read again is
	// kept. kafkaMaxHistories bounds the clusters kept, and
	// kafkaMaxHistoryPartitions their partitions together, about 50 MiB of
	// points: past either, the least recently read clusters are dropped, the
	// one being read kept, as it alone has at most kafkaMaxPartitions.
	kafkaHistoryIdle          = time.Hour
	kafkaMaxHistories         = 100
	kafkaMaxHistoryPartitions = 50_000
)

// kafkaHistories are the clusters' histories, by collector, target and
// cluster ID.
var kafkaHistories = struct {
	sync.Mutex
	clusters map[string]*kafkaClusterHistory
}{clusters: map[string]*kafkaClusterHistory{}}

// kafkaNow is the clock the offsets are read by; a test sets it.
var kafkaNow = time.Now

// kafkaRecord keeps the newest offsets of cluster, read at at, and returns
// the points kept of those of them wanted, the partitions a group lags on.
func kafkaRecord(cluster string, newest map[kafkaPartitionKey]int64, at time.Time, wanted map[kafkaPartitionKey]bool) map[kafkaPartitionKey][]kafkaOffsetPoint {
	kafkaHistories.Lock()
	defer kafkaHistories.Unlock()
	for key, h := range kafkaHistories.clusters {
		if at.Sub(h.used) > kafkaHistoryIdle {
			delete(kafkaHistories.clusters, key)
		}
	}
	h := kafkaHistories.clusters[cluster]
	if h == nil {
		h = &kafkaClusterHistory{partitions: map[kafkaPartitionKey]*kafkaPartitionHistory{}}
		kafkaHistories.clusters[cluster] = h
	}
	h.used = at
	for key, p := range h.partitions {
		if at.Sub(p.used) > kafkaHistoryIdle {
			delete(h.partitions, key)
		}
	}
	out := make(map[kafkaPartitionKey][]kafkaOffsetPoint, len(wanted))
	for key, offset := range newest {
		p := h.partitions[key]
		if p == nil {
			p = &kafkaPartitionHistory{}
			h.partitions[key] = p
		}
		p.add(offset, at)
		if wanted[key] {
			out[key] = slices.Clone(p.points)
		}
	}
	for {
		partitions := 0
		var oldest string
		for key, other := range kafkaHistories.clusters {
			partitions += len(other.partitions)
			if key != cluster && (oldest == "" || other.used.Before(kafkaHistories.clusters[oldest].used)) {
				oldest = key
			}
		}
		if oldest == "" || (len(kafkaHistories.clusters) <= kafkaMaxHistories && partitions <= kafkaMaxHistoryPartitions) {
			return out
		}
		delete(kafkaHistories.clusters, oldest)
	}
}

// add keeps the newest offset of a partition read at when.
func (p *kafkaPartitionHistory) add(offset int64, when time.Time) {
	at := when.UnixNano()
	n := len(p.points)
	if n > 0 && at <= p.points[n-1].at {
		// A scrape that read the offsets before the last one kept, as one
		// running beside it can, says nothing newer.
		return
	}
	p.used = when
	if n > 0 && offset < p.points[n-1].offset {
		// The partition is not the one kept: its topic was deleted and made
		// again, or its log truncated.
		p.points, n = p.points[:0], 0
	}
	if n >= 2 && offset == p.points[n-1].offset && offset == p.points[n-2].offset {
		// A partition not written to: the first point at the offset says when
		// it was reached, the last how long it stayed.
		p.points[n-1].at = at
		return
	}
	p.points = append(p.points, kafkaOffsetPoint{offset, at})
	if len(p.points) > kafkaHistoryPoints {
		half := len(p.points) / 2
		kept := p.points[:1]
		for i := 2; i < half; i += 2 {
			kept = append(kept, p.points[i])
		}
		p.points = append(kept, p.points[half:]...)
	}
}

// kafkaLagSeconds is how long ago, at at, the message at offset committed,
// the first a group has not read, was written, as points say; nil when
// they cannot say.
func kafkaLagSeconds(points []kafkaOffsetPoint, committed int64, at time.Time) *float64 {
	n := len(points)
	if n == 0 {
		return nil
	}
	first, last := points[0], points[n-1]
	if committed >= last.offset {
		return new(float64)
	}
	// The newest offset at a point is the next to be written, so the
	// message at committed was written after the last point whose offset is
	// committed or lower, and by the first whose offset is higher.
	i, _ := slices.BinarySearchFunc(points, committed+1, func(p kafkaOffsetPoint, offset int64) int {
		return int(min(max(p.offset-offset, -1), 1))
	})
	seconds := func(from, to int64) float64 { return float64(to-from) / 1e9 }
	now := at.UnixNano()
	var lag float64
	if i > 0 {
		a, b := points[i-1], points[i]
		share := float64(committed-a.offset+1) / float64(b.offset-a.offset)
		lag = seconds(a.at, now) - share*seconds(a.at, b.at)
	} else {
		if n < 2 || last.offset == first.offset {
			return nil
		}
		rate := float64(last.offset-first.offset) / seconds(first.at, last.at)
		lag = seconds(first.at, now) + float64(first.offset-1-committed)/rate
	}
	lag = math.Round(max(lag, 0)*1000) / 1000
	return &lag
}
