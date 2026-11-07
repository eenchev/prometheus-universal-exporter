//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// recoveredLabel is the label above an error that a later success put
// behind.
const recoveredLabel = `<span class="recovered-label">Earlier failure, recovered since</span>`

// recoveredAt is the time line under a recovered error: when, and how long
// ago as the page writes an age.
var recoveredAt = regexp.MustCompile(`</div><span class="muted">at <time datetime="(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ)">\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ</time> \((\d+[hm])*\d+[hms] ago\)</span>`)

// isRecovered is a last_error_recovered given and true, isFailing one given
// and false: an error is there, and what failed has not succeeded since.
func isRecovered(p *bool) bool { return p != nil && *p }

func isFailing(p *bool) bool { return p != nil && !*p }

// shown is a last_error_recovered as a message writes it.
func shown(p *bool) string {
	if p == nil {
		return "left out"
	}
	return strconv.FormatBool(*p)
}

// statusPageRow is the row of the page's table that holds marker.
func statusPageRow(t *testing.T, page, marker string) string {
	t.Helper()
	for _, row := range regexp.MustCompile(`<tr[ >]`).Split(page, -1) {
		if strings.Contains(row, marker) {
			return row
		}
	}
	t.Fatalf("the page has no row with %q:\n%s", marker, page)
	return ""
}

// requireRecovered checks a cell shows its error as recovered: the label,
// the error muted, the time with its age; or, failing, as it always was.
func requireRecovered(t *testing.T, what, cell, errorText string, recovered bool) {
	t.Helper()
	escaped := templateEscaped(errorText)
	if recovered {
		if !strings.Contains(cell, recoveredLabel+`<div class="recovered">`+escaped+`</div>`) || !recoveredAt.MatchString(cell) {
			t.Errorf("%s does not show its error as recovered, muted under the label with the time and age:\n%s", what, cell)
		}
		return
	}
	if strings.Contains(cell, "recovered") || !strings.Contains(cell, escaped+`<br><span class="muted">at `) {
		t.Errorf("%s does not show its error as a failing one is:\n%s", what, cell)
	}
}

// A static target whose latest scrape succeeded after one that failed keeps
// the error, shown muted and said to be past, with when it was and how long
// ago; a target still failing shows its error as before. The JSON says
// which is which.
func TestTheStatusPageShowsAStaticTargetsRecoveredErrorMuted(t *testing.T) {
	flaky := newDeadTarget(t)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "recovered", Collector: "text", Target: flaky.URL + "/flaky"},
		{Name: "failing", Collector: "text", Target: flaky.URL + "/down"},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	scrapeAll := func() {
		for _, target := range server.manager.StaticTargets() {
			server.scrapeTarget(t.Context(), server.manager.Get(), target)
		}
	}
	scrapeAll()
	if row := statusRow(t, server, "recovered"); !isFailing(row.LastErrorRecovered) || row.LastError == "" {
		t.Fatalf("after a failed scrape the row is %+v; want its error, not recovered", row)
	}
	flaky.failing.Store(false)
	scrapeAll()

	recovered, failing := statusRow(t, server, "recovered"), statusRow(t, server, "failing")
	if recovered.Result != statusSuccess || recovered.Failures != 0 || recovered.LastError == "" || !isRecovered(recovered.LastErrorRecovered) {
		t.Errorf("the recovered target's row is %+v; want success, its error kept and last_error_recovered", recovered)
	}
	if failing.Result != statusFailed || failing.LastError == "" || !isFailing(failing.LastErrorRecovered) {
		t.Errorf("the failing target's row is %+v; want failed, its error not recovered", failing)
	}
	raw := statusJSONText(t, server)
	if strings.Count(raw, `"last_error_recovered":true`) != 1 || !strings.Contains(raw, `"last_error_recovered":false`) {
		t.Errorf("the JSON does not say last_error_recovered of each target:\n%s", raw)
	}
	page := getPage(t, server, "/static-targets/status")
	requireRecovered(t, "the recovered target's row", statusPageRow(t, page, "<strong>recovered</strong>"), recovered.LastError, true)
	requireRecovered(t, "the failing target's row", statusPageRow(t, page, "<strong>failing</strong>"), failing.LastError, false)
}

// A collector and target whose latest probe succeeded after one that failed
// shows the error as recovered in the recent probes; one whose latest probe
// failed shows it as before.
func TestTheRecentProbesShowARecoveredErrorMuted(t *testing.T) {
	flaky := newDeadTarget(t)
	server := modeServer(t, testutil.Collector("text", "text"))
	server.logger = testutil.QuietLogger(t)
	probeOnce(t, server, probeOf("text", flaky.URL+"/flaky"), nil)
	probeOnce(t, server, probeOf("text", flaky.URL+"/down"), nil)
	flaky.failing.Store(false)
	if r := probeOnce(t, server, probeOf("text", flaky.URL+"/flaky"), nil); r.Code != http.StatusOK {
		t.Fatalf("the probe after the target recovered answered %d: %s", r.Code, r.Body)
	}
	rows := statusJSON(t, server, "/static-targets/status").Probes
	var recovered, failing probeStatusRow
	for _, row := range rows {
		switch row.Target {
		case flaky.URL + "/flaky":
			recovered = row
		case flaky.URL + "/down":
			failing = row
		}
	}
	if recovered.Failing || recovered.LastError == "" || !isRecovered(recovered.LastErrorRecovered) {
		t.Errorf("the recovered probe's row is %+v; want its error kept and last_error_recovered", recovered)
	}
	if !failing.Failing || failing.LastError == "" || !isFailing(failing.LastErrorRecovered) {
		t.Errorf("the failing probe's row is %+v; want its error, not recovered", failing)
	}
	page := getPage(t, server, "/static-targets/status")
	requireRecovered(t, "the recovered probe's row", statusPageRow(t, page, flaky.URL+"/flaky"), recovered.LastError, true)
	requireRecovered(t, "the failing probe's row", statusPageRow(t, page, flaky.URL+"/down"), failing.LastError, false)
}

// otlpLastErrorCell is the Last error row of the OTLP section's table that
// follows heading on the page, or the first table's with heading empty.
func otlpLastErrorCell(t *testing.T, page, heading string) string {
	t.Helper()
	if heading != "" {
		_, page, _ = strings.Cut(page, heading)
	} else {
		_, page, _ = strings.Cut(page, `<h2 id="otlp">`)
	}
	return statusPageRow(t, page, "<th>Last error</th>")
}

// The metrics export's Last error, while the last export failed, shows as
// it always did; once an export gets through after it, as recovered.
func TestTheOTLPExportsRecoveredErrorIsShownMuted(t *testing.T) {
	fastRetries(t)
	var down atomic.Bool
	down.Store(true)
	endpoint := flakyOTLPEndpoint(t, &down)
	server := otlpServer(t, endpoint.URL)
	server.logger = testutil.QuietLogger(t)
	queueProbeMetric(server, "first_value", 1)
	server.exportOTLP(context.Background(), 300*time.Millisecond)
	d := otlpDetails(t, server)
	if !d.Failing || d.LastError == "" || !isFailing(d.LastErrorRecovered) {
		t.Fatalf("after a failed export the section is %+v; want failing with its error, not recovered", d)
	}
	requireRecovered(t, "the failing export's Last error", otlpLastErrorCell(t, getPage(t, server, "/static-targets/status"), ""), d.LastError, false)

	down.Store(false)
	server.exportOTLP(context.Background(), time.Minute)
	after := otlpDetails(t, server)
	if after.Failing || after.LastError != d.LastError || !isRecovered(after.LastErrorRecovered) {
		t.Errorf("after an export got through the section is %+v; want the error kept and last_error_recovered", after)
	}
	if raw := rawOTLPSection(t, server); string(raw["last_error_recovered"]) != "true" {
		t.Errorf("the JSON's otlp.last_error_recovered is %s, want true", raw["last_error_recovered"])
	}
	requireRecovered(t, "the recovered export's Last error", otlpLastErrorCell(t, getPage(t, server, "/static-targets/status"), ""), d.LastError, true)
}

// The log and trace exports' Last error rows follow the same rule: shown as
// recovered once an export got through after the failure, as before while
// the last export failed.
func TestTheLogAndTraceExportsRecoveredErrorsAreShownMuted(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	cfg.OTLP.Logs = model.OTLPLogsConfig{Enabled: true, Endpoint: "http://collector.invalid/v1/logs", Level: "info"}
	cfg.OTLP.Traces = model.OTLPTracesConfig{Enabled: true, Endpoint: "http://collector.invalid/v1/traces"}
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	logs := NewOTLPLogs(testutil.QuietLogger(t).Handler())
	server.SetOTLPLogs(logs)
	failed := time.Now().Add(-35 * time.Minute)
	logs.record(cfg.OTLP.Logs.Endpoint, failed, 3, 0, errors.New("the log endpoint refused it"))
	server.otlpTraces.record(cfg.OTLP.Traces.Endpoint, failed, 2, 0, errors.New("the trace endpoint refused it"))

	page := getPage(t, server, "/static-targets/status")
	requireRecovered(t, "the failing log export's Last error", otlpLastErrorCell(t, page, `<h3 id="otlp-logs">`), "the log endpoint refused it", false)
	requireRecovered(t, "the failing trace export's Last error", otlpLastErrorCell(t, page, `<h3 id="otlp-traces">`), "the trace endpoint refused it", false)
	if d := otlpDetails(t, server); !isFailing(d.Logs.LastErrorRecovered) || !isFailing(d.Traces.LastErrorRecovered) {
		t.Errorf("while the last exports failed the log export's last_error_recovered is %s, the trace export's %s; want false", shown(d.Logs.LastErrorRecovered), shown(d.Traces.LastErrorRecovered))
	}

	logs.record(cfg.OTLP.Logs.Endpoint, time.Now(), 1, 1, nil)
	server.otlpTraces.record(cfg.OTLP.Traces.Endpoint, time.Now(), 1, 1, nil)
	page = getPage(t, server, "/static-targets/status")
	logsCell, tracesCell := otlpLastErrorCell(t, page, `<h3 id="otlp-logs">`), otlpLastErrorCell(t, page, `<h3 id="otlp-traces">`)
	requireRecovered(t, "the recovered log export's Last error", logsCell, "the log endpoint refused it", true)
	requireRecovered(t, "the recovered trace export's Last error", tracesCell, "the trace endpoint refused it", true)
	for what, cell := range map[string]string{"log": logsCell, "trace": tracesCell} {
		if !strings.Contains(cell, "(35m ago)") {
			t.Errorf("the recovered %s export's error does not say it was 35m ago:\n%s", what, cell)
		}
	}
	if d := otlpDetails(t, server); !isRecovered(d.Logs.LastErrorRecovered) || !isRecovered(d.Traces.LastErrorRecovered) {
		t.Errorf("after exports got through the log export's last_error_recovered is %s, the trace export's %s; want true", shown(d.Logs.LastErrorRecovered), shown(d.Traces.LastErrorRecovered))
	}
}
