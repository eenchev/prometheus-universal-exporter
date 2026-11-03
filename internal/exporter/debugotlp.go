package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The debug page's OTLP tab, with otlp.enabled: what the next export would
// send of a run's series, made by the export's own code (otlpProbeSet,
// targetResource, withoutResourceLabels, otlpResourceMetricsOf) and not by
// a copy of it, so the tab cannot say one thing while the export does
// another. It is made of the run alone. Nothing is queued, no start time is
// asked of the series remembered (otlpStartTimes) or remembered, nothing
// counts in the export's self-metrics or its status, and nothing is sent:
//
//   - a probe's run — a pasted response or a target fetched, of a
//     configured collector or of the playground — is under the exporter-wide
//     resource, with otlp.probe_attributes' collector and target, as a
//     probe of its collector is exported; a static target's scrape is under
//     the target's resource, its otlp.labels_as_resource moved to it, as its
//     scrape with export_via_otlp is;
//   - a cumulative point's start time is its own time, which is the start
//     time a series' first export gives it: the export remembers when each
//     series started, which a run must neither read nor change;
//   - the points are checked against one another as the export checks
//     those of a resource (otlpMetricsOf), and against nothing else: the
//     points of other probes and static targets of the same resource, and
//     the exporter's own series, are not the run's, and an export may have
//     any of them or none.
//
// With otlp.logs on, the run's log lines are shown as the log records the
// tee would make of them (otlpLogHandler), and those it would take, at
// otlp.logs.level or above, as the request the log export would send. A run
// logs to its answer alone, so none of them is queued either. With
// otlp.traces on, the run's trace is shown as the spans the trace export
// would make of it (debugotlptraces.go), and never queued.

// debugOTLP is the otlp field of a run's answer.
type debugOTLP struct {
	// MetricNames is otlp.metric_names: prometheus or otel.
	MetricNames string `json:"metric_names"`
	// Resource is the resource's attributes, and ResourceNotes where they
	// come from.
	Resource      []debugOTLPAttribute `json:"resource"`
	ResourceNotes []string             `json:"resource_notes,omitempty"`
	Scope         otlpScope            `json:"scope"`
	// Points are the data points the export would send, each with what
	// the export made of its series; LeftOut those it would not, and
	// Clashes what it would log of them.
	Points  []debugOTLPPoint   `json:"points"`
	LeftOut []debugOTLPLeftOut `json:"left_out,omitempty"`
	Clashes []string           `json:"clashes,omitempty"`
	// Notes say what holds for every point.
	Notes []string `json:"notes,omitempty"`
	// Body is the ExportMetricsServiceRequest, as JSON indented to be read.
	Body string `json:"body"`
	// Logs is the run's log as records, with otlp.logs on.
	Logs *debugOTLPLogs `json:"logs,omitempty"`
	// Traces is the run's trace as spans, with otlp.traces on
	// (debugotlptraces.go).
	Traces *debugOTLPTraces `json:"traces,omitempty"`
}

type debugOTLPAttribute struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// debugOTLPPoint is a data point: Prometheus, the name the exposition
// writes it under, Name, Kind and Unit its metric's, Value its value or, of
// a histogram or a summary, its count, sum and buckets or quantiles, and
// Notes each change the export made of its series.
type debugOTLPPoint struct {
	Prometheus string               `json:"prometheus"`
	Name       string               `json:"name"`
	Kind       string               `json:"kind"`
	Unit       string               `json:"unit,omitempty"`
	Attributes []debugOTLPAttribute `json:"attributes,omitempty"`
	Value      string               `json:"value"`
	Notes      []string             `json:"notes,omitempty"`
}

// debugOTLPLeftOut is a point the export would leave out, and why.
type debugOTLPLeftOut struct {
	Prometheus string               `json:"prometheus"`
	Name       string               `json:"name"`
	Kind       string               `json:"kind"`
	Attributes []debugOTLPAttribute `json:"attributes,omitempty"`
	Reason     string               `json:"reason"`
}

// debugOTLPLogs is the run's log as the log export would send it.
type debugOTLPLogs struct {
	// Level is otlp.logs.level: a record below it is not exported.
	Level   string               `json:"level"`
	Records []debugOTLPLogRecord `json:"records"`
	Notes   []string             `json:"notes,omitempty"`
	// Body is the ExportLogsServiceRequest of the records exported, or of
	// each request where they are several.
	Body string `json:"body"`
}

type debugOTLPLogRecord struct {
	Time           string               `json:"time,omitempty"`
	SeverityNumber int                  `json:"severity_number"`
	SeverityText   string               `json:"severity_text"`
	Body           string               `json:"body"`
	Attributes     []debugOTLPAttribute `json:"attributes,omitempty"`
	Exported       bool                 `json:"exported"`
	Note           string               `json:"note,omitempty"`
	// TraceID and SpanID are the run's trace and the span the line was
	// logged in, which a probe's record is linked to when its trace is
	// exported (otlptraceslink.go), and Link says so: the run's trace is
	// never exported, and its records are not linked.
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
	Link    string `json:"link,omitempty"`
}

// debugLogRecords are a run's log lines as records, with their levels, as
// the tee makes them (otlpLogHandler).
type debugLogRecords struct {
	mu      sync.Mutex
	levels  []slog.Level
	records [][]byte
	// links are the run's trace each record was logged in, if any, and
	// times when it was logged.
	links []*traceLink
	times []time.Time
}

func (d *debugLogRecords) add(level slog.Level, record []byte, link *traceLink, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.levels = append(d.levels, level)
	d.records = append(d.records, record)
	d.links = append(d.links, link)
	d.times = append(d.times, at)
}

// debugOTLPOf is the OTLP tab of a run made at, nil while the export is
// off.
func (s *Server) debugOTLPOf(report *debugReport, at time.Time) *debugOTLP {
	cfg := s.manager.Get().OTLP
	if !cfg.Enabled || cfg.Endpoint == "" {
		return nil
	}
	p := report.probe
	var answer model.MetricSet
	if report.answer != nil {
		answer = *report.answer
	}
	out := &debugOTLP{MetricNames: cfg.MetricNames, Scope: otlpScopeOf(cfg)}
	if out.MetricNames == "" {
		out.MetricNames = model.OTLPMetricNamesPrometheus
	}
	// The set as it is queued, and the resource it is queued under.
	var identity otlpResourceIdentity
	var set model.MetricSet
	if p.static != nil {
		identity = targetResource(p.static, cfg)
		set = withoutResourceLabels(answer, *p.static)
		out.ResourceNotes = staticResourceNotes(p.static)
	} else {
		identity = defaultResourceIdentity(cfg)
		set = otlpProbeSet(cfg, answer, p.collector, p.logTarget)
		out.ResourceNotes = probeResourceNotes(p, cfg)
	}
	// The replica's name is on every resource; where it comes from is not
	// in the configuration when the host name is its value.
	if e := cfg.ExporterInstance; e.Enabled && e.Attribute != "" && e.Value != "" {
		out.ResourceNotes = append(out.ResourceNotes, e.Attribute+" names this exporter replica, on every resource it sends (otlp.exporter_instance).")
	}
	for _, a := range identity.attributes() {
		out.Resource = append(out.Resource, debugOTLPAttribute{Key: a.Key, Value: a.Value.StringValue})
	}
	// The run's points are at its time, as the queue keeps it, in whole
	// milliseconds.
	now := otlpPointTime(at.UnixMilli())
	// The series as an export drains them, each written as it was queued,
	// one after another; places says where each is in the run's answer.
	places := otlpQueueOrder(set)
	queued := otlpResourceSet{Identity: identity, Set: model.MetricSet{Metrics: make([]model.Metric, len(places))}, seqs: make([]int64, len(places))}
	kept := make([]bool, len(set.Metrics))
	for k, i := range places {
		queued.Set.Metrics[k], queued.seqs[k], kept[i] = set.Metrics[i], int64(i+1), true
	}
	// A series' first export starts it at its point's time.
	start := func(_ model.Metric, at string) string { return at }
	sources := otlpPointSources{index: map[string]int{}}
	made, clashes := otlpResourceMetricsOf(cfg, queued, now, start, &sources)
	payload := otlpPayload{ResourceMetrics: []otlpResourceMetrics{}}
	if len(places) > 0 {
		payload.ResourceMetrics = append(payload.ResourceMetrics, made)
	}
	for i, m := range set.Metrics {
		if !kept[i] {
			// One series twice in the answer, which validation refuses of
			// one probe's: the queue keeps the later.
			out.LeftOut = append(out.LeftOut, debugOTLPLeftOut{Prometheus: m.Name, Name: m.Name, Kind: debugOTLPKind(otlpKindOf(m)), Attributes: debugAttributes(otlpAttributesForLabels(m.Labels)),
				Reason: "a later point of the same series replaces it where it waits for the export"})
		}
	}
	untyped := untypedFamilies(set)
	cumulative := false
	for m, metric := range made.ScopeMetrics[0].Metrics {
		kind := otlpKind(metric)
		cumulative = cumulative || kind != otlpKindGauge
		for j := range otlpMetricPoints(metric) {
			i := places[sources.of[m][j]]
			exported := set.Metrics[i]
			point := debugOTLPPoint{Name: metric.Name, Kind: debugOTLPKind(kind), Unit: metric.Unit, Prometheus: exported.Name}
			exploded := untyped[exported.Name] && otlpKindOf(exported) != otlpKindGauge
			if exploded {
				// Under its sample's name, which the metric keeps.
				point.Prometheus = metric.Name
			}
			var attributes []otlpAttribute
			point.Value, attributes = debugOTLPValue(metric, j)
			point.Attributes = debugAttributes(attributes)
			point.Notes = debugPointNotes(answer.Metrics[i], exported, metric, exploded, untyped)
			out.Points = append(out.Points, point)
		}
	}
	clashOf := map[string]otlpNameClash{}
	for _, clash := range clashes {
		clashOf[clash.name] = clash
		out.Clashes = append(out.Clashes, otlpNameClashLines(clash)...)
	}
	for _, left := range sources.leftOut {
		out.LeftOut = append(out.LeftOut, debugOTLPLeftOut{
			Prometheus: left.source, Name: left.name, Kind: debugOTLPKind(left.shape.kind), Attributes: debugAttributes(left.attributes),
			Reason: debugLeftOutReason(left, clashOf[left.name]),
		})
	}
	out.Notes = debugOTLPNotes(cfg, payload, p, cumulative)
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		// Not reached: the export encodes every point it makes.
		body = []byte(err.Error())
	}
	out.Body = string(body)
	// The spans first: the log records are shown with the ids of the span
	// each was logged in.
	out.Traces = s.debugOTLPTracesOf(cfg, report)
	if records := report.trace.otlpRecords; records != nil {
		out.Logs = debugOTLPLogsOf(cfg, records)
	}
	return out
}

// debugOTLPKind names a kind as the tab shows it.
func debugOTLPKind(kind int) string {
	switch kind {
	case otlpKindSum:
		return "sum (cumulative, monotonic)"
	case otlpKindHistogram:
		return "histogram (cumulative)"
	case otlpKindExponentialHistogram:
		return "exponential histogram (cumulative)"
	}
	return otlpKindNames[kind]
}

func debugAttributes(attributes []otlpAttribute) []debugOTLPAttribute {
	var out []debugOTLPAttribute
	for _, a := range attributes {
		out = append(out, debugOTLPAttribute{Key: a.Key, Value: a.Value.StringValue})
	}
	return out
}

// debugOTLPValue is the j'th point of a metric as the tab shows its value,
// and the point's attributes.
func debugOTLPValue(metric otlpMetric, j int) (string, []otlpAttribute) {
	double := func(v otlpDouble) string {
		b, _ := v.MarshalJSON()
		return strings.Trim(string(b), `"`)
	}
	switch point := otlpMetricPoint(metric, j).(type) {
	case otlpHistogramDataPoint:
		parts := []string{"count " + point.Count}
		if point.Sum != nil {
			parts = append(parts, "sum "+double(*point.Sum))
		}
		var buckets []string
		for k, n := range point.BucketCounts {
			switch {
			case k < len(point.ExplicitBounds):
				buckets = append(buckets, "≤"+double(point.ExplicitBounds[k])+": "+n)
			case k > 0:
				buckets = append(buckets, ">"+double(point.ExplicitBounds[k-1])+": "+n)
			default:
				buckets = append(buckets, "all: "+n)
			}
		}
		return strings.Join(parts, ", ") + "; buckets " + strings.Join(buckets, ", "), point.Attributes
	case otlpSummaryDataPoint:
		text := "count " + point.Count + ", sum " + double(point.Sum)
		var quantiles []string
		for _, q := range point.QuantileValues {
			quantiles = append(quantiles, double(q.Quantile)+": "+double(q.Value))
		}
		if len(quantiles) > 0 {
			text += "; quantiles " + strings.Join(quantiles, ", ")
		}
		return text, point.Attributes
	case otlpNumberDataPoint:
		if point.AsDouble != nil {
			return double(*point.AsDouble), point.Attributes
		}
		return point.AsInt, point.Attributes
	}
	return "", nil
}

// debugPointNotes says what the export made of a point's series: original
// as the run answered it, exported as it is queued, metric what it is
// exported in, exploded when its family is exported as gauges.
func debugPointNotes(original, exported model.Metric, metric otlpMetric, exploded bool, untyped map[string]bool) []string {
	var notes []string
	kind := otlpKindOf(exported)
	switch {
	case exploded:
		notes = append(notes, "exported as gauges under the names of its samples, as OpenMetrics' unknown: "+untypedReason(exported))
	case untyped[exported.Name]:
	case exported.Type == model.HistogramMetricType && exported.Histogram == nil, exported.Type == model.SummaryMetricType && exported.Summary == nil:
		notes = append(notes, "a "+string(exported.Type)+" without its data: exported as a gauge of its value")
	case exported.Type == model.UntypedMetricType:
		notes = append(notes, "untyped: exported as a gauge")
	}
	if !exploded && metric.Name != exported.Name {
		// The name is otlpNameOf's: what is left of it is the OTLP name.
		base := exported.Name
		if kind == otlpKindSum {
			if trimmed, ok := strings.CutSuffix(base, "_total"); ok && trimmed != "" {
				base = trimmed
				notes = append(notes, "_total taken off (otlp.metric_names: otel)")
			}
		}
		if metric.Unit != "" && len(base) > len(metric.Name) {
			notes = append(notes, "unit "+metric.Unit+" from the suffix "+base[len(metric.Name):]+", taken off (otlp.metric_names: otel)")
		}
	}
	for _, name := range model.SortedKeys(exported.Labels) {
		if _, had := original.Labels[name]; !had {
			notes = append(notes, "attribute "+name+" added (otlp.probe_attributes)")
		}
	}
	for _, name := range model.SortedKeys(original.Labels) {
		if _, has := exported.Labels[name]; !has {
			notes = append(notes, "label "+name+" moved to the resource (otlp.labels_as_resource)")
		}
	}
	if exported.Timestamp != nil {
		notes = append(notes, "its time is the series' own timestamp")
	}
	if !exploded {
		switch {
		case kind == otlpKindHistogram && exported.Histogram.NoSum:
			notes = append(notes, "sent without a sum: the histogram has no _sum")
		case kind == otlpKindSummary && exported.Summary.NoCount && exported.Summary.NoSum:
			notes = append(notes, "count and sum sent as 0: the summary has neither _count nor _sum, which OTLP cannot leave out")
		case kind == otlpKindSummary && exported.Summary.NoCount:
			notes = append(notes, "count sent as 0: the summary has no _count, which OTLP cannot leave out")
		case kind == otlpKindSummary && exported.Summary.NoSum:
			notes = append(notes, "sum sent as 0: the summary has no _sum, which OTLP cannot leave out")
		}
	}
	return notes
}

// untypedReason says why a series' family is exported as gauges: its own
// values, where its type does not allow them (typedValues), or another
// series' of the family.
func untypedReason(m model.Metric) string {
	if typedValues(m) {
		return "another series of the family has values its type does not allow"
	}
	switch {
	case m.Histogram != nil:
		h := m.Histogram
		var e expositionWriter
		var below uint64
		for _, b := range e.ascendingBuckets(h.Buckets) {
			if math.IsInf(b.UpperBound, 1) {
				continue
			}
			if b.CumulativeCount < below {
				return "its bucket counts fall"
			}
			below = b.CumulativeCount
		}
		inf, has := h.InfBucket()
		switch {
		case !has:
			return "it has no +Inf bucket"
		case !h.NoCount && inf != h.Count:
			return "its +Inf bucket is not its _count"
		case below > inf:
			return "a bucket counts more than its +Inf bucket"
		}
		return "its sum or a bound is not one a histogram has"
	case m.Summary != nil:
		return "a quantile is outside 0 to 1, a value is negative, or its sum is not a count's"
	case math.IsNaN(m.Value):
		return "its value is NaN, which a counter cannot be"
	}
	return "its value is negative, which a counter cannot be"
}

// debugLeftOutReason says why the export would leave a point out, in the
// words its log line says it in.
func debugLeftOutReason(left otlpLeftOutPoint, clash otlpNameClash) string {
	switch left.why {
	case otlpLeftOutKind:
		return fmt.Sprintf("%s: %s is exported as %s, the kind of its point written last; %v", otlpNameClashMessage, left.name, clash.kept, errOTLPNameClash)
	case otlpLeftOutName:
		return fmt.Sprintf("%s: %s is exported for %s, the Prometheus name of its point written last; %v", otlpNamesMergedMessage, left.name, clash.source, errOTLPNamesMerged)
	}
	return "a point of the same metric with the same attributes written later replaces it, as a later scrape's point replaces a series' waiting for the export"
}

// staticResourceNotes say where a static target's resource comes from.
func staticResourceNotes(t *model.StaticTarget) []string {
	notes := []string{fmt.Sprintf("Static target %s's resource: otlp.service_name and otlp.resource_attributes, its own over the exporter-wide ones.", t.Name)}
	if len(t.OTLP.LabelsAsResource) > 0 {
		notes = append(notes, "otlp.labels_as_resource moves "+strings.Join(t.OTLP.LabelsAsResource, ", ")+" from its points to the resource.")
	}
	if t.OTLP.InstanceID {
		notes = append(notes, otlpServiceInstanceID+" is the target's name (otlp.instance_id).")
	}
	if !t.ExportViaOTLP {
		notes = append(notes, "The target does not set export_via_otlp, so its scrapes are not exported: this is what they would be with it.")
	}
	notes = append(notes, "A scheduled scrape exports the target's health series with these; a run does not make them.")
	return notes
}

// probeResourceNotes say where a probe's resource and attributes come from.
func probeResourceNotes(p debugProbe, cfg model.OTLPConfig) []string {
	notes := []string{"The exporter-wide resource: otlp.service_name and otlp.resource_attributes. At each export the exporter's own series go under it too; they are not shown."}
	if p.edited {
		notes = append(notes, "The playground is no collector of the configuration: this is what a probe of it would export.")
	}
	if cfg.ProbeAttributes {
		if p.logTarget == "" {
			notes = append(notes, "otlp.probe_attributes: each point carries the collector as "+probeCollectorAttribute+"; the run has no target, so no "+probeTargetAttribute+" attribute.")
		} else {
			notes = append(notes, "otlp.probe_attributes: each point carries the collector as "+probeCollectorAttribute+" and the target as "+probeTargetAttribute+".")
		}
	}
	return notes
}

// debugOTLPNotes say what holds for every point of the run's export.
func debugOTLPNotes(cfg model.OTLPConfig, payload otlpPayload, p debugProbe, cumulative bool) []string {
	var notes []string
	if len(payload.ResourceMetrics) == 0 {
		if p.static != nil {
			notes = append(notes, "The run gave no series: a scheduled scrape would export the target's health series alone.")
		} else {
			notes = append(notes, "The run gave no series a probe would answer: a probe would queue nothing for the export.")
		}
	}
	if cumulative {
		notes = append(notes, "Start times: each cumulative point starts at its own time, as at a series' first export. The export remembers when each series started, which a run neither reads nor changes, so a later export of a series it has seen starts it where it was first seen.")
	}
	if points := otlpPayloadPoints(payload); points > 0 {
		notes = append(notes, "Points are as of the run, but for a series with a timestamp of its own; a probe's are as of its fetch.")
		notes = append(notes, "Only the run's own points are checked against one another; those of other probes and static targets under the same resource in the same export, and the exporter's own, are not.")
		maxPoints, maxBytes := otlpBatchBounds(cfg)
		if split, err := splitOTLP(payload, maxPoints, maxBytes); err == nil && len(split) > 1 {
			notes = append(notes, fmt.Sprintf("Its %d points are more than one request holds (otlp.batch_max_size %d, otlp.batch_max_bytes %d): the export sends them in %d requests; the body below is all of them.", points, maxPoints, maxBytes, len(split)))
		}
	}
	if cfg.Compression == model.OTLPCompressionNone {
		notes = append(notes, "The body is sent as it is (otlp.compression: none), on one line; it is indented here to be read.")
	} else {
		notes = append(notes, "The body is sent gzipped (otlp.compression), on one line; it is indented here to be read.")
	}
	return notes
}

// debugOTLPLogsOf is the run's log as the log export would send it.
func debugOTLPLogsOf(cfg model.OTLPConfig, records *debugLogRecords) *debugOTLPLogs {
	records.mu.Lock()
	levels, raw, links, times := records.levels, records.records, records.links, records.times
	records.mu.Unlock()
	level, _ := model.LogLevel(cfg.Logs.Level)
	out := &debugOTLPLogs{Level: strings.ToLower(level.String()), Records: []debugOTLPLogRecord{}}
	var exported [][]byte
	for k, record := range raw {
		var r otlpLogRecord
		if err := json.Unmarshal(record, &r); err != nil {
			continue
		}
		shown := debugOTLPLogRecord{SeverityNumber: r.SeverityNumber, SeverityText: r.SeverityText, Body: debugAnyValue(r.Body), Exported: levels[k] >= level}
		if nanos, err := strconv.ParseInt(r.TimeUnixNano, 10, 64); err == nil {
			shown.Time = time.Unix(0, nanos).UTC().Format(time.RFC3339Nano)
		}
		for _, a := range r.Attributes {
			shown.Attributes = append(shown.Attributes, debugOTLPAttribute{Key: a.Key, Value: debugAnyValue(a.Value)})
		}
		if trace, span, ok := links[k].spanAt(times[k]); ok {
			shown.TraceID, shown.SpanID = trace, span
			shown.Link = "would link to the trace and the span of the OTLP tab's spans, with the sampled flag, if the trace were exported; a run's never is, and its record carries no ids"
		}
		if shown.Exported {
			exported = append(exported, record)
		} else {
			shown.Note = "not exported: below otlp.logs.level " + out.Level
		}
		out.Records = append(out.Records, shown)
	}
	out.Notes = []string{
		"Each line the run logged, as the record the log export makes of it, under the exporter-wide resource. A probe logs its failures as \"probe failed\" where a run says \"debug run failed\", and logs a failure that repeats once and then every five minutes, with its count.",
		"The export's own lines — of its exports, retries and failures — are never exported, and a run makes none.",
	}
	plain := cfg
	plain.Compression = model.OTLPCompressionNone
	requests, err := otlpLogRequests(plain, exported)
	switch {
	case err != nil:
		out.Body = err.Error()
	case len(requests) == 0:
		out.Body = `{"resourceLogs": []}`
		out.Notes = append(out.Notes, "No record would be exported: the log export sends nothing for them.")
	default:
		var bodies []string
		for _, request := range requests {
			var b bytes.Buffer
			if err := json.Indent(&b, request.body, "", "  "); err != nil {
				bodies = append(bodies, string(request.body))
				continue
			}
			bodies = append(bodies, b.String())
		}
		if len(requests) > 1 {
			out.Notes = append(out.Notes, fmt.Sprintf("The records are more than one request holds: the log export sends them in %d requests, one after another below.", len(requests)))
		}
		out.Body = strings.Join(bodies, "\n\n")
	}
	return out
}

// debugAnyValue is a log record's value as text.
func debugAnyValue(v otlpAnyValue) string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	}
	return v.IntValue
}
