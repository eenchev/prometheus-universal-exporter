//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// unavailableTarget answers 503 to every request.
func unavailableTarget(t *testing.T) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
	}))
	t.Cleanup(target.Close)
	return target
}

// A probe of a collector with report_failures that error_handling carried
// on past a failed stage is answered success 0 with the stage, and is a
// failure everywhere else the exporter counts it: the self-metrics count no
// success, the status page shows it failed with its error. It is logged as
// its policy says, at warning under log, and at no level the log keeps
// under ignore.
func TestReportFailuresCountsACarriedOnProbeAFailure(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	down := unavailableTarget(t)
	logging := reportingCollector("logging")
	logging.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	ignoring := reportingCollector("ignoring")
	ignoring.ErrorHandling.OnFetchError = model.ErrorPolicyIgnore
	server := modeServer(t, logging, ignoring)

	for _, name := range []string{"logging", "ignoring"} {
		got := probeOnce(t, server, probeOf(name, down.URL), nil)
		if body := got.Body.String(); got.Code != http.StatusOK || !strings.Contains(body, "http_exporter_probe_success 0\n") || !strings.Contains(body, `http_exporter_probe_failed_stage{stage="http_status"} 1`) {
			t.Errorf("%s: %d %s", name, got.Code, body)
		}
	}
	self := selfMetrics(t, server)
	for _, name := range []string{"logging", "ignoring"} {
		for _, want := range []string{
			`http_exporter_scrapes_total{collector="` + name + `"} 1`,
			`http_exporter_scrape_success_total{collector="` + name + `"} 0`,
			`http_exporter_scrape_http_status_code{collector="` + name + `"} 503`,
			`http_exporter_probe_duration_seconds_count{collector="` + name + `",kind="probe"} 1`,
		} {
			if !strings.Contains(self, want) {
				t.Errorf("the self-metrics lack %s", want)
			}
		}
	}
	rows := probeRows(t, server)
	for _, name := range []string{"logging", "ignoring"} {
		row, ok := rows[name+" "+down.URL]
		if !ok || row.Result != probeFailed || !row.Failing || row.Status != http.StatusOK || row.Series != 3 || !strings.Contains(row.LastError, "503") {
			t.Errorf("the status page's row of %s: %+v", name, row)
		}
	}
	text := logs.String()
	if !strings.Contains(text, `"level":"WARN","msg":"probe stage failed; continuing","collector":"logging"`) {
		t.Errorf("under log the failure is not a warning: %s", text)
	}
	if strings.Contains(text, `"collector":"ignoring"`) || strings.Contains(text, `"msg":"probe failed"`) {
		t.Errorf("the log has more than the policies say: %s", text)
	}
}

// Without report_failures a probe carrying on past a failed stage is a
// success, as it always was: answered with nothing of the collector's,
// counted a success and shown so on the status page.
func TestACarriedOnProbeWithoutReportFailuresStaysASuccess(t *testing.T) {
	testutil.CaptureLogs(t)
	down := unavailableTarget(t)
	plain := testutil.Collector("plain", "text")
	plain.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	server := modeServer(t, plain)
	got := probeOnce(t, server, probeOf("plain", down.URL), nil)
	if body := got.Body.String(); got.Code != http.StatusOK || strings.Contains(body, "demo_value") || strings.Contains(body, "http_exporter_probe_") {
		t.Errorf("the probe: %d %s", got.Code, body)
	}
	if self := selfMetrics(t, server); !strings.Contains(self, `http_exporter_scrape_success_total{collector="plain"} 1`) {
		t.Errorf("the probe is not counted a success")
	}
	if row := probeRows(t, server)["plain "+down.URL]; row.Result != probeSuccess || row.LastError != "" {
		t.Errorf("the status page's row: %+v", row)
	}
}

// A rule under error_mode log that misses its value drops its series and
// fails no stage: under report_failures the probe is a success, answered,
// counted and shown as one.
func TestARuleCarryingOnIsNoFailureUnderReportFailures(t *testing.T) {
	testutil.CaptureLogs(t)
	page := textTarget(t, "nothing to read here\n")
	lenient := reportingCollector("lenient")
	lenient.Metrics[0].ErrorMode = model.ErrorModeLog
	server := modeServer(t, lenient)
	got := probeOnce(t, server, probeOf("lenient", page.URL), nil)
	if body := got.Body.String(); got.Code != http.StatusOK || !strings.Contains(body, "http_exporter_probe_success 1\n") || strings.Contains(body, "failed_stage") || strings.Contains(body, "demo_value") {
		t.Errorf("the probe: %d %s", got.Code, body)
	}
	self := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_scrape_success_total{collector="lenient"} 1`, `http_exporter_rule_failures_total{collector="lenient",metric="demo_value"} 1`} {
		if !strings.Contains(self, want) {
			t.Errorf("the self-metrics lack %s", want)
		}
	}
	if row := probeRows(t, server)["lenient "+page.URL]; row.Result != probeSuccess || row.LastError != "" {
		t.Errorf("the status page's row: %+v", row)
	}
}

// Under report_failures the last good result stands in for a probe that
// carried on past a failed stage, as it does for any failed trip
// (cache.stale_if_error): its series, marked stale, with success 0 and the
// stage, and the status page shows a stale result served. Without the key
// the probe is a success, which no stale result answers for. A carried-on
// probe is never cached. A debug probe says what the probe answers.
func TestAStaleResultStandsInForACarriedOnFailureUnderReportFailures(t *testing.T) {
	testutil.CaptureLogs(t)
	reporting := staleCollector(time.Millisecond, time.Hour)
	reporting.ReportFailures = true
	reporting.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	plain := staleCollector(time.Millisecond, time.Hour)
	plain.Name = "plain"
	plain.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	server := modeServer(t, reporting, plain)
	flaky, target := newFlakyTarget(t)
	flaky.value.Store(3)
	for _, name := range []string{"flaky", "plain"} {
		if got := probeOnce(t, server, probeOf(name, target.URL), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "demo_value 3\n") {
			t.Fatalf("the first probe of %s: %d %s", name, got.Code, got.Body)
		}
	}
	entries := len(server.cache.entries)
	ageEntries(server, time.Second)
	flaky.mode.Store("status")

	got := probeOnce(t, server, probeOf("flaky", target.URL), nil)
	for _, want := range []string{"demo_value 3\n", "http_exporter_result_stale 1\n", "http_exporter_probe_success 0\n", `http_exporter_probe_failed_stage{stage="http_status"} 1`} {
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Errorf("under report_failures: %d %s; want %q", got.Code, got.Body, want)
		}
	}
	if row := probeRows(t, server)["flaky "+target.URL]; row.Result != probeStale {
		t.Errorf("the status page's row: %+v", row)
	}
	// A debug probe says the same of the probe.
	server.SetDebug(DebugFull)
	report := probeOnce(t, server, probeOf("flaky", target.URL)+"&debug=true", nil).Body.String()
	if !strings.Contains(report, "200 with the last good result") || !strings.Contains(report, "demo_value 3") {
		t.Errorf("the debug report does not answer the stale result:\n%s", report)
	}
	got = probeOnce(t, server, probeOf("plain", target.URL), nil)
	if body := got.Body.String(); got.Code != http.StatusOK || strings.Contains(body, "demo_value") || !strings.Contains(body, "http_exporter_result_stale 0\n") {
		t.Errorf("without report_failures: %d %s", got.Code, body)
	}
	self := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_cache_stale_served_total{collector="flaky"} 1`, `http_exporter_cache_stale_served_total{collector="plain"} 0`} {
		if !strings.Contains(self, want) {
			t.Errorf("the self-metrics lack %s", want)
		}
	}
	if len(server.cache.entries) != entries {
		t.Errorf("the cache holds %d entries after the failures, %d before", len(server.cache.entries), entries)
	}
}

// The trace of a probe carried on past a failed stage under report_failures
// is a failed one: exported under errors, however fast, its span ERROR with
// the failure and the stage as its error.type, answered 200.
func TestTheTraceOfACarriedOnProbeUnderReportFailuresIsAFailure(t *testing.T) {
	down := unavailableTarget(t)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Hour, func(c *model.Config) {
		c.OTLP.Traces.Errors = true
		c.Collectors[0].ReportFailures = true
		c.Collectors[0].ErrorHandling.OnFetchError = model.ErrorPolicyLog
	}, nil)
	if got := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(down.URL), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "http_exporter_probe_success 0\n") {
		t.Fatalf("the probe: %d %s", got.Code, got.Body)
	}
	exportTracesNow(server)
	var root *receivedSpan
	spans := receiver.spans(t)
	for i := range spans {
		if spans[i].Name == "probe text" {
			root = &spans[i]
		}
	}
	if root == nil {
		t.Fatalf("no probe span was exported: %+v", spans)
	}
	attrs := spanAttributes(root.otlpSpan)
	if root.Status == nil || root.Status.Code != otlpStatusError || !strings.Contains(root.Status.Message, "503") || attrs["error.type"] != "http_status" || attrs["http.response.status_code"] != "200" {
		t.Errorf("the probe's span: %+v", root.otlpSpan)
	}
}

// The trace of any failed probe of a collector with report_failures, one
// that failed a stage under error_handling fail or panicked as much as one
// carried on past it, has the stage as its error.type: it was answered 200,
// which a status as its error.type would contradict.
func TestTheTraceOfAFailedProbeUnderReportFailuresNamesItsStage(t *testing.T) {
	testutil.CaptureLogs(t)
	down := unavailableTarget(t)
	for _, tc := range []struct {
		name, target, stage string
		panics              bool
	}{
		{"a failed stage", down.URL, "http_status", false},
		{"a panic", "panics://data", "panic", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receiver := newLogReceiver(t)
			server := tracesServer(t, receiver, time.Hour, func(c *model.Config) {
				c.OTLP.Traces.Errors = true
				if tc.panics {
					c.Collectors[0] = panickingCollector(t, "text")
				}
				c.Collectors[0].ReportFailures = true
			}, nil)
			if got := probeOnce(t, server, probeOf("text", tc.target), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "http_exporter_probe_success 0\n") {
				t.Fatalf("the probe: %d %s", got.Code, got.Body)
			}
			exportTracesNow(server)
			var root *receivedSpan
			spans := receiver.spans(t)
			for i := range spans {
				if spans[i].Name == "probe text" {
					root = &spans[i]
				}
			}
			if root == nil {
				t.Fatalf("no probe span was exported: %+v", spans)
			}
			attrs := spanAttributes(root.otlpSpan)
			if root.Status == nil || root.Status.Code != otlpStatusError || attrs["error.type"] != tc.stage || attrs["http.response.status_code"] != "200" {
				t.Errorf("the probe's span: %+v; want error.type %q", root.otlpSpan, tc.stage)
			}
		})
	}
}

// A debug probe of a collector with report_failures says that a probe whose
// decode stage failed and was carried on past, with the last good result
// standing in for it, is answered that result, marked stale, with success
// 0 and the stage, as the probe is; not the failure's own answer.
func TestADebugProbeSaysAStaleResultStandsInUnderReportFailures(t *testing.T) {
	testutil.CaptureLogs(t)
	var body atomic.Value
	body.Store(`{"v": 3}`)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(target.Close)
	c := staleCollector(time.Millisecond, time.Hour)
	c.Decoder.Type, c.Transform.Type = "json", "jq"
	c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: ".v"}}
	c.ErrorHandling.OnDecodeError = model.ErrorPolicyLog
	c.ReportFailures = true
	server := modeServer(t, c)
	if got := probeOnce(t, server, probeOf("flaky", target.URL), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "demo_value 3\n") {
		t.Fatalf("the first probe: %d %s", got.Code, got.Body)
	}
	ageEntries(server, 5*time.Second)
	body.Store("not json")
	server.SetDebug(DebugFull)
	report := probeOnce(t, server, probeOf("flaky", target.URL)+"&debug=true", nil).Body.String()
	want := "A probe would have answered 200 with the last good result, 5s old, marked stale (cache.stale_if_error), with http_exporter_probe_success 0 and the decode stage failed (report_failures)."
	if !strings.Contains(report, want) {
		t.Errorf("the debug report lacks %q:\n%s", want, report)
	}
	for _, series := range []string{"demo_value 3", "http_exporter_result_stale 1", "http_exporter_probe_success 0", `http_exporter_probe_failed_stage{stage="decode"} 1`} {
		if !strings.Contains(report, series) {
			t.Errorf("the debug report lacks the series %s:\n%s", series, report)
		}
	}
}
