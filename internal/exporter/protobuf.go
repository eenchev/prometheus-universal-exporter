package exporter

import (
	"encoding/binary"
	"math"
	"net/http"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The self-metrics answer a scraper that asks for it in Prometheus's protobuf
// exposition format: length-delimited io.prometheus.client.MetricFamily
// messages. It is the only format that carries native histograms — the text
// format cannot, and OpenMetrics 2.0, which will, is still a draft — so it is
// what lets Prometheus store the exporter's own durations as native
// histograms, with buckets that follow the durations observed. Only the
// self-metrics answer it: /probe and the static targets pass on what targets
// answered, whose histograms have classic buckets alone, so protobuf would
// add nothing there.
//
// The messages are written field by field with encoding/binary rather than
// from generated types or with google.golang.org/protobuf, which only the
// grpc request type may link (a build without it carries no protobuf code):
// the wire format of these few messages is varints, zigzag varints, 64-bit
// doubles and length-prefixed bytes. The field numbers are those of
// client_model's metrics.proto:
//
//	MetricFamily  name 1, help 2, type 3, metric 4
//	Metric        label 1, gauge 2, counter 3, summary 4, untyped 5,
//	              timestamp_ms 6, histogram 7
//	LabelPair     name 1, value 2
//	Gauge, Untyped, Counter  value 1; Counter created_timestamp 3
//	Summary       sample_count 1, sample_sum 2, quantile 3 (quantile 1,
//	              value 2), created_timestamp 4
//	Histogram     sample_count 1, sample_sum 2, bucket 3 (cumulative_count
//	              1, upper_bound 2, exemplar 3), schema 5, zero_threshold 6,
//	              zero_count 7, positive_span 12 (offset 1, length 2),
//	              positive_delta 13, created_timestamp 15, exemplars 16
//	Exemplar      label 1, value 2, timestamp 3
//	Timestamp     seconds 1, nanos 2
//
// A histogram with exponential buckets is written with both: its classic
// buckets, which a Prometheus that scrapes classic histograms (or
// always_scrape_classic_histograms) stores, and its native ones, as Prometheus'
// Go client writes a histogram given a NativeHistogramBucketFactor.

// protobufContentType is the media type of the delimited protobuf format,
// which a scraper asks for and the answer says it is.
const protobufContentType = "application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily; encoding=delimited"

// nativeHistogramSchema is the finest schema the native histograms are
// written at, the one of Prometheus' Go client's usual bucket factor of
// 1.1: eight buckets per doubling, each 9% wider than the one before. The
// recorder keeps a finer scale while its durations fit 160 buckets, which
// is lowered to this one when written; a scraper can ask for a coarser one
// still with Prometheus' native_histogram_min_bucket_factor.
const nativeHistogramSchema int32 = 3

// nativeZeroThreshold is the width of the zero bucket, Prometheus' Go
// client's default, 2^-128: a duration is either 0 or far above it.
var nativeZeroThreshold = math.Ldexp(1, -128)

// protoField is a field's number, and the wire types the messages use.
type protoField uint64

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

func appendProtoTag(b []byte, field protoField, wire uint64) []byte {
	return binary.AppendUvarint(b, uint64(field)<<3|wire)
}

func appendProtoBytes(b, value []byte) []byte {
	return append(binary.AppendUvarint(b, uint64(len(value))), value...)
}

func appendProtoString(b []byte, value string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(value))), value...)
}

// zigzag is the varint of a signed field (sint32, sint64): 0, -1, 1, -2, …
// as 0, 1, 2, 3, ….
func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) } //nolint:gosec // G115: the bits, as the encoding wants them

// The values of io.prometheus.client.MetricType.
const (
	protoCounter   = 0
	protoGauge     = 1
	protoSummary   = 2
	protoUntyped   = 3
	protoHistogram = 4
)

// writeProtobufMetricSet answers with s in the delimited protobuf format.
func writeProtobufMetricSet(w http.ResponseWriter, s *model.MetricSet) {
	w.Header().Set("Content-Type", protobufContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Add("Vary", "Accept")
	_, _ = w.Write(appendProtobufMetricSet(nil, s))
}

// appendProtobufMetricSet appends each family of s as one length-delimited
// MetricFamily, its series in the order the set has them.
func appendProtobufMetricSet(b []byte, s *model.MetricSet) []byte {
	order := seriesByFamily(s)
	var family, metric []byte
	for start := 0; start < len(order); {
		first := s.Metrics[order[start]]
		end := start + 1
		for end < len(order) && s.Metrics[order[end]].Name == first.Name {
			end++
		}
		family = appendProtoTag(family[:0], 1, wireBytes)
		family = appendProtoString(family, first.Name)
		if first.Help != "" {
			family = appendProtoTag(family, 2, wireBytes)
			family = appendProtoString(family, first.Help)
		}
		family = appendProtoTag(family, 3, wireVarint)
		family = binary.AppendUvarint(family, protoType(first.Type))
		for _, i := range order[start:end] {
			metric = appendProtoMetric(metric[:0], s.Metrics[i])
			family = appendProtoTag(family, 4, wireBytes)
			family = appendProtoBytes(family, metric)
		}
		b = appendProtoBytes(b, family)
		start = end
	}
	return b
}

func protoType(t model.MetricType) uint64 {
	switch t {
	case model.CounterMetricType:
		return protoCounter
	case model.GaugeMetricType:
		return protoGauge
	case model.SummaryMetricType:
		return protoSummary
	case model.HistogramMetricType:
		return protoHistogram
	}
	return protoUntyped
}

// appendProtoMetric appends one Metric: its labels in order of their names,
// its value in the field of its type, and its timestamp.
func appendProtoMetric(b []byte, m model.Metric) []byte {
	for _, name := range model.SortedKeys(m.Labels) {
		var pair []byte
		pair = appendProtoTag(pair, 1, wireBytes)
		pair = appendProtoString(pair, name)
		pair = appendProtoTag(pair, 2, wireBytes)
		pair = appendProtoString(pair, m.Labels[name])
		b = appendProtoTag(b, 1, wireBytes)
		b = appendProtoBytes(b, pair)
	}
	var value []byte
	field := protoField(5)
	switch {
	case m.Type == model.HistogramMetricType && m.Histogram != nil:
		field, value = 7, appendProtoHistogram(value, m)
	case m.Type == model.SummaryMetricType && m.Summary != nil:
		field, value = 4, appendProtoSummary(value, m)
	case m.Type == model.CounterMetricType:
		field, value = 3, appendProtoDouble(value, 1, m.Value)
		value = appendProtoCreated(value, 3, m.Created)
	case m.Type == model.GaugeMetricType:
		field, value = 2, appendProtoDouble(value, 1, m.Value)
	default:
		value = appendProtoDouble(value, 1, m.Value)
	}
	b = appendProtoTag(b, field, wireBytes)
	b = appendProtoBytes(b, value)
	if m.Timestamp != nil {
		b = appendProtoTag(b, 6, wireVarint)
		b = binary.AppendUvarint(b, uint64(*m.Timestamp)) //nolint:gosec // G115: an int64 field's varint is its two's complement
	}
	return b
}

func appendProtoDouble(b []byte, field protoField, v float64) []byte {
	b = appendProtoTag(b, field, wireFixed64)
	return binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
}

func appendProtoUint(b []byte, field protoField, v uint64) []byte {
	b = appendProtoTag(b, field, wireVarint)
	return binary.AppendUvarint(b, v)
}

// appendProtoCreated appends a google.protobuf.Timestamp of a creation time
// in milliseconds, when there is one.
func appendProtoCreated(b []byte, field protoField, ms int64) []byte {
	if ms == 0 {
		return b
	}
	var ts []byte
	ts = appendProtoTag(ts, 1, wireVarint)
	ts = binary.AppendUvarint(ts, uint64(ms/1000)) //nolint:gosec // G115: an int64 field's varint is its two's complement
	if nanos := ms % 1000 * 1e6; nanos != 0 {
		ts = appendProtoTag(ts, 2, wireVarint)
		ts = binary.AppendUvarint(ts, uint64(nanos)) //nolint:gosec // G115: an int32 field's varint is its two's complement
	}
	b = appendProtoTag(b, field, wireBytes)
	return appendProtoBytes(b, ts)
}

func appendProtoSummary(b []byte, m model.Metric) []byte {
	s := m.Summary
	b = appendProtoUint(b, 1, s.Count)
	b = appendProtoDouble(b, 2, s.Sum)
	var e expositionWriter
	for _, q := range e.ascendingQuantiles(s.Quantiles) {
		var quantile []byte
		quantile = appendProtoDouble(quantile, 1, q.Quantile)
		quantile = appendProtoDouble(quantile, 2, q.Value)
		b = appendProtoTag(b, 3, wireBytes)
		b = appendProtoBytes(b, quantile)
	}
	return appendProtoCreated(b, 4, m.Created)
}

// appendProtoHistogram appends a histogram's classic buckets and its native
// buckets when it has exponential ones. The +Inf bucket is sample_count, and
// is written only when it has an exemplar, as client_golang writes it: the
// format has no other place for that bucket's exemplar, and a Prometheus that
// keeps the classic histogram reads exemplars from the classic buckets alone.
func appendProtoHistogram(b []byte, m model.Metric) []byte {
	h := m.Histogram
	count, _ := h.InfBucket()
	b = appendProtoUint(b, 1, count)
	b = appendProtoDouble(b, 2, h.Sum)
	var e expositionWriter
	infExemplar := h.InfExemplar
	for _, bucket := range e.ascendingBuckets(h.Buckets) {
		if math.IsInf(bucket.UpperBound, 1) {
			if bucket.Exemplar != nil {
				infExemplar = bucket.Exemplar
			}
			continue
		}
		b = appendProtoBucket(b, bucket.CumulativeCount, bucket.UpperBound, bucket.Exemplar)
	}
	if infExemplar != nil {
		b = appendProtoBucket(b, count, math.Inf(1), infExemplar)
	}
	if h.Exponential != nil {
		b = appendNativeBuckets(b, h.Exponential.AtMostScale(nativeHistogramSchema))
		// A native histogram's exemplars are a list of their own, which
		// Prometheus reads when it stores the histogram as a native one:
		// the classic buckets', the +Inf one's among them, in the order of
		// their values, as the buckets have them.
		for _, x := range histogramExemplars(h) {
			b = appendProtoTag(b, 16, wireBytes)
			b = appendProtoBytes(b, appendProtoExemplar(nil, x))
		}
	}
	return appendProtoCreated(b, 15, m.Created)
}

// appendProtoBucket appends a classic Bucket: its cumulative_count,
// upper_bound and exemplar, when it has one.
func appendProtoBucket(b []byte, cumulative uint64, upper float64, x *model.Exemplar) []byte {
	var classic []byte
	classic = appendProtoUint(classic, 1, cumulative)
	classic = appendProtoDouble(classic, 2, upper)
	if x != nil {
		classic = appendProtoTag(classic, 3, wireBytes)
		classic = appendProtoBytes(classic, appendProtoExemplar(nil, x))
	}
	b = appendProtoTag(b, 3, wireBytes)
	return appendProtoBytes(b, classic)
}

// histogramExemplars are a histogram's exemplars, its buckets' in ascending
// order of their bounds and the +Inf bucket's last.
func histogramExemplars(h *model.Histogram) []*model.Exemplar {
	var out []*model.Exemplar
	var e expositionWriter
	for _, bucket := range e.ascendingBuckets(h.Buckets) {
		if bucket.Exemplar != nil {
			out = append(out, bucket.Exemplar)
		}
	}
	if h.InfExemplar != nil {
		out = append(out, h.InfExemplar)
	}
	return out
}

// appendProtoExemplar appends an Exemplar: its trace_id and span_id labels,
// its value and its timestamp.
func appendProtoExemplar(b []byte, x *model.Exemplar) []byte {
	for _, label := range [][2]string{{"trace_id", x.TraceID}, {"span_id", x.SpanID}} {
		var pair []byte
		pair = appendProtoTag(pair, 1, wireBytes)
		pair = appendProtoString(pair, label[0])
		pair = appendProtoTag(pair, 2, wireBytes)
		pair = appendProtoString(pair, label[1])
		b = appendProtoTag(b, 1, wireBytes)
		b = appendProtoBytes(b, pair)
	}
	b = appendProtoDouble(b, 2, x.Value)
	var ts []byte
	ts = appendProtoTag(ts, 1, wireVarint)
	ts = binary.AppendUvarint(ts, uint64(x.Time.Unix())) //nolint:gosec // G115: an int64 field's varint is its two's complement
	if nanos := x.Time.Nanosecond(); nanos != 0 {
		ts = appendProtoTag(ts, 2, wireVarint)
		ts = binary.AppendUvarint(ts, uint64(nanos)) //nolint:gosec // G115: an int32 field's varint is its two's complement
	}
	b = appendProtoTag(b, 3, wireBytes)
	return appendProtoBytes(b, ts)
}

// appendNativeBuckets appends the native fields of exponential buckets.
// Prometheus numbers a bucket one above OpenTelemetry, whose bucket i is
// Prometheus's i+1 (model.ExponentialHistogram). The buckets are written as
// spans of buckets that are not empty, each span's offset counted from where
// the one before ended, the first's from 0, and their counts as deltas: the
// first count, then each less the one before it.
func appendNativeBuckets(b []byte, x *model.ExponentialHistogram) []byte {
	b = appendProtoTag(b, 5, wireVarint)
	b = binary.AppendUvarint(b, zigzag(int64(x.Scale)))
	b = appendProtoDouble(b, 6, nativeZeroThreshold)
	b = appendProtoUint(b, 7, x.ZeroCount)
	var deltas []int64
	var previous int64
	next := int32(0) // the bucket after the last span's
	for k := 0; k < len(x.Counts); {
		if x.Counts[k] == 0 {
			k++
			continue
		}
		first := k
		for k < len(x.Counts) && x.Counts[k] != 0 {
			count := int64(x.Counts[k]) //nolint:gosec // G115: no count reaches 2^63
			deltas = append(deltas, count-previous)
			previous = count
			k++
		}
		// At most model.ExponentialMaxSize buckets.
		index, length := x.Offset+1+int32(first), int32(k-first) //nolint:gosec // G115: as above
		var span []byte
		span = appendProtoTag(span, 1, wireVarint)
		span = binary.AppendUvarint(span, zigzag(int64(index-next)))
		span = appendProtoUint(span, 2, uint64(k-first)) //nolint:gosec // G115: a length
		b = appendProtoTag(b, 12, wireBytes)
		b = appendProtoBytes(b, span)
		next = index + length
	}
	for _, delta := range deltas {
		b = appendProtoTag(b, 13, wireVarint)
		b = binary.AppendUvarint(b, zigzag(delta))
	}
	return b
}

// prefersProtobuf reports whether an Accept header prefers the delimited
// protobuf format to every text format it accepts: whether it gives
// application/vnd.google.protobuf with proto=io.prometheus.client.MetricFamily
// and encoding=delimited a higher quality than the format negotiateFormat
// would answer. Prometheus asks for it first when it scrapes native
// histograms.
func prefersProtobuf(accept string) bool {
	best := -1.0
	for entry := range strings.SplitSeq(accept, ",") {
		mediaType, params := parseMediaRange(entry)
		if mediaType != "application/vnd.google.protobuf" || params["proto"] != "io.prometheus.client.MetricFamily" || params["encoding"] != "delimited" {
			continue
		}
		if q, ok := acceptQuality(params); ok {
			best = max(best, q)
		}
	}
	if best <= 0 {
		return false
	}
	a := acceptQualities(accept)
	return best > max(a.om, a.text)
}
