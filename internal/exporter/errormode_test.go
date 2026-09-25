package exporter

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// error_mode decides what a metric rule does when it cannot produce its value:
//
//   - ignore drops the metric and carries on, so the probe serves every metric
//     that could be extracted, or an empty response when none could;
//   - log does the same and logs why;
//   - fail logs why and fails the whole probe with a JSON error, so a response
//     is either complete or an error and never quietly partial.
//
// These tests pin that contract end to end, through the probe handler, because
// that is where the difference between the modes is visible to a scraper.

// jsonTarget serves a document with one field present and one absent, so a
// collector can declare a metric that works beside one that cannot.
func jsonTarget(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"up":1,"temperature":21.5}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// modeCollector declares demo_up, which the target always provides, and
// demo_latency_seconds, which it never does, the latter with the given mode.
func modeCollector(name, mode string) model.Collector {
	return model.Collector{
		Name:          name,
		Request:       model.RequestConfig{Type: fetch.RequestTypeHTTP, Method: "GET"},
		Decoder:       model.DecoderConfig{Type: "json"},
		Transform:     model.TransformConfig{Type: "jq"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
		Limits:        model.Limits{MaxResponseBytes: 4096, MaxMetrics: 10},
		Metrics: []model.MetricRule{
			{Name: "demo_up", Type: model.GaugeMetricType, Expression: ".up", ErrorMode: model.ErrorModeLog},
			{Name: "demo_latency_seconds", Type: model.GaugeMetricType, Expression: ".latency", ErrorMode: mode},
		},
	}
}

// modeServer validates the configuration exactly as startup would, and builds
// the server on the current default logger so testutil.CaptureLogs sees its lines.
func modeServer(t *testing.T, collectors ...model.Collector) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: collectors}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

func probe(t *testing.T, server *Server, target, collector string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+target+"&collector="+collector, nil))
	return recorder
}

// extractionFailures counts the per-rule log lines, which log and fail write
// and ignore does not.
func extractionFailures(t *testing.T, records []map[string]any) int {
	t.Helper()
	n := 0
	for _, record := range records {
		if record["msg"] == "metric extraction failed" {
			n++
		}
	}
	return n
}

func logRecords(t *testing.T, out interface{ String() string }) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		records = append(records, record)
	}
	return records
}

func TestErrorModesThroughAProbe(t *testing.T) {
	tests := []struct {
		mode         string
		wantStatus   int
		wantUp       bool // demo_up served in the exposition
		wantLogLines int  // "metric extraction failed" lines
	}{
		{mode: model.ErrorModeIgnore, wantStatus: http.StatusOK, wantUp: true, wantLogLines: 0},
		{mode: model.ErrorModeLog, wantStatus: http.StatusOK, wantUp: true, wantLogLines: 1},
		// fail's one line is the probe's failure, which names the metric
		// (TestARuleFailureIsLoggedOnce).
		{mode: model.ErrorModeFail, wantStatus: http.StatusBadGateway, wantUp: false, wantLogLines: 0},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			out := testutil.CaptureLogs(t)
			target := jsonTarget(t, nil)
			server := modeServer(t, modeCollector("modes", test.mode))

			response := probe(t, server, target.URL, "modes")
			body := response.Body.String()
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.wantStatus, body)
			}
			if got := strings.Contains(body, "demo_up 1"); got != test.wantUp {
				t.Errorf("demo_up served=%v, want %v; body=%s", got, test.wantUp, body)
			}
			// The failing metric never appears, whatever the mode.
			if strings.Contains(body, "demo_latency_seconds") && !strings.HasPrefix(strings.TrimSpace(body), "{") {
				t.Errorf("the failing metric was served: %s", body)
			}
			if got := extractionFailures(t, logRecords(t, out)); got != test.wantLogLines {
				t.Errorf("logged %d extraction failures, want %d:\n%s", got, test.wantLogLines, out.String())
			}
		})
	}
}

// One failing rule is enough. The other metrics were extracted perfectly well,
// and fail still refuses to serve them: a partial response is exactly what the
// mode exists to rule out.
func TestFailServesNoMetricsWhenAnyRuleFails(t *testing.T) {
	testutil.CaptureLogs(t)
	target := jsonTarget(t, nil)
	server := modeServer(t, modeCollector("strict", model.ErrorModeFail))

	response := probe(t, server, target.URL, "strict")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", response.Code)
	}
	if strings.Contains(response.Body.String(), "# TYPE") {
		t.Fatalf("a failed probe must not carry exposition text: %s", response.Body.String())
	}
}

// The error is JSON a person or a script can read without the exporter's log to
// hand: which collector, which rule, which target and why.
func TestFailRespondsWithAJSONError(t *testing.T) {
	testutil.CaptureLogs(t)
	target := jsonTarget(t, nil)
	server := modeServer(t, modeCollector("strict", model.ErrorModeFail))

	response := probe(t, server, target.URL, "strict")
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, response.Body.String())
	}
	for key, want := range map[string]string{
		"status":    "error",
		"stage":     "metric",
		"collector": "strict",
		"metric":    "demo_latency_seconds",
		"target":    target.URL,
	} {
		if body[key] != want {
			t.Errorf("%s=%v, want %q", key, body[key], want)
		}
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "demo_latency_seconds") || !strings.Contains(message, "missing") {
		t.Errorf("error=%q should say which metric is missing", message)
	}
}

// The target is echoed back to whoever sent the probe, but credentials in it
// must not be: the body can end up in a proxy's log or a ticket.
func TestFailErrorDoesNotEchoTargetCredentials(t *testing.T) {
	testutil.CaptureLogs(t)
	target := jsonTarget(t, nil)
	server := modeServer(t, modeCollector("strict", model.ErrorModeFail))
	withCredentials := strings.Replace(target.URL, "http://", "http://operator:s3cret@", 1)

	response := probe(t, server, withCredentials, "strict")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", response.Code)
	}
	if strings.Contains(response.Body.String(), "s3cret") {
		t.Fatalf("the error body leaked the target password: %s", response.Body.String())
	}
}

// ignore and log serve what they can. When that is nothing at all, the probe
// still succeeds with an empty body rather than turning into an error.
func TestIgnoreAndLogServeAnEmptyResponseWhenNothingIsExtracted(t *testing.T) {
	for _, mode := range []string{model.ErrorModeIgnore, model.ErrorModeLog} {
		t.Run(mode, func(t *testing.T) {
			testutil.CaptureLogs(t)
			target := jsonTarget(t, nil)
			c := modeCollector("empty", mode)
			c.Metrics[0].Expression = ".absent"
			c.Metrics[0].ErrorMode = mode
			server := modeServer(t, c)

			response := probe(t, server, target.URL, "empty")
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200; body=%s", response.Code, response.Body.String())
			}
			if body := strings.TrimSpace(response.Body.String()); body != "" {
				t.Fatalf("body=%q, want an empty response", body)
			}
		})
	}
}

// Modes are per rule. A rule that is allowed to fail does not drag down a probe
// whose strict rules all succeed.
func TestModesApplyPerRule(t *testing.T) {
	testutil.CaptureLogs(t)
	target := jsonTarget(t, nil)
	c := modeCollector("mixed", model.ErrorModeLog)
	c.Metrics[0].ErrorMode = model.ErrorModeFail // demo_up: strict, and present
	server := modeServer(t, c)

	response := probe(t, server, target.URL, "mixed")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "demo_up 1") {
		t.Fatalf("the strict rule's metric is missing: %s", response.Body.String())
	}
}

// A rule marked optional is not failing when its value is absent: there is
// nothing to handle, so no mode applies, fail included.
func TestAnOptionalRuleIsNeverAFailure(t *testing.T) {
	optional := false
	for name, adjust := range map[string]func(*model.Collector){
		"required: false":    func(c *model.Collector) { c.Metrics[1].Required = &optional },
		"allow_missing_keys": func(c *model.Collector) { c.ErrorHandling.AllowMissingKeys = true },
	} {
		t.Run(name, func(t *testing.T) {
			out := testutil.CaptureLogs(t)
			target := jsonTarget(t, nil)
			c := modeCollector("optional", model.ErrorModeFail)
			adjust(&c)
			server := modeServer(t, c)

			response := probe(t, server, target.URL, "optional")
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200; body=%s", response.Code, response.Body.String())
			}
			if got := extractionFailures(t, logRecords(t, out)); got != 0 {
				t.Fatalf("an absent optional value was logged as a failure:\n%s", out.String())
			}
		})
	}
}

// on_transform_error covers the transform as a whole. A rule that asks for the
// probe to fail is more specific, so a lenient collector policy does not turn
// its failure back into a quiet, partial success.
func TestFailTakesPrecedenceOverALenientTransformPolicy(t *testing.T) {
	for _, policy := range []string{"ignore", "log"} {
		t.Run(policy, func(t *testing.T) {
			testutil.CaptureLogs(t)
			target := jsonTarget(t, nil)
			c := modeCollector("lenient", model.ErrorModeFail)
			c.ErrorHandling.OnTransformError = policy
			server := modeServer(t, c)

			response := probe(t, server, target.URL, "lenient")
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status=%d, want 502 despite on_transform_error=%s", response.Code, policy)
			}
			if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("Content-Type=%q, want the JSON error", response.Header().Get("Content-Type"))
			}
		})
	}
}

// Only complete results are cached, so a failed probe goes back to the target
// next time instead of replaying the failure for the cache interval.
func TestFailIsNotCached(t *testing.T) {
	testutil.CaptureLogs(t)
	var hits atomic.Int64
	target := jsonTarget(t, &hits)
	c := modeCollector("cached", model.ErrorModeFail)
	c.Cache.TTL = model.Duration(time.Minute)
	c.Limits.MaxCacheEntries = 10
	server := modeServer(t, c)

	for i := 0; i < 2; i++ {
		if response := probe(t, server, target.URL, "cached"); response.Code != http.StatusBadGateway {
			t.Fatalf("probe %d status=%d, want 502", i+1, response.Code)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("target was contacted %d times, want 2: a failure must not be served from the cache", got)
	}
}

// The self-metrics describe a fail the way they describe any failed probe.
func TestFailIsCountedAsAFailedProbe(t *testing.T) {
	testutil.CaptureLogs(t)
	target := jsonTarget(t, nil)
	server := modeServer(t, modeCollector("counted", model.ErrorModeFail))
	probe(t, server, target.URL, "counted")

	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_scrapes_total{collector="counted"}`:          1,
		`http_exporter_scrape_success_total{collector="counted"}`:   0,
		`http_exporter_transform_errors_total{collector="counted"}`: 1,
		`http_exporter_missing_keys_total{collector="counted"}`:     1,
	} {
		if got := metricValue(t, exposition, series); got != want {
			t.Errorf("%s=%v, want %v", series, got, want)
		}
	}
}

// A static target has no HTTP response to carry an error in. fail there
// means the scrape exports nothing but the health metric saying it failed, and
// log means it exports what it could.
func TestErrorModesOnAStaticTarget(t *testing.T) {
	for mode, wantUp := range map[string]float64{model.ErrorModeFail: 0, model.ErrorModeLog: 1} {
		t.Run(mode, func(t *testing.T) {
			testutil.CaptureLogs(t)
			target := jsonTarget(t, nil)
			cfg := &model.Config{Collectors: []model.Collector{modeCollector("scheduled", mode)}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
			file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "one", Collector: "scheduled", Target: target.URL}}}
			server := newStaticServer(t, cfg, file)

			server.scrapeStaticTargets(context.Background(), 10*time.Second)
			resources := server.drainOTLP()
			if len(resources) != 1 {
				t.Fatalf("resources=%d", len(resources))
			}
			up := metricByName(resources[0].Set, "http_exporter_target_up")
			if up == nil || up.Value != wantUp {
				t.Fatalf("http_exporter_target_up=%+v, want %v", up, wantUp)
			}
			served := metricByName(resources[0].Set, "demo_up") != nil
			if served != (mode == model.ErrorModeLog) {
				t.Fatalf("demo_up exported=%v under %s", served, mode)
			}
		})
	}
}

// A required label a series has no value for fails the probe under
// error_mode fail, naming the label, and is counted as a missing key.
func TestAMissingRequiredLabelFailsTheProbe(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "v=1 a\nv=2\n")
	c := regexCollector("tenants")
	c.Metrics[0].Expression = `v=(\d+)(?: (?P<who>\S+))?`
	c.Metrics[0].ErrorMode = model.ErrorModeFail
	c.Metrics[0].Labels[0].Required = true
	server, _ := newCacheTestServer(t, c)
	recorder := probeOnce(t, server, "/probe?collector=tenants&target="+url.QueryEscape(target.URL), nil)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), `label \"who\" is missing`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_missing_keys_total{collector="tenants"}`); got != 1 {
		t.Fatalf("missing keys %v, want 1", got)
	}
}

// Rules that carry on without some series, under log or ignore, are counted:
// each series in http_exporter_rule_failures_total by metric, and missing
// values in http_exporter_missing_keys_total too. A rule that never failed
// has its series at zero, and the probe still answers with what worked.
func TestRuleFailuresThatCarryOnAreCounted(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"a","v":1},{"name":"b"},{"name":"c"},{"name":"d","v":4}]`))
	}))
	t.Cleanup(target.Close)
	c := model.Collector{
		Name: "rows", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{
			{Name: "logged_value", Type: model.GaugeMetricType, Items: ".[]", Expression: ".v", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{{Name: "row", Expression: ".name"}}},
			{Name: "ignored_value", Type: model.GaugeMetricType, Items: ".[]", Expression: ".v", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{{Name: "row", Expression: ".name"}}},
			{Name: "rows", Type: model.GaugeMetricType, Expression: "length", ErrorMode: model.ErrorModeLog},
		},
	}
	server, _ := newCacheTestServer(t, c)
	response := probeOnce(t, server, "/probe?collector=rows&target="+url.QueryEscape(target.URL), nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `logged_value{row="a"} 1`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	exposition := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_rule_failures_total{collector="rows",metric="logged_value"}`:  2,
		`http_exporter_rule_failures_total{collector="rows",metric="ignored_value"}`: 2,
		`http_exporter_rule_failures_total{collector="rows",metric="rows"}`:          0,
		`http_exporter_missing_keys_total{collector="rows"}`:                         4,
		`http_exporter_transform_errors_total{collector="rows"}`:                     0,
	} {
		if got := seriesValue(t, exposition, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

// A rule under log that fails on every scrape of a target is one failing
// thing: logged once, at warning level, with the target, then only as a
// repeat, and its recovery once it produces its series again. Under fail, the
// probe's failure, which names the metric, is the only line.
func TestARuleFailureIsLoggedOnce(t *testing.T) {
	t.Run("log", func(t *testing.T) {
		out := testutil.CaptureLogs(t)
		var missing atomic.Bool
		missing.Store(true)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if missing.Load() {
				_, _ = w.Write([]byte(`{"up":1}`))
				return
			}
			_, _ = w.Write([]byte(`{"up":1,"latency":0.5}`))
		}))
		t.Cleanup(target.Close)
		server := modeServer(t, modeCollector("modes", model.ErrorModeLog))
		for i := 0; i < 3; i++ {
			if response := probe(t, server, target.URL, "modes"); response.Code != http.StatusOK {
				t.Fatalf("status=%d", response.Code)
			}
		}
		records := logRecords(t, out)
		if got := extractionFailures(t, records); got != 1 {
			t.Fatalf("three failing scrapes logged %d extraction failures, want 1:\n%s", got, out.String())
		}
		for _, record := range records {
			if record["msg"] != "metric extraction failed" {
				continue
			}
			for key, want := range map[string]any{"level": "WARN", "collector": "modes", "target": target.URL, "metric": "demo_latency_seconds", "error_mode": model.ErrorModeLog} {
				if record[key] != want {
					t.Errorf("%s=%v, want %v", key, record[key], want)
				}
			}
		}
		// The log says it once; the self-metrics still count every
		// failed series.
		if got := server.statsFor("modes").ruleFailureCount("demo_latency_seconds"); got != 3 {
			t.Errorf("rule failures=%d, want 3", got)
		}

		missing.Store(false)
		probe(t, server, target.URL, "modes")
		probe(t, server, target.URL, "modes")
		recovered := 0
		for _, record := range logRecords(t, out) {
			if record["msg"] == "metric extraction recovered" {
				recovered++
				if record["target"] != target.URL || record["metric"] != "demo_latency_seconds" {
					t.Errorf("recovery line %v", record)
				}
			}
		}
		if recovered != 1 {
			t.Fatalf("logged %d recoveries, want 1:\n%s", recovered, out.String())
		}
	})
	t.Run("fail", func(t *testing.T) {
		out := testutil.CaptureLogs(t)
		target := jsonTarget(t, nil)
		server := modeServer(t, modeCollector("strict", model.ErrorModeFail))
		probe(t, server, target.URL, "strict")
		records := logRecords(t, out)
		naming := 0
		for _, record := range records {
			if strings.Contains(jsonText(t, record), "demo_latency_seconds") {
				naming++
				if record["msg"] != "probe failed" || record["metric"] != "demo_latency_seconds" || record["target"] != target.URL {
					t.Errorf("the failure is logged as %v", record)
				}
			}
		}
		if naming != 1 {
			t.Fatalf("the failing rule is in %d log lines, want 1:\n%s", naming, out.String())
		}
	})
}

func jsonText(t *testing.T, record map[string]any) string {
	t.Helper()
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
