package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Anything consuming these logs parses them, so one differently shaped line is
// not a cosmetic problem: it is a line the consumer drops or chokes on. The
// failure this guards against is a log written through slog's default logger
// rather than the exporter's own, which without newLogger installing the
// default would come out as `2026/09/18 21:43:35 ERROR ...` in the middle of an
// otherwise JSON stream.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var out bytes.Buffer
	newLogger("info", &out)
	return &out
}

// assertJSONLines fails unless every line is a JSON object carrying the fields
// slog's JSON handler produces.
func assertJSONLines(t *testing.T, out *bytes.Buffer, wantLines int) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("this log line is not JSON:\n%s\n%v", line, err)
		}
		for _, field := range []string{"time", "level", "msg"} {
			if _, ok := record[field]; !ok {
				t.Errorf("log line %q has no %q field", line, field)
			}
		}
		records = append(records, record)
	}
	if len(records) != wantLines {
		t.Fatalf("captured %d log lines, want %d:\n%s", len(records), wantLines, out.String())
	}
	return records
}

// The line from the report: a metric rule with error_mode `log` failing inside
// a transform, which reports through the default logger.
func TestMetricExtractionErrorIsLoggedAsJSON(t *testing.T) {
	out := captureLogs(t)
	collector := &Collector{Name: "exchange_rates"}
	rule := MetricRule{Name: "exchange_rate_observation_timestamp_seconds", ErrorMode: "log"}
	if !handleMetricError(collector, rule, errors.New(`metric "exchange_rate_observation_timestamp_seconds" value is missing`)) {
		t.Fatal("an error_mode of log should continue the scrape")
	}
	records := assertJSONLines(t, out, 1)
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

// The other error modes must stay silent, so turning a rule to `ignore` really
// does stop the noise.
func TestOtherErrorModesLogNothing(t *testing.T) {
	out := captureLogs(t)
	collector := &Collector{Name: "quiet"}
	if !handleMetricError(collector, MetricRule{Name: "ignored", ErrorMode: "ignore"}, errors.New("boom")) {
		t.Fatal("ignore should continue the scrape")
	}
	if handleMetricError(collector, MetricRule{Name: "failing", ErrorMode: "fail"}, errors.New("boom")) {
		t.Fatal("fail should stop the scrape")
	}
	assertJSONLines(t, out, 0)
}

// A logger obtained from newLogger and slog's default must be the same
// handler, so it cannot matter which one a given call site reaches for.
func TestTheDefaultLoggerIsTheExportersLogger(t *testing.T) {
	out := captureLogs(t)
	slog.Default().Info("through the default logger", "which", "default")
	slog.Info("through the package function", "which", "package")
	records := assertJSONLines(t, out, 2)
	for _, record := range records {
		if record["which"] == nil {
			t.Fatalf("attributes were dropped: %v", record)
		}
	}
}

// A logging path must never be the thing that crashes a scrape, so an absent
// collector degrades to an empty name rather than a nil dereference.
func TestMetricErrorWithoutACollectorStillLogs(t *testing.T) {
	out := captureLogs(t)
	if !handleMetricError(nil, MetricRule{Name: "orphan", ErrorMode: "log"}, errors.New("boom")) {
		t.Fatal("log mode should continue the scrape")
	}
	records := assertJSONLines(t, out, 1)
	if records[0]["collector"] != "" {
		t.Fatalf("collector=%v, want an empty name", records[0]["collector"])
	}
}

func TestLogLevelIsHonoured(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var out bytes.Buffer
	logger := newLogger("error", &out)
	logger.Info("suppressed")
	slog.Default().Warn("also suppressed")
	logger.Error("kept")
	records := assertJSONLines(t, &out, 1)
	if records[0]["msg"] != "kept" {
		t.Fatalf("msg=%v, want only the error line", records[0]["msg"])
	}
}

// The watch interval bounds how stale a running configuration can be, so it
// belongs in the startup line whenever the watch is on.
func TestWatchIntervalIsReported(t *testing.T) {
	manager, _ := watchedManager(t)
	if manager.WatchEnabled() {
		t.Fatal("the watch should start disabled")
	}
	if manager.WatchInterval() != 0 {
		t.Fatalf("interval=%s with the watch off, want zero", manager.WatchInterval())
	}
	manager.SetWatchInterval(90 * time.Second)
	if !manager.WatchEnabled() || manager.WatchInterval() != 90*time.Second {
		t.Fatalf("enabled=%v interval=%s", manager.WatchEnabled(), manager.WatchInterval())
	}
	if got := manager.WatchInterval().String(); got != "1m30s" {
		t.Fatalf("the logged interval is %q, which should be a readable duration", got)
	}
}

// HELP is what a reader sees in Grafana's metric browser or in `curl` output,
// so sixteen identical lines are as good as none. This keeps every family
// described, distinctly, and keeps the descriptor list in step with what is
// actually exposed.
func TestEverySelfMetricHasItsOwnDescription(t *testing.T) {
	seen := map[string]string{}
	for _, d := range selfMetricDescriptors {
		if strings.TrimSpace(d.Help) == "" {
			t.Errorf("%s has no description", d.Name)
			continue
		}
		if strings.EqualFold(strings.TrimSpace(d.Help), "Exporter self metric.") {
			t.Errorf("%s still carries the placeholder description", d.Name)
		}
		if !strings.HasSuffix(d.Help, ".") {
			t.Errorf("%s: %q should read as a sentence", d.Name, d.Help)
		}
		if other, duplicate := seen[d.Help]; duplicate {
			t.Errorf("%s and %s share the description %q", other, d.Name, d.Help)
		}
		seen[d.Help] = d.Name
		if selfMetricHelp[d.Name] != d.Help {
			t.Errorf("%s: the indexed help does not match the descriptor", d.Name)
		}
	}
	if len(selfMetricDescriptors) != len(selfMetricNames()) {
		t.Fatal("the name list and the descriptors disagree")
	}
}

// The descriptors and the exposition must not drift: a family exposed without a
// descriptor loses its description, and a descriptor for a family nothing emits
// documents something that does not exist.
func TestSelfMetricDescriptionsMatchTheExposition(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server := verboseServer(t, false, testCollector("described", "text"))
	probeOnce(t, server, "/probe?target="+target.URL+"&collector=described", nil)
	exposition := selfMetrics(t, server)

	for _, d := range selfMetricDescriptors {
		if !strings.Contains(exposition, "# HELP "+d.Name+" "+d.Help+"\n") {
			t.Errorf("%s is not published with its description", d.Name)
		}
	}
	if strings.Contains(exposition, "Exporter self metric") {
		t.Errorf("the placeholder description is still being served:\n%s", exposition)
	}
	// Every family the endpoint declares has a descriptor behind it.
	for _, line := range strings.Split(exposition, "\n") {
		if !strings.HasPrefix(line, "# HELP http_exporter_") {
			continue
		}
		name := strings.Fields(line)[2]
		if _, ok := selfMetricHelp[name]; ok {
			continue
		}
		// The families verbose mode adds carry their own help inline.
		if strings.HasPrefix(name, "http_exporter_request_") ||
			name == "http_exporter_collector_config_valid" || name == "http_exporter_scheduled_targets" {
			continue
		}
		t.Errorf("%s is exposed but has no descriptor", name)
	}
}

// The self-metrics delivered over OTLP carry the same descriptions as the text
// exposition, so a dashboard built on either reads the same.
func TestSelfMetricSetCarriesTheSameDescriptions(t *testing.T) {
	server := verboseServer(t, false, testCollector("described", "text"))
	set := server.selfMetricSet()
	found := map[string]bool{}
	for _, metric := range set.Metrics {
		help, ok := selfMetricHelp[metric.Name]
		if !ok {
			continue
		}
		found[metric.Name] = true
		if metric.Help != help {
			t.Errorf("%s: help=%q, want %q", metric.Name, metric.Help, help)
		}
	}
	for _, d := range selfMetricDescriptors {
		if !found[d.Name] {
			t.Errorf("%s is missing from the self-metric set", d.Name)
		}
	}
}
