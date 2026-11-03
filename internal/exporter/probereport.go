package exporter

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector with report_failures answers every probe of a target with an
// exposition, as an uptime check wants: a target that cannot be reached is a
// value to alert on, 0, rather than a scrape that fails and leaves nothing.
// Its probes carry, besides the collector's own series:
//
//	http_exporter_probe_success              1 when the probe succeeded,
//	                                         0 when a stage of its trip failed
//	http_exporter_probe_elapsed_seconds      the probe's own duration, the
//	                                         one its probe duration histogram
//	                                         observes
//	http_exporter_probe_failed_stage{stage}  1, naming the stage that failed,
//	                                         on a failure alone
//
// A failed probe is answered 200 with them alone; a stale result standing in
// for a failed trip (cache.stale_if_error) keeps its series beside them and
// is a failure, as it is to the self-metrics. A stage error_handling carried
// on past failed too: the answer says so, and so does everything else that
// counts the probe, the self-metrics, the status page, a stale result
// standing in and the trace export (probeTrip), where without the key such a
// probe is a success. Nothing of the
// error's text is answered, so nothing of the target's answer is: the stage
// names where it failed, and the log has the rest as before.
//
// What the target was not asked for answers as before: a probe the target
// policy refused (403) is the caller's to change, one the concurrency limit
// turned away (503) and one whose caller went away are the exporter's, and
// none says anything of the target.
//
// The series are added to each probe's answer as it is written: the
// response cache and the OTLP export keep the collector's own series, the
// probes sharing a trip each answer their own duration, and the self-metrics
// count a failure as one. Static targets have
// http_exporter_target_up and are not changed by it.

// probeSeriesHelp is the help of each series of report_failures.
var probeSeriesHelp = map[string]string{
	model.ProbeSuccessMetric:     "1 when the probe succeeded, 0 when a stage of its trip failed or one carried on past failed (report_failures).",
	model.ProbeElapsedMetric:     "Seconds the probe took, from its request to its answer (report_failures).",
	model.ProbeFailedStageMetric: "1 for the stage of the probe's trip that failed: http, http_status, decode, transform, validation, metric, the request type's own, or panic (report_failures).",
}

// reportsProbe says whether the probe of c whose trip ended as result is
// answered with the series of report_failures.
func reportsProbe(c *model.Collector, result *probeResult) bool {
	return c.ReportFailures && !result.abandoned && !result.refused && result.outcome != probeRejected
}

// reportedSeries is how many series the answer of reportsProbe has.
func reportedSeries(result *probeResult) int {
	n := 2
	if result.metrics != nil {
		n += len(result.metrics.Metrics)
	}
	if !result.ok && result.stage != "" {
		n++
	}
	return n
}

// reportedAnswer is the answer of a probe with report_failures: the series
// of answer, which may be nil, and after them the probe's own. success is
// whether the probe succeeded, as the self-metrics count it, and
// failedStage the stage that failed, of a failure. A probe is not a success
// for having no stage: one that panicked failed as much as one whose stage
// did, and the answer may not say otherwise than the self-metrics. answer
// is not changed: the probes sharing a trip each make their own from it.
func reportedAnswer(answer *model.MetricSet, success bool, failedStage string, elapsed time.Duration) model.MetricSet {
	var own []model.Metric
	if answer != nil {
		own = answer.Metrics
	}
	out := model.MetricSet{Metrics: make([]model.Metric, 0, len(own)+3)}
	out.Metrics = append(out.Metrics, own...)
	value := 0.0
	if success {
		value = 1
	}
	out.Metrics = append(out.Metrics,
		model.Metric{Name: model.ProbeSuccessMetric, Help: probeSeriesHelp[model.ProbeSuccessMetric], Type: model.GaugeMetricType, Value: value},
		model.Metric{Name: model.ProbeElapsedMetric, Help: probeSeriesHelp[model.ProbeElapsedMetric], Type: model.GaugeMetricType, Value: max(elapsed.Seconds(), 0)},
	)
	if !success && failedStage != "" {
		out.Metrics = append(out.Metrics, model.Metric{Name: model.ProbeFailedStageMetric, Help: probeSeriesHelp[model.ProbeFailedStageMetric], Type: model.GaugeMetricType, Value: 1, Labels: map[string]string{"stage": failedStage}})
	}
	return out
}

// writeProbeAnswer writes result, the answer of a probe of c that took elapsed,
// as report_failures has it written, or as it is.
func writeProbeAnswer(w http.ResponseWriter, r *http.Request, c *model.Collector, result *probeResult, elapsed time.Duration, utf8Names bool) {
	if !reportsProbe(c, result) {
		result.writeTo(w, r, utf8Names)
		return
	}
	answer := reportedAnswer(result.metrics, result.ok, result.stage, elapsed)
	writeMetricSet(w, r, &answer, utf8Names)
}

// reportedTrace says in end, the end of the trace of a probe of c whose trip
// ended as result, what report_failures answered: 200 and its series. The
// probe stays failed in it, as it is to the self-metrics.
func reportedTrace(c *model.Collector, result *probeResult, end *probeTraceEnd) {
	if reportsProbe(c, result) {
		end.status, end.series = http.StatusOK, reportedSeries(result)
	}
}

// reportedRecent says in recent, a probe of c whose trip ended as result as
// the status page shows it, what report_failures answered: 200 and its
// series. Its result and failure stay what they are.
func reportedRecent(c *model.Collector, result *probeResult, recent recentProbe) recentProbe {
	if reportsProbe(c, result) {
		recent.status, recent.series = http.StatusOK, reportedSeries(result)
	}
	return recent
}

// reportedVerdict is what a debug run says a probe of c would have
// answered, verdict and answer as debugVerdict says them, with
// report_failures: the same answer with the probe's series, 200 in place of
// a failure. elapsed is the run's trip, which stands for the probe's
// duration.
func reportedVerdict(c *model.Collector, result collected, verdict string, answer *model.MetricSet, elapsed time.Duration) (string, *model.MetricSet) {
	if !c.ReportFailures || result.aborted || result.refused {
		return verdict, answer
	}
	success := !result.failed() && !result.carriedOn
	reported := reportedAnswer(answer, success, result.stage, elapsed)
	if success {
		return fmt.Sprintf("200 with %d series, %s 1 among them (report_failures)", len(reported.Metrics), model.ProbeSuccessMetric), &reported
	}
	return fmt.Sprintf("200 with %s 0 and the %s stage failed (report_failures), in place of %s", model.ProbeSuccessMetric, result.stage, verdict), &reported
}

// checkProbeNames refuses, of a collector with report_failures, a result
// with a series of a name of the probe's own series: one a Python script or
// a prometheus transform made on the scrape, which the load could not see
// (config.checkProbeSeriesNames refuses a rule's).
func checkProbeNames(c *model.Collector, set model.MetricSet) error {
	if !c.ReportFailures {
		return nil
	}
	var clash []string
	for _, m := range set.Metrics {
		if slices.Contains(model.ProbeSeriesNames, m.Name) && !slices.Contains(clash, m.Name) {
			clash = append(clash, m.Name)
		}
	}
	if len(clash) == 0 {
		return nil
	}
	slices.Sort(clash)
	return fmt.Errorf("the metric name %s is reserved for the series a collector with report_failures adds itself", strings.Join(clash, " and "))
}
