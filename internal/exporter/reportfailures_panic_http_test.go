//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// panickingCollector is a collector with report_failures whose request type
// panics on every fetch.
func panickingCollector(t *testing.T, name string) model.Collector {
	t.Helper()
	fetch.RequestTypes["panics"] = &fetch.RequestType{
		Name:         "panics",
		Fields:       []string{"path"},
		TargetFields: []string{"path"},
		Requester: fetchOnly(func(context.Context, string, *model.Collector, fetch.RequestOverrides, http.Header) (*fetch.HTTPResponse, error) {
			panic("boom")
		}),
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "panics") })
	c := testutil.Collector(name, "text")
	c.Request = model.RequestConfig{Type: "panics", Path: "/data"}
	c.ReportFailures = true
	return c
}

// A probe of a collector with report_failures that panics is answered 200
// with http_exporter_probe_success 0 and the panic stage, as the
// self-metrics count it a failure; a debug probe says the same.
func TestReportFailuresAnswersAPanickedProbeAFailure(t *testing.T) {
	testutil.CaptureLogs(t)
	server := modeServer(t, panickingCollector(t, "broken"))
	got := probeOnce(t, server, probeOf("broken", "panics://data"), nil)
	body := got.Body.String()
	for _, want := range []string{"http_exporter_probe_success 0\n", `http_exporter_probe_failed_stage{stage="panic"} 1`} {
		if got.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("the panicked probe: %d %s; want %q", got.Code, body, want)
		}
	}
	self := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_scrapes_total{collector="broken"} 1`, `http_exporter_scrape_success_total{collector="broken"} 0`} {
		if !strings.Contains(self, want) {
			t.Errorf("the self-metrics lack %s", want)
		}
	}
	server.SetDebug(DebugFull)
	report := probeOnce(t, server, probeOf("broken", "panics://data")+"&debug=true", nil).Body.String()
	for _, want := range []string{"A probe would have answered 200 with http_exporter_probe_success 0 and the panic stage failed (report_failures), in place of 500: probe failed: " + errScrapePanicked.Error(), `http_exporter_probe_failed_stage{stage="panic"} 1`} {
		if !strings.Contains(report, want) {
			t.Errorf("the debug report lacks %q:\n%s", want, report)
		}
	}
}
