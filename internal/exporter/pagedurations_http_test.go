//go:build !select_request_types || request_type_http

package exporter

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The status page writes a duration as pagekit.Duration does, every unit
// that is zero left out: a static target's interval, the OTLP exports'
// intervals and the trace threshold. The JSON keeps them as Go writes them.
func TestTheStatusPageWritesDurationsWithoutZeroUnits(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	cfg.OTLP.Logs = model.OTLPLogsConfig{Enabled: true, Endpoint: "http://collector.invalid/v1/logs", Level: "info", Interval: model.Duration(2 * time.Hour)}
	cfg.OTLP.Traces = model.OTLPTracesConfig{Enabled: true, Endpoint: "http://collector.invalid/v1/traces", Threshold: model.Duration(time.Minute), Interval: model.Duration(time.Hour + 30*time.Second)}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "a", Collector: "text", Target: "http://127.0.0.1:1/"}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.SetOTLPLogs(NewOTLPLogs(testutil.QuietLogger(t).Handler()))

	page := getPage(t, server, "/static-targets/status")
	requireIn := func(want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(page, w) {
				t.Errorf("the page does not have %q", w)
			}
		}
	}
	requireIn("<td>1m</td>", "Exported http/json every 1m:", "exported every 2h.", "took 1m or longer", "exported every 1h30s", "threshold 1m;")
	if strings.Contains(page, "m0s") || strings.Contains(page, "h0m") {
		t.Errorf("the page writes a zero unit:\n%s", page)
	}
	if row := statusJSON(t, server, "/static-targets/status").Rows[0]; row.Interval != "1m0s" {
		t.Errorf("the JSON's interval is %q, want Go's 1m0s", row.Interval)
	}
	if d := otlpDetails(t, server); d.Interval != "1m0s" || d.Logs.Interval != "2h0m0s" || d.Traces.Interval != "1h0m30s" || d.Traces.Threshold != "1m0s" {
		t.Errorf("the JSON's OTLP durations are %q, %q, %q and %q", d.Interval, d.Logs.Interval, d.Traces.Interval, d.Traces.Threshold)
	}
}
