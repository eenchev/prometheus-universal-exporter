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
// of each reaches every broker from 2.1 on. None of them but
// ConsumerGroupDescribe is a "flexible" version, so every field is
// fixed-width or prefixed with its length. ConsumerGroupDescribe, which only
// Kafka 3.7 and later speak, is flexible: its strings and arrays are
// prefixed with their length plus one as an unsigned varint, and its
// structures, the request's header and the answer's end with tagged
// fields, which a scrape sends none of and skips.

// kafkaAPI is an API a scrape sends: its key, its name in a refusal, the
// version it is sent at, and whether that version is flexible.
type kafkaAPI struct {
	key      int16
	name     string
	version  int16
	flexible bool
}

var (
	kafkaListOffsets           = kafkaAPI{2, "ListOffsets", 4, false}
	kafkaMetadata              = kafkaAPI{3, "Metadata", 7, false}
	kafkaOffsetFetch           = kafkaAPI{9, "OffsetFetch", 5, false}
	kafkaDescribeGroups        = kafkaAPI{15, "DescribeGroups", 2, false}
	kafkaListGroups            = kafkaAPI{16, "ListGroups", 2, false}
	kafkaDescribeConfigs       = kafkaAPI{32, "DescribeConfigs", 2, false}
	kafkaDescribeLogDirs       = kafkaAPI{35, "DescribeLogDirs", 1, false}
	kafkaSaslHandshake         = kafkaAPI{17, "SaslHandshake", 1, false}
	kafkaAPIVersions           = kafkaAPI{18, "ApiVersions", 2, false}
	kafkaSaslAuthenticate      = kafkaAPI{36, "SaslAuthenticate", 0, false}
	kafkaConsumerGroupDescribe = kafkaAPI{69, "ConsumerGroupDescribe", 0, true}
)

// kafkaAPIs are the APIs a scrape needs a broker to speak at their
// versions; SaslAuthenticate, sent at version 1 where the broker speaks
// it, the SASL ones, needed only with a login, and DescribeLogDirs and
// DescribeConfigs, needed only with request.log_dirs and
// request.topic_configs, are checked apart.
var kafkaAPIs = []kafkaAPI{kafkaMetadata, kafkaListOffsets, kafkaListGroups, kafkaDescribeGroups, kafkaOffsetFetch}

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

// compactArray writes the count of a flexible version's array, plus one.
func (w *kafkaWriter) compactArray(n int) { w.b = binary.AppendUvarint(w.b, uint64(n)+1) } //nolint:gosec // a count, never negative

// compactString writes a flexible version's string, prefixed with its
// length plus one.
func (w *kafkaWriter) compactString(s string) {
	w.b = binary.AppendUvarint(w.b, uint64(len(s))+1)
	w.b = append(w.b, s...)
}

// noTags writes a flexible version's tagged fields: none.
func (w *kafkaWriter) noTags() { w.b = append(w.b, 0) }

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

// uvarint reads a flexible version's unsigned varint, at most 32 bits as
// Kafka writes them.
func (r *kafkaReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 || v > math.MaxUint32 {
		r.err = fmt.Errorf("a varint does not read, which is %w", errKafkaProtocol)
		return 0
	}
	r.b = r.b[n:]
	return v
}

// compactNullableString reads a flexible version's string, its length plus
// one first, 0 for null, which it returns as "" and false.
func (r *kafkaReader) compactNullableString() (string, bool) {
	n := r.uvarint()
	if n == 0 {
		return "", false
	}
	return string(r.take(int(n - 1))), r.err == nil //nolint:gosec // at most 32 bits, as uvarint reads it
}

// compactString reads a flexible version's string that may not be null.
func (r *kafkaReader) compactString() string {
	s, ok := r.compactNullableString()
	if !ok && r.err == nil {
		r.err = fmt.Errorf("a string that may not be null is null, which is %w", errKafkaProtocol)
	}
	return s
}

// compactArray reads the count of a flexible version's array, as array
// does; 0, null, is read as no items.
func (r *kafkaReader) compactArray(least int) int {
	n := r.uvarint()
	switch {
	case r.err != nil || n == 0:
		return 0
	case n-1 > uint64(len(r.b)/max(least, 1)):
		r.err = fmt.Errorf("an array claims %d items, more than the response holds, which is %w", n-1, errKafkaProtocol)
		return 0
	}
	return int(n - 1) //nolint:gosec // bounded by what is left
}

// skipTags reads past a flexible version's tagged fields.
func (r *kafkaReader) skipTags() {
	n := r.uvarint()
	if n > uint64(len(r.b)/2) {
		r.err = fmt.Errorf("tagged fields claim %d fields, more than the response holds, which is %w", n, errKafkaProtocol)
		return
	}
	for ; n > 0 && r.err == nil; n-- {
		r.uvarint()              // tag
		r.take(int(r.uvarint())) //nolint:gosec // at most 32 bits, as uvarint reads it
	}
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

// kafkaFrame is one request on conn, as its bytes go out. A flexible
// version's header ends with its tagged fields.
func kafkaFrame(api kafkaAPI, version int16, correlation int32, body []byte) []byte {
	w := kafkaWriter{b: make([]byte, 4, 4+11+len(kafkaClientID)+len(body))}
	w.int16(api.key)
	w.int16(version)
	w.int32(correlation)
	w.string(kafkaClientID)
	if api.flexible {
		w.noTags()
	}
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
	r := &kafkaReader{b: frame[4:]}
	if api.flexible {
		r.skipTags() // the answer's header's
	}
	return r, nil
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
	56: "KAFKA_STORAGE_ERROR",
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
