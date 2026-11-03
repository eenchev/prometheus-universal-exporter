//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// reportingCollector is a regex collector of demo_value with report_failures.
func reportingCollector(name string) model.Collector {
	c := testutil.Collector(name, "text")
	c.ReportFailures = true
	return c
}

// elapsedRE reads http_exporter_probe_elapsed_seconds from an answer.
var elapsedRE = regexp.MustCompile(`(?m)^http_exporter_probe_elapsed_seconds ([0-9.e+-]+)$`)

// A probe of a collector with report_failures is answered 200 whatever
// becomes of it: a success with the collector's series first and
// http_exporter_probe_success 1 and http_exporter_probe_elapsed_seconds
// after them; a target that refuses the connection, answers 503, sends what
// the decoder cannot read, or lacks a value a rule with error_mode fail
// needs, with success 0, the elapsed time and
// http_exporter_probe_failed_stage naming the stage, http, http_status,
// decode or metric, and nothing of the error or of the target's answer. A
// collector without it answers each failure 502 as before. The failure is
// logged and counted in the self-metrics as before.
func TestReportFailuresAnswersAFailedProbeWithSeries(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	up := textTarget(t, "value=42\n")
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret-body", http.StatusServiceUnavailable)
	}))
	t.Cleanup(unavailable.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"secret-value": [`)
	}))
	t.Cleanup(broken.Close)

	reporting := reportingCollector("uptime")
	decoding := reportingCollector("decoding")
	decoding.Decoder.Type, decoding.Transform.Type = "json", "jq"
	decoding.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: ".x"}}
	strict := reportingCollector("strict")
	strict.Metrics[0].ErrorMode = model.ErrorModeFail
	plain := testutil.Collector("plain", "text")
	server := modeServer(t, reporting, decoding, strict, plain)

	got := probeOnce(t, server, probeOf("uptime", up.URL), nil)
	body := got.Body.String()
	own, success := strings.Index(body, "demo_value 42\n"), strings.Index(body, "http_exporter_probe_success 1\n")
	if got.Code != http.StatusOK || own < 0 || success < own || !elapsedRE.MatchString(body) || strings.Contains(body, "failed_stage") {
		t.Fatalf("a success: %d %s", got.Code, body)
	}
	for _, failure := range []struct {
		collector, target, stage string
	}{
		{"uptime", gone.URL, "http"},
		{"uptime", unavailable.URL, "http_status"},
		{"decoding", broken.URL, "decode"},
		{"strict", textTarget(t, "no value here\n").URL, "metric"},
	} {
		got := probeOnce(t, server, probeOf(failure.collector, failure.target), nil)
		body := got.Body.String()
		want := []string{
			"# TYPE http_exporter_probe_success gauge\nhttp_exporter_probe_success 0\n",
			fmt.Sprintf("http_exporter_probe_failed_stage{stage=%q} 1\n", failure.stage),
		}
		for _, w := range want {
			if got.Code != http.StatusOK || !strings.Contains(body, w) {
				t.Errorf("%s of %s: %d %s; want %q", failure.collector, failure.stage, got.Code, body, w)
			}
		}
		if m := elapsedRE.FindStringSubmatch(body); m == nil || m[1] == "0" {
			t.Errorf("%s of %s: no elapsed time: %s", failure.collector, failure.stage, body)
		}
		// The elapsed time's digits may hold any number.
		rest := elapsedRE.ReplaceAllString(body, "")
		for _, withheld := range []string{"secret", "503", "refused", "127.0.0.1", "demo_value", "error"} {
			if strings.Contains(rest, withheld) {
				t.Errorf("%s of %s answers %q: %s", failure.collector, failure.stage, withheld, body)
			}
		}
	}
	for _, target := range []string{gone.URL, unavailable.URL} {
		if got := probeOnce(t, server, probeOf("plain", target), nil); got.Code != http.StatusBadGateway {
			t.Errorf("without report_failures, %s: %d %s", target, got.Code, got.Body)
		}
	}
	// Logged as a failed probe always was, the stage and the error with it.
	if text := logs.String(); !strings.Contains(text, `"msg":"probe failed","collector":"uptime"`) || !strings.Contains(text, `"stage":"http_status"`) || !strings.Contains(text, "secret-body") {
		t.Errorf("the failures were not logged as before: %s", text)
	}
	self := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_scrape_success_total{collector="uptime"} 1`,
		`http_exporter_scrapes_total{collector="uptime"} 3`,
		`http_exporter_scrape_http_status_code{collector="uptime"} 503`,
		`http_exporter_parse_errors_total{collector="decoding"} 1`,
		`http_exporter_scrape_success_total{collector="decoding"} 0`,
		`http_exporter_probe_duration_seconds_count{collector="uptime",kind="probe"} 3`,
	} {
		if !strings.Contains(self, want) {
			t.Errorf("the self-metrics lack %s", want)
		}
	}
}

// What the target was not asked for answers as before: a target the
// collector's target policy refuses is answered 403, and a probe the
// concurrency limit turns away 503.
func TestReportFailuresLeavesARefusedOrRejectedProbeAsItWas(t *testing.T) {
	testutil.CaptureLogs(t)
	guarded := reportingCollector("guarded")
	guarded.Request.AllowedTargets = []string{"example.invalid"}
	single := reportingCollector("single")
	single.MaxConcurrentProbes = 1
	server := modeServer(t, guarded, single)
	if got := probeOnce(t, server, probeOf("guarded", textTarget(t, "value=1\n").URL), nil); got.Code != http.StatusForbidden || strings.Contains(got.Body.String(), "probe_success") {
		t.Errorf("a refused target: %d %s", got.Code, got.Body)
	}
	held := newHeldTarget(t)
	busy := make(chan int, 1)
	go func() { busy <- probeOnce(t, server, probeOf("single", held.server.URL+"/a"), nil).Code }()
	testutil.WaitFor(t, "the collector's slot to be taken", func() bool { return server.trips.count("single") == 1 })
	if got := probeOnce(t, server, probeOf("single", held.server.URL+"/b"), nil); got.Code != http.StatusServiceUnavailable {
		t.Errorf("a rejected probe: %d %s", got.Code, got.Body)
	}
	held.open()
	if code := <-busy; code != http.StatusOK {
		t.Errorf("the probe that held the slot: %d", code)
	}
}

// error_handling log carries on past a failed stage as before, and
// report_failures says the stage failed, success 0 and the stage, and
// counts the probe a failure in the self-metrics. A response cache answers a hit with success 1,
// and keeps the collector's series alone. cache.stale_if_error answers a
// failed trip with the last good result, marked stale, and success 0 with
// the stage that failed. Each probe answers its own elapsed time, the one
// its probe duration histogram observes.
func TestReportFailuresWithCarryingOnCachingAndStaleResults(t *testing.T) {
	testutil.CaptureLogs(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	carrying := reportingCollector("carrying")
	carrying.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	cached := reportingCollector("cached")
	cached.Cache.TTL = model.Duration(time.Hour)
	flaky := staleCollector(time.Millisecond, time.Hour)
	flaky.ReportFailures = true
	server := modeServer(t, carrying, cached, flaky)

	got := probeOnce(t, server, probeOf("carrying", gone.URL), nil)
	if body := got.Body.String(); got.Code != http.StatusOK || !strings.Contains(body, "http_exporter_probe_success 0\n") || !strings.Contains(body, `http_exporter_probe_failed_stage{stage="http"} 1`) {
		t.Errorf("carried on: %d %s", got.Code, body)
	}
	if self := selfMetrics(t, server); !strings.Contains(self, `http_exporter_scrape_success_total{collector="carrying"} 0`) {
		t.Errorf("a probe that carried on under report_failures is counted a success")
	}

	up := textTarget(t, "value=7\n")
	for range 2 {
		got := probeOnce(t, server, probeOf("cached", up.URL), nil)
		if body := got.Body.String(); got.Code != http.StatusOK || !strings.Contains(body, "demo_value 7\nhttp_exporter_probe_success 1\n") && !strings.Contains(body, "demo_value 7\n# HELP http_exporter_probe_success") {
			t.Errorf("cached: %d %s", got.Code, body)
		}
	}
	for _, entry := range server.cache.entries {
		for _, m := range entry.set.Metrics {
			if strings.HasPrefix(m.Name, "http_exporter_probe_") {
				t.Errorf("the cache keeps %s", m.Name)
			}
		}
	}

	stable, target := newFlakyTarget(t)
	stable.value.Store(3)
	if got := probeOnce(t, server, probeOf("flaky", target.URL), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "http_exporter_probe_success 1\n") {
		t.Fatalf("the first probe: %d %s", got.Code, got.Body)
	}
	ageEntries(server, time.Second)
	stable.mode.Store("status")
	got = probeOnce(t, server, probeOf("flaky", target.URL), nil)
	for _, want := range []string{"demo_value 3\n", "http_exporter_result_stale 1\n", "http_exporter_probe_success 0\n", `http_exporter_probe_failed_stage{stage="http_status"} 1`} {
		if !strings.Contains(got.Body.String(), want) || got.Code != http.StatusOK {
			t.Errorf("stale: %d %s; want %q", got.Code, got.Body, want)
		}
	}
}

// The answer is written in the format the scrape asks for, OpenMetrics with
// its # EOF among them, and a HEAD probe is answered 200, as a GET.
func TestReportFailuresAnswersEveryFormat(t *testing.T) {
	testutil.CaptureLogs(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	server := modeServer(t, reportingCollector("uptime"))
	got := probeOnce(t, server, probeOf("uptime", gone.URL), http.Header{"Accept": {"application/openmetrics-text;version=1.0.0"}})
	if body := got.Body.String(); got.Code != http.StatusOK || !strings.HasPrefix(got.Header().Get("Content-Type"), "application/openmetrics-text") || !strings.Contains(body, `http_exporter_probe_failed_stage{stage="http"} 1`) || !strings.HasSuffix(body, "# EOF\n") {
		t.Errorf("OpenMetrics: %d %q %s", got.Code, got.Header().Get("Content-Type"), body)
	}
	head := httptest.NewRecorder()
	server.Handler().ServeHTTP(head, httptest.NewRequest(http.MethodHead, probeOf("uptime", gone.URL), nil))
	if head.Code != http.StatusOK || !strings.HasPrefix(head.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("HEAD: %d %q", head.Code, head.Header())
	}
}

// The probe series of a shared trip are each probe's own: writing one
// probe's answer leaves the trip's result as it was for the others.
func TestReportedAnswersLeaveTheSharedResultAsItWas(t *testing.T) {
	c := reportingCollector("uptime")
	set := model.MetricSet{Metrics: make([]model.Metric, 1, 8)}
	set.Metrics[0] = model.Metric{Name: "demo_value", Type: model.GaugeMetricType, Value: 1}
	result := &probeResult{status: http.StatusOK, metrics: &set, ok: true}
	for elapsed := range 3 {
		w := httptest.NewRecorder()
		writeProbeAnswer(w, httptest.NewRequest(http.MethodGet, "/probe", nil), &c, result, time.Duration(elapsed)*time.Second, false)
		if want := fmt.Sprintf("http_exporter_probe_elapsed_seconds %d\n", elapsed); !strings.Contains(w.Body.String(), want) || strings.Count(w.Body.String(), "\nhttp_exporter_probe_success 1\n") != 1 {
			t.Errorf("probe %d: %s; want %q", elapsed, w.Body, want)
		}
	}
	if len(set.Metrics) != 1 || set.Metrics[:2][1].Name != "" {
		t.Errorf("the shared result was changed: %+v", set.Metrics[:2])
	}
}

// The status page shows a failed probe of a collector with report_failures
// as failed, with its error, and answered 200 with its three series; a debug
// probe says it would have been answered 200 with success 0 in place of 502,
// and shows the series.
func TestReportFailuresOnTheStatusPageAndInADebugProbe(t *testing.T) {
	testutil.CaptureLogs(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	server := modeServer(t, reportingCollector("uptime"))
	server.SetDebug(DebugFull)
	probeOnce(t, server, probeOf("uptime", gone.URL), nil)
	row, ok := probeRows(t, server)["uptime "+gone.URL]
	if !ok || row.Result != probeFailed || row.Status != http.StatusOK || row.Series != 3 || row.LastError == "" {
		t.Errorf("the status page's row: %+v", row)
	}
	report := probeOnce(t, server, probeOf("uptime", gone.URL)+"&debug=true", nil).Body.String()
	for _, want := range []string{"A probe would have answered 200 with http_exporter_probe_success 0 and the http stage failed (report_failures), in place of 502: collector uptime http failed", `http_exporter_probe_failed_stage{stage="http"} 1`} {
		if !strings.Contains(report, want) {
			t.Errorf("the debug report lacks %q:\n%s", want, report)
		}
	}
}

// A series a collector with report_failures makes on the scrape under a
// name of the probe's own, which no rule names, fails the validation stage,
// which the answer reports; a collector without it answers the series.
func TestAProbeSeriesNameMadeOnTheScrapeFailsValidation(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "# TYPE http_exporter_probe_success gauge\nhttp_exporter_probe_success 1\nother 2\n")
	passing := reportingCollector("passing")
	passing.Decoder.Type, passing.Transform.Type, passing.Metrics = "prometheus", "prometheus", nil
	plain := passing
	plain.Name, plain.ReportFailures = "plain", false
	server := modeServer(t, passing, plain)
	got := probeOnce(t, server, probeOf("passing", target.URL), nil)
	if body := got.Body.String(); got.Code != http.StatusOK || !strings.Contains(body, "http_exporter_probe_success 0\n") || !strings.Contains(body, `http_exporter_probe_failed_stage{stage="validation"} 1`) || strings.Contains(body, "other 2") {
		t.Errorf("with report_failures: %d %s", got.Code, body)
	}
	if got := probeOnce(t, server, probeOf("plain", target.URL), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "http_exporter_probe_success 1\n") {
		t.Errorf("without it: %d %s", got.Code, got.Body)
	}
}

// The OTLP export of a probe is the collector's series as before, without
// the probe's own: a success exports demo_value alone, and a failure
// nothing. A static target of a collector with report_failures is scraped
// as before: a failure is http_exporter_target_up 0, with no probe series.
func TestReportFailuresLeavesTheOTLPExportAndStaticTargetsAsTheyWere(t *testing.T) {
	testutil.CaptureLogs(t)
	up := textTarget(t, "value=5\n")
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	cfg := &model.Config{Collectors: []model.Collector{reportingCollector("uptime")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "down", Collector: "uptime", Target: gone.URL}}}
	server := newStaticServer(t, cfg, file)
	if got := probeOnce(t, server, probeOf("uptime", up.URL), nil); !strings.Contains(got.Body.String(), "http_exporter_probe_success 1\n") {
		t.Fatalf("the probe: %d %s", got.Code, got.Body)
	}
	queued := server.drainOTLP()
	if len(queued) != 1 || metricByName(queued[0].Set, "demo_value") == nil || len(queued[0].Set.Metrics) != 1 {
		t.Errorf("a success queued %+v for OTLP, want demo_value alone", queued)
	}
	if got := probeOnce(t, server, probeOf("uptime", gone.URL), nil); !strings.Contains(got.Body.String(), "http_exporter_probe_success 0\n") {
		t.Fatalf("the failed probe: %d %s", got.Code, got.Body)
	}
	if queued := server.drainOTLP(); len(queued) != 0 {
		t.Errorf("a failure queued %+v for OTLP", queued)
	}
	server.scrapeStaticTargets(t.Context(), 0)
	if body := getStaticTargets(t, server, "/static-targets"); !strings.Contains(body, `http_exporter_target_up{collector="uptime",static_target="down",target="`+gone.URL+`"} 0`) || strings.Contains(body, "http_exporter_probe_") {
		t.Errorf("the static target: %s", body)
	}
}
