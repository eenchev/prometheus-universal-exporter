package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
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

// otlpResourceIdentity is the OTLP resource a metric set belongs to. Scheduled
// targets may each declare their own service name and resource attributes, so
// one export can carry several resources.
type otlpResourceIdentity struct {
	ServiceName string
	Attributes  map[string]string
}

// otlpResourceSet pairs a drained metric set with the resource it belongs to.
type otlpResourceSet struct {
	Identity otlpResourceIdentity
	Set      MetricSet
}

// key is the stable identity used to group pending metrics by resource.
func (r otlpResourceIdentity) key() string {
	var b strings.Builder
	b.WriteString(r.ServiceName)
	for _, name := range sortedKeys(r.Attributes) {
		b.WriteByte(0)
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(r.Attributes[name])
	}
	return b.String()
}

func (r otlpResourceIdentity) attributes() []otlpAttribute {
	out := []otlpAttribute{{Key: "service.name", Value: otlpValue{StringValue: r.ServiceName}}}
	for _, name := range sortedKeys(r.Attributes) {
		out = append(out, otlpAttribute{Key: name, Value: otlpValue{StringValue: r.Attributes[name]}})
	}
	return out
}

// defaultResourceIdentity is the exporter-wide resource used for probe output
// and self-health metrics.
func defaultResourceIdentity(cfg OTLPConfig) otlpResourceIdentity {
	identity := otlpResourceIdentity{ServiceName: cfg.ServiceName, Attributes: map[string]string{}}
	for key, value := range cfg.ResourceAttributes {
		identity.Attributes[key] = value
	}
	return identity
}

func sortedKeys(in map[string]string) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (s *Server) pushOTLP(resources []otlpResourceSet) {
	cfg := s.manager.Get().OTLP
	total := 0
	for _, resource := range resources {
		total += len(resource.Set.Metrics)
	}
	if !cfg.Enabled || cfg.Endpoint == "" || total == 0 {
		return
	}
	timeout := time.Duration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	payload := otlpPayload{}
	for _, resource := range resources {
		if len(resource.Set.Metrics) == 0 {
			continue
		}
		payload.ResourceMetrics = append(payload.ResourceMetrics, otlpResourceMetrics{Resource: otlpResource{Attributes: resource.Identity.attributes()}, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "prometheus-universal-exporter"}, Metrics: otlpMetrics(resource.Set, now)}}})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		s.logger.Warn("OTLP encoding failed", "error", err)
		return
	}
	tlsSettings := cfg.TLS
	if cfg.InsecureSkipVerify {
		tlsSettings.InsecureSkipVerify = true
	}
	// One connection to the collector is reused from export to export.
	client, err := httpClient(transportSettings{TLS: tlsSettings}, true, timeout)
	if err != nil {
		s.logger.Warn("OTLP TLS configuration failed", "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		s.logger.Warn("OTLP request creation failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("OTLP export failed", "error", err)
		return
	}
	// The body is read to the end, however little of it matters, so the
	// connection goes back to the pool for the next export.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.logger.Warn("OTLP endpoint returned an error", "status", resp.StatusCode)
	}
}

func otlpAttributesForLabels(labels map[string]string) []otlpAttribute {
	out := make([]otlpAttribute, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		out = append(out, otlpAttribute{Key: k, Value: otlpValue{StringValue: labels[k]}})
	}
	return out
}

// otlpMetrics converts a metric set to OTLP metrics. Series of one family
// become the data points of one metric, in the order the family first
// appears: a gauge, a monotonic cumulative sum for a counter, a cumulative
// histogram, or a summary.
func otlpMetrics(set MetricSet, now string) []otlpMetric {
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
func otlpKindOf(m Metric) int {
	switch {
	case m.Type == HistogramMetricType && m.Histogram != nil:
		return otlpKindHistogram
	case m.Type == SummaryMetricType && m.Summary != nil:
		return otlpKindSummary
	case m.Type == CounterMetricType:
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

func newOTLPMetric(m Metric) otlpMetric {
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
func otlpHistogramPoint(m Metric, attributes []otlpAttribute, at string) otlpHistogramDataPoint {
	h := m.Histogram
	buckets := make([]Bucket, 0, len(h.Buckets))
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

func otlpSummaryPoint(m Metric, attributes []otlpAttribute, at string) otlpSummaryDataPoint {
	s := m.Summary
	point := otlpSummaryDataPoint{Attributes: attributes, TimeUnixNano: at, Count: strconv.FormatUint(s.Count, 10), Sum: otlpDouble(s.Sum)}
	for _, q := range s.Quantiles {
		point.QuantileValues = append(point.QuantileValues, otlpQuantileValue{Quantile: otlpDouble(q.Quantile), Value: otlpDouble(q.Value)})
	}
	return point
}
