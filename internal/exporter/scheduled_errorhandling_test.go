package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// errorStageCase makes one stage of a collection fail: the target answers
// with status and body, and the collector is configured by setup.
type errorStageCase struct {
	name   string
	stage  string
	status int
	body   string
	setup  func(c *model.Collector, policy string)
}

var errorStageCases = []errorStageCase{
	{
		name: "fetch", stage: "http_status", status: http.StatusServiceUnavailable, body: "unavailable",
		setup: func(c *model.Collector, policy string) { c.ErrorHandling.OnFetchError = policy },
	},
	{
		name: "decode", stage: "decode", status: http.StatusOK, body: "{not json",
		setup: func(c *model.Collector, policy string) {
			c.Decoder.Type = "json"
			c.Transform.Type = "jq"
			c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: ".value"}}
			c.ErrorHandling.OnDecodeError = policy
		},
	},
	{
		// A regex transform over a JSON response fails in the transform
		// stage itself, before any metric rule runs.
		name: "transform", stage: "transform", status: http.StatusOK, body: `{"value": 1}`,
		setup: func(c *model.Collector, policy string) {
			c.Decoder.Type = "json"
			c.ErrorHandling.OnTransformError = policy
		},
	},
}

func errorStageTarget(t *testing.T, tc errorStageCase) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(tc.status)
		_, _ = w.Write([]byte(tc.body))
	}))
	t.Cleanup(target.Close)
	return target
}

// A scheduled target follows the collector's error_handling as a probe does:
// under log or ignore the failed stage is passed over, the target is up and
// nothing of the collector's is exported; under fail the scrape fails.
func TestScheduledTargetsFollowErrorHandling(t *testing.T) {
	for _, tc := range errorStageCases {
		for _, policy := range []string{model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
			t.Run(tc.name+"/"+policy, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				target := errorStageTarget(t, tc)
				collector := testutil.Collector("demo", "text")
				tc.setup(&collector, policy)
				cfg := &model.Config{Collectors: []model.Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
				file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "flaky", Collector: "demo", Target: target.URL}}}
				server := newScheduledServer(t, cfg, file)

				server.scrapeScheduledTargets(context.Background(), 10*time.Second)
				resources := server.drainOTLP()
				if len(resources) != 1 {
					t.Fatalf("resources=%d", len(resources))
				}
				up := metricByName(resources[0].Set, "http_exporter_target_up")
				wantUp := 1.0
				if policy == model.ErrorPolicyFail {
					wantUp = 0
				}
				if up == nil || up.Value != wantUp {
					t.Fatalf("target up=%+v, want %v", up, wantUp)
				}
				if metricByName(resources[0].Set, "demo_value") != nil {
					t.Fatalf("a failed stage must not export collector metrics: %+v", resources[0].Set.Metrics)
				}

				output := logs.String()
				carriedOn := strings.Count(output, "scheduled target stage failed; continuing")
				failed := strings.Count(output, "scheduled target scrape failed")
				switch policy {
				case model.ErrorPolicyFail:
					if failed != 1 || carriedOn != 0 {
						t.Fatalf("fail should log the failed scrape once:\n%s", output)
					}
				case model.ErrorPolicyLog:
					if carriedOn != 1 || failed != 0 || !strings.Contains(output, `"stage":"`+tc.stage+`"`) {
						t.Fatalf("log should warn once that the %s stage was passed over:\n%s", tc.stage, output)
					}
				case model.ErrorPolicyIgnore:
					if carriedOn != 0 || failed != 0 {
						t.Fatalf("ignore should log nothing at info or above:\n%s", output)
					}
				}

				stats := selfMetrics(t, server)
				wantSuccess := `http_exporter_scrape_success_total{collector="demo"} 1`
				if policy == model.ErrorPolicyFail {
					wantSuccess = `http_exporter_scrape_success_total{collector="demo"} 0`
				}
				if !strings.Contains(stats, wantSuccess) {
					t.Fatalf("self-metrics should show %s:\n%s", wantSuccess, stats)
				}
			})
		}
	}
}

// A probe and a scheduled scrape of the same failing target agree on whether
// it is up, whatever the policy.
func TestScheduledTargetsAndProbesAgreeOnErrorHandling(t *testing.T) {
	for _, tc := range errorStageCases {
		for _, policy := range []string{model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
			t.Run(tc.name+"/"+policy, func(t *testing.T) {
				testutil.CaptureLogs(t)
				target := errorStageTarget(t, tc)
				collector := testutil.Collector("demo", "text")
				tc.setup(&collector, policy)
				cfg := &model.Config{Collectors: []model.Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
				file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "flaky", Collector: "demo", Target: target.URL}}}
				server := newScheduledServer(t, cfg, file)

				probe := probeOnce(t, server, "/probe?collector=demo&target="+target.URL, nil)
				probeUp := probe.Code == http.StatusOK
				server.scrapeScheduledTargets(context.Background(), 10*time.Second)
				up := metricByName(server.drainOTLP()[0].Set, "http_exporter_target_up")
				if scheduledUp := up != nil && up.Value == 1; scheduledUp != probeUp {
					t.Fatalf("probe answered %d but the scheduled target is up=%v", probe.Code, scheduledUp)
				}
			})
		}
	}
}

// A metric rule with error_mode fail fails the scheduled scrape even when the
// collector would carry on past transform errors, as it fails a probe.
func TestScheduledMetricFailureFailsDespiteTransformPolicy(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("nothing to see\n"))
	}))
	defer target.Close()
	collector := testutil.Collector("demo", "text")
	collector.ErrorHandling.OnTransformError = model.ErrorPolicyLog
	collector.Metrics[0].ErrorMode = "fail"
	cfg := &model.Config{Collectors: []model.Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "strict", Collector: "demo", Target: target.URL}}}
	server := newScheduledServer(t, cfg, file)

	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	up := metricByName(server.drainOTLP()[0].Set, "http_exporter_target_up")
	if up == nil || up.Value != 0 {
		t.Fatalf("a metric rule with error_mode fail should fail the scrape: %+v", up)
	}
	if !strings.Contains(logs.String(), `"msg":"scheduled target scrape failed"`) || !strings.Contains(logs.String(), `"stage":"metric","metric":"demo_value"`) {
		t.Fatalf("the failure should name the metric, as a probe's does:\n%s", logs.String())
	}
	if probe := probeOnce(t, server, "/probe?collector=demo&target="+target.URL, nil); probe.Code == http.StatusOK {
		t.Fatalf("the probe should fail as well: %d", probe.Code)
	}
}

// A target that carries on past a failed stage is not "recovered" in the
// logs: only a scrape that went through whole ends a run of failures.
func TestScheduledCarryOnDoesNotLogRecovery(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	tc := errorStageCases[0]
	target := errorStageTarget(t, tc)
	collector := testutil.Collector("demo", "text")
	tc.setup(&collector, model.ErrorPolicyLog)
	cfg := &model.Config{Collectors: []model.Collector{collector}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.TargetFile{Targets: []model.ScheduledTarget{{Name: "flaky", Collector: "demo", Target: target.URL}}}
	server := newScheduledServer(t, cfg, file)

	for range 3 {
		server.scrapeScheduledTargets(context.Background(), 10*time.Second)
		_ = server.drainOTLP()
	}
	if strings.Contains(logs.String(), "scheduled target recovered") {
		t.Fatalf("carrying on past a failed stage is not a recovery:\n%s", logs.String())
	}
}
