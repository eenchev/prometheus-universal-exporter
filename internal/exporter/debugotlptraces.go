package exporter

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// With otlp.traces on, the debug page's OTLP tab shows the spans the trace
// export would make of the run — by the code that makes a probe's or a
// scrape's (buildSpans, queuedSpans, otlpTraceRequests), from the stages and
// requests the run recorded as they do — and whether the export would send
// them, by the sampling rules a probe's or a scrape's trace is decided by
// (decideTrace): the run's duration against otlp.traces.threshold, whether
// it failed against otlp.traces.errors, and its trace id against
// otlp.traces.sample_ratio. The rate cap is not taken: what it would leave
// depends on the traces exported before. The run's own trace is never
// exported: a run is never seen by the export.

// debugOTLPTraces is the run's trace as the trace export would make it.
type debugOTLPTraces struct {
	// Threshold is otlp.traces.threshold, Errors otlp.traces.errors and
	// the rest the other rules in force.
	Threshold    string  `json:"threshold"`
	Errors       bool    `json:"errors"`
	ParentBased  bool    `json:"parent_based"`
	SampleRatio  float64 `json:"sample_ratio"`
	MaxPerSecond int     `json:"max_per_second"`
	// Took is how long the run took, Failed whether it failed as a probe or
	// a scrape fails, and Exported whether the export would send a probe's
	// or a scrape's trace that took as long and ended as it did, Why why.
	Took     string `json:"took"`
	Failed   bool   `json:"failed"`
	Exported bool   `json:"exported"`
	// Reason is the rule that would keep it, as sampling.reason names it.
	Reason string          `json:"reason,omitempty"`
	Why    string          `json:"why"`
	Spans  []debugOTLPSpan `json:"spans"`
	Notes  []string        `json:"notes,omitempty"`
	// Body is the ExportTraceServiceRequest of the spans.
	Body string `json:"body"`
}

// debugOTLPSpan is a span as the tab lists it: in the order of the tree, at
// its depth in it, with its start from the trace's and its duration in
// milliseconds, and its status.
type debugOTLPSpan struct {
	Name       string               `json:"name"`
	Kind       string               `json:"kind"`
	Depth      int                  `json:"depth"`
	StartMs    float64              `json:"start_ms"`
	DurationMs float64              `json:"duration_ms"`
	Status     string               `json:"status,omitempty"`
	Attributes []debugOTLPAttribute `json:"attributes,omitempty"`
}

// debugOTLPTracesOf is the run's trace, nil when the run recorded none.
func (s *Server) debugOTLPTracesOf(cfg model.OTLPConfig, report *debugReport) *debugOTLPTraces {
	if report.spans == nil {
		return nil
	}
	p := report.probe
	stages, requests, fetched := report.spans.close()
	end := report.started.Add(report.took)
	series := 0
	if report.answer != nil {
		series = len(report.answer.Metrics)
	}
	var root spanRoot
	var identity otlpResourceIdentity
	if p.static != nil {
		var failure error
		if report.failed {
			failure = fmt.Errorf("%s", report.verdict)
		}
		root = scrapeSpanRoot(p.static, p.collector, p.logTarget, report.started, end, series, false, failure, fetched)
		identity = targetResource(p.static, cfg)
	} else {
		e := probeTraceEnd{status: debugVerdictStatus(report.verdict), series: series, failed: report.failed}
		if report.failed {
			e.stage, e.failure = strconv.Itoa(e.status), report.verdict
		}
		root = probeSpanRoot(p.collector, p.logTarget, "GET", report.started, end, traceParent{}, e, fetched)
		identity = defaultResourceIdentity(cfg)
	}
	// The run's trace is decided as a probe's or a scrape's would be, by
	// the rules alone: a run has no caller's traceparent, and the rate cap,
	// which goes by the traces exported before it, is not taken.
	decision := decideTrace(cfg, report.took, report.failed, traceParent{})
	sampledRoot(&root, cfg, decision, traceParent{})
	made := buildSpans(root, stages, requests)
	t := cfg.Traces
	out := &debugOTLPTraces{Threshold: otlpTraceThreshold(cfg).String(), Errors: t.Errors, ParentBased: t.ParentBased, SampleRatio: t.SampleRatio, MaxPerSecond: t.MaxPerSecond,
		Took: shownScrapeDuration(report.took), Failed: report.failed}
	out.Exported, out.Reason = decision.reason != keptNone, samplingReasonNames[decision.reason]
	out.Why = decision.why(cfg, report.took, report.failed, traceParent{})
	if out.Exported && t.MaxPerSecond > 0 {
		out.Why += fmt.Sprintf(", unless otlp.traces.max_per_second %d is spent, which a run does not take part in", t.MaxPerSecond)
	}
	out.Spans = debugSpanTree(made)
	out.Notes = []string{
		"The run itself is never exported: a probe or a scrape that took as long and ended as the run did would be, or not, as said above.",
		"Ids are made anew for every trace, from crypto/rand; a probe's caller that sends a traceparent has its probe's span made a span of its trace.",
	}
	if p.kind == debugKindPage && p.static == nil {
		out.Notes = append(out.Notes, "A probe's span has the probe's own method and status; the run's are GET and the status a probe would have answered.")
	}
	plain := cfg
	plain.Compression = model.OTLPCompressionNone
	requestsOut, err := otlpTraceRequests(plain, queuedSpans(identity, made))
	switch {
	case err != nil:
		out.Body = err.Error()
	default:
		var bodies []string
		for _, request := range requestsOut {
			var b bytes.Buffer
			if err := json.Indent(&b, request.body, "", "  "); err != nil {
				bodies = append(bodies, string(request.body))
				continue
			}
			bodies = append(bodies, b.String())
		}
		if len(requestsOut) > 1 {
			out.Notes = append(out.Notes, fmt.Sprintf("The spans are more than one request holds: the trace export sends them in %d requests, one after another below.", len(requestsOut)))
		}
		out.Body = strings.Join(bodies, "\n\n")
	}
	return out
}

// debugVerdictStatus is the status a probe would have answered, which its
// verdict begins with.
func debugVerdictStatus(verdict string) int {
	if len(verdict) >= 3 {
		if n, err := strconv.Atoi(verdict[:3]); err == nil {
			return n
		}
	}
	return 0
}

// debugSpanTree lists spans in the order of their tree, each after its
// parent and its children by their starts, with their depths.
func debugSpanTree(spans []otlpSpan) []debugOTLPSpan {
	if len(spans) == 0 {
		return nil
	}
	children := map[string][]int{}
	for i := 1; i < len(spans); i++ {
		children[spans[i].ParentSpanID] = append(children[spans[i].ParentSpanID], i)
	}
	start := func(i int) int64 { n, _ := strconv.ParseInt(spans[i].StartTimeUnixNano, 10, 64); return n }
	end := func(i int) int64 { n, _ := strconv.ParseInt(spans[i].EndTimeUnixNano, 10, 64); return n }
	origin := start(0)
	var out []debugOTLPSpan
	var walk func(i, depth int)
	walk = func(i, depth int) {
		span := spans[i]
		shown := debugOTLPSpan{Name: span.Name, Kind: debugSpanKind(span.Kind), Depth: depth,
			StartMs: float64(start(i)-origin) / float64(time.Millisecond), DurationMs: float64(end(i)-start(i)) / float64(time.Millisecond)}
		if span.Status != nil {
			shown.Status = "ERROR"
			if span.Status.Message != "" {
				shown.Status += ": " + span.Status.Message
			}
		}
		for _, a := range span.Attributes {
			shown.Attributes = append(shown.Attributes, debugOTLPAttribute{Key: a.Key, Value: debugAnyValue(a.Value)})
		}
		out = append(out, shown)
		kids := children[span.SpanID]
		slices.SortStableFunc(kids, func(a, b int) int { return cmp.Compare(start(a), start(b)) })
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	walk(0, 0)
	return out
}

// debugSpanKind names a span's kind as OTLP does.
func debugSpanKind(kind int) string {
	switch kind {
	case otlpSpanKindServer:
		return "SERVER"
	case otlpSpanKindClient:
		return "CLIENT"
	}
	return "INTERNAL"
}
