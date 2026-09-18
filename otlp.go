package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	Gauge       *otlpGauge `json:"gauge,omitempty"`
	Sum         *otlpSum   `json:"sum,omitempty"`
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
	AsDouble          *float64        `json:"asDouble,omitempty"`
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
	tlsCfg, err := tlsConfig(tlsSettings)
	if err != nil {
		s.logger.Warn("OTLP TLS configuration failed", "error", err)
		return
	}
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
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
	defer resp.Body.Close()
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
func otlpMetrics(set MetricSet, now string) []otlpMetric {
	out := make([]otlpMetric, 0, len(set.Metrics))
	for _, m := range set.Metrics {
		p := otlpNumberDataPoint{Attributes: otlpAttributesForLabels(m.Labels), TimeUnixNano: now}
		if m.Timestamp != nil {
			p.TimeUnixNano = strconv.FormatInt(*m.Timestamp*int64(time.Millisecond), 10)
		}
		v := m.Value
		p.AsDouble = &v
		switch m.Type {
		case CounterMetricType:
			out = append(out, otlpMetric{Name: m.Name, Description: m.Help, Sum: &otlpSum{DataPoints: []otlpNumberDataPoint{p}, AggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE", IsMonotonic: true}})
		case HistogramMetricType, SummaryMetricType:
			out = append(out, otlpMetric{Name: m.Name, Description: m.Help, Gauge: &otlpGauge{DataPoints: []otlpNumberDataPoint{p}}})
		default:
			out = append(out, otlpMetric{Name: m.Name, Description: m.Help, Gauge: &otlpGauge{DataPoints: []otlpNumberDataPoint{p}}})
		}
	}
	return out
}
