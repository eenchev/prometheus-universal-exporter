package exporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// OTLP is emitted using the standard OTLP/HTTP JSON representation. Keeping
// this optional avoids making the Prometheus scrape path depend on a collector,
// while allowing both probe output and exporter self-health to be forwarded.
type otlpPayload struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes,omitempty"`
}

type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue string   `json:"stringValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	IntValue    string   `json:"intValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Gauge       *otlpGauge     `json:"gauge,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
	Summary     *otlpSummary   `json:"summary,omitempty"`
}

type otlpHistogram struct {
	DataPoints             []otlpHistogramDataPoint `json:"dataPoints"`
	AggregationTemporality string                   `json:"aggregationTemporality"`
}

// otlpHistogramDataPoint is one series of a histogram. OTLP's bucket counts
// are per bucket, not cumulative as Prometheus's are, and there is one more
// count than bounds: the last is everything above the highest bound, which is
// Prometheus's +Inf bucket.
type otlpHistogramDataPoint struct {
	Attributes        []otlpAttribute `json:"attributes,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	Count             string          `json:"count"`
	Sum               *otlpDouble     `json:"sum,omitempty"`
	BucketCounts      []string        `json:"bucketCounts"`
	ExplicitBounds    []otlpDouble    `json:"explicitBounds"`
}

type otlpSummary struct {
	DataPoints []otlpSummaryDataPoint `json:"dataPoints"`
}

type otlpSummaryDataPoint struct {
	Attributes        []otlpAttribute     `json:"attributes,omitempty"`
	StartTimeUnixNano string              `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string              `json:"timeUnixNano"`
	Count             string              `json:"count"`
	Sum               otlpDouble          `json:"sum"`
	QuantileValues    []otlpQuantileValue `json:"quantileValues,omitempty"`
}

type otlpQuantileValue struct {
	Quantile otlpDouble `json:"quantile"`
	Value    otlpDouble `json:"value"`
}

// otlpDouble is a double as the protobuf JSON mapping writes it: a number, or
// "NaN", "Infinity" or "-Infinity", which JSON numbers cannot express. A
// passed-through NaN would otherwise fail the encoding of the whole export.
type otlpDouble float64

func (d otlpDouble) MarshalJSON() ([]byte, error) {
	v := float64(d)
	switch {
	case math.IsNaN(v):
		return []byte(`"NaN"`), nil
	case math.IsInf(v, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(v, -1):
		return []byte(`"-Infinity"`), nil
	}
	return []byte(strconv.FormatFloat(v, 'g', -1, 64)), nil
}

// UnmarshalJSON reads what MarshalJSON writes.
func (d *otlpDouble) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case `"NaN"`:
		*d = otlpDouble(math.NaN())
		return nil
	case `"Infinity"`:
		*d = otlpDouble(math.Inf(1))
		return nil
	case `"-Infinity"`:
		*d = otlpDouble(math.Inf(-1))
		return nil
	}
	v, err := strconv.ParseFloat(strings.Trim(string(b), `"`), 64)
	*d = otlpDouble(v)
	return err
}

type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints"`
}

type otlpSum struct {
	DataPoints             []otlpNumberDataPoint `json:"dataPoints"`
	AggregationTemporality string                `json:"aggregationTemporality"`
	IsMonotonic            bool                  `json:"isMonotonic"`
}

type otlpNumberDataPoint struct {
	Attributes        []otlpAttribute `json:"attributes,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	AsDouble          *otlpDouble     `json:"asDouble,omitempty"`
	AsInt             string          `json:"asInt,omitempty"`
}

// otlpResourceIdentity is the OTLP resource a metric set belongs to. Static
// targets may each declare their own service name and resource attributes, so
// one export can carry several resources.
type otlpResourceIdentity struct {
	ServiceName string
	Attributes  map[string]string
}

// otlpResourceSet pairs a drained metric set with the resource it belongs to.
// seqs, set by drainOTLP, holds each metric's place in the queue, so a failed
// export queues it again with its age.
type otlpResourceSet struct {
	Identity otlpResourceIdentity
	Set      model.MetricSet
	seqs     []int64
}

// key is what tells the resource from every other (otlpKey): its service
// name, and each attribute's name and value. Pending metrics are grouped by
// it, and the resources of an export are in the order of their keys.
func (r otlpResourceIdentity) key() string {
	return otlpKey("", r.Attributes, r.ServiceName)
}

func (r otlpResourceIdentity) attributes() []otlpAttribute {
	out := []otlpAttribute{{Key: "service.name", Value: otlpValue{StringValue: r.ServiceName}}}
	for _, name := range model.SortedKeys(r.Attributes) {
		out = append(out, otlpAttribute{Key: name, Value: otlpValue{StringValue: r.Attributes[name]}})
	}
	return out
}

// defaultResourceIdentity is the exporter-wide resource used for probe output
// and self-health metrics. A resource attribute written "" is the attribute
// left out, as a transform.labels value written "" is the label left out:
// the resource carries no attribute that says nothing.
func defaultResourceIdentity(cfg model.OTLPConfig) otlpResourceIdentity {
	identity := otlpResourceIdentity{ServiceName: cfg.ServiceName, Attributes: map[string]string{}}
	for key, value := range cfg.ResourceAttributes {
		if value != "" {
			identity.Attributes[key] = value
		}
	}
	return identity
}

// otlpRequests are the requests of one export, in the order they are sent:
// each one's body, and, where there are several, the request whose points
// each series is sent in last, by its place among the resources of the
// export and in its resource's set; an export of one request has none. A
// series is accounted for by that request alone: one whose points are in
// two requests — a histogram exported as gauges under its samples' names —
// is kept again when the later fails, and a series none of whose points is
// sent, left out for another written later (otlpMetricsOf), is the first
// request's.
type otlpRequests struct {
	bodies [][]byte
	last   [][]int
}

// otlpRequestsOf makes the requests that send resources to the endpoint. Of
// up to otlp.batch_max_size data points and otlp.batch_max_bytes bytes of
// JSON, as nearly every export is, it is one request, of every resource
// (otlpPayloadOf). An export of more is split (splitOTLP), and its points
// are then gone through again to know which series each request carries
// (otlpPointSources), so that a request that fails is accounted for by the
// series of its own points.
func (s *Server) otlpRequestsOf(cfg model.OTLPConfig, resources []otlpResourceSet) (otlpRequests, error) {
	payload, now := s.otlpPayloadOf(cfg, resources)
	if len(payload.ResourceMetrics) == 0 {
		return otlpRequests{}, nil
	}
	maxPoints, maxBytes := otlpBatchBounds(cfg)
	if otlpPayloadPoints(payload) <= maxPoints {
		raw, err := json.Marshal(payload)
		if err != nil {
			return otlpRequests{}, fmt.Errorf("encoding the export: %w", err)
		}
		if len(raw) <= maxBytes {
			body, err := compressOTLP(raw, cfg.Compression)
			if err != nil {
				return otlpRequests{}, fmt.Errorf("encoding the export: %w", err)
			}
			return otlpRequests{bodies: [][]byte{body}}, nil
		}
	}
	split, err := splitOTLP(payload, maxPoints, maxBytes)
	if err != nil {
		return otlpRequests{}, fmt.Errorf("encoding the export: %w", err)
	}
	requests := otlpRequests{bodies: make([][]byte, len(split)), last: make([][]int, len(resources))}
	for r := range resources {
		requests.last[r] = make([]int, len(resources[r].Set.Metrics))
	}
	// The resources that have a place in the payload, in its order, and the
	// series of each of their points.
	var made []int
	var sources []otlpPointSources
	for r := range resources {
		if len(resources[r].Set.Metrics) > 0 {
			made = append(made, r)
			sources = append(sources, otlpPointSources{index: map[string]int{}})
			otlpMetricsWith(resources[r], now, nil, &sources[len(sources)-1])
		}
	}
	for k, pieces := range split {
		part := otlpPayload{}
		for p, piece := range pieces {
			rm := &payload.ResourceMetrics[piece.resource]
			if p == 0 || pieces[p-1].resource != piece.resource {
				part.ResourceMetrics = append(part.ResourceMetrics, otlpResourceMetrics{Resource: rm.Resource, ScopeMetrics: []otlpScopeMetrics{{Scope: rm.ScopeMetrics[0].Scope}}})
			}
			scope := &part.ResourceMetrics[len(part.ResourceMetrics)-1].ScopeMetrics[0]
			scope.Metrics = append(scope.Metrics, otlpMetricPart(rm.ScopeMetrics[0].Metrics[piece.metric], piece.from, piece.to))
			series := sources[piece.resource].of[piece.metric]
			for _, i := range series[piece.from:piece.to] {
				requests.last[made[piece.resource]][i] = k
			}
		}
		raw, err := json.Marshal(part)
		if err != nil {
			return otlpRequests{}, fmt.Errorf("encoding the export: %w", err)
		}
		if requests.bodies[k], err = compressOTLP(raw, cfg.Compression); err != nil {
			return otlpRequests{}, fmt.Errorf("encoding the export: %w", err)
		}
	}
	return requests, nil
}

// otlpBatchBounds are otlp.batch_max_size and otlp.batch_max_bytes, their
// defaults where they are unset.
func otlpBatchBounds(cfg model.OTLPConfig) (maxPoints, maxBytes int) {
	maxPoints, maxBytes = cfg.BatchMaxSize, int(min(cfg.BatchMaxBytes, math.MaxInt32))
	if maxPoints <= 0 {
		maxPoints = model.DefaultOTLPBatchMaxSize
	}
	if maxBytes <= 0 {
		maxBytes = model.DefaultOTLPBatchMaxBytes
	}
	return maxPoints, maxBytes
}

// from are the series of pending sent last in request k or one after it:
// what the export has not delivered when request k fails for a reason worth
// retrying, or was not sent. pending are the resources of the export without
// the exporter's own series, which come after a resource's queued ones.
func (q otlpRequests) from(pending []otlpResourceSet, k int) []otlpResourceSet {
	if q.last == nil {
		return pending
	}
	var out []otlpResourceSet
	for r := range pending {
		var kept otlpResourceSet
		for i, m := range pending[r].Set.Metrics {
			if q.last[r][i] >= k {
				kept.Set.Metrics = append(kept.Set.Metrics, m)
				kept.seqs = append(kept.seqs, pending[r].seqs[i])
			}
		}
		if len(kept.Set.Metrics) > 0 {
			kept.Identity = pending[r].Identity
			out = append(out, kept)
		}
	}
	return out
}

// of is how many series of pending are sent last in request k: the points
// dropped when the endpoint refuses it.
func (q otlpRequests) of(pending []otlpResourceSet, k int) int {
	if q.last == nil {
		return countPoints(pending)
	}
	n := 0
	for r := range pending {
		for i := range pending[r].Set.Metrics {
			if q.last[r][i] == k {
				n++
			}
		}
	}
	return n
}

// otlpPayloadOf is the payload of every resource of an export with a series,
// and the time its points are exported at that have none of their own.
func (s *Server) otlpPayloadOf(cfg model.OTLPConfig, resources []otlpResourceSet) (otlpPayload, string) {
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	payload := otlpPayload{}
	s.otlpStarts.bound(cfg.MaxPendingPoints)
	for _, resource := range resources {
		if len(resource.Set.Metrics) == 0 {
			continue
		}
		key := resource.Identity.key()
		metrics, clashes := otlpMetricsOf(resource, now, s.otlpStarts.forResource(key))
		s.logOTLPNameClashes(resource.Identity, key, clashes)
		payload.ResourceMetrics = append(payload.ResourceMetrics, otlpResourceMetrics{Resource: otlpResource{Attributes: resource.Identity.attributes()}, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "prometheus-universal-exporter"}, Metrics: metrics}}})
	}
	return payload, now
}

// deliverOTLP sends one request's body, and retries a network error, 429,
// 502, 503 or 504 — the responses the OTLP specification makes retryable —
// with exponential backoff, or after the Retry-After the endpoint asks for,
// for as long as another attempt can start before deadline. Each attempt is
// bounded by otlp.timeout. Any other status is an otlpRefusedError, not
// retried. It returns the number of retries made, and the partial success the
// endpoint reported when it accepted the request without some of it.
func (s *Server) deliverOTLP(ctx context.Context, cfg model.OTLPConfig, body []byte, deadline time.Time) (int, otlpPartialSuccess, error) {
	timeout := time.Duration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	tlsSettings := cfg.TLS
	if cfg.InsecureSkipVerify {
		tlsSettings.InsecureSkipVerify = true
	}
	for retries := 0; ; retries++ {
		attempt := min(timeout, time.Until(deadline))
		answer, err := s.sendOTLP(ctx, cfg, tlsSettings, body, attempt)
		if err == nil || !answer.retryable {
			return retries, answer.partial, err
		}
		wait := answer.wait
		if wait <= 0 {
			wait = otlpBackoff(retries)
		}
		// Another attempt is only worth starting if it has time to finish.
		if time.Until(deadline) < wait+otlpMinAttempt {
			return retries, otlpPartialSuccess{}, err
		}
		s.logger.Debug("retrying the OTLP export", "error", err, "retry", retries+1, "after", wait.String())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return retries, otlpPartialSuccess{}, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

// otlpRetryBackoff is the wait before the first retry; each retry doubles it,
// up to otlpMaxBackoff. A variable, so tests need not wait a second.
var otlpRetryBackoff = time.Second

const (
	otlpMaxBackoff = 16 * time.Second
	// otlpMinAttempt is the least time worth giving an attempt.
	otlpMinAttempt = 100 * time.Millisecond
)

func otlpBackoff(retries int) time.Duration {
	wait := otlpRetryBackoff << min(retries, 8)
	return min(wait, otlpMaxBackoff)
}

// otlpRefusedError is a response the endpoint will give again: the data is
// dropped rather than sent again. body is the start of the endpoint's
// explanation, for the log.
type otlpRefusedError struct {
	status int
	body   string
}

func (e *otlpRefusedError) Error() string {
	return fmt.Sprintf("the OTLP endpoint answered %d", e.status)
}

// otlpAnswer is how one attempt ended: whether a failure is worth retrying and
// how long the endpoint asked to wait first, if it did, and on success, what
// the endpoint said it rejected.
type otlpAnswer struct {
	retryable bool
	wait      time.Duration
	partial   otlpPartialSuccess
}

// otlpPartialSuccess is the partialSuccess of an ExportMetricsServiceResponse:
// the endpoint accepted the export but rejected rejected of its data points,
// saying why in message. The endpoint may also send a message alone, as a
// warning. Both are zero for a full success.
type otlpPartialSuccess struct {
	rejected int64
	message  string
}

// otlpResponseLimit is the most of an answer's body read: an
// ExportMetricsServiceResponse or an error Status is far smaller.
const otlpResponseLimit = 1 << 20

// sendOTLP makes one attempt.
func (s *Server) sendOTLP(ctx context.Context, cfg model.OTLPConfig, tlsSettings model.TLSConfig, body []byte, timeout time.Duration) (otlpAnswer, error) {
	// One connection to the collector is reused from export to export.
	client, err := fetch.HTTPClient(fetch.TransportSettings{TLS: tlsSettings}, true, timeout)
	if err != nil {
		return otlpAnswer{}, fmt.Errorf("OTLP TLS configuration: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return otlpAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Compression != model.OTLPCompressionNone {
		req.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Unreachable, reset, timed out: the next attempt may get through.
		return otlpAnswer{retryable: true}, err
	}
	// The body is read to the end, however little of it matters, so the
	// connection goes back to the pool for the next export.
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, otlpResponseLimit))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, otlpResponseLimit))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return otlpAnswer{partial: partialSuccess(answer, resp.Header.Get("Content-Type"))}, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable, resp.StatusCode == http.StatusGatewayTimeout:
		return otlpAnswer{retryable: true, wait: retryAfter(resp.Header.Get("Retry-After"), time.Now())}, fmt.Errorf("the OTLP endpoint answered %d", resp.StatusCode)
	default:
		return otlpAnswer{}, &otlpRefusedError{status: resp.StatusCode, body: bodyExcerpt(answer)}
	}
}

// partialSuccess reads the partialSuccess of a JSON export response. An
// answer that is empty, is not JSON, or does not carry one is a full success:
// the field is optional, and a protobuf answer is not read.
func partialSuccess(body []byte, contentType string) otlpPartialSuccess {
	if len(bytes.TrimSpace(body)) == 0 || contentType != "" && !strings.Contains(strings.ToLower(contentType), "json") {
		return otlpPartialSuccess{}
	}
	var response struct {
		PartialSuccess struct {
			// OTLP/JSON writes an int64 as a string, and a number is
			// accepted too; json.Number reads either.
			RejectedDataPoints json.Number `json:"rejectedDataPoints"`
			ErrorMessage       string      `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return otlpPartialSuccess{}
	}
	partial := otlpPartialSuccess{message: response.PartialSuccess.ErrorMessage}
	if rejected, err := strconv.ParseInt(strings.Trim(string(response.PartialSuccess.RejectedDataPoints), `"`), 10, 64); err == nil && rejected > 0 {
		partial.rejected = rejected
	}
	return partial
}

// retryAfter reads a Retry-After header, in seconds or as an HTTP date; zero
// when there is none or it cannot be read.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return max(time.Duration(seconds)*time.Second, 0)
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}

// encodeOTLP renders the payload as JSON, gzipped unless compression is none.
func encodeOTLP(payload otlpPayload, compression string) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return compressOTLP(raw, compression)
}

// compressOTLP gzips a request's JSON unless compression is none.
func compressOTLP(raw []byte, compression string) ([]byte, error) {
	if compression == model.OTLPCompressionNone {
		return raw, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// An export of more data points than otlp.batch_max_size, or of more bytes
// of JSON than otlp.batch_max_bytes, is sent as several requests. A receiver
// refuses a request past its own bound — the OpenTelemetry Collector's
// OTLP/HTTP receiver one of over 20MiB, its gRPC receiver one of over 4MiB —
// and a refused request is dropped, so one export of every point waiting
// would be dropped whole, at every interval, once there were enough of them.
//
// The points are taken in the order of the export — its resources, the
// metrics of each, the points of each metric — and each request is filled
// with as many as it holds within both bounds before the next is begun. A
// request carries the resource and the scope of each of its points, and the
// name, the kind and the description of each of their metrics, so those of
// a resource or a metric that does not fit in one request are written again
// in the next. A point whose request would be over otlp.batch_max_bytes with
// it alone is sent alone, for the receiver to take or refuse. What a request
// holds is measured, not encoded, as it is filled: the JSON of a request is
// the JSON of each of its parts, so its size is the sum of theirs, which is
// exact.

// otlpPiece is what a request carries of one metric: its points from up to
// to, of the metric'th metric of the resource'th resource of the payload.
type otlpPiece struct {
	resource, metric, from, to int
}

// splitOTLP is the requests a payload is sent in, each within maxPoints data
// points and maxBytes bytes of JSON, as the pieces each is made of.
func splitOTLP(payload otlpPayload, maxPoints, maxBytes int) ([][]otlpPiece, error) {
	size := func(v any) (int, error) {
		b, err := json.Marshal(v)
		return len(b), err
	}
	empty, err := size(otlpPayload{ResourceMetrics: []otlpResourceMetrics{}})
	if err != nil {
		return nil, err
	}
	var requests [][]otlpPiece
	var pieces []otlpPiece
	used, points := empty, 0
	for r := range payload.ResourceMetrics {
		rm := &payload.ResourceMetrics[r]
		scope := rm.ScopeMetrics[0]
		resourceSize, err := size(otlpResourceMetrics{Resource: rm.Resource, ScopeMetrics: []otlpScopeMetrics{{Scope: scope.Scope, Metrics: []otlpMetric{}}}})
		if err != nil {
			return nil, err
		}
		for m := range scope.Metrics {
			metricSize, err := size(otlpMetricPart(scope.Metrics[m], 0, 0))
			if err != nil {
				return nil, err
			}
			n := otlpMetricPoints(scope.Metrics[m])
			for j := range n {
				pointSize, err := size(otlpMetricPoint(scope.Metrics[m], j))
				if err != nil {
					return nil, err
				}
				// What the point adds: itself, and what it needs before it in
				// the request, each part after another of its kind with a
				// comma between them.
				adds := func() int {
					switch last := len(pieces) - 1; {
					case last < 0:
						return resourceSize + metricSize + pointSize
					case pieces[last].resource != r:
						return 1 + resourceSize + metricSize + pointSize
					case pieces[last].metric != m:
						return 1 + metricSize + pointSize
					}
					return 1 + pointSize
				}
				if points > 0 && (points+1 > maxPoints || used+adds() > maxBytes) {
					requests = append(requests, pieces)
					pieces, used, points = nil, empty, 0
				}
				used += adds()
				points++
				if last := len(pieces) - 1; last >= 0 && pieces[last].resource == r && pieces[last].metric == m {
					pieces[last].to++
				} else {
					pieces = append(pieces, otlpPiece{resource: r, metric: m, from: j, to: j + 1})
				}
			}
		}
	}
	if len(pieces) > 0 {
		requests = append(requests, pieces)
	}
	return requests, nil
}

// otlpPayloadPoints is how many data points a payload carries.
func otlpPayloadPoints(payload otlpPayload) int {
	n := 0
	for r := range payload.ResourceMetrics {
		for _, scope := range payload.ResourceMetrics[r].ScopeMetrics {
			for m := range scope.Metrics {
				n += otlpMetricPoints(scope.Metrics[m])
			}
		}
	}
	return n
}

// otlpMetricPoints is how many data points a metric has.
func otlpMetricPoints(m otlpMetric) int {
	switch {
	case m.Histogram != nil:
		return len(m.Histogram.DataPoints)
	case m.Summary != nil:
		return len(m.Summary.DataPoints)
	case m.Sum != nil:
		return len(m.Sum.DataPoints)
	case m.Gauge != nil:
		return len(m.Gauge.DataPoints)
	}
	return 0
}

// otlpMetricPoint is the j'th data point of a metric.
func otlpMetricPoint(m otlpMetric, j int) any {
	switch {
	case m.Histogram != nil:
		return m.Histogram.DataPoints[j]
	case m.Summary != nil:
		return m.Summary.DataPoints[j]
	case m.Sum != nil:
		return m.Sum.DataPoints[j]
	}
	return m.Gauge.DataPoints[j]
}

// otlpMetricPart is a metric with its data points from up to to alone: none,
// written [], for from equal to to.
func otlpMetricPart(m otlpMetric, from, to int) otlpMetric {
	switch {
	case m.Histogram != nil:
		h := *m.Histogram
		h.DataPoints = h.DataPoints[from:to:to]
		m.Histogram = &h
	case m.Summary != nil:
		summary := *m.Summary
		summary.DataPoints = summary.DataPoints[from:to:to]
		m.Summary = &summary
	case m.Sum != nil:
		sum := *m.Sum
		sum.DataPoints = sum.DataPoints[from:to:to]
		m.Sum = &sum
	case m.Gauge != nil:
		gauge := *m.Gauge
		gauge.DataPoints = gauge.DataPoints[from:to:to]
		m.Gauge = &gauge
	}
	return m
}

func otlpAttributesForLabels(labels map[string]string) []otlpAttribute {
	out := make([]otlpAttribute, 0, len(labels))
	for _, k := range model.SortedKeys(labels) {
		out = append(out, otlpAttribute{Key: k, Value: otlpValue{StringValue: labels[k]}})
	}
	return out
}

// otlpMetrics converts a metric set to OTLP metrics. Series of one family
// become the data points of one metric, in the order the family first
// appears: a gauge, a monotonic cumulative sum for a counter, a cumulative
// histogram, or a summary. start gives a cumulative point its start time;
// nil leaves it out.
//
// A family with a series whose values its type does not allow (typedValues)
// — a counter that is NaN or negative, a histogram whose bucket counts fall
// or whose +Inf bucket and _count differ, a summary with a quantile of 1.5 —
// cannot be a monotonic sum, a histogram or a summary in OTLP either: a
// bucket's own count would be negative, a histogram's point has one count,
// and OTLP's quantiles are within 0 to 1 and not negative as OpenMetrics'
// are. It is exported as gauges under the names of its samples, with the
// series and values the exposition formats write: what OpenMetrics' unknown,
// which such a family is written as there (planOpenMetrics), is in OTLP.
// Every point then is valid, and none is left out.
func otlpMetrics(set model.MetricSet, now string, start func(m model.Metric, at string) string) []otlpMetric {
	out, _ := otlpMetricsOf(otlpResourceSet{Set: set}, now, start)
	return out
}

// otlpPoints is the conversion otlpMetrics describes, of the series as the
// set has them. own is where the exporter's own series begin in the set,
// after those that waited in the queue (exportOTLP). keep, when there is
// one, is asked of every point before it is made — the series it is of, by
// its place in the set, and the name, the kind and the attributes it would
// be exported with — and a point it refuses is left out, and its series'
// start time not asked for.
//
// shared says that two points of the result may be one to a receiver — one
// metric's, with the same attributes — or that a name is two metrics of
// different kinds in it, which otlpMetricsOf then looks into. It is told
// without a look at any point's attributes, by where the points of a metric
// come from. The series of one name and type that follow one another in the
// set are a run, and those of a run differ in their labels, as the keys
// they waited under and the exporter's own series do. So a metric whose
// points are all of one run, each exported under its series' own name with
// its labels for attributes, has no two alike; and shared is said where a
// metric gets a point of a second run, where a name is exported as a second
// kind, and where a histogram or a summary is exported as gauges under its
// samples' names, with le or quantile among the attributes.
func otlpPoints(set model.MetricSet, own int, now string, start func(m model.Metric, at string) string, keep func(series int, name string, kind int, attributes []otlpAttribute) bool) (out []otlpMetric, shared bool) {
	// Where the metric of a name is in out, and the run that made it.
	type place struct{ at, run int }
	index := map[string]place{}
	run := 0
	// metric is the metric of a name and a kind that a point is added to. A
	// name first seen, or reused with another kind, is a new metric, so
	// points of different kinds are never mixed in one.
	metric := func(name, help string, kind int) *otlpMetric {
		p, seen := index[name]
		if seen && otlpKind(out[p.at]) == kind {
			shared = shared || p.run != run
			return &out[p.at]
		}
		shared = shared || seen
		index[name] = place{at: len(out), run: run}
		out = append(out, newOTLPMetric(name, help, kind))
		return &out[len(out)-1]
	}
	untyped := untypedFamilies(set)
	var e expositionWriter
	for i, m := range set.Metrics {
		if i == 0 || i == own || m.Name != set.Metrics[i-1].Name || m.Type != set.Metrics[i-1].Type {
			run++
		}
		at := now
		if m.Timestamp != nil {
			at = strconv.FormatInt(*m.Timestamp*int64(time.Millisecond), 10)
		}
		attributes := otlpAttributesForLabels(m.Labels)
		gauge := func(name string, attributes []otlpAttribute, value float64) {
			if keep != nil && !keep(i, name, otlpKindGauge, attributes) {
				return
			}
			v := otlpDouble(value)
			g := metric(name, m.Help, otlpKindGauge).Gauge
			g.DataPoints = append(g.DataPoints, otlpNumberDataPoint{Attributes: attributes, TimeUnixNano: at, AsDouble: &v})
		}
		kind := otlpKindOf(m)
		if kind != otlpKindGauge && untyped[m.Name] {
			// The lines the text format writes for the series, each a gauge.
			shared = shared || kind != otlpKindSum
			switch kind {
			case otlpKindHistogram:
				for _, b := range e.ascendingBuckets(m.Histogram.Buckets) {
					if !math.IsInf(b.UpperBound, 1) {
						gauge(m.Name+"_bucket", otlpAttributesWith(m.Labels, "le", strconv.FormatFloat(b.UpperBound, 'g', -1, 64)), float64(b.CumulativeCount))
					}
				}
				if inf, has := m.Histogram.InfBucket(); has {
					gauge(m.Name+"_bucket", otlpAttributesWith(m.Labels, "le", "+Inf"), float64(inf))
				}
				if !m.Histogram.NoSum {
					gauge(m.Name+"_sum", attributes, m.Histogram.Sum)
				}
				if !m.Histogram.NoCount {
					gauge(m.Name+"_count", attributes, float64(m.Histogram.Count))
				}
			case otlpKindSummary:
				for _, q := range e.ascendingQuantiles(m.Summary.Quantiles) {
					gauge(m.Name, otlpAttributesWith(m.Labels, "quantile", strconv.FormatFloat(q.Quantile, 'g', -1, 64)), q.Value)
				}
				if !m.Summary.NoSum {
					gauge(m.Name+"_sum", attributes, m.Summary.Sum)
				}
				if !m.Summary.NoCount {
					gauge(m.Name+"_count", attributes, float64(m.Summary.Count))
				}
			default:
				gauge(m.Name, attributes, m.Value)
			}
			continue
		}
		if keep != nil && kind != otlpKindGauge && !keep(i, m.Name, kind, attributes) {
			continue
		}
		startAt := ""
		switch {
		case kind == otlpKindGauge:
		case m.Created != 0:
			// One of the exporter's own series, which says when it began to
			// count (selfcreated.go).
			startAt = strconv.FormatInt(m.Created*int64(time.Millisecond), 10)
		case start != nil:
			startAt = start(m, at)
		}
		switch kind {
		case otlpKindHistogram:
			point := otlpHistogramPoint(m, attributes, at)
			point.StartTimeUnixNano = startAt
			h := metric(m.Name, m.Help, kind).Histogram
			h.DataPoints = append(h.DataPoints, point)
		case otlpKindSummary:
			point := otlpSummaryPoint(m, attributes, at)
			point.StartTimeUnixNano = startAt
			s := metric(m.Name, m.Help, kind).Summary
			s.DataPoints = append(s.DataPoints, point)
		case otlpKindSum:
			v := otlpDouble(m.Value)
			s := metric(m.Name, m.Help, kind).Sum
			s.DataPoints = append(s.DataPoints, otlpNumberDataPoint{Attributes: attributes, StartTimeUnixNano: startAt, TimeUnixNano: at, AsDouble: &v})
		default:
			gauge(m.Name, attributes, m.Value)
		}
	}
	return out, shared
}

// otlpNameClash is a name that the writers of one resource export as
// metrics of different kinds in one export: the kind the latest of them
// wrote is exported, and points of the others, of the kinds leftOut, are
// not.
type otlpNameClash struct {
	name    string
	kept    int
	leftOut []int
	points  int
}

// otlpMetricsOf is otlpMetrics of what one resource has in an export — the
// series that waited in the queue, each with its place in it, and after
// them the exporter's own, for its own resource — as one writer's.
//
// To a receiver a point is its resource's, its metric's — the name and the
// kind — and its attributes', and each such stream has one writer: two
// points of one in a request are one too many, of which it keeps either,
// or refuses the request; and two metrics of one name and different kinds
// are a conflict it settles as it likes. The queue holds a series once, by
// its name, type and labels (otlpMetricKey), which is not what it is
// exported as: a gauge and an untyped series of one name and labels are one
// gauge's point twice; so are the h_bucket of a histogram h exported as
// gauges and a gauge h_bucket with that le, and a series of a probe named
// and labelled like one of the exporter's own; and what one probe makes a
// gauge of and another a counter are two metrics of one name. One set a
// probe answers with has none of these (MetricSet.Validate); two sets
// under one resource have. What a series is exported as is not known when
// it is queued — a family is exported as gauges when any of its series in
// the export has values its type does not allow (untypedFamilies), and the
// exporter's own never wait — so it is settled here, where the points are
// made, by the rule of the queue: the later replaces the earlier.
//
//   - Of the kinds a name is exported as, the one of the point written last
//     is exported, and no point of another: they are reported, to be logged
//     (logOTLPNameClashes).
//   - Of the points of one metric with the same attributes, the one written
//     last is exported, as a later scrape's point replaces a series' in
//     the queue, without a word.
//
// Written last is queued last, and one of the exporter's own is written
// after every queued one, at the export; of two written at once, the later
// in the set.
//
// Nearly no export has either, and it is then made once, as it was
// (otlpPoints): only where that says two points may be one are the points
// gone through again, to find those to leave out, and, where there are any,
// a third time without them. A start time is then asked for twice, for the
// same point, which gives it the same; and a cumulative point left out has
// been a point of its series to the start times, a count the series had
// (otlpStartTimes).
func otlpMetricsOf(resource otlpResourceSet, now string, start func(m model.Metric, at string) string) ([]otlpMetric, []otlpNameClash) {
	return otlpMetricsWith(resource, now, start, nil)
}

// otlpPointSources are the series the points of a resource's metrics are
// of: of[m][j] is the place in the resource's set of the series the j'th
// point of the m'th metric is of. index and kinds are what otlpPoints keeps
// to find the metric of a name and a kind a point is added to.
type otlpPointSources struct {
	index map[string]int
	kinds []int
	of    [][]int
}

// add says that the next point made is of the series'th series, for the
// metric of name and kind, found as otlpPoints finds it.
func (o *otlpPointSources) add(series int, name string, kind int) {
	at, seen := o.index[name]
	if !seen || o.kinds[at] != kind {
		at = len(o.of)
		o.index[name] = at
		o.kinds = append(o.kinds, kind)
		o.of = append(o.of, nil)
	}
	o.of[at] = append(o.of[at], series)
}

// otlpMetricsWith is otlpMetricsOf, which, given sources, says in them what
// series each point it makes is of. A point is made after keep is asked of
// it, and only then, so sources are told of a point where keep says yes.
func otlpMetricsWith(resource otlpResourceSet, now string, start func(m model.Metric, at string) string, sources *otlpPointSources) ([]otlpMetric, []otlpNameClash) {
	set, own := resource.Set, len(resource.seqs)
	var keep func(series int, name string, kind int, attributes []otlpAttribute) bool
	if sources != nil {
		keep = func(series int, name string, kind int, _ []otlpAttribute) bool {
			sources.add(series, name, kind)
			return true
		}
	}
	out, shared := otlpPoints(set, own, now, start, keep)
	if !shared {
		return out, nil
	}
	// Every point the export would have, in the order they are made in.
	type point struct {
		name, identity string
		kind           int
		written        int64
	}
	var points []point
	otlpPoints(set, own, now, nil, func(series int, name string, kind int, attributes []otlpAttribute) bool {
		written := int64(math.MaxInt64)
		if series < own {
			written = resource.seqs[series]
		}
		points = append(points, point{name: name, identity: otlpPointIdentity(name, attributes), kind: kind, written: written})
		return false
	})
	later := func(a, b int) bool {
		return points[a].written > points[b].written || points[a].written == points[b].written && a > b
	}
	// The point of each name written last, whose kind the name is exported
	// as.
	last := map[string]int{}
	for n := range points {
		if of, seen := last[points[n].name]; !seen || later(n, of) {
			last[points[n].name] = n
		}
	}
	leftOut, some := make([]bool, len(points)), false
	latest := map[string]int{}
	var clashes map[string]*otlpNameClash
	for n := range points {
		p := &points[n]
		if kept := points[last[p.name]].kind; p.kind != kept {
			leftOut[n], some = true, true
			if clashes == nil {
				clashes = map[string]*otlpNameClash{}
			}
			clash := clashes[p.name]
			if clash == nil {
				clash = &otlpNameClash{name: p.name, kept: kept}
				clashes[p.name] = clash
			}
			if !slices.Contains(clash.leftOut, p.kind) {
				clash.leftOut = append(clash.leftOut, p.kind)
			}
			clash.points++
			continue
		}
		earlier, seen := latest[p.identity]
		switch {
		case !seen:
			latest[p.identity] = n
		case later(n, earlier):
			leftOut[earlier], some = true, true
			latest[p.identity] = n
		default:
			leftOut[n], some = true, true
		}
	}
	if !some {
		return out, nil
	}
	n := 0
	if sources != nil {
		*sources = otlpPointSources{index: map[string]int{}}
	}
	out, _ = otlpPoints(set, own, now, start, func(series int, name string, kind int, _ []otlpAttribute) bool {
		n++
		if leftOut[n-1] {
			return false
		}
		if sources != nil {
			sources.add(series, name, kind)
		}
		return true
	})
	reported := make([]otlpNameClash, 0, len(clashes))
	for _, name := range model.SortedKeys(clashes) {
		slices.Sort(clashes[name].leftOut)
		reported = append(reported, *clashes[name])
	}
	return out, reported
}

// otlpPointIdentity is what tells a point of a resource from every other
// in an export, among those of the kind its name is exported as: the
// metric's name, and each attribute's name and value, each after its length
// (appendKeyPart), so nothing is read from what they hold.
func otlpPointIdentity(name string, attributes []otlpAttribute) string {
	b := appendKeyPart(nil, name)
	for _, a := range attributes {
		b = appendKeyPart(appendKeyPart(b, a.Key), a.Value.StringValue)
	}
	return string(b)
}

// errOTLPNameClash is what is logged of a name exported as two kinds.
var errOTLPNameClash = errors.New("two writers of one OTLP resource - probes, static targets, or the exporter with its own metrics - export this metric name as different kinds, and a name is one metric of one kind there; rename one of the metrics, or give a static target a resource of its own with its otlp.service_name or otlp.resource_attributes")

// logOTLPNameClashes logs each name of a resource that an export had as
// two kinds, once and then as a repeat (failureLog), whichever of the kinds
// was written last at each export. The line shows the name as an error
// shows one (model.ShownName), and the clash is remembered under the whole
// name.
func (s *Server) logOTLPNameClashes(identity otlpResourceIdentity, resource string, clashes []otlpNameClash) {
	for _, clash := range clashes {
		kinds := make([]string, 0, len(clash.leftOut))
		for _, kind := range clash.leftOut {
			kinds = append(kinds, otlpKindNames[kind])
		}
		s.failures.failed(s.logger, slog.LevelWarn, otlpNameClashKey(resource, clash.name),
			"OTLP metric name written as two kinds under one resource; the data points of the kind written earlier are left out of the export", "otlp", errOTLPNameClash,
			"metric", model.ShownName(clash.name), "kind", otlpKindNames[clash.kept], "left_out_kind", strings.Join(kinds, ", "), "left_out_points", clash.points, "service_name", identity.ServiceName)
	}
}

// untypedFamilies are the names of the families of a set that have a series
// whose values its type does not allow (typedValues), which are exported as
// gauges; nil when there is none, as nearly always. It is decided for the
// family, as in OpenMetrics, so that a name is one kind of metric in an
// export.
func untypedFamilies(set model.MetricSet) map[string]bool {
	var untyped map[string]bool
	for _, m := range set.Metrics {
		if !typedValues(m) {
			if untyped == nil {
				untyped = map[string]bool{}
			}
			untyped[m.Name] = true
		}
	}
	return untyped
}

// otlpAttributesWith is otlpAttributesForLabels with one more attribute, a
// bucket's le or a quantile, set over a label of that name, as the
// exposition writes it.
func otlpAttributesWith(labels map[string]string, name, value string) []otlpAttribute {
	with := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		with[k] = v
	}
	with[name] = value
	return otlpAttributesForLabels(with)
}

const (
	otlpKindGauge = iota
	otlpKindSum
	otlpKindHistogram
	otlpKindSummary
)

// otlpKindNames are the kinds as a log line names them.
var otlpKindNames = [...]string{otlpKindGauge: "gauge", otlpKindSum: "sum", otlpKindHistogram: "histogram", otlpKindSummary: "summary"}

// otlpKindOf is the OTLP kind a metric is exported as. A histogram or summary
// type without its data is exported as a gauge of its value.
func otlpKindOf(m model.Metric) int {
	switch {
	case m.Type == model.HistogramMetricType && m.Histogram != nil:
		return otlpKindHistogram
	case m.Type == model.SummaryMetricType && m.Summary != nil:
		return otlpKindSummary
	case m.Type == model.CounterMetricType:
		return otlpKindSum
	}
	return otlpKindGauge
}

func otlpKind(m otlpMetric) int {
	switch {
	case m.Histogram != nil:
		return otlpKindHistogram
	case m.Summary != nil:
		return otlpKindSummary
	case m.Sum != nil:
		return otlpKindSum
	}
	return otlpKindGauge
}

func newOTLPMetric(name, help string, kind int) otlpMetric {
	out := otlpMetric{Name: name, Description: help}
	switch kind {
	case otlpKindHistogram:
		out.Histogram = &otlpHistogram{AggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE"}
	case otlpKindSummary:
		out.Summary = &otlpSummary{}
	case otlpKindSum:
		out.Sum = &otlpSum{AggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE", IsMonotonic: true}
	default:
		out.Gauge = &otlpGauge{}
	}
	return out
}

// otlpHistogramPoint turns Prometheus's cumulative buckets into OTLP's
// per-bucket counts, in ascending order of their bounds whatever order the
// source had them in. The +Inf bucket, when the histogram carries one, is
// not a bound: what lies above the highest finite bound is the count less
// the last finite bucket's cumulative count. The histogram is one whose
// values its type allows (typedValues): its bounds are numbers, its +Inf
// bucket's count is its _count where it has both, and its cumulative counts
// never fall, up to that count, so every bucket's own count is its
// cumulative count less the one before, and they add up to the count, as
// OTLP requires.
func otlpHistogramPoint(m model.Metric, attributes []otlpAttribute, at string) otlpHistogramDataPoint {
	h := m.Histogram
	var e expositionWriter
	buckets := e.ascendingBuckets(h.Buckets)
	count, _ := h.InfBucket()
	point := otlpHistogramDataPoint{
		Attributes:     attributes,
		TimeUnixNano:   at,
		Count:          strconv.FormatUint(count, 10),
		BucketCounts:   make([]string, 0, len(buckets)+1),
		ExplicitBounds: make([]otlpDouble, 0, len(buckets)),
	}
	// OTLP's sum is optional, so a histogram read without a _sum is sent
	// without one rather than with a sum of 0.
	if !h.NoSum {
		sum := otlpDouble(h.Sum)
		point.Sum = &sum
	}
	var previous uint64
	for _, b := range buckets {
		if math.IsInf(b.UpperBound, 1) {
			continue
		}
		point.ExplicitBounds = append(point.ExplicitBounds, otlpDouble(b.UpperBound))
		point.BucketCounts = append(point.BucketCounts, strconv.FormatUint(b.CumulativeCount-previous, 10))
		previous = b.CumulativeCount
	}
	point.BucketCounts = append(point.BucketCounts, strconv.FormatUint(count-previous, 10))
	return point
}

// otlpSummaryPoint is a summary as an OTLP point. OTLP's summary has no way
// to leave out its count or its sum, which a field left unset is 0 of, so a
// summary read without a _count or a _sum is sent with 0 for it.
func otlpSummaryPoint(m model.Metric, attributes []otlpAttribute, at string) otlpSummaryDataPoint {
	s := m.Summary
	point := otlpSummaryDataPoint{Attributes: attributes, TimeUnixNano: at, Count: "0"}
	if !s.NoCount {
		point.Count = strconv.FormatUint(s.Count, 10)
	}
	if !s.NoSum {
		point.Sum = otlpDouble(s.Sum)
	}
	var e expositionWriter
	for _, q := range e.ascendingQuantiles(s.Quantiles) {
		point.QuantileValues = append(point.QuantileValues, otlpQuantileValue{Quantile: otlpDouble(q.Quantile), Value: otlpDouble(q.Value)})
	}
	return point
}

// OTLP export is best-effort: a failed export never fails a probe. Best-effort
// is not the same as silent, though. An endpoint that went away, a token that
// expired or a collector that answers 429 for hours would otherwise show only
// as warnings in the log and as data missing from the backend, where nobody
// looks for a cause. These self-metrics say how exports are going, for as long
// as OTLP is enabled:
//
//	http_exporter_otlp_exports_total{result}
//	http_exporter_otlp_export_retries_total
//	http_exporter_otlp_points_dropped_total
//	http_exporter_otlp_export_duration_seconds
//	http_exporter_otlp_last_export_success_timestamp_seconds
//
// An export is one delivery of everything pending, its retries included,
// however many requests it is sent in, so a retried export that got through
// is one success, and one that ran out of retries, or of which the endpoint
// refused a request, is one failure. They are exported over OTLP too, so the backend
// learns of a failed export at the next one that gets through.
//
// With otlp.unready_after_failures set, consecutive failures also make the
// exporter unready (readiness.go). They are counted per endpoint: a reload
// that points OTLP somewhere else starts the count again, so a new endpoint
// is not held responsible for the old one's failures.

type otlpStatus struct {
	mu                  sync.Mutex
	successes, failures uint64
	retries, dropped    int
	lastDuration        time.Duration
	lastSuccess         time.Time
	// consecutiveFailures counts the exports to endpoint since the last
	// success.
	consecutiveFailures int
	endpoint            string
}

// record records an export to endpoint and how many retries it took.
func (o *otlpStatus) record(endpoint string, duration time.Duration, retries int, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if endpoint != o.endpoint {
		o.endpoint, o.consecutiveFailures = endpoint, 0
	}
	o.lastDuration = duration
	o.retries += retries
	if ok {
		o.successes++
		o.lastSuccess = time.Now()
		o.consecutiveFailures = 0
		return
	}
	o.failures++
	o.consecutiveFailures++
}

// drop counts data points given up on.
func (o *otlpStatus) drop(points int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropped += points
}

// failing reports the exports to endpoint that have failed since the last
// success; none when the last exports went elsewhere.
func (o *otlpStatus) failing(endpoint string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if endpoint != o.endpoint {
		return 0
	}
	return o.consecutiveFailures
}

// otlpStatusMetrics renders the export status while OTLP is enabled, one
// family at a time.
func (s *Server) otlpStatusMetrics() []model.Metric {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" {
		return nil
	}
	o := s.otlp
	o.mu.Lock()
	defer o.mu.Unlock()
	metric := func(name string, typ model.MetricType, value float64, labels map[string]string) model.Metric {
		return model.Metric{Name: name, Help: exporterMetricHelp[name], Type: typ, Value: value, Labels: labels}
	}
	return []model.Metric{
		metric("http_exporter_otlp_exports_total", model.CounterMetricType, float64(o.successes), map[string]string{"result": "success"}),
		metric("http_exporter_otlp_exports_total", model.CounterMetricType, float64(o.failures), map[string]string{"result": "failure"}),
		metric("http_exporter_otlp_export_retries_total", model.CounterMetricType, float64(o.retries), nil),
		metric("http_exporter_otlp_points_dropped_total", model.CounterMetricType, float64(o.dropped), nil),
		metric("http_exporter_otlp_export_duration_seconds", model.GaugeMetricType, o.lastDuration.Seconds(), nil),
		metric("http_exporter_otlp_last_export_success_timestamp_seconds", model.GaugeMetricType, model.ScrapeTimestamp(o.lastSuccess), nil),
	}
}

// otlpBatch holds the metrics pending export for one OTLP resource.
type otlpBatch struct {
	identity otlpResourceIdentity
	metrics  map[string]pendingMetric
}

// pendingMetric is a metric waiting for export, with its age.
type pendingMetric struct {
	metric model.Metric
	seq    int64
}

// scrapeTime is when the series of a result queued for OTLP were scraped. A
// data point carries the time its value came from the target, and an answer
// from the cache, fresh or stale (cache.go), is made of values that came
// from it earlier than the answer was given: exported as of the answer, an
// old value would look newly measured every time it was served again.
//
// The first data series of the result came from the target at at. The rest
// are the exporter's own account of the result as it is queued — a static
// target's health metrics — and so are, wherever they stand, the freshness
// series of a collector with cache.stale_if_error, which say how old the
// result is now. Those are as of now, as every series is when at is zero.
type scrapeTime struct {
	at        time.Time
	data      int
	freshness bool
}

// scraped is scrapeTime for a result that is all the target's but for its
// freshness series, as a probe's answer is.
func scraped(set model.MetricSet, c *model.Collector, at time.Time) scrapeTime {
	return scrapeTime{at: at, data: len(set.Metrics), freshness: model.StaleIfError(c) > 0}
}

// covers reports whether the i-th series of the result, m, came from the
// target at t.at.
func (t scrapeTime) covers(i int, m model.Metric) bool {
	if t.at.IsZero() || i >= t.data {
		return false
	}
	if _, own := resultFreshnessHelp[m.Name]; own && t.freshness {
		return false
	}
	return true
}

// Probes of every target and collector are queued under the one exporter-wide
// resource, where a series is known by its name, type and labels alone. Two
// probes answering the same series — two targets behind one collector, two
// collectors naming a metric alike — are then one series, and the later
// probe's point replaces the earlier's before the export. With
// otlp.probe_attributes, each probe's points carry collector and target
// attributes, so they stay apart; a label of the series' own by either name
// is kept. What two probes write that is one stream to a receiver without
// being one series here — a gauge and an untyped series of one name, a name
// that is a gauge of one and a counter of the other — is settled where the
// export is made (otlpMetricsOf).

// Probe attribute names.
const (
	probeCollectorAttribute = "collector"
	probeTargetAttribute    = "target"
)

// queueProbeOTLP stages a probe's answer under the exporter-wide resource,
// with collector and target attributes when otlp.probe_attributes says so.
// fetched is when the answer's data came from the target: the trip just
// made, or the earlier one whose result the cache answered with.
func (s *Server) queueProbeOTLP(set model.MetricSet, c *model.Collector, target string, fetched time.Time) {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || len(set.Metrics) == 0 {
		return
	}
	at := scraped(set, c, fetched)
	if cfg.ProbeAttributes {
		attributes := map[string]string{probeCollectorAttribute: c.Name}
		if target != "" {
			attributes[probeTargetAttribute] = target
		}
		set = withTargetLabels(set, attributes)
	}
	s.queueOTLPResource(set, defaultResourceIdentity(cfg), at)
}

// queueOTLPResource stages metrics under a specific resource, so a static
// target's own service name and resource attributes survive to the exporter.
// at says when they were scraped.
func (s *Server) queueOTLPResource(set model.MetricSet, identity otlpResourceIdentity, at scrapeTime) {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" || len(set.Metrics) == 0 {
		return
	}
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	batch := s.pendingBatchLocked(identity)
	queued, fetched := time.Now().UnixMilli(), at.at.UnixMilli()
	for i, metric := range set.Metrics {
		s.otlpSeq++
		point := model.CloneMetric(metric)
		// A point is exported at the time it was scraped, which the queue
		// keeps through the wait for the next export and any retries, not
		// at the time the export is sent, and which for a result answered
		// from the cache is when the cached trip was made (scrapeTime).
		switch {
		case point.Timestamp != nil:
		case at.covers(i, metric):
			point.Timestamp = &fetched
		default:
			point.Timestamp = &queued
		}
		s.putPendingLocked(batch, otlpMetricKey(point), pendingMetric{metric: point, seq: s.otlpSeq})
	}
	s.capPendingLocked(cfg.MaxPendingPoints)
}

func (s *Server) pendingBatchLocked(identity otlpResourceIdentity) *otlpBatch {
	key := identity.key()
	batch := s.otlpPending[key]
	if batch == nil {
		batch = &otlpBatch{identity: identity, metrics: map[string]pendingMetric{}}
		s.otlpPending[key] = batch
	}
	return batch
}

func (s *Server) putPendingLocked(batch *otlpBatch, key string, m pendingMetric) {
	if _, exists := batch.metrics[key]; !exists {
		s.otlpPoints++
	}
	batch.metrics[key] = m
}

// capPendingLocked keeps the pending data points within otlp.max_pending_points
// while exports are failing. Past it, the oldest points are dropped down to
// nine tenths of it, so the sort is paid once per tenth of the buffer rather
// than on every point, and are counted and logged.
func (s *Server) capPendingLocked(limit int) {
	if limit <= 0 {
		limit = model.DefaultOTLPMaxPendingPoints
	}
	if s.otlpPoints <= limit {
		return
	}
	type aged struct {
		batch, key string
		seq        int64
	}
	all := make([]aged, 0, s.otlpPoints)
	for batchKey, batch := range s.otlpPending {
		for key, m := range batch.metrics {
			all = append(all, aged{batchKey, key, m.seq})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	drop := s.otlpPoints - limit*9/10
	for _, a := range all[:drop] {
		batch := s.otlpPending[a.batch]
		delete(batch.metrics, a.key)
		if len(batch.metrics) == 0 {
			delete(s.otlpPending, a.batch)
		}
	}
	s.otlpPoints -= drop
	s.otlp.drop(drop)
	s.logger.Warn("OTLP data points waiting for export reached otlp.max_pending_points; the oldest were dropped", "max_pending_points", limit, "dropped_points", drop)
}

func (s *Server) drainOTLP() []otlpResourceSet {
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	keys := make([]string, 0, len(s.otlpPending))
	for key := range s.otlpPending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]otlpResourceSet, 0, len(keys))
	for _, key := range keys {
		batch := s.otlpPending[key]
		set := model.MetricSet{Metrics: make([]model.Metric, 0, len(batch.metrics))}
		seqs := make([]int64, 0, len(batch.metrics))
		for _, metricKey := range model.SortedKeys(batch.metrics) {
			set.Metrics = append(set.Metrics, batch.metrics[metricKey].metric)
			seqs = append(seqs, batch.metrics[metricKey].seq)
		}
		out = append(out, otlpResourceSet{Identity: batch.identity, Set: set, seqs: seqs})
	}
	s.otlpPending = make(map[string]*otlpBatch)
	s.otlpPoints = 0
	return out
}

// appendToResource adds metrics to the matching resource in resources, creating
// the entry when the identity is not present yet.
func appendToResource(resources []otlpResourceSet, identity otlpResourceIdentity, set model.MetricSet) []otlpResourceSet {
	if len(set.Metrics) == 0 {
		return resources
	}
	key := identity.key()
	for i := range resources {
		if resources[i].Identity.key() == key {
			resources[i].Set.Metrics = append(resources[i].Set.Metrics, set.Metrics...)
			return resources
		}
	}
	return append(resources, otlpResourceSet{Identity: identity, Set: set})
}

// OTLPExportLoop exports everything pending every otlp.interval until ctx
// ends. It returns without a last export, which
// is FlushOTLP's to make once the HTTP server has finished its probes.
func (s *Server) OTLPExportLoop(ctx context.Context) {
	for {
		interval := time.Duration(s.manager.Get().OTLP.Interval)
		if interval <= 0 {
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		cfg := s.manager.Get().OTLP
		if !cfg.Enabled || cfg.Endpoint == "" {
			_ = s.drainOTLP()
			continue
		}
		// Static targets are scraped on their own intervals
		// (StaticScrapeLoop); an export delivers what the probes and the
		// targets with export_via_otlp queued since the last one. It may retry for up to an interval, so it
		// never runs into the next one.
		s.exportOTLP(ctx, interval)
	}
}

// FlushOTLP makes the last export at shutdown: the metrics probes queued since
// the last export, and a final self-metric snapshot, within otlp.timeout.
// Static targets are not scraped again.
func (s *Server) FlushOTLP() {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" {
		return
	}
	s.logger.Info("sending the last OTLP export before exiting")
	s.exportOTLPOnce(context.Background(), time.Duration(cfg.Timeout), true)
}

// exportOTLP sends everything pending, with a self-metric snapshot, retrying
// within budget, in as many requests as otlp.batch_max_size and
// otlp.batch_max_bytes make of it, one after another. Metrics that could not
// be delivered for a reason worth retrying — a network error, 429, 502, 503,
// 504, the budget running out, or the export being cut short by shutdown —
// are queued again for the next export, with those of every request not sent
// yet, unless a newer value of the same series has been queued since; the
// self-metrics are not, since the next export takes a new snapshot, and
// neither are the metrics of the requests delivered before. Metrics the
// endpoint refused outright are dropped and counted, those of the request it
// refused, since sending them again would be refused again, and the export
// goes on with the next request.
func (s *Server) exportOTLP(ctx context.Context, budget time.Duration) {
	s.exportOTLPOnce(ctx, budget, false)
}

// exportOTLPOnce is exportOTLP, and the last export at shutdown when last
// says so (FlushOTLP): there is no next export then, so the metrics a
// failure would queue again for it are dropped instead, counted in
// http_exporter_otlp_points_dropped_total and logged as dropped.
//
// It is one export to the self-metrics however many requests it is sent
// in: its duration is the whole export's, its retries those of every
// request, and it is a success when the endpoint accepted every request.
func (s *Server) exportOTLPOnce(ctx context.Context, budget time.Duration, last bool) {
	cfg := s.manager.Get().OTLP
	pending := s.drainOTLP()
	if !cfg.Enabled || cfg.Endpoint == "" {
		return
	}
	// The copy keeps the self-metrics out of pending, which may be queued again.
	resources := appendToResource(append([]otlpResourceSet(nil), pending...), defaultResourceIdentity(cfg), s.selfMetricSet())
	start := time.Now()
	deadline := start.Add(budget)
	requests, err := s.otlpRequestsOf(cfg, resources)
	retries, ok := 0, true
	for k := 0; err == nil && k < len(requests.bodies); k++ {
		// A request after the first is only worth starting if it has time
		// to finish; the first is sent as an export of one request always
		// was.
		if k > 0 && time.Until(deadline) < otlpMinAttempt {
			err = fmt.Errorf("the export ran out of time after %d of its %d requests", k, len(requests.bodies))
			pending = requests.from(pending, k)
			break
		}
		var tries int
		var partial otlpPartialSuccess
		tries, partial, err = s.deliverOTLP(ctx, cfg, requests.bodies[k], deadline)
		retries += tries
		if err != nil && ctx.Err() != nil {
			// Shutting down: the last export sends these.
			s.requeueOTLP(requests.from(pending, k))
			return
		}
		if err == nil {
			// The endpoint took the request but not all of it. The rejected
			// points would be rejected again, so they are dropped and counted
			// like a refused request's.
			switch {
			case partial.rejected > 0:
				s.otlp.drop(int(min(partial.rejected, int64(math.MaxInt32))))
				s.logger.Warn("OTLP endpoint accepted an export but rejected some of its data points; they are dropped", "rejected_points", partial.rejected, "error_message", partial.message, "retries", tries)
			case partial.message != "":
				s.logger.Warn("OTLP endpoint accepted an export with a warning", "error_message", partial.message)
			}
			continue
		}
		var refused *otlpRefusedError
		if errors.As(err, &refused) {
			ok, err = false, nil
			points := requests.of(pending, k)
			s.otlp.drop(points)
			attrs := []any{"status", refused.status, "dropped_points", points, "retries", tries}
			if len(requests.bodies) > 1 {
				attrs = append(attrs, "request", k+1, "requests", len(requests.bodies))
			}
			if refused.body != "" {
				attrs = append(attrs, "response_body", refused.body)
			}
			s.logger.Warn("OTLP endpoint refused an export; its data points are dropped", attrs...)
			continue
		}
		// Worth retrying, and out of retries: this request and those after
		// it are what is left to deliver.
		pending = requests.from(pending, k)
	}
	s.otlp.record(cfg.Endpoint, time.Since(start), retries, ok && err == nil)
	if err == nil {
		return
	}
	if last {
		points := countPoints(pending)
		s.otlp.drop(points)
		s.logger.Warn("the last OTLP export before exiting failed; its data points are dropped", "error", err, "dropped_points", points, "retries", retries)
		return
	}
	s.requeueOTLP(pending)
	s.logger.Warn("OTLP export failed; its data points are kept for the next export", "error", err, "retries", retries)
}

// requeueOTLP queues metrics that were not delivered again, each unless a
// newer value of its series has been queued since it was drained. Each keeps
// the place in the queue it was drained with, older than anything queued
// since, so otlp.max_pending_points drops the oldest points first however
// many exports in a row have failed.
func (s *Server) requeueOTLP(resources []otlpResourceSet) {
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	for _, resource := range resources {
		batch := s.pendingBatchLocked(resource.Identity)
		for i, metric := range resource.Set.Metrics {
			key := otlpMetricKey(metric)
			if _, newer := batch.metrics[key]; !newer {
				s.putPendingLocked(batch, key, pendingMetric{metric: metric, seq: resource.seqs[i]})
			}
		}
	}
	s.capPendingLocked(s.manager.Get().OTLP.MaxPendingPoints)
}

func countPoints(resources []otlpResourceSet) int {
	n := 0
	for _, resource := range resources {
		n += len(resource.Set.Metrics)
	}
	return n
}

// otlpMetricKey is what tells a series of a resource from every other
// (otlpKey): its name, its type, and each label's name and value. A point
// queued replaces the one waiting under it, and a resource's points are
// exported in the order of their keys.
func otlpMetricKey(metric model.Metric) string {
	return otlpSeriesKey("", &metric)
}

// otlpSeriesKey is otlpMetricKey written after before: where the start
// times are kept, the key of the series' resource (otlpStartTimes).
func otlpSeriesKey(before string, metric *model.Metric) string {
	return otlpKey(before, metric.Labels, metric.Name, string(metric.Type))
}

// What a key of the export has after each NUL of its join, and what ends
// the join (otlpKey).
const (
	otlpKeyNUL = "\x00\x01"
	otlpKeyEnd = "\x00\x00"
)

// otlpKey is the key of a resource or of a series: what the export tells one
// from every other by, where points wait for it (otlpBatch), among the
// resources of one (appendToResource) and where start times are kept
// (otlpStartTimes). head are the parts every one has — a resource's service
// name; a series' name and type — and pairs its attributes or its labels.
// before is written first, as it is.
//
// The key was once the parts joined: the head with a NUL between its parts,
// and then a NUL, the name, = and the value of each pair, by the pairs'
// names in order. But a label's value is the target's and a resource's
// attributes are the operator's, of any bytes: m{a="1\x00b=2"} and
// m{a="1",b="2"} are joined to the same bytes, as are the attribute a=b of
// the value c and the attribute a of the value b=c, and a resource whose
// last attribute holds what another's series begin with. Two series with
// one key were one in the queue, where the later point replaced the other
// and that series was never exported; two resources with one key were
// exported as one, under the attributes of the first; and two series with
// one key among the start times each took the other's count for its own,
// and were given a start time, and a reset, that the other had.
//
// So the key is the join, and after it the length of every part, each a
// varint (binary.AppendUvarint), in the order the parts are joined in. The
// lengths say where each part is within the join, so nothing is read from
// what a part holds, and no two subjects have one key whatever their parts
// hold. Where the join ends is told without a length before it: every NUL of
// the join, a part's own or one between two parts, is written with a 0x01
// after it (otlpKeyNUL), and two NULs end it (otlpKeyEnd), which the join
// so written never holds.
//
// The join is kept, where the failure log's keys have each part after its
// own length (appendKeyPart), and it ends as it does, because the resources
// of an export and the points of each are exported in the order of their
// keys (drainOTLP), which was the order of the joins: and written so, keys
// are in the order of their joins still. Up to where two joins first differ
// their keys are the same; there the smaller byte is the smaller byte of
// the keys, a NUL too; and a join that is the beginning of another ends
// with two NULs where the other goes on with a byte that is not a NUL, or
// with a NUL and 0x01. Only subjects whose parts are joined to the same
// bytes, which had one key, are in the order of the lengths.
//
// It is made in the one allocation that holds it, where the old key took
// one for the names in order and several as it grew: the parts are put in
// the order they are joined in, in place, for all but a subject of more
// pairs than any is likely to have.
func otlpKey(before string, pairs map[string]string, head ...string) string {
	// The head, and then each pair's name and value. A subject has few
	// pairs, so each is moved down to its place among those before it.
	var few [2 + 2*8]string
	parts := append(few[:0], head...)
	for name, value := range pairs {
		parts = append(parts, name, value)
		for at := len(parts) - 2; at > len(head) && parts[at] < parts[at-2]; at -= 2 {
			parts[at], parts[at-2] = parts[at-2], parts[at]
			parts[at+1], parts[at-1] = parts[at-1], parts[at+1]
		}
	}
	// What the key takes: before a name and between the parts of the head
	// a NUL as a key has it, and before a value an =; of each part its
	// bytes, one more for each NUL among them, and its length.
	size := len(before) + len(otlpKeyNUL)*(len(head)-1+len(pairs)) + len(pairs) + len(otlpKeyEnd)
	nuls := 0
	for _, part := range parts {
		nuls += strings.Count(part, "\x00")
		size += len(part) + 1
		for more := len(part) >> 7; more > 0; more >>= 7 {
			size++
		}
	}
	var b strings.Builder
	b.Grow(size + nuls)
	b.WriteString(before)
	for i, part := range parts {
		switch {
		case i == 0:
		case i >= len(head) && (i-len(head))%2 == 1:
			b.WriteByte('=')
		default:
			b.WriteString(otlpKeyNUL)
		}
		// Hardly any subject has a NUL in a part, and then none is looked
		// for again.
		if nuls > 0 {
			for nul := strings.IndexByte(part, 0); nul >= 0; nul = strings.IndexByte(part, 0) {
				b.WriteString(part[:nul])
				b.WriteString(otlpKeyNUL)
				part = part[nul+1:]
			}
		}
		b.WriteString(part)
	}
	b.WriteString(otlpKeyEnd)
	var length [binary.MaxVarintLen64]byte
	for _, part := range parts {
		b.Write(binary.AppendUvarint(length[:0], uint64(len(part))))
	}
	return b.String()
}
