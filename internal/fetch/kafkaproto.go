//go:build request_type_kafka

package fetch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync/atomic"
)

// Kafka's wire protocol, as much of it as a kafka scrape needs: a request
// is a frame of its size, its API's key and version, a correlation ID and
// the client's ID, then its body; its response is a frame of its size, the
// same correlation ID and the body. Every API is sent at one version,
// the newest that a Kafka 2.1 client sends (kafkaAPIs). Kafka 4 removed
// the versions older than those (KIP-896) and keeps these, so one version
// of each reaches every broker from 2.1 on. None of them is a "flexible"
// version, so every field is fixed-width or prefixed with its length.

// kafkaAPI is an API a scrape sends: its key, its name in a refusal, and
// the version it is sent at.
type kafkaAPI struct {
	key     int16
	name    string
	version int16
}

var (
	kafkaListOffsets      = kafkaAPI{2, "ListOffsets", 4}
	kafkaMetadata         = kafkaAPI{3, "Metadata", 7}
	kafkaOffsetFetch      = kafkaAPI{9, "OffsetFetch", 5}
	kafkaFindCoordinator  = kafkaAPI{10, "FindCoordinator", 2}
	kafkaDescribeGroups   = kafkaAPI{15, "DescribeGroups", 2}
	kafkaListGroups       = kafkaAPI{16, "ListGroups", 2}
	kafkaSaslHandshake    = kafkaAPI{17, "SaslHandshake", 1}
	kafkaAPIVersions      = kafkaAPI{18, "ApiVersions", 2}
	kafkaSaslAuthenticate = kafkaAPI{36, "SaslAuthenticate", 0}
)

// kafkaAPIs are the APIs a scrape needs a broker to speak at their
// versions; SaslAuthenticate, sent at version 1 where the broker speaks
// it, and the SASL ones, needed only with a login, are checked apart.
var kafkaAPIs = []kafkaAPI{kafkaMetadata, kafkaListOffsets, kafkaFindCoordinator, kafkaListGroups, kafkaDescribeGroups, kafkaOffsetFetch}

// kafkaClientID is the client ID every request carries, which a broker
// logs and quotas may name.
const kafkaClientID = "prometheus-universal-exporter"

// errKafkaProtocol marks an answer that does not read as Kafka's protocol.
var errKafkaProtocol = errors.New("not the Kafka protocol")

// kafkaWriter builds a request's body.
type kafkaWriter struct{ b []byte }

func (w *kafkaWriter) int8(v int8)   { w.b = append(w.b, byte(v)) }                          //nolint:gosec // a field's bits, as the protocol writes them
func (w *kafkaWriter) int16(v int16) { w.b = binary.BigEndian.AppendUint16(w.b, uint16(v)) } //nolint:gosec // a field's bits, as the protocol writes them
func (w *kafkaWriter) int32(v int32) { w.b = binary.BigEndian.AppendUint32(w.b, uint32(v)) } //nolint:gosec // a field's bits, as the protocol writes them
func (w *kafkaWriter) int64(v int64) { w.b = binary.BigEndian.AppendUint64(w.b, uint64(v)) } //nolint:gosec // a field's bits, as the protocol writes them

func (w *kafkaWriter) bool(v bool) {
	if v {
		w.int8(1)
	} else {
		w.int8(0)
	}
}

// string writes a string prefixed with its length as an int16.
func (w *kafkaWriter) string(s string) {
	w.int16(int16(len(s))) //nolint:gosec // every string sent is far shorter than 32,767 bytes (kafkaMaxName)
	w.b = append(w.b, s...)
}

// bytes writes bytes prefixed with their length as an int32.
func (w *kafkaWriter) bytes(b []byte) {
	w.int32(int32(len(b))) //nolint:gosec // a SASL message is a few hundred bytes
	w.b = append(w.b, b...)
}

// array writes the count of an array, whose items follow; -1 is null.
func (w *kafkaWriter) array(n int) { w.int32(int32(n)) } //nolint:gosec // a count is bounded by kafkaMaxGroups and kafkaMaxPartitions

// kafkaReader reads a response's body. The first field that does not fit
// what is left sets err, and every read after it returns zero, so a
// decoder reads on and checks err once.
type kafkaReader struct {
	b   []byte
	err error
}

func (r *kafkaReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.err = fmt.Errorf("a field runs past the end of the response, which is %w", errKafkaProtocol)
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *kafkaReader) int8() int8 {
	if b := r.take(1); b != nil {
		return int8(b[0]) //nolint:gosec // a field's bits, as the protocol writes them
	}
	return 0
}

func (r *kafkaReader) int16() int16 {
	if b := r.take(2); b != nil {
		return int16(binary.BigEndian.Uint16(b)) //nolint:gosec // a field's bits, as the protocol writes them
	}
	return 0
}

func (r *kafkaReader) int32() int32 {
	if b := r.take(4); b != nil {
		return int32(binary.BigEndian.Uint32(b)) //nolint:gosec // a field's bits, as the protocol writes them
	}
	return 0
}

func (r *kafkaReader) int64() int64 {
	if b := r.take(8); b != nil {
		return int64(binary.BigEndian.Uint64(b)) //nolint:gosec // a field's bits, as the protocol writes them
	}
	return 0
}

func (r *kafkaReader) bool() bool { return r.int8() != 0 }

// nullableString reads a string prefixed with its length as an int16, -1
// for null, which it returns as "" and false.
func (r *kafkaReader) nullableString() (string, bool) {
	n := r.int16()
	if n == -1 {
		return "", false
	}
	return string(r.take(int(n))), r.err == nil
}

// string reads a string that may not be null.
func (r *kafkaReader) string() string {
	s, ok := r.nullableString()
	if !ok && r.err == nil {
		r.err = fmt.Errorf("a string that may not be null is null, which is %w", errKafkaProtocol)
	}
	return s
}

// bytes reads bytes prefixed with their length as an int32, nil for null.
func (r *kafkaReader) bytes() []byte {
	n := r.int32()
	if n == -1 {
		return nil
	}
	return r.take(int(n))
}

// array reads the count of an array. Each item takes at least least
// bytes, so a count larger than what is left could hold is refused before
// anything is made for it. -1, null, is read as 0.
func (r *kafkaReader) array(least int) int {
	n := int(r.int32())
	switch {
	case r.err != nil || n == -1:
		return 0
	case n < 0 || n > len(r.b)/max(least, 1):
		r.err = fmt.Errorf("an array claims %d items, more than the response holds, which is %w", n, errKafkaProtocol)
		return 0
	}
	return n
}

// int32s reads an array of int32.
func (r *kafkaReader) int32s() []int32 {
	n := r.array(4)
	out := make([]int32, 0, n)
	for range n {
		out = append(out, r.int32())
	}
	return out
}

// kafkaBudget is what is left of the bytes a scrape may read, its response
// limit, across every broker's responses.
type kafkaBudget struct{ left atomic.Int64 }

// kafkaFrame is one request on conn, as its bytes go out.
func kafkaFrame(api kafkaAPI, version int16, correlation int32, body []byte) []byte {
	w := kafkaWriter{b: make([]byte, 4, 4+10+len(kafkaClientID)+len(body))}
	w.int16(api.key)
	w.int16(version)
	w.int32(correlation)
	w.string(kafkaClientID)
	w.b = append(w.b, body...)
	binary.BigEndian.PutUint32(w.b, uint32(len(w.b)-4)) //nolint:gosec // a request is far smaller than 4 GiB
	return w.b
}

// roundTrip sends one request on c and reads its response's body.
func (c *kafkaConn) roundTrip(api kafkaAPI, version int16, body []byte) (*kafkaReader, error) {
	c.correlation++
	if _, err := c.Write(kafkaFrame(api, version, c.correlation, body)); err != nil {
		return nil, c.fail(api, "sending", err)
	}
	var head [8]byte
	if _, err := io.ReadFull(c.r, head[:4]); err != nil {
		return nil, c.fail(api, "reading the answer to", err)
	}
	size := int64(binary.BigEndian.Uint32(head[:4]))
	if size < 4 || size > math.MaxInt32 || (!c.greeted && size > kafkaMaxGreeting) {
		return nil, fmt.Errorf("%s answered %s with a frame of %d bytes, which is %w", c.broker, api.name, size, errKafkaProtocol)
	}
	if left := c.budget.left.Add(-size); left < 0 {
		return nil, kafkaTooLarge(c.limit)
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(c.r, frame); err != nil {
		return nil, c.fail(api, "reading the answer to", err)
	}
	if got := int32(binary.BigEndian.Uint32(frame[:4])); got != c.correlation { //nolint:gosec // the field's bits
		return nil, fmt.Errorf("%s answered %s with correlation ID %d, not %d, which is %w", c.broker, api.name, got, c.correlation, errKafkaProtocol)
	}
	return &kafkaReader{b: frame[4:]}, nil
}

// decoded is r's error, worded with what was being read.
func (c *kafkaConn) decoded(api kafkaAPI, r *kafkaReader) error {
	if r.err != nil {
		return fmt.Errorf("%s answered %s with what does not decode: %w", c.broker, api.name, r.err)
	}
	return nil
}

// kafkaErrorNames are the names of the error codes a scrape is likely to
// meet, as Kafka's documentation and its logs write them.
var kafkaErrorNames = map[int16]string{
	-1: "UNKNOWN_SERVER_ERROR",
	3:  "UNKNOWN_TOPIC_OR_PARTITION",
	5:  "LEADER_NOT_AVAILABLE",
	6:  "NOT_LEADER_OR_FOLLOWER",
	7:  "REQUEST_TIMED_OUT",
	8:  "BROKER_NOT_AVAILABLE",
	9:  "REPLICA_NOT_AVAILABLE",
	14: "COORDINATOR_LOAD_IN_PROGRESS",
	15: "COORDINATOR_NOT_AVAILABLE",
	16: "NOT_COORDINATOR",
	29: "TOPIC_AUTHORIZATION_FAILED",
	30: "GROUP_AUTHORIZATION_FAILED",
	31: "CLUSTER_AUTHORIZATION_FAILED",
	33: "UNSUPPORTED_SASL_MECHANISM",
	34: "ILLEGAL_SASL_STATE",
	35: "UNSUPPORTED_VERSION",
	58: "SASL_AUTHENTICATION_FAILED",
	69: "GROUP_ID_NOT_FOUND",
	74: "FENCED_LEADER_EPOCH",
	75: "UNKNOWN_LEADER_EPOCH",
}

// kafkaError is how an error code reads: its name, or its number.
func kafkaError(code int16) string {
	if name, ok := kafkaErrorNames[code]; ok {
		return name
	}
	return "error code " + strconv.Itoa(int(code))
}
