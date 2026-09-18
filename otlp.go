package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// OTLP is emitted using the standard OTLP/HTTP JSON representation. Keeping
// this optional avoids making the Prometheus scrape path depend on a collector,
// while allowing both probe output and exporter self-health to be forwarded.
type otlpPayload struct { ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"` }
type otlpResourceMetrics struct { Resource otlpResource `json:"resource"`; ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"` }
type otlpResource struct { Attributes []otlpAttribute `json:"attributes,omitempty"` }
type otlpScopeMetrics struct { Scope otlpScope `json:"scope"`; Metrics []otlpMetric `json:"metrics"` }
type otlpScope struct { Name string `json:"name"`; Version string `json:"version,omitempty"` }
type otlpAttribute struct { Key string `json:"key"`; Value otlpValue `json:"value"` }
type otlpValue struct { StringValue string `json:"stringValue,omitempty"`; DoubleValue *float64 `json:"doubleValue,omitempty"`; IntValue string `json:"intValue,omitempty"`; BoolValue *bool `json:"boolValue,omitempty"` }
type otlpMetric struct { Name string `json:"name"`; Description string `json:"description,omitempty"`; Unit string `json:"unit,omitempty"`; Gauge *otlpGauge `json:"gauge,omitempty"`; Sum *otlpSum `json:"sum,omitempty"` }
type otlpGauge struct { DataPoints []otlpNumberDataPoint `json:"dataPoints"` }
type otlpSum struct { DataPoints []otlpNumberDataPoint `json:"dataPoints"`; AggregationTemporality string `json:"aggregationTemporality"`; IsMonotonic bool `json:"isMonotonic"` }
type otlpNumberDataPoint struct { Attributes []otlpAttribute `json:"attributes,omitempty"`; StartTimeUnixNano string `json:"startTimeUnixNano,omitempty"`; TimeUnixNano string `json:"timeUnixNano"`; AsDouble *float64 `json:"asDouble,omitempty"`; AsInt string `json:"asInt,omitempty"` }

func (s *Server) pushOTLP(set MetricSet) {
	cfg:=s.manager.Get().OTLP
	if !cfg.Enabled||cfg.Endpoint==""||len(set.Metrics)==0{return}
	timeout:=time.Duration(cfg.Timeout);if timeout<=0{timeout=5*time.Second};ctx,cancel:=context.WithTimeout(context.Background(),timeout);defer cancel()
	now:=strconv.FormatInt(time.Now().UnixNano(),10);payload:=otlpPayload{ResourceMetrics:[]otlpResourceMetrics{{Resource:otlpResource{Attributes:otlpAttributes(cfg)},ScopeMetrics:[]otlpScopeMetrics{{Scope:otlpScope{Name:"prometheus-universal-exporter"},Metrics:otlpMetrics(set,now)}}}}}
	body,err:=json.Marshal(payload);if err!=nil{s.logger.Warn("OTLP encoding failed","error",err);return}
	tlsConfig:=&tls.Config{MinVersion:tls.VersionTLS12,InsecureSkipVerify:cfg.InsecureSkipVerify};client:=&http.Client{Timeout:timeout,Transport:&http.Transport{TLSClientConfig:tlsConfig}};req,err:=http.NewRequestWithContext(ctx,http.MethodPost,cfg.Endpoint,bytes.NewReader(body));if err!=nil{s.logger.Warn("OTLP request creation failed","error",err);return};req.Header.Set("Content-Type","application/json");for k,v:=range cfg.Headers{req.Header.Set(k,v)};resp,err:=client.Do(req);if err!=nil{s.logger.Warn("OTLP export failed","error",err);return};defer resp.Body.Close();if resp.StatusCode<200||resp.StatusCode>=300{s.logger.Warn("OTLP endpoint returned an error","status",resp.StatusCode)}
}

func otlpAttributes(cfg OTLPConfig)[]otlpAttribute{attrs:=[]otlpAttribute{{Key:"service.name",Value:otlpValue{StringValue:cfg.ServiceName}}};for k,v:=range cfg.ResourceAttributes{attrs=append(attrs,otlpAttribute{Key:k,Value:otlpValue{StringValue:v}})};return attrs}
func otlpAttributesForLabels(labels map[string]string)[]otlpAttribute{out:=make([]otlpAttribute,0,len(labels));for k,v:=range labels{out=append(out,otlpAttribute{Key:k,Value:otlpValue{StringValue:v}})};return out}
func otlpMetrics(set MetricSet,now string)[]otlpMetric{out:=make([]otlpMetric,0,len(set.Metrics));for _,m:=range set.Metrics{p:=otlpNumberDataPoint{Attributes:otlpAttributesForLabels(m.Labels),TimeUnixNano:now};if m.Timestamp!=nil{p.TimeUnixNano=strconv.FormatInt(*m.Timestamp*int64(time.Millisecond),10)};v:=m.Value;p.AsDouble=&v;switch m.Type{case CounterMetricType:out=append(out,otlpMetric{Name:m.Name,Description:m.Help,Sum:&otlpSum{DataPoints:[]otlpNumberDataPoint{p},AggregationTemporality:"AGGREGATION_TEMPORALITY_CUMULATIVE",IsMonotonic:true}});case HistogramMetricType,SummaryMetricType:out=append(out,otlpMetric{Name:m.Name,Description:m.Help,Gauge:&otlpGauge{DataPoints:[]otlpNumberDataPoint{p}}});default:out=append(out,otlpMetric{Name:m.Name,Description:m.Help,Gauge:&otlpGauge{DataPoints:[]otlpNumberDataPoint{p}}})}};return out}
