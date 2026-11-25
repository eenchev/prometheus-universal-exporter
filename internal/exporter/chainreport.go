package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An http collector with request.then makes a chain of requests per trip
// (fetch/chain.go), which the trip's response, or its error, reports step
// by step (fetch.ChainReport). The collector's own series keep describing
// the trip. Four families describe the steps, by collector and step:
//
//   - http_exporter_chain_requests_total, the requests each step sent;
//   - http_exporter_chain_step_failures_total, the trips that failed at the
//     step, and the items that failed under on_item_error: skip;
//   - http_exporter_chain_items, the entries of the last fan-out;
//   - http_exporter_chain_items_cut_total, the fan-outs max_items cut.
//
// Verbose mode adds http_exporter_chain_step_duration_seconds, a histogram
// of each step's duration. A step is a name of the configuration, so a
// collector has at most 13 of them; a fan-out's URLs never become labels.

// chainStepStats are a step's counters, kept in its collector's statistics
// (serverStats.chain), whose lock guards them.
type chainStepStats struct {
	requests, failures, cut uint64
	items                   float64
	durations               durationHistogram
}

// chainStep is the counters of a step of the collector, made on first use.
func (s *serverStats) chainStep(name string) *chainStepStats {
	if s.chain == nil {
		s.chain = map[string]*chainStepStats{}
	}
	step := s.chain[name]
	if step == nil {
		step = &chainStepStats{}
		s.chain[name] = step
	}
	return step
}

// chainReportOf is the chain report of a trip's response, or of its error,
// nil for a trip that made no chain.
func chainReportOf(response *fetch.HTTPResponse, err error) *fetch.ChainReport {
	if response != nil && response.Chain != nil {
		return response.Chain
	}
	var chainErr *fetch.ChainError
	if errors.As(err, &chainErr) {
		return chainErr.Report
	}
	return nil
}

// chainFailureAttrs are the attributes a trip's failure is logged with
// when a step of its chain failed: the step and, for a fan-out item, its
// place and how many items there were.
func chainFailureAttrs(report *fetch.ChainReport) []any {
	if report == nil || report.FailedStep == "" {
		return nil
	}
	attrs := []any{"step", report.FailedStep}
	if report.FailedItem > 0 {
		attrs = append(attrs, "item", report.FailedItem, "items", report.Items)
	}
	return attrs
}

// The aspects of a chain's step that the log reports sparingly: a fan-out
// that max_items cut, and one whose items failed under on_item_error: skip.
const (
	chainCutAspect  failureAspect = "\x00chain cut"
	chainSkipAspect failureAspect = "\x00chain skipped"
)

// noteChain counts a trip's chain in the collector's statistics, and logs
// what is not a failure of the trip: a fan-out cut by max_items, items that
// failed under skip, and, at debug level, each request. target is the
// target as logs show it, keyTarget as the failure log tells targets apart.
func (s *Server) noteChain(ctx context.Context, report *fetch.ChainReport, c *model.Collector, rec statsRecorder, target, keyTarget string) {
	verbose := s.verboseSelfMetrics()
	rec.update(func(x *serverStats) {
		for i := range report.Steps {
			step := &report.Steps[i]
			if !step.Reached() {
				continue
			}
			stats := x.chainStep(step.Name)
			stats.requests += uint64(max(step.Requests, 0))
			if step.Name == report.FailedStep {
				stats.failures++
			} else if step.OnItemError == model.ChainItemSkip {
				stats.failures += uint64(max(step.ItemsFailed, 0))
			}
			if step.ForEach {
				stats.items = float64(step.ItemsTotal)
				if step.ItemsCut > 0 {
					stats.cut++
				}
			}
			if verbose {
				stats.durations.observe(step.Duration)
			}
		}
	})
	for i := range report.Steps {
		step := &report.Steps[i]
		s.logChainStep(ctx, rec, c, step, report, target, keyTarget)
	}
}

// logChainStep logs what a step of a trip's chain did that is not the
// trip's failure, and each of its requests at debug level.
func (s *Server) logChainStep(ctx context.Context, rec statsRecorder, c *model.Collector, step *fetch.ChainStepReport, report *fetch.ChainReport, target, keyTarget string) {
	if !step.Reached() {
		return
	}
	attrs := []any{"collector", c.Name, "target", target, "step", step.Name}
	if step.ForEach {
		cutKey := aspectKey(c.Name, keyTarget, step.Name, chainCutAspect)
		if step.ItemsCut > 0 {
			s.tripFailed(ctx, rec.read, slog.LevelWarn, cutKey, fmt.Sprintf("step %q fanned out over %d of %d entries, cut at max_items; the series are of the items asked for", step.Name, step.ItemsTotal-step.ItemsCut, step.ItemsTotal), "chain", nil, append(attrs, "items", step.ItemsTotal)...)
		} else if report.FailedStep == "" {
			s.tripRecovered(ctx, rec.read, cutKey, fmt.Sprintf("step %q fanned out over every entry again", step.Name), attrs...)
		}
		if step.OnItemError == model.ChainItemSkip {
			skipKey := aspectKey(c.Name, keyTarget, step.Name, chainSkipAspect)
			if step.ItemsFailed > 0 {
				s.tripFailed(ctx, rec.read, slog.LevelWarn, skipKey, fmt.Sprintf("step %q: %d of %d items failed and were skipped", step.Name, step.ItemsFailed, len(step.Items)), "chain", errors.New(step.Error), append(attrs, "items", len(step.Items))...)
			} else if report.FailedStep == "" {
				s.tripRecovered(ctx, rec.read, skipKey, fmt.Sprintf("step %q: no item failed again", step.Name), attrs...)
			}
		}
	}
	if !slog.Default().Enabled(ctx, slog.LevelDebug) && probeTraceFrom(ctx) == nil {
		return
	}
	if !step.ForEach {
		s.tripDebug(ctx, "chain request", append(attrs, "method", step.Method, "url", step.URL, "status", step.Status, "duration", step.Duration.Round(time.Millisecond).String())...)
		return
	}
	for _, item := range step.Items {
		s.tripDebug(ctx, "chain request", append(attrs, "item", item.Index+1, "items", len(step.Items), "method", step.Method, "url", item.URL, "status", item.Status, "duration", item.Duration.Round(time.Millisecond).String())...)
	}
}

// The help texts of the chain's families.
const (
	chainRequestsHelp      = "Requests each step of this http collector's chain sent (request.then), each fan-out item and retry counted, in trips that reached the step; step is the step's name, or first for the collector's own request."
	chainStepFailuresHelp  = "Trips of this http collector that failed at the step of its chain, and, under on_item_error: skip, the fan-out items that failed."
	chainItemsHelp         = "Entries the last fan-out of the step had (for_each), before max_items cut them."
	chainItemsCutHelp      = "Fan-outs of the step that max_items cut; each is logged at warn level, sparingly."
	chainStepDurationHelp  = "Duration of each step of this http collector's chain, its fan-out items together, in seconds."
	chainRequestsName      = "http_exporter_chain_requests_total"
	chainStepFailuresName  = "http_exporter_chain_step_failures_total"
	chainItemsName         = "http_exporter_chain_items"
	chainItemsCutName      = "http_exporter_chain_items_cut_total"
	chainStepDurationsName = "http_exporter_chain_step_duration_seconds"
)

// chainMetrics are the chain families of every collector with request.then,
// a series for each step from zero, so a rate works from the start. The
// histogram is there in verbose mode alone.
func (s *Server) chainMetrics() []model.Metric {
	var out []model.Metric
	verbose := s.verboseSelfMetrics()
	collectors := s.manager.Get().Collectors
	for i := range collectors {
		c := &collectors[i]
		steps := fetch.CollectorPolicy(c).Chain
		if steps == nil {
			continue
		}
		stats := s.statsFor(c.Name)
		stats.mu.Lock()
		created := createdMillis(stats.created)
		names := append([]string{model.ChainFirst}, chainStepNames(steps)...)
		for j, name := range names {
			step := stats.chainStep(name)
			labels := func() map[string]string { return map[string]string{"collector": c.Name, "step": name} }
			out = append(out,
				model.Metric{Name: chainRequestsName, Help: chainRequestsHelp, Type: model.CounterMetricType, Value: float64(step.requests), Labels: labels(), Created: created},
				model.Metric{Name: chainStepFailuresName, Help: chainStepFailuresHelp, Type: model.CounterMetricType, Value: float64(step.failures), Labels: labels(), Created: created},
			)
			if j > 0 && steps[j-1].ForEach != "" {
				out = append(out,
					model.Metric{Name: chainItemsName, Help: chainItemsHelp, Type: model.GaugeMetricType, Value: step.items, Labels: labels()},
					model.Metric{Name: chainItemsCutName, Help: chainItemsCutHelp, Type: model.CounterMetricType, Value: float64(step.cut), Labels: labels(), Created: created},
				)
			}
			if verbose {
				out = append(out, model.Metric{Name: chainStepDurationsName, Help: chainStepDurationHelp, Type: model.HistogramMetricType, Histogram: step.durations.histogram(), Labels: labels(), Created: created})
			}
		}
		stats.mu.Unlock()
	}
	return out
}

// chainStepNames are the names of steps, in order.
func chainStepNames(steps []model.ChainStep) []string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = step.Name
	}
	return names
}
