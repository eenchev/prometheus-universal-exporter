package exporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
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
	Attributes     []otlpAttribute `json:"attributes,omitempty"`
	TimeUnixNano   string          `json:"timeUnixNano"`
	Count          string          `json:"count"`
	Sum            *otlpDouble     `json:"sum,omitempty"`
	BucketCounts   []string        `json:"bucketCounts"`
	ExplicitBounds []otlpDouble    `json:"explicitBounds"`
}

type otlpSummary struct {
	DataPoints []otlpSummaryDataPoint `json:"dataPoints"`
}

type otlpSummaryDataPoint struct {
	Attributes     []otlpAttribute     `json:"attributes,omitempty"`
	TimeUnixNano   string              `json:"timeUnixNano"`
	Count          string              `json:"count"`
	Sum            otlpDouble          `json:"sum"`
	QuantileValues []otlpQuantileValue `json:"quantileValues,omitempty"`
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
type otlpResourceSet struct {
	Identity otlpResourceIdentity
	Set      model.MetricSet
}

// key is the stable identity used to group pending metrics by resource.
func (r otlpResourceIdentity) key() string {
	var b strings.Builder
	b.WriteString(r.ServiceName)
	for _, name := range model.SortedKeys(r.Attributes) {
		b.WriteByte(0)
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(r.Attributes[name])
	}
	return b.String()
}

func (r otlpResourceIdentity) attributes() []otlpAttribute {
	out := []otlpAttribute{{Key: "service.name", Value: otlpValue{StringValue: r.ServiceName}}}
	for _, name := range model.SortedKeys(r.Attributes) {
		out = append(out, otlpAttribute{Key: name, Value: otlpValue{StringValue: r.Attributes[name]}})
	}
	return out
}

// defaultResourceIdentity is the exporter-wide resource used for probe output
// and self-health metrics.
func defaultResourceIdentity(cfg model.OTLPConfig) otlpResourceIdentity {
	identity := otlpResourceIdentity{ServiceName: cfg.ServiceName, Attributes: map[string]string{}}
	for key, value := range cfg.ResourceAttributes {
		identity.Attributes[key] = value
	}
	return identity
}

// pushOTLP sends resources to the endpoint, and retries a network error, 429,
// 502, 503 or 504 — the responses the OTLP specification makes retryable —
// with exponential backoff, or after the Retry-After the endpoint asks for,
// for as long as another attempt can start within budget. Each attempt is
// bounded by otlp.timeout. Any other status is an otlpRefusedError, not
// retried. It returns the number of retries made, and the partial success the
// endpoint reported when it accepted the export without some of it.
func (s *Server) pushOTLP(ctx context.Context, cfg model.OTLPConfig, resources []otlpResourceSet, budget time.Duration) (int, otlpPartialSuccess, error) {
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	payload := otlpPayload{}
	for _, resource := range resources {
		if len(resource.Set.Metrics) == 0 {
			continue
		}
		payload.ResourceMetrics = append(payload.ResourceMetrics, otlpResourceMetrics{Resource: otlpResource{Attributes: resource.Identity.attributes()}, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "prometheus-universal-exporter"}, Metrics: otlpMetrics(resource.Set, now)}}})
	}
	if len(payload.ResourceMetrics) == 0 {
		return 0, otlpPartialSuccess{}, nil
	}
	body, err := encodeOTLP(payload, cfg.Compression)
	if err != nil {
		return 0, otlpPartialSuccess{}, fmt.Errorf("encoding the export: %w", err)
	}
	timeout := time.Duration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	tlsSettings := cfg.TLS
	if cfg.InsecureSkipVerify {
		tlsSettings.InsecureSkipVerify = true
	}
	deadline := time.Now().Add(budget)
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
// histogram, or a summary.
func otlpMetrics(set model.MetricSet, now string) []otlpMetric {
	var out []otlpMetric
	index := map[string]int{}
	for _, m := range set.Metrics {
		at := now
		if m.Timestamp != nil {
			at = strconv.FormatInt(*m.Timestamp*int64(time.Millisecond), 10)
		}
		attributes := otlpAttributesForLabels(m.Labels)
		i, seen := index[m.Name]
		if !seen || otlpKind(out[i]) != otlpKindOf(m) {
			// A name first seen, or reused with another type: a new metric,
			// so points of different kinds are never mixed in one.
			index[m.Name] = len(out)
			i = len(out)
			out = append(out, newOTLPMetric(m))
		}
		metric := &out[i]
		switch {
		case metric.Histogram != nil:
			metric.Histogram.DataPoints = append(metric.Histogram.DataPoints, otlpHistogramPoint(m, attributes, at))
		case metric.Summary != nil:
			metric.Summary.DataPoints = append(metric.Summary.DataPoints, otlpSummaryPoint(m, attributes, at))
		default:
			v := otlpDouble(m.Value)
			point := otlpNumberDataPoint{Attributes: attributes, TimeUnixNano: at, AsDouble: &v}
			if metric.Sum != nil {
				metric.Sum.DataPoints = append(metric.Sum.DataPoints, point)
			} else {
				metric.Gauge.DataPoints = append(metric.Gauge.DataPoints, point)
			}
		}
	}
	return out
}

const (
	otlpKindGauge = iota
	otlpKindSum
	otlpKindHistogram
	otlpKindSummary
)

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

func newOTLPMetric(m model.Metric) otlpMetric {
	out := otlpMetric{Name: m.Name, Description: m.Help}
	switch otlpKindOf(m) {
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
// per-bucket counts. The +Inf bucket, when the histogram carries one, is not a
// bound: what lies above the highest finite bound is the count less the last
// finite bucket's cumulative count.
func otlpHistogramPoint(m model.Metric, attributes []otlpAttribute, at string) otlpHistogramDataPoint {
	h := m.Histogram
	buckets := make([]model.Bucket, 0, len(h.Buckets))
	for _, b := range h.Buckets {
		if !math.IsInf(b.UpperBound, 1) && !math.IsNaN(b.UpperBound) {
			buckets = append(buckets, b)
		}
	}
	sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].UpperBound < buckets[j].UpperBound })
	point := otlpHistogramDataPoint{
		Attributes:     attributes,
		TimeUnixNano:   at,
		Count:          strconv.FormatUint(h.Count, 10),
		BucketCounts:   make([]string, 0, len(buckets)+1),
		ExplicitBounds: make([]otlpDouble, 0, len(buckets)),
	}
	sum := otlpDouble(h.Sum)
	point.Sum = &sum
	var previous uint64
	for _, b := range buckets {
		point.ExplicitBounds = append(point.ExplicitBounds, otlpDouble(b.UpperBound))
		point.BucketCounts = append(point.BucketCounts, strconv.FormatUint(delta(b.CumulativeCount, previous), 10))
		if b.CumulativeCount > previous {
			previous = b.CumulativeCount
		}
	}
	point.BucketCounts = append(point.BucketCounts, strconv.FormatUint(delta(h.Count, previous), 10))
	return point
}

// delta is a bucket's own count. Cumulative counts never fall in a valid
// histogram; one that does counts as empty rather than wrapping around.
func delta(cumulative, previous uint64) uint64 {
	if cumulative < previous {
		return 0
	}
	return cumulative - previous
}

func otlpSummaryPoint(m model.Metric, attributes []otlpAttribute, at string) otlpSummaryDataPoint {
	s := m.Summary
	point := otlpSummaryDataPoint{Attributes: attributes, TimeUnixNano: at, Count: strconv.FormatUint(s.Count, 10), Sum: otlpDouble(s.Sum)}
	for _, q := range s.Quantiles {
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
// An export is one delivery of everything pending, its retries included, so a
// retried export that got through is one success, and one that ran out of
// retries is one failure. They are exported over OTLP too, so the backend
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

// queueOTLP stages metrics under the exporter-wide OTLP resource.
func (s *Server) queueOTLP(set model.MetricSet) {
	s.queueOTLPResource(set, defaultResourceIdentity(s.manager.Get().OTLP))
}

// queueOTLPResource stages metrics under a specific resource, so a static
// target's own service name and resource attributes survive to the exporter.
func (s *Server) queueOTLPResource(set model.MetricSet, identity otlpResourceIdentity) {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" || len(set.Metrics) == 0 {
		return
	}
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	batch := s.pendingBatchLocked(identity)
	for _, metric := range set.Metrics {
		s.otlpSeq++
		s.putPendingLocked(batch, otlpMetricKey(metric), pendingMetric{metric: model.CloneMetric(metric), seq: s.otlpSeq})
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
		for _, metricKey := range model.SortedKeys(batch.metrics) {
			set.Metrics = append(set.Metrics, batch.metrics[metricKey].metric)
		}
		out = append(out, otlpResourceSet{Identity: batch.identity, Set: set})
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
	s.exportOTLP(context.Background(), time.Duration(cfg.Timeout))
}

// exportOTLP sends everything pending, with a self-metric snapshot, retrying
// within budget. Metrics that could not be delivered for a reason worth
// retrying — a network error, 429, 502, 503, 504, or the export being cut short
// by shutdown — are queued again for the next export, unless a newer value of
// the same series has been queued since; the self-metrics are not, since the
// next export takes a new snapshot. Metrics the endpoint refused outright are
// dropped and counted, since sending them again would be refused again.
func (s *Server) exportOTLP(ctx context.Context, budget time.Duration) {
	cfg := s.manager.Get().OTLP
	pending := s.drainOTLP()
	if !cfg.Enabled || cfg.Endpoint == "" {
		return
	}
	// The copy keeps the self-metrics out of pending, which may be queued again.
	resources := appendToResource(append([]otlpResourceSet(nil), pending...), defaultResourceIdentity(cfg), s.selfMetricSet())
	start := time.Now()
	retries, partial, err := s.pushOTLP(ctx, cfg, resources, budget)
	if err != nil && ctx.Err() != nil {
		// Shutting down: the last export sends these.
		s.requeueOTLP(pending)
		return
	}
	s.otlp.record(cfg.Endpoint, time.Since(start), retries, err == nil)
	if err == nil {
		// The endpoint took the export but not all of it. The rejected
		// points would be rejected again, so they are dropped and counted
		// like a refused export's.
		switch {
		case partial.rejected > 0:
			s.otlp.drop(int(min(partial.rejected, int64(math.MaxInt32))))
			s.logger.Warn("OTLP endpoint accepted an export but rejected some of its data points; they are dropped", "rejected_points", partial.rejected, "error_message", partial.message, "retries", retries)
		case partial.message != "":
			s.logger.Warn("OTLP endpoint accepted an export with a warning", "error_message", partial.message)
		}
		return
	}
	var refused *otlpRefusedError
	if errors.As(err, &refused) {
		points := countPoints(pending)
		s.otlp.drop(points)
		attrs := []any{"status", refused.status, "dropped_points", points, "retries", retries}
		if refused.body != "" {
			attrs = append(attrs, "response_body", refused.body)
		}
		s.logger.Warn("OTLP endpoint refused an export; its data points are dropped", attrs...)
		return
	}
	s.requeueOTLP(pending)
	s.logger.Warn("OTLP export failed; its data points are kept for the next export", "error", err, "retries", retries)
}

// requeueOTLP queues metrics that were not delivered again, each unless a
// newer value of its series has been queued since it was drained.
func (s *Server) requeueOTLP(resources []otlpResourceSet) {
	s.otlpMu.Lock()
	defer s.otlpMu.Unlock()
	for _, resource := range resources {
		batch := s.pendingBatchLocked(resource.Identity)
		for _, metric := range resource.Set.Metrics {
			key := otlpMetricKey(metric)
			if _, newer := batch.metrics[key]; !newer {
				s.putPendingLocked(batch, key, pendingMetric{metric: metric, seq: s.otlpRequeueSeq})
				s.otlpRequeueSeq--
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

func otlpMetricKey(metric model.Metric) string {
	keys := make([]string, 0, len(metric.Labels))
	for key := range metric.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(metric.Name)
	b.WriteByte(0)
	b.WriteString(string(metric.Type))
	for _, key := range keys {
		b.WriteByte(0)
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(metric.Labels[key])
	}
	return b.String()
}
