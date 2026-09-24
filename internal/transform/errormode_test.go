package transform

import (
	"errors"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The line from the report: a metric rule with error_mode `log` failing inside
// a transform, which reports through the default logger.
func TestMetricExtractionErrorIsLoggedAsJSON(t *testing.T) {
	out := testutil.CaptureLogs(t)
	collector := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "exchange_rates"}
	rule := model.MetricRule{Name: "exchange_rate_observation_timestamp_seconds", ErrorMode: "log"}
	if !handleMetricError(collector, rule, errors.New(`metric "exchange_rate_observation_timestamp_seconds" value is missing`)) {
		t.Fatal("an error_mode of log should continue the scrape")
	}
	records := testutil.AssertJSONLines(t, out, 1)
	if records[0]["msg"] != "metric extraction failed" {
		t.Fatalf("msg=%v", records[0]["msg"])
	}
	if records[0]["metric"] != rule.Name {
		t.Fatalf("the failing rule should be named: %v", records[0]["metric"])
	}
	// A metric name is not unique across collectors, so the line has to say
	// which collector to go and look at.
	if records[0]["collector"] != collector.Name {
		t.Fatalf("the collector should be named: %v", records[0]["collector"])
	}
	if records[0]["level"] != "ERROR" {
		t.Fatalf("level=%v, want ERROR", records[0]["level"])
	}
}

// ignore must stay silent, so turning a rule to `ignore` really does stop the
// noise.
func TestIgnoreErrorModeLogsNothing(t *testing.T) {
	out := testutil.CaptureLogs(t)
	collector := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "quiet"}
	if !handleMetricError(collector, model.MetricRule{Name: "ignored", ErrorMode: model.ErrorModeIgnore}, errors.New("boom")) {
		t.Fatal("ignore should continue the scrape")
	}
	testutil.AssertJSONLines(t, out, 0)
}

// fail stops the scrape, and says why in the same line log mode writes: a rule
// that fails reads the same in the log whichever of the two modes it has, and
// the line is there whether the failure surfaced on a probe or on a scheduled
// target, which has no HTTP response to put it in.
func TestFailErrorModeLogsAndStops(t *testing.T) {
	out := testutil.CaptureLogs(t)
	collector := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Name: "strict"}
	if handleMetricError(collector, model.MetricRule{Name: "failing", ErrorMode: model.ErrorModeFail}, errors.New("boom")) {
		t.Fatal("fail should stop the scrape")
	}
	records := testutil.AssertJSONLines(t, out, 1)
	for key, want := range map[string]string{
		"msg":        "metric extraction failed",
		"collector":  "strict",
		"metric":     "failing",
		"error_mode": "fail",
		"error":      "boom",
		"level":      "ERROR",
	} {
		if records[0][key] != want {
			t.Errorf("%s=%v, want %q", key, records[0][key], want)
		}
	}
}

// A logging path must never be the thing that crashes a scrape, so an absent
// collector degrades to an empty name rather than a nil dereference.
func TestMetricErrorWithoutACollectorStillLogs(t *testing.T) {
	out := testutil.CaptureLogs(t)
	if !handleMetricError(nil, model.MetricRule{Name: "orphan", ErrorMode: "log"}, errors.New("boom")) {
		t.Fatal("log mode should continue the scrape")
	}
	records := testutil.AssertJSONLines(t, out, 1)
	if records[0]["collector"] != "" {
		t.Fatalf("collector=%v, want an empty name", records[0]["collector"])
	}
}
