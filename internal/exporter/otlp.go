package exporter

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
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
	// ExponentialHistogram carries the exporter's own durations under
	// otlp.exponential_histograms; nothing a target answers has one.
	ExponentialHistogram *otlpExponentialHistogram `json:"exponentialHistogram,omitempty"`
}

// otlpCumulative is AGGREGATION_TEMPORALITY_CUMULATIVE as OTLP/JSON writes
// it: an enum is its number there, which the OTLP specification's JSON
// encoding requires, not its name, which some receivers refuse.
const otlpCumulative = 2

type otlpHistogram struct {
	DataPoints             []otlpHistogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                      `json:"aggregationTemporality"`
}

// otlpHistogramDataPoint is one series of a histogram. OTLP bucket counts
// are per bucket, not cumulative like Prometheus buckets. There is one more
// count than there are bounds: the last count covers everything above the
// highest bound, which is Prometheus's +Inf bucket.
type otlpHistogramDataPoint struct {
	Attributes        []otlpAttribute `json:"attributes,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	Count             string          `json:"count"`
	Sum               *otlpDouble     `json:"sum,omitempty"`
	BucketCounts      []string        `json:"bucketCounts"`
	ExplicitBounds    []otlpDouble    `json:"explicitBounds"`
	Exemplars         []otlpExemplar  `json:"exemplars,omitempty"`
}

// otlpExemplar is an exemplar of a histogram's point (model.Exemplar): its
// time, its value and the trace and the span it was observed in, hex as
// OTLP/JSON writes ids. Only the exporter's own durations have them.
type otlpExemplar struct {
	TimeUnixNano string     `json:"timeUnixNano"`
	AsDouble     otlpDouble `json:"asDouble"`
	SpanID       string     `json:"spanId"`
	TraceID      string     `json:"traceId"`
}

// otlpExemplars are a histogram's exemplars as OTLP points carry them, nil
// for none.
func otlpExemplars(h *model.Histogram) []otlpExemplar {
	var out []otlpExemplar
	for _, x := range histogramExemplars(h) {
		out = append(out, otlpExemplar{TimeUnixNano: strconv.FormatInt(x.Time.UnixNano(), 10), AsDouble: otlpDouble(x.Value), SpanID: x.SpanID, TraceID: x.TraceID})
	}
	return out
}

type otlpExponentialHistogram struct {
	DataPoints             []otlpExponentialHistogramDataPoint `json:"dataPoints"`
	AggregationTemporality int                                 `json:"aggregationTemporality"`
}

// otlpExponentialHistogramDataPoint is one series of an exponential
// histogram (model.ExponentialHistogram). It holds the scale, the count of
// the zero bucket, and the per-bucket counts of the positive buckets from
// the bucket numbered offset on. The exporter's durations are never
// negative, so there are no negative buckets. The zero bucket holds exact
// zeros only, which is the zero threshold OTLP assumes when it is omitted.
// min and max are omitted for a histogram that has observed nothing, since
// it has none.
type otlpExponentialHistogramDataPoint struct {
	Attributes        []otlpAttribute    `json:"attributes,omitempty"`
	StartTimeUnixNano string             `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string             `json:"timeUnixNano"`
	Count             string             `json:"count"`
	Sum               *otlpDouble        `json:"sum,omitempty"`
	Scale             int32              `json:"scale"`
	ZeroCount         string             `json:"zeroCount"`
	Positive          otlpExponentialSet `json:"positive"`
	Min               *otlpDouble        `json:"min,omitempty"`
	Max               *otlpDouble        `json:"max,omitempty"`
	Exemplars         []otlpExemplar     `json:"exemplars,omitempty"`
}

type otlpExponentialSet struct {
	Offset       int32    `json:"offset"`
	BucketCounts []string `json:"bucketCounts"`
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
	AggregationTemporality int                   `json:"aggregationTemporality"`
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
	// otelNames says the export names its metrics as OpenTelemetry does
	// (otlp.metric_names: otel, otlpNameOf); set where an export's
	// metrics are made, from the configuration it is made under.
	otelNames bool
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
// and self-health metrics. A resource attribute set to "" is omitted, just as
// a transform.labels value set to "" omits the label. The resource carries no
// empty attributes.
//
// With otlp.exporter_instance on, it also carries the replica's name, and so
// does every resource built from it: a static target's (targetResource), the
// log records' (otlplogs.go) and the debug page's (debugotlp.go). Like any
// attribute, it is part of the resource's identity (key). The configuration
// check filled in its value and rejected an attribute with that name
// elsewhere on the resource (config.checkExporterInstance). An exporter that
// could not read its host name never gets here.
func defaultResourceIdentity(cfg model.OTLPConfig) otlpResourceIdentity {
	identity := otlpResourceIdentity{ServiceName: cfg.ServiceName, Attributes: map[string]string{}}
	for key, value := range cfg.ResourceAttributes {
		if value != "" {
			identity.Attributes[key] = value
		}
	}
	if e := cfg.ExporterInstance; e.Enabled && e.Attribute != "" && e.Value != "" {
		identity.Attributes[e.Attribute] = e.Value
	}
	return identity
}

// otlpRequests are the requests of one export, in the order they are sent.
// It holds each request's body. With several requests, it also records the
// last request that carries each series' points, indexed by the series'
// place among the export's resources and within its resource's set. An
// export of one request records none. A series is accounted for by that request
// alone. A series with points in two requests (a histogram exported as
// gauges under its samples' names) is kept again when the later one fails.
// A series with no point sent, left out for another written later
// (otlpMetricsOf), belongs to the first request. points are the data points
// of each request, which the status page counts for the requests sent.
type otlpRequests struct {
	bodies [][]byte
	last   [][]int
	points []int
}

// otlpRequestsOf builds the requests that send resources to the endpoint.
// An export of up to otlp.batch_max_size data points and otlp.batch_max_bytes
// bytes of JSON, which is nearly every export, is one request holding every
// resource (otlpPayloadOf). A larger export is split (splitOTLP). Its points
// are then walked again to find which series each request carries
// (otlpPointSources), so that a failed request is accounted for by the series
// of its own points.
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
			return otlpRequests{bodies: [][]byte{body}, points: []int{otlpPayloadPoints(payload)}}, nil
		}
	}
	split, err := splitOTLP(payload, maxPoints, maxBytes)
	if err != nil {
		return otlpRequests{}, fmt.Errorf("encoding the export: %w", err)
	}
	requests := otlpRequests{bodies: make([][]byte, len(split)), last: make([][]int, len(resources)), points: make([]int, len(split))}
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
			resource := resources[r]
			resource.otelNames = cfg.MetricNames == model.OTLPMetricNamesOTel
			otlpMetricsWith(resource, now, nil, &sources[len(sources)-1])
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
			requests.points[k] += piece.to - piece.from
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

// from returns the series of pending whose last request is k or later. These
// are what the export has not delivered when request k fails for a retryable
// reason, or is not sent. pending are the export's resources without the
// exporter's own series, which come after a resource's queued series.
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
		made, clashes := otlpResourceMetricsOf(cfg, resource, now, s.otlpStarts.forResource(key), nil)
		s.logOTLPNameClashes(resource.Identity, key, clashes)
		payload.ResourceMetrics = append(payload.ResourceMetrics, made)
	}
	return payload, now
}

// otlpResourceMetricsOf builds what an export sends for one resource: its
// attributes, the scope, and its metrics built under cfg's otlp.metric_names
// (otlpMetricsWith), plus the name clashes found among them. The export uses
// it for each resource of its payload. The debug page uses it for the
// resource of one run (debugotlp.go), so the page shows what the export would
// send without anything queued or remembered. start gives each cumulative
// point its start time. sources, when given, records which series each point
// belongs to and which points are left out.
func otlpResourceMetricsOf(cfg model.OTLPConfig, resource otlpResourceSet, now string, start func(m model.Metric, at string) string, sources *otlpPointSources) (otlpResourceMetrics, []otlpNameClash) {
	resource.otelNames = cfg.MetricNames == model.OTLPMetricNamesOTel
	metrics, clashes := otlpMetricsWith(resource, now, start, sources)
	return otlpResourceMetrics{Resource: otlpResource{Attributes: resource.Identity.attributes()}, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScopeOf(cfg), Metrics: metrics}}}, clashes
}

// deliverOTLP sends one request's body. It retries a network error, 429,
// 502, 503 or 504 (the responses the OTLP specification makes retryable)
// with exponential backoff, or after the Retry-After the endpoint asks for,
// while another attempt can start before deadline. Each attempt is bounded
// by otlp.timeout. Any other status is an otlpRefusedError and is not
// retried. It returns the number of retries made, and the partial success
// the endpoint reported when it accepted only part of the request.
func (s *Server) deliverOTLP(ctx context.Context, cfg model.OTLPConfig, body []byte, deadline time.Time) (int, otlpPartialSuccess, error) {
	return s.deliverOTLPOf(ctx, otlpMetricsSignal(s.logger), cfg, body, deadline)
}

// otlpSignal tells the metrics, logs and traces exports apart. It gives the
// field in which the endpoint's partialSuccess counts rejected items, and the
// logger that retries are logged to. The exports of the exporter's own log
// and of its traces log to stderr only (otlplogs.go, otlptraces.go).
// Otherwise each retry of the log export would become a line to export.
type otlpSignal struct {
	kind   otlpSignalKind
	logger *slog.Logger
}

// otlpSignalKind is which of OTLP's signals an export sends.
type otlpSignalKind int

const (
	otlpSignalMetrics otlpSignalKind = iota
	otlpSignalLogs
	otlpSignalTraces
)

func otlpMetricsSignal(logger *slog.Logger) otlpSignal { return otlpSignal{logger: logger} }

// deliverOTLPOf is deliverOTLP for the signal, to cfg.Endpoint.
func (s *Server) deliverOTLPOf(ctx context.Context, signal otlpSignal, cfg model.OTLPConfig, body []byte, deadline time.Time) (int, otlpPartialSuccess, error) {
	timeout := time.Duration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	tlsSettings := cfg.TLS
	for retries := 0; ; retries++ {
		attempt := min(timeout, time.Until(deadline))
		answer, err := s.sendOTLPOf(ctx, signal, cfg, tlsSettings, body, attempt)
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
		signal.logger.Debug("retrying the OTLP export", "error", err, "retry", retries+1, "after", wait.String())
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
	// redirect is set for a 3xx answer, which is not followed; location is
	// the host its Location names, empty when it names none.
	redirect bool
	location string
}

func (e *otlpRefusedError) Error() string {
	if e.redirect {
		return fmt.Sprintf("the OTLP endpoint answered %d, a redirect to %s, which is not followed: set otlp.endpoint to the URL the export should go to", e.status, e.locationText())
	}
	return fmt.Sprintf("the OTLP endpoint answered %d", e.status)
}

func (e *otlpRefusedError) locationText() string {
	if e.location == "" {
		return "no host"
	}
	return e.location
}

// redirectHost is the host a redirect's Location names, resolved against the
// URL redirected, or empty when it names none.
func redirectHost(resp *http.Response) string {
	location, err := resp.Location()
	if err != nil {
		return ""
	}
	return location.Host
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
// the endpoint accepted the export but rejected some data points. rejected
// counts them and message says why. The endpoint may also send a message
// alone, as a warning. Both are zero for a full success.
type otlpPartialSuccess struct {
	rejected int64
	message  string
}

// otlpResponseLimit is the most of an answer's body read: an
// ExportMetricsServiceResponse or an error Status is far smaller.
const otlpResponseLimit = 1 << 20

// sendOTLPOf makes one attempt of an export of the signal.
func (s *Server) sendOTLPOf(ctx context.Context, signal otlpSignal, cfg model.OTLPConfig, tlsSettings model.TLSConfig, body []byte, timeout time.Duration) (otlpAnswer, error) {
	// One connection to the collector is reused across exports.
	// Redirects are not followed. Go would send otlp.headers (which carry
	// the endpoint's API keys and tenant) and the body to wherever the
	// endpoint points, including another host and plain http://. A redirect
	// means a misconfigured endpoint. It is refused below for the operator to fix.
	resp, err := postOTLP(ctx, cfg, tlsSettings, body, timeout)
	if err != nil {
		var unusable *otlpTLSError
		// A TLS configuration that cannot be used is no attempt, and gives
		// none to retry.
		return otlpAnswer{retryable: !errors.As(err, &unusable)}, err
	}
	return otlpAnswerOf(resp, signal.kind)
}

// otlpResponse is what the endpoint answered one request: its status, its
// headers and the start of its body.
type otlpResponse struct {
	status   int
	header   http.Header
	body     []byte
	location string
}

// otlpTLSError is otlp.tls that cannot be made a client of.
type otlpTLSError struct{ err error }

func (e *otlpTLSError) Error() string { return "OTLP TLS configuration: " + e.err.Error() }
func (e *otlpTLSError) Unwrap() error { return e.err }

// postOTLP sends body to cfg.Endpoint once, with the export's headers, TLS,
// compression and timeout, and reads the response. Both an export attempt
// (sendOTLPOf) and the debug page's endpoint test (debugotlptest.go) use it,
// so both are sent the same way.
func postOTLP(ctx context.Context, cfg model.OTLPConfig, tlsSettings model.TLSConfig, body []byte, timeout time.Duration) (otlpResponse, error) {
	client, err := fetch.HTTPClient(fetch.TransportSettings{TLS: tlsSettings}, false, timeout)
	if err != nil {
		return otlpResponse{}, &otlpTLSError{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return otlpResponse{}, fetch.RedactURLErrors(err)
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
		// Unreachable, reset, or timed out: the next attempt may get through.
		// Go's client quotes the endpoint in its error with the query as
		// written, which may contain a token. The error is logged and shown
		// on the status page with the URL's credentials withheld.
		return otlpResponse{}, fetch.RedactURLErrors(err)
	}
	// The body is read to the end, however little of it matters, so the
	// connection goes back to the pool for the next export.
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, otlpResponseLimit))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, otlpResponseLimit))
	_ = resp.Body.Close()
	out := otlpResponse{status: resp.StatusCode, header: resp.Header, body: answer}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		out.location = redirectHost(resp)
	}
	return out, nil
}

// otlpAnswerOf is what an export makes of the endpoint's answer: a success,
// with what its partialSuccess says the endpoint rejected; a 429, 502, 503
// or 504 to retry; or a refusal, a redirect among them, not retried.
func otlpAnswerOf(resp otlpResponse, kind otlpSignalKind) (otlpAnswer, error) {
	switch {
	case resp.status >= 200 && resp.status < 300:
		return otlpAnswer{partial: partialSuccessOf(resp.body, resp.header.Get("Content-Type"), kind)}, nil
	case resp.status == http.StatusTooManyRequests, resp.status == http.StatusBadGateway,
		resp.status == http.StatusServiceUnavailable, resp.status == http.StatusGatewayTimeout:
		return otlpAnswer{retryable: true, wait: retryAfter(resp.header.Get("Retry-After"), time.Now())}, fmt.Errorf("the OTLP endpoint answered %d", resp.status)
	case resp.status >= 300 && resp.status < 400:
		return otlpAnswer{}, &otlpRefusedError{status: resp.status, body: bodyExcerpt(resp.body), redirect: true, location: resp.location}
	default:
		return otlpAnswer{}, &otlpRefusedError{status: resp.status, body: bodyExcerpt(resp.body)}
	}
}

// partialSuccess reads the partialSuccess of a JSON export response. An
// answer that is empty, is not JSON, or does not carry one is a full success:
// the field is optional, and a protobuf answer is not read.
//
// It reads the response of the signal's export: an
// ExportMetricsServiceResponse, an ExportLogsServiceResponse, which counts
// what it rejected in rejectedLogRecords, or an ExportTraceServiceResponse,
// which counts it in rejectedSpans.
func partialSuccessOf(body []byte, contentType string, kind otlpSignalKind) otlpPartialSuccess {
	if len(bytes.TrimSpace(body)) == 0 || contentType != "" && !strings.Contains(strings.ToLower(contentType), "json") {
		return otlpPartialSuccess{}
	}
	var response struct {
		PartialSuccess struct {
			// OTLP/JSON writes an int64 as a string, and a number is
			// accepted too; json.Number reads either.
			RejectedDataPoints json.Number `json:"rejectedDataPoints"`
			RejectedLogRecords json.Number `json:"rejectedLogRecords"`
			RejectedSpans      json.Number `json:"rejectedSpans"`
			ErrorMessage       string      `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return otlpPartialSuccess{}
	}
	partial := otlpPartialSuccess{message: response.PartialSuccess.ErrorMessage}
	count := response.PartialSuccess.RejectedDataPoints
	switch kind {
	case otlpSignalLogs:
		count = response.PartialSuccess.RejectedLogRecords
	case otlpSignalTraces:
		count = response.PartialSuccess.RejectedSpans
	}
	if rejected, err := strconv.ParseInt(strings.Trim(string(count), `"`), 10, 64); err == nil && rejected > 0 {
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

// An export with more data points than otlp.batch_max_size, or more bytes of
// JSON than otlp.batch_max_bytes, is sent as several requests. A receiver
// refuses a request over its own limit (the OpenTelemetry Collector's
// OTLP/HTTP receiver over 20MiB, its gRPC receiver over 4MiB), and a refused
// request is dropped. So a single export of every waiting point would be
// dropped whole, at every interval, once there were enough points.
//
// Points are taken in export order: the resources, the metrics of each, the
// points of each metric. Each request is filled with as many points as fit
// within both limits before the next one starts. A request carries the
// resource and scope of each of its points, and the name, kind and
// description of each of their metrics. So a resource or metric that does
// not fit in one request is written again in the next. A point that alone
// would put its request over otlp.batch_max_bytes is sent alone, for the
// receiver to accept or refuse. A request's size is measured, not encoded,
// while it is filled: a request's JSON is the JSON of its parts, so its size
// is the exact sum of theirs.

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
	case m.ExponentialHistogram != nil:
		return len(m.ExponentialHistogram.DataPoints)
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
	case m.ExponentialHistogram != nil:
		return m.ExponentialHistogram.DataPoints[j]
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
	case m.ExponentialHistogram != nil:
		h := *m.ExponentialHistogram
		h.DataPoints = h.DataPoints[from:to:to]
		m.ExponentialHistogram = &h
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
// nil omits it.
//
// Some families have a series with values their type does not allow
// (typedValues): a counter that is NaN or negative, a histogram whose bucket
// counts fall or whose +Inf bucket and _count differ, a summary with a
// quantile of 1.5. Such a family cannot be a monotonic sum, histogram or
// summary in OTLP either: a bucket's own count would be negative, a
// histogram point has one count, and OTLP quantiles lie within 0 to 1 and
// are not negative as OpenMetrics' are. It is exported as gauges under its
// samples' names, with the series and values the exposition formats write.
// That is OTLP's form of OpenMetrics' unknown, which such a family is
// written as there (planOpenMetrics). Every point is then valid, none left out.
func otlpMetrics(set model.MetricSet, now string, start func(m model.Metric, at string) string) []otlpMetric {
	out, _ := otlpMetricsOf(otlpResourceSet{Set: set}, now, start)
	return out
}

// otlpPoints performs the conversion otlpMetrics describes, on the series as
// the set holds them. own is where the exporter's own series begin in the
// set, after the series that waited in the queue (exportOTLP). keep, when
// set, is called for every point before it is made, with the point's series
// (by its place in the set), the name the exposition writes the point under,
// and the name, kind and attributes it would be exported with. A point keep
// refuses is left out, and its series' start time is not requested.
//
// shared reports that two points of the result may be the same to a
// receiver (one metric's, with the same attributes), or that a name is two
// metrics of different kinds in it. otlpMetricsOf then examines it. shared
// is decided without looking at any point's attributes, only at where a
// metric's points come from. Series of one name and type that follow each
// other in the set form a run. Series of a run differ in their labels, as
// the keys they waited under and the exporter's own series do. So a metric
// whose points all come from one run, each exported under its series' own
// name with its labels as attributes, has no two alike. shared is set where
// a metric gets a point from a second run, where a name is exported as a
// second kind, and where a histogram or summary is exported as gauges under
// its samples' names, with le or quantile among the attributes.
func otlpPoints(set model.MetricSet, own int, otel bool, now string, start func(m model.Metric, at string) string, keep func(series int, source, name string, shape otlpShape, attributes []otlpAttribute) bool) (out []otlpMetric, shared bool) {
	// Where the metric of a name is in out, and the run that made it.
	type place struct{ at, run int }
	index := map[string]place{}
	run := 0
	// metric is the metric of a name and a shape that a point is added to.
	// A name first seen, or reused with another kind or unit, is a new
	// metric, so points of different shapes are never mixed in one.
	metric := func(name, help string, shape otlpShape) *otlpMetric {
		p, seen := index[name]
		if seen && otlpKind(out[p.at]) == shape.kind && out[p.at].Unit == shape.unit {
			shared = shared || p.run != run
			return &out[p.at]
		}
		shared = shared || seen
		index[name] = place{at: len(out), run: run}
		out = append(out, newOTLPMetric(name, help, shape))
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
			at = otlpPointTime(*m.Timestamp)
		}
		attributes := otlpAttributesForLabels(m.Labels)
		// gaugeOf adds a gauge's point, which the exposition writes under
		// source.
		gaugeOf := func(source, name, unit string, attributes []otlpAttribute, value float64) {
			shape := otlpShape{kind: otlpKindGauge, unit: unit}
			if keep != nil && !keep(i, source, name, shape, attributes) {
				return
			}
			v := otlpDouble(value)
			g := metric(name, m.Help, shape).Gauge
			g.DataPoints = append(g.DataPoints, otlpNumberDataPoint{Attributes: attributes, TimeUnixNano: at, AsDouble: &v})
		}
		// gauge adds the point of a sample of a family exported as gauges,
		// which keeps the name the exposition writes it under, and has no
		// unit, whatever otlp.metric_names says.
		gauge := func(name string, attributes []otlpAttribute, value float64) {
			gaugeOf(name, name, "", attributes, value)
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
		name, unit := otlpNameOf(m.Name, kind, otel)
		shape := otlpShape{kind: kind, unit: unit}
		if keep != nil && kind != otlpKindGauge && !keep(i, m.Name, name, shape, attributes) {
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
		case otlpKindExponentialHistogram:
			point := otlpExponentialHistogramPoint(m, attributes, at)
			point.StartTimeUnixNano = startAt
			h := metric(name, m.Help, shape).ExponentialHistogram
			h.DataPoints = append(h.DataPoints, point)
		case otlpKindHistogram:
			point := otlpHistogramPoint(m, attributes, at)
			point.StartTimeUnixNano = startAt
			h := metric(name, m.Help, shape).Histogram
			h.DataPoints = append(h.DataPoints, point)
		case otlpKindSummary:
			point := otlpSummaryPoint(m, attributes, at)
			point.StartTimeUnixNano = startAt
			s := metric(name, m.Help, shape).Summary
			s.DataPoints = append(s.DataPoints, point)
		case otlpKindSum:
			v := otlpDouble(m.Value)
			s := metric(name, m.Help, shape).Sum
			s.DataPoints = append(s.DataPoints, otlpNumberDataPoint{Attributes: attributes, StartTimeUnixNano: startAt, TimeUnixNano: at, AsDouble: &v})
		default:
			gaugeOf(m.Name, name, unit, attributes, m.Value)
		}
	}
	return out, shared
}

// otlpNameClash is a name that the writers of one resource export, in one
// export, as metrics of different kinds, or (under otlp.metric_names: otel)
// as the name of metrics with different Prometheus names. The kind the
// latest writer wrote is exported. Points of the other kinds, listed in
// leftOut, are not. Within that kind, the Prometheus name the latest writer
// wrote (source) is exported. Points of the other names, sourcePoints of
// the names in leftOutSources, are not.
type otlpNameClash struct {
	name    string
	kept    otlpShape
	leftOut []otlpShape
	points  int

	source         string
	leftOutSources []string
	sourcePoints   int
}

// otlpMetricsOf is otlpMetrics of what one resource has in an export, as if
// written by one writer: the series that waited in the queue, each with its
// place in it, and after them the exporter's own, for its own resource.
//
// To a receiver, a point belongs to a stream: its resource, its metric (name
// and kind) and its attributes. Each stream has one writer. Two points of
// one stream in a request are one too many: the receiver keeps either, or
// refuses the request. Two metrics of one name and different kinds are a
// conflict it settles as it likes. The queue holds a series once, by its
// name, type and labels (otlpMetricKey), which is not what it is exported
// as. A gauge and an untyped series of one name and labels are one gauge's
// point twice. So are the h_bucket of a histogram h exported as gauges and a
// gauge h_bucket with that le, and a probe series named and labelled like
// one of the exporter's own. A name one probe makes a gauge and another a
// counter is two metrics of one name. One set a probe answers with has none
// of these (MetricSet.Validate); two sets under one resource can. What a
// series is exported as is not known when it is queued: a family is
// exported as gauges when any of its series in the export has values its
// type does not allow (untypedFamilies), and the exporter's own series never
// wait. So it is settled here, where the points are made, by the queue's
// rule: the later replaces the earlier.
//
//   - Of the kinds a name is exported as, the kind of the point written last
//     is exported, and no point of another kind. These are reported, to be
//     logged (logOTLPNameClashes).
//   - Of the Prometheus names a name is exported for, the one of the point
//     written last is exported, and no point of another, among the points of
//     the exported kind. These are reported too. Under otlp.metric_names:
//     otel there can be more than one, where x_seconds and x_seconds_total,
//     or requests and requests_total, are both counters. Two series are one
//     stream only where the exposition writes them under one name. Two that
//     it does not are two series to Prometheus. One metric of the export
//     would merge them where their labels are the same, and a receiver could
//     not tell them apart where they are not.
//   - Of the points of one metric with the same attributes, the one written
//     last is exported, silently, as a later scrape's point replaces a
//     series' point in the queue.
//
// Written last means queued last. The exporter's own series are written
// after every queued one, at the export. Of two written at once, the later
// in the set counts as written last.
//
// Nearly no export has any of this, and it is then built once, as before
// (otlpPoints). Only where that reports that two points may be one are the
// points walked again to find those to leave out, and, if there are any, a
// third time without them. A start time is then requested twice for the
// same point, which gets the same one. A cumulative point left out has
// still counted as a point of its series for the start times, a count the
// series had (otlpStartTimes).
func otlpMetricsOf(resource otlpResourceSet, now string, start func(m model.Metric, at string) string) ([]otlpMetric, []otlpNameClash) {
	return otlpMetricsWith(resource, now, start, nil)
}

// otlpPointSources records the series the points of a resource's metrics
// come from: of[m][j] is the place in the resource's set of the series of
// the j'th point of the m'th metric. index and kinds are what otlpPoints
// keeps to find the metric, by name and kind, that a point is added to.
type otlpPointSources struct {
	index  map[string]int
	shapes []otlpShape
	of     [][]int
	// leftOut are the points the export leaves out for another written
	// later, in the order they would have been made, which the debug page
	// lists (debugotlp.go).
	leftOut []otlpLeftOutPoint
}

// otlpLeftOutPoint is a point an export leaves out (otlpMetricsOf). It is
// of the series'th series of the resource's set, which the exposition writes
// under source, and is exported as name, with shape and attributes. why
// says which rule left it out.
type otlpLeftOutPoint struct {
	series       int
	source, name string
	shape        otlpShape
	attributes   []otlpAttribute
	why          otlpLeftOutReason
}

// otlpLeftOutReason is which of otlpMetricsOf's rules left a point out.
type otlpLeftOutReason int

const (
	// otlpLeftOutKind: its name is exported as another kind or unit, that
	// of the point of the name written last.
	otlpLeftOutKind otlpLeftOutReason = iota
	// otlpLeftOutName: its name is exported for another Prometheus name,
	// under otlp.metric_names: otel.
	otlpLeftOutName
	// otlpLeftOutReplaced: a point of the same metric with the same
	// attributes was written later.
	otlpLeftOutReplaced
)

// add says that the next point made is of the series'th series, for the
// metric of name and shape, found as otlpPoints finds it.
func (o *otlpPointSources) add(series int, name string, shape otlpShape) {
	at, seen := o.index[name]
	if !seen || o.shapes[at] != shape {
		at = len(o.of)
		o.index[name] = at
		o.shapes = append(o.shapes, shape)
		o.of = append(o.of, nil)
	}
	o.of[at] = append(o.of[at], series)
}

// otlpMetricsWith is otlpMetricsOf, which, given sources, says in them what
// series each point it makes is of. A point is made after keep is asked of
// it, and only then, so sources are told of a point where keep says yes.
func otlpMetricsWith(resource otlpResourceSet, now string, start func(m model.Metric, at string) string, sources *otlpPointSources) ([]otlpMetric, []otlpNameClash) {
	set, own, otel := resource.Set, len(resource.seqs), resource.otelNames
	var keep func(series int, source, name string, shape otlpShape, attributes []otlpAttribute) bool
	if sources != nil {
		keep = func(series int, _, name string, shape otlpShape, _ []otlpAttribute) bool {
			sources.add(series, name, shape)
			return true
		}
	}
	out, shared := otlpPoints(set, own, otel, now, start, keep)
	if !shared {
		return out, nil
	}
	// Every point the export would have, in the order they are made in.
	type point struct {
		name, source, identity string
		shape                  otlpShape
		written                int64
		series                 int
		attributes             []otlpAttribute
	}
	var points []point
	otlpPoints(set, own, otel, now, nil, func(series int, source, name string, shape otlpShape, attributes []otlpAttribute) bool {
		written := int64(math.MaxInt64)
		if series < own {
			written = resource.seqs[series]
		}
		points = append(points, point{name: name, source: source, identity: otlpPointIdentity(name, attributes), shape: shape, written: written, series: series, attributes: attributes})
		return false
	})
	later := func(a, b int) bool {
		return points[a].written > points[b].written || points[a].written == points[b].written && a > b
	}
	// The point of each name written last, whose kind the name is exported
	// as, for the Prometheus name it is of.
	last := map[string]int{}
	for n := range points {
		if of, seen := last[points[n].name]; !seen || later(n, of) {
			last[points[n].name] = n
		}
	}
	leftOut, some := make([]bool, len(points)), false
	why := make([]otlpLeftOutReason, len(points))
	latest := map[string]int{}
	var clashes map[string]*otlpNameClash
	clashOf := func(p *point) *otlpNameClash {
		if clashes == nil {
			clashes = map[string]*otlpNameClash{}
		}
		clash := clashes[p.name]
		if clash == nil {
			kept := &points[last[p.name]]
			clash = &otlpNameClash{name: p.name, kept: kept.shape, source: kept.source}
			clashes[p.name] = clash
		}
		return clash
	}
	for n := range points {
		p := &points[n]
		kept := &points[last[p.name]]
		if p.shape != kept.shape {
			leftOut[n], some, why[n] = true, true, otlpLeftOutKind
			clash := clashOf(p)
			if !slices.Contains(clash.leftOut, p.shape) {
				clash.leftOut = append(clash.leftOut, p.shape)
			}
			clash.points++
			continue
		}
		if p.source != kept.source {
			leftOut[n], some, why[n] = true, true, otlpLeftOutName
			clash := clashOf(p)
			if !slices.Contains(clash.leftOutSources, p.source) {
				clash.leftOutSources = append(clash.leftOutSources, p.source)
			}
			clash.sourcePoints++
			continue
		}
		earlier, seen := latest[p.identity]
		switch {
		case !seen:
			latest[p.identity] = n
		case later(n, earlier):
			leftOut[earlier], some, why[earlier] = true, true, otlpLeftOutReplaced
			latest[p.identity] = n
		default:
			leftOut[n], some, why[n] = true, true, otlpLeftOutReplaced
		}
	}
	if !some {
		return out, nil
	}
	n := 0
	if sources != nil {
		*sources = otlpPointSources{index: map[string]int{}}
		for k := range points {
			if leftOut[k] {
				p := &points[k]
				sources.leftOut = append(sources.leftOut, otlpLeftOutPoint{series: p.series, source: p.source, name: p.name, shape: p.shape, attributes: p.attributes, why: why[k]})
			}
		}
	}
	out, _ = otlpPoints(set, own, otel, now, start, func(series int, _, name string, shape otlpShape, _ []otlpAttribute) bool {
		n++
		if leftOut[n-1] {
			return false
		}
		if sources != nil {
			sources.add(series, name, shape)
		}
		return true
	})
	reported := make([]otlpNameClash, 0, len(clashes))
	for _, name := range model.SortedKeys(clashes) {
		slices.SortFunc(clashes[name].leftOut, func(a, b otlpShape) int {
			return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.unit, b.unit))
		})
		slices.Sort(clashes[name].leftOutSources)
		reported = append(reported, *clashes[name])
	}
	return out, reported
}

// otlpPointIdentity identifies a point of a resource among all others in an
// export that have the kind its name is exported as. It is the metric's
// name and each attribute's name and value, each preceded by its length
// (appendKeyPart), so nothing is read from their content.
func otlpPointIdentity(name string, attributes []otlpAttribute) string {
	b := appendKeyPart(nil, name)
	for _, a := range attributes {
		b = appendKeyPart(appendKeyPart(b, a.Key), a.Value.StringValue)
	}
	return string(b)
}

// errOTLPNameClash is what is logged of a name exported as two kinds.
var errOTLPNameClash = errors.New("two writers of one OTLP resource - probes, static targets, or the exporter with its own metrics - export this metric name as different kinds, and a name is one metric of one kind there; rename one of the metrics, or give a static target a resource of its own with its otlp.service_name or otlp.resource_attributes")

// errOTLPNamesMerged is what is logged of a name exported for two
// Prometheus names.
var errOTLPNamesMerged = errors.New("otlp.metric_names: otel exports these Prometheus metric names of one OTLP resource under one name, and a name is one metric there, whose points of the two would be one stream where their labels are the same; rename one of the metrics, or export with otlp.metric_names: prometheus")

// What is logged of a name exported as two kinds, and of one exported for
// two Prometheus names, which the debug page says of the points left out.
const (
	otlpNameClashMessage   = "OTLP metric name written as two kinds under one resource; the data points of the kind written earlier are left out of the export"
	otlpNamesMergedMessage = "OTLP metric name written for two Prometheus metric names under one resource; the data points of the name written earlier are left out of the export"
)

// logOTLPNameClashes logs each name of a resource that an export had as two
// kinds, or for two Prometheus names, once and then as a repeat
// (failureLog), naming whichever was written last at each export. The line
// shows names as an error does (model.ShownName). The clash is remembered
// under the whole name, with kind and Prometheus-name clashes kept apart.
func (s *Server) logOTLPNameClashes(identity otlpResourceIdentity, resource string, clashes []otlpNameClash) {
	for _, clash := range clashes {
		if len(clash.leftOutSources) > 0 {
			s.failures.failed(s.logger, slog.LevelWarn, otlpNameMergeKey(resource, clash.name),
				otlpNamesMergedMessage, "otlp", errOTLPNamesMerged, append(otlpNamesMergedAttrs(clash), "service_name", identity.ServiceName)...)
		}
		if len(clash.leftOut) == 0 {
			continue
		}
		s.failures.failed(s.logger, slog.LevelWarn, otlpNameClashKey(resource, clash.name),
			otlpNameClashMessage, "otlp", errOTLPNameClash, append(otlpKindsClashAttrs(clash), "service_name", identity.ServiceName)...)
	}
}

// otlpNamesMergedAttrs are the attributes the line of a name exported for
// two Prometheus names has, but its resource's service name.
func otlpNamesMergedAttrs(clash otlpNameClash) []any {
	names := make([]string, 0, len(clash.leftOutSources))
	for _, source := range clash.leftOutSources {
		names = append(names, model.ShownName(source))
	}
	return []any{"metric", model.ShownName(clash.name), "prometheus_name", model.ShownName(clash.source), "left_out_prometheus_name", strings.Join(names, ", "), "left_out_points", clash.sourcePoints}
}

// otlpKindsClashAttrs are the attributes the line of a name exported as
// two kinds has, but its resource's service name.
func otlpKindsClashAttrs(clash otlpNameClash) []any {
	kinds := make([]string, 0, len(clash.leftOut))
	for _, shape := range clash.leftOut {
		kinds = append(kinds, shape.String())
	}
	return []any{"metric", model.ShownName(clash.name), "kind", clash.kept.String(), "left_out_kind", strings.Join(kinds, ", "), "left_out_points", clash.points}
}

// otlpNameClashLines are the lines an export logs of a clash, as the debug
// page shows them: the message, the attributes as key=value and the error.
func otlpNameClashLines(clash otlpNameClash) []string {
	line := func(message string, attrs []any, err error) string {
		var b strings.Builder
		b.WriteString(message)
		for k := 0; k+1 < len(attrs); k += 2 {
			fmt.Fprintf(&b, " %v=%v", attrs[k], attrs[k+1])
		}
		return b.String() + ": " + err.Error()
	}
	var lines []string
	if len(clash.leftOutSources) > 0 {
		lines = append(lines, line(otlpNamesMergedMessage, otlpNamesMergedAttrs(clash), errOTLPNamesMerged))
	}
	if len(clash.leftOut) > 0 {
		lines = append(lines, line(otlpNameClashMessage, otlpKindsClashAttrs(clash), errOTLPNameClash))
	}
	return lines
}

// untypedFamilies returns the names of a set's families that have a series
// whose values its type does not allow (typedValues). These are exported as
// gauges. It is nil when there are none, as nearly always. The decision is
// per family, as in OpenMetrics, so that a name is one kind of metric in an
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
	otlpKindExponentialHistogram
)

// otlpKindNames are the kinds as a log line names them.
var otlpKindNames = [...]string{otlpKindGauge: "gauge", otlpKindSum: "sum", otlpKindHistogram: "histogram", otlpKindSummary: "summary", otlpKindExponentialHistogram: "exponential histogram"}

// otlpKindOf is the OTLP kind a metric is exported as. A histogram or summary
// type without its data is exported as a gauge of its value.
func otlpKindOf(m model.Metric) int {
	switch {
	case m.Type == model.HistogramMetricType && m.Histogram != nil && m.Histogram.Exponential != nil:
		return otlpKindExponentialHistogram
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
	case m.ExponentialHistogram != nil:
		return otlpKindExponentialHistogram
	case m.Histogram != nil:
		return otlpKindHistogram
	case m.Summary != nil:
		return otlpKindSummary
	case m.Sum != nil:
		return otlpKindSum
	}
	return otlpKindGauge
}

func newOTLPMetric(name, help string, shape otlpShape) otlpMetric {
	out := otlpMetric{Name: name, Description: help, Unit: shape.unit}
	switch shape.kind {
	case otlpKindExponentialHistogram:
		out.ExponentialHistogram = &otlpExponentialHistogram{AggregationTemporality: otlpCumulative}
	case otlpKindHistogram:
		out.Histogram = &otlpHistogram{AggregationTemporality: otlpCumulative}
	case otlpKindSummary:
		out.Summary = &otlpSummary{}
	case otlpKindSum:
		out.Sum = &otlpSum{AggregationTemporality: otlpCumulative, IsMonotonic: true}
	default:
		out.Gauge = &otlpGauge{}
	}
	return out
}

// otlpHistogramPoint turns Prometheus's cumulative buckets into OTLP's
// per-bucket counts, in ascending order of bounds, whatever order the
// source had them in. The +Inf bucket, if present, is not a bound: the
// count above the highest finite bound is the count minus the last finite
// bucket's cumulative count. The histogram's values are allowed by its type
// (typedValues): its bounds are numbers, its +Inf bucket's count is its
// _count where it has both, and its cumulative counts never fall, up to that
// count. So each bucket's own count is its cumulative count minus the one
// before, and they add up to the count, as OTLP requires.
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
	point.Exemplars = otlpExemplars(h)
	return point
}

// otlpExponentialHistogramPoint is a histogram's exponential buckets as an
// OTLP point, at the scale they were recorded at: OTLP takes every scale from
// -10 to 20, and the recorder keeps them within 160 buckets.
func otlpExponentialHistogramPoint(m model.Metric, attributes []otlpAttribute, at string) otlpExponentialHistogramDataPoint {
	h, x := m.Histogram, m.Histogram.Exponential
	sum := otlpDouble(h.Sum)
	point := otlpExponentialHistogramDataPoint{
		Attributes:   attributes,
		TimeUnixNano: at,
		Count:        strconv.FormatUint(h.Count, 10),
		Sum:          &sum,
		Scale:        x.Scale,
		ZeroCount:    strconv.FormatUint(x.ZeroCount, 10),
		Positive:     otlpExponentialSet{Offset: x.Offset, BucketCounts: make([]string, 0, len(x.Counts))},
	}
	for _, count := range x.Counts {
		point.Positive.BucketCounts = append(point.Positive.BucketCounts, strconv.FormatUint(count, 10))
	}
	if h.Count > 0 {
		low, high := otlpDouble(x.Min), otlpDouble(x.Max)
		point.Min, point.Max = &low, &high
	}
	point.Exemplars = otlpExemplars(h)
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
	retries             int
	// dropped counts the data points given up on, by why
	// (otlpDropReason); http_exporter_otlp_points_dropped_total is their
	// sum.
	dropped      [otlpDropReasons]int
	lastDuration time.Duration
	lastSuccess  time.Time
	// consecutiveFailures counts the exports to endpoint since the last
	// success.
	consecutiveFailures int
	endpoint            string
	// history is what the status page shows of the exports to endpoint
	// (otlpstatuspage.go). Like consecutiveFailures, it starts again when a
	// reload points OTLP somewhere else. It is a few values: one error text
	// of at most model.MaxFailureBytes and at most
	// model.MaxOTLPUnreadyAfterFailures times, whatever the exports.
	history otlpHistory
	// now is time.Now unless a test sets a clock of its own, to tell the
	// failures of a run apart by more than the page's second.
	now func() time.Time
}

// otlpDropReason is why data points were given up on.
type otlpDropReason int

const (
	// otlpDropRefused: the endpoint refused a request with an answer that
	// is not retried, a redirect included.
	otlpDropRefused otlpDropReason = iota
	// otlpDropRejected: the endpoint accepted a request but rejected some
	// of its points in its partialSuccess.
	otlpDropRejected
	// otlpDropPendingCap: the oldest waiting past otlp.max_pending_points.
	otlpDropPendingCap
	// otlpDropShutdown: those of the last export before exiting, which
	// failed.
	otlpDropShutdown
	otlpDropReasons
)

// otlpExport is how one export went, as the status page shows it: when it
// began, how long it took with its retries, how many requests of it were
// sent and how many data points they carried, what the endpoint rejected of
// them in a partialSuccess, and how many retries it made.
type otlpExport struct {
	started  time.Time
	duration time.Duration
	requests int
	points   int
	rejected int64
	retries  int
}

// otlpHistory is what the status page keeps of the exports to one endpoint.
type otlpHistory struct {
	// last is the last export, ok whether it got through; zero before the
	// first.
	last otlpExport
	ok   bool
	// lastSuccess is when the last export that got through ended.
	lastSuccess time.Time
	// errorText is the shown error of the last failed export, which
	// failedAt began, kept through the successes after it.
	errorText string
	failedAt  time.Time
	// failureEnds are the end times of the failed exports in the current run
	// of failures, oldest first, up to model.MaxOTLPUnreadyAfterFailures of
	// them. It is empty after a success. Under otlp.unready_after_failures N,
	// the exporter is unready from the end of the Nth, whatever N a reload
	// sets during the run. An N below the run's length names a kept failure;
	// an N above it leaves the exporter ready. N is at most the bound, so
	// failures past it can never be the one the exporter is unready from, and
	// are not kept: at most a thousand times, whatever the run's length.
	failureEnds []time.Time
}

// unreadySince is when the exporter became unready under
// otlp.unready_after_failures unreadyAfter, by the failures of the current
// run; zero while it is ready by them.
func (h *otlpHistory) unreadySince(unreadyAfter int) time.Time {
	if unreadyAfter <= 0 || unreadyAfter > len(h.failureEnds) {
		return time.Time{}
	}
	return h.failureEnds[unreadyAfter-1]
}

// record records an export to endpoint, which failed with err unless err is
// nil.
func (o *otlpStatus) record(endpoint string, export otlpExport, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if endpoint != o.endpoint {
		o.endpoint, o.consecutiveFailures, o.history = endpoint, 0, otlpHistory{}
	}
	now := time.Now()
	if o.now != nil {
		now = o.now()
	}
	o.lastDuration = export.duration
	o.retries += export.retries
	h := &o.history
	h.last, h.ok = export, err == nil
	if err == nil {
		o.successes++
		o.lastSuccess, h.lastSuccess = now, now
		o.consecutiveFailures = 0
		h.failureEnds = nil
		return
	}
	o.failures++
	o.consecutiveFailures++
	// Only the text is kept, shown as the page shows it, so no error value
	// outlives the export.
	h.errorText = shownOTLPError(err)
	h.failedAt = export.started
	if len(h.failureEnds) < model.MaxOTLPUnreadyAfterFailures {
		h.failureEnds = append(h.failureEnds, now)
	}
}

// drop counts data points given up on, and why.
func (o *otlpStatus) drop(points int, why otlpDropReason) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropped[why] += points
}

// droppedLocked is every data point given up on, under mu.
func (o *otlpStatus) droppedLocked() int {
	n := 0
	for _, points := range o.dropped {
		n += points
	}
	return n
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
		metric("http_exporter_otlp_points_dropped_total", model.CounterMetricType, float64(o.droppedLocked()), nil),
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
// data point carries the time its value came from the target. An answer
// from the cache, fresh or stale (cache.go), holds values that came from the
// target before the answer was given. Exported with the answer's time, an
// old value would look newly measured each time it was served again.
//
// The first data series of the result came from the target at at. The rest
// are the exporter's own account of the result as it is queued (a static
// target's health metrics). So are the freshness series of a collector with
// cache.stale_if_error, wherever they stand, which say how old the result is
// now. Those are as of now, as every series is when at is zero.
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

// Probes of every target and collector are queued under the one
// exporter-wide resource, where a series is identified by its name, type and
// labels alone. Two probes that answer the same series (two targets behind
// one collector, two collectors naming a metric alike) then give one series,
// and the later probe's point replaces the earlier one before the export.
// With otlp.probe_attributes, each probe's points carry collector and target
// attributes, so they stay apart. A series' own label with either name is
// kept. Some things two probes write are one stream to a receiver without
// being one series here: a gauge and an untyped series of one name, or a
// name that is a gauge in one and a counter in the other. These are settled
// where the export is made (otlpMetricsOf).

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
	s.queueOTLPResource(otlpProbeSet(cfg, set, c, target), defaultResourceIdentity(cfg), at)
}

// otlpProbeSet is a probe's answer as it is exported. When
// otlp.probe_attributes says so, its series get collector and target
// attributes, and a series' own label with either name is kept. The debug
// page shows a run's points made from it (debugotlp.go).
func otlpProbeSet(cfg model.OTLPConfig, set model.MetricSet, c *model.Collector, target string) model.MetricSet {
	if !cfg.ProbeAttributes {
		return set
	}
	attributes := map[string]string{probeCollectorAttribute: c.Name}
	if target != "" {
		// The target is the probe's query's, and may be a part of the
		// request's line, which a point waiting would keep whole: the
		// probe's points share a copy of the target alone.
		attributes[probeTargetAttribute] = strings.Clone(target)
	}
	return withTargetLabels(set, attributes)
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
		// A point is exported with the time it was scraped, not the time the
		// export is sent. The queue keeps that time through the wait for the
		// next export and any retries. For a result answered from the cache,
		// it is when the cached trip was made (scrapeTime).
		switch {
		case point.Timestamp != nil:
		case at.covers(i, metric):
			point.Timestamp = &fetched
		default:
			point.Timestamp = &queued
		}
		s.putPendingLocked(batch, otlpPendingKey(&point), pendingMetric{metric: point, seq: s.otlpSeq})
	}
	s.capPendingLocked(cfg.MaxPendingPoints)
}

// otlpPointTime returns a point's time as an export writes it, in
// nanoseconds. ms is the milliseconds since the epoch that the queue keeps
// for the point (queueOTLPResource), the precision of a Prometheus sample's
// timestamp. So an export's times are whole milliseconds.
func otlpPointTime(ms int64) string {
	return strconv.FormatInt(ms*int64(time.Millisecond), 10)
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
	s.otlp.drop(drop, otlpDropPendingCap)
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
		for _, metricKey := range otlpPendingOrder(batch.metrics) {
			set.Metrics = append(set.Metrics, batch.metrics[metricKey].metric)
			seqs = append(seqs, batch.metrics[metricKey].seq)
		}
		out = append(out, otlpResourceSet{Identity: batch.identity, Set: set, seqs: seqs})
	}
	s.otlpPending = make(map[string]*otlpBatch)
	s.otlpPoints = 0
	return out
}

// otlpQueueOrder returns the series of set as the queue would hold them if
// they were queued one after another under one resource and drained: each
// series once, by its place in set, with its last point in set, in the
// order drainOTLP gives. The debug page shows a run's points in this order
// (debugotlp.go), which the export's checks of a resource's points depend on.
func otlpQueueOrder(set model.MetricSet) []int {
	pending := make(map[string]pendingMetric, len(set.Metrics))
	place := make(map[string]int, len(set.Metrics))
	for i := range set.Metrics {
		key := otlpPendingKey(&set.Metrics[i])
		pending[key] = pendingMetric{metric: set.Metrics[i], seq: int64(i)}
		place[key] = i
	}
	keys := otlpPendingOrder(pending)
	out := make([]int, len(keys))
	for k, key := range keys {
		out[k] = place[key]
	}
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
	s.otlpEvery(ctx, func(cfg model.OTLPConfig) time.Duration {
		if interval := time.Duration(cfg.Interval); interval > 0 {
			return interval
		}
		return 30 * time.Second
	}, func(cfg model.OTLPConfig, interval time.Duration) {
		if !cfg.Enabled || cfg.Endpoint == "" {
			_ = s.drainOTLP()
			return
		}
		// Static targets are scraped on their own intervals
		// (StaticScrapeLoop). An export delivers what the probes and the
		// targets with export_via_otlp queued since the last one. It may
		// retry for up to an interval, so it never runs into the next one.
		s.exportOTLP(ctx, interval)
	})
}

// otlpEvery calls export with the otlp block in force and its interval (as
// intervalOf reads it), one interval after the last call ended, until ctx
// ends. The metrics, log records and spans export loops use it. The interval
// is that of the configuration in force now, not when the wait began. A
// reload, or the first bundle of an exporter in allocator mode, that puts
// another configuration in force wakes the wait. The wait then runs to one
// interval of the new configuration after the last export, and ends at once
// if that is already past. An exporter in allocator mode starts with no
// configuration, whose interval is the default. Otherwise it would make its
// first export that long after its bundle came.
func (s *Server) otlpEvery(ctx context.Context, intervalOf func(model.OTLPConfig) time.Duration, export func(model.OTLPConfig, time.Duration)) {
	last := time.Now()
	for {
		// Asked for before the configuration is read: one put in force
		// after the read wakes the wait.
		installed := s.installs.next()
		timer := time.NewTimer(time.Until(last.Add(intervalOf(s.manager.Get().OTLP))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-installed:
			timer.Stop()
			continue
		case <-timer.C:
		}
		cfg := s.manager.Get().OTLP
		export(cfg, intervalOf(cfg))
		last = time.Now()
	}
}

// installSignal tells those waiting that a configuration was put in force:
// the channel next returns is closed at the next install.
type installSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// next is a channel closed when a configuration is next put in force.
func (i *installSignal) next() <-chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ch == nil {
		i.ch = make(chan struct{})
	}
	return i.ch
}

// installed wakes those waiting on next.
func (i *installSignal) installed() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ch != nil {
		close(i.ch)
		i.ch = nil
	}
}

// FlushOTLP makes the last export at shutdown: the metrics probes queued since
// the last export, and a final self-metric snapshot, within otlp.timeout.
// Static targets are not scraped again. With otlp.traces, the spans waiting
// follow, and with otlp.logs, the log records waiting, in what is left of
// otlp.timeout (flushOTLPTraces, flushOTLPLogs).
func (s *Server) FlushOTLP() {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" {
		return
	}
	s.logger.Info("sending the last OTLP export before exiting")
	start := time.Now()
	s.exportOTLPOnce(context.Background(), time.Duration(cfg.Timeout), true)
	// The trace export's last and the log export's are within the same
	// otlp.timeout, after the metrics', the log's last, so the lines the
	// others logged go with it.
	s.flushOTLPTraces(cfg, start.Add(time.Duration(cfg.Timeout)))
	s.flushOTLPLogs(cfg, start.Add(time.Duration(cfg.Timeout)))
}

// exportOTLP sends everything pending, with a self-metric snapshot, retrying
// within budget. It sends as many requests as otlp.batch_max_size and
// otlp.batch_max_bytes make of it, one after another. Metrics not delivered
// for a reason worth retrying (a network error, 429, 502, 503, 504, the
// budget running out, or shutdown cutting the export short) are queued again
// for the next export, with those of every request not yet sent, unless a
// newer value of the same series has been queued since. Self-metrics are not
// queued again, since the next export takes a new snapshot, and neither are
// the metrics of requests already delivered. Metrics of a request the
// endpoint refused outright are dropped and counted, since sending them
// again would be refused again. The export goes on with the next request.
func (s *Server) exportOTLP(ctx context.Context, budget time.Duration) {
	s.exportOTLPOnce(ctx, budget, false)
}

// exportOTLPOnce is exportOTLP, and also the last export at shutdown when
// last says so (FlushOTLP). There is no next export then, so the metrics a
// failure would queue again are dropped instead, counted in
// http_exporter_otlp_points_dropped_total and logged as dropped.
//
// It counts as one export in the self-metrics, however many requests it is
// sent in. Its duration is the whole export's, its retries are those of
// every request, and it is a success when the endpoint accepted every request.
func (s *Server) exportOTLPOnce(ctx context.Context, budget time.Duration, last bool) {
	cfg := s.manager.Get().OTLP
	pending := s.drainOTLP()
	if !cfg.Enabled || cfg.Endpoint == "" {
		return
	}
	// The copy keeps the self-metrics out of pending, which may be queued again.
	self := s.selfMetricSet()
	if !cfg.ExponentialHistograms {
		withoutExponential(&self)
	}
	resources := appendToResource(append([]otlpResourceSet(nil), pending...), defaultResourceIdentity(cfg), self)
	start := time.Now()
	deadline := start.Add(budget)
	requests, err := s.otlpRequestsOf(cfg, resources)
	retries, ok := 0, true
	export := otlpExport{started: start}
	// refusedErr is why the endpoint last refused a request, which fails
	// the export without stopping it, for the status page.
	var refusedErr error
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
		export.requests++
		export.points += requests.points[k]
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
				export.rejected += partial.rejected
				s.otlp.drop(int(min(partial.rejected, int64(math.MaxInt32))), otlpDropRejected)
				s.logger.Warn("OTLP endpoint accepted an export but rejected some of its data points; they are dropped", "rejected_points", partial.rejected, "error_message", partial.message, "retries", tries)
			case partial.message != "":
				s.logger.Warn("OTLP endpoint accepted an export with a warning", "error_message", partial.message)
			}
			continue
		}
		var refused *otlpRefusedError
		if errors.As(err, &refused) {
			ok, refusedErr, err = false, err, nil
			points := requests.of(pending, k)
			s.otlp.drop(points, otlpDropRefused)
			attrs := []any{"status", refused.status, "dropped_points", points, "retries", tries}
			if len(requests.bodies) > 1 {
				attrs = append(attrs, "request", k+1, "requests", len(requests.bodies))
			}
			if refused.body != "" {
				attrs = append(attrs, "response_body", refused.body)
			}
			if refused.redirect {
				attrs = append(attrs, "location_host", refused.locationText())
				s.logger.Warn("OTLP endpoint answered a redirect, which is not followed; its data points are dropped; set otlp.endpoint to the URL the export should go to", attrs...)
				continue
			}
			s.logger.Warn("OTLP endpoint refused an export; its data points are dropped", attrs...)
			continue
		}
		// Worth retrying, and out of retries: this request and those after
		// it are what is left to deliver.
		pending = requests.from(pending, k)
	}
	export.duration, export.retries = time.Since(start), retries
	failure := err
	if failure == nil && !ok {
		failure = refusedErr
	}
	s.otlp.record(cfg.Endpoint, export, failure)
	if err == nil {
		return
	}
	if last {
		points := countPoints(pending)
		s.otlp.drop(points, otlpDropShutdown)
		s.logger.Warn("the last OTLP export before exiting failed; its data points are dropped", "error", err, "dropped_points", points, "retries", retries)
		return
	}
	s.requeueOTLP(pending)
	s.logger.Warn("OTLP export failed; its data points are kept for the next export", "error", err, "retries", retries)
}

// requeueOTLP queues undelivered metrics again, except those whose series
// has had a newer value queued since they were drained. Each keeps the
// queue position it was drained with, older than anything queued since, so
// otlp.max_pending_points drops the oldest points first, however many
// exports in a row have failed.
func (s *Server) requeueOTLP(resources []otlpResourceSet) {
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	for _, resource := range resources {
		batch := s.pendingBatchLocked(resource.Identity)
		for i, metric := range resource.Set.Metrics {
			key := otlpPendingKey(&metric)
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

// otlpMetricKey identifies a series of a resource among all others
// (otlpKey): its name, its type, and each label's name and value. A queued
// point replaces the one waiting under the same key, and a resource's points
// are exported in key order.
func otlpMetricKey(metric model.Metric) string {
	return otlpSeriesKey("", &metric)
}

// otlpSeriesKey is otlpMetricKey written after before: where the start
// times are kept, the key of the series' resource (otlpStartTimes).
func otlpSeriesKey(before string, metric *model.Metric) string {
	return otlpKey(before, metric.Labels, metric.Name, string(metric.Type))
}

// A key is stored where a point waits for the export (otlpBatch) and where
// a series' start time is kept (otlpStartTimes). It is stored as itself
// when it is at most otlpKeyLongest bytes long, as nearly every key is, and
// otherwise as its SHA-256 digest. A series' labels are the target's, and
// with otlp.probe_attributes its target attribute is what the probe's caller
// wrote, up to 8 KiB. Stored as themselves, every waiting point (up to
// otlp.max_pending_points) held a copy of its target, and so did twice as
// many start times, for an hour: about 1.5 GiB of them at 8 KiB. A digest
// has the same length whatever the key. Keys of up to otlpKeyLongest bytes
// (a series with a few labels and a target of a hundred bytes) are not
// digested because a digest costs: about half a microsecond for a key of
// 100 bytes, which would make a point a fifth slower where it waits and a
// quarter slower where its start time is found (BenchmarkOTLPPoints).
//
// A key of exactly the digest's length is also stored as its digest. So a
// key stored as itself is never as long as a digest, no key is stored as
// another key's digest, and a digest is recognized by its length
// (otlpKeyDigested).
const otlpKeyLongest = 256

// otlpKeyOf is what key is remembered by: key itself, or its digest,
// written in digest.
func otlpKeyOf(key []byte, digest *[sha256.Size]byte) []byte {
	if len(key) <= otlpKeyLongest && len(key) != sha256.Size {
		return key
	}
	*digest = sha256.Sum256(key)
	return digest[:]
}

// otlpKeyDigested says whether a key remembered is a digest.
func otlpKeyDigested(remembered string) bool {
	return len(remembered) == sha256.Size
}

// otlpPendingKey is what a point waits under in its resource's queue: its
// series key (otlpMetricKey) as it is remembered (otlpKeyOf).
func otlpPendingKey(metric *model.Metric) string {
	var room [otlpKeyRoom]byte
	var digest [sha256.Size]byte
	return string(otlpKeyOf(appendOTLPKey(room[:0], "", metric.Labels, metric.Name, string(metric.Type)), &digest))
}

// otlpPendingOrder returns the keys of a resource's waiting points in the
// order of their series keys, which is the order the points are exported in.
// A key stored as its digest is rebuilt just while the order is found, so
// the order is the one the original keys had.
func otlpPendingOrder(pending map[string]pendingMetric) []string {
	keys := make([]string, 0, len(pending))
	var long map[string]string
	for key, m := range pending {
		keys = append(keys, key)
		if otlpKeyDigested(key) {
			if long == nil {
				long = map[string]string{}
			}
			long[key] = otlpMetricKey(m.metric)
		}
	}
	if long == nil {
		sort.Strings(keys)
		return keys
	}
	series := func(key string) string {
		if made, digested := long[key]; digested {
			return made
		}
		return key
	}
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(series(a), series(b)) })
	return keys
}

// What a key of the export has after each NUL of its join, and what ends
// the join (otlpKey).
const (
	otlpKeyNUL = "\x00\x01"
	otlpKeyEnd = "\x00\x00"
)

// otlpKey is the key of a resource or a series. The export uses it to tell
// one from every other: where points wait for it (otlpBatch), among the
// resources of an export (appendToResource), and where start times are kept
// (otlpStartTimes). head holds the parts every one has (a resource's service
// name; a series' name and type), and pairs its attributes or labels.
// before is written first, as it is.
//
// The key used to be the parts joined: the head with a NUL between its
// parts, then, for each pair in name order, a NUL, the name, = and the
// value. But a label value is the target's and resource attributes are the
// operator's, of any bytes. m{a="1\x00b=2"} and m{a="1",b="2"} joined to the
// same bytes, as did the attribute a=b of value c and the attribute a of
// value b=c, and a resource whose last attribute held what another's series
// began with. Two series with one key were one in the queue: the later point
// replaced the other and that series was never exported. Two resources with
// one key were exported as one, under the first one's attributes. Two series
// with one key among the start times each took the other's count as its
// own, and got a start time and a reset that the other had.
//
// So the key is the join followed by the length of every part, each a
// varint (binary.AppendUvarint), in join order. The lengths say where each
// part lies within the join, so nothing is read from a part's content, and
// no two subjects share a key whatever their parts hold. The end of the
// join is found without a length before it: every NUL of the join, a part's
// own or a separator, is followed by 0x01 (otlpKeyNUL), and two NULs end it
// (otlpKeyEnd), which the join written this way never contains.
//
// The join is kept (the failure log's keys instead put each part after its
// own length, appendKeyPart), and it ends this way, because an export's
// resources and the points of each are exported in key order (drainOTLP),
// which was join order. Written this way, keys still sort in join order. Up
// to where two joins first differ, their keys are the same. There the
// smaller byte is the smaller byte of the keys, a NUL too. A join that is
// a prefix of another ends with two NULs where the other goes on with a
// byte that is not a NUL, or with a NUL and 0x01. Only subjects whose parts
// join to the same bytes, which shared a key, are ordered by the lengths.
//
// A key of up to otlpKeyRoom bytes is built in the one allocation that holds
// it, where the old key took one for the names in order and several as it
// grew. The parts are placed in join order, in place, for all but a subject
// with more pairs than any is likely to have, and written where the caller
// has room (appendOTLPKey). A longer key takes one more allocation.
func otlpKey(before string, pairs map[string]string, head ...string) string {
	var room [otlpKeyRoom]byte
	return string(appendOTLPKey(room[:0], before, pairs, head...))
}

// otlpKeyRoom is the room a key is written in where it is made (otlpKey,
// otlpKeyOf): as much as a key is remembered by as itself, otlpKeyLongest.
const otlpKeyRoom = otlpKeyLongest

// appendOTLPKey appends the key otlpKey makes to dst, growing it once at
// most.
func appendOTLPKey(dst []byte, before string, pairs map[string]string, head ...string) []byte {
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
	// Grown by hand, where slices.Grow would take two allocations under the
	// race detector.
	b := dst
	if cap(b)-len(b) < size+nuls {
		b = make([]byte, len(dst), len(dst)+size+nuls)
		copy(b, dst)
	}
	b = append(b, before...)
	for i, part := range parts {
		switch {
		case i == 0:
		case i >= len(head) && (i-len(head))%2 == 1:
			b = append(b, '=')
		default:
			b = append(b, otlpKeyNUL...)
		}
		// Hardly any subject has a NUL in a part, and then none is looked
		// for again.
		if nuls > 0 {
			for nul := strings.IndexByte(part, 0); nul >= 0; nul = strings.IndexByte(part, 0) {
				b = append(b, part[:nul]...)
				b = append(b, otlpKeyNUL...)
				part = part[nul+1:]
			}
		}
		b = append(b, part...)
	}
	b = append(b, otlpKeyEnd...)
	for _, part := range parts {
		b = binary.AppendUvarint(b, uint64(len(part)))
	}
	return b
}

// withoutExponential leaves the exporter's own histograms with only their
// classic buckets, which the OTLP export sends as explicit-bucket histograms
// unless otlp.exponential_histograms is on. A histogram is copied before it
// is changed, since the set may share it.
func withoutExponential(set *model.MetricSet) {
	for i := range set.Metrics {
		if h := set.Metrics[i].Histogram; h != nil && h.Exponential != nil {
			classic := *h
			classic.Exponential = nil
			set.Metrics[i].Histogram = &classic
		}
	}
}
