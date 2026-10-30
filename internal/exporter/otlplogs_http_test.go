//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The exporter's own log over OTLP (otlplogs.go): the records a collector
// receives, the level they are taken from, the bound on what waits, the
// export's own lines kept out of it, the last export at shutdown, a reload
// switching it, and its self-metrics and status.

// logReceiver is an OTLP/HTTP collector that keeps every request's path
// and decoded body, answering each with status, or 200.
type logReceiver struct {
	server *httptest.Server
	mu     sync.Mutex
	status int
	paths  []string
	bodies [][]byte
}

func newLogReceiver(t *testing.T) *logReceiver {
	t.Helper()
	r := &logReceiver{status: http.StatusOK}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := readOTLPBody(t, req)
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.bodies = append(r.bodies, body)
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *logReceiver) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// logsPayload is an ExportLogsServiceRequest as OTLP/JSON writes it.
type logsPayload struct {
	ResourceLogs []struct {
		Resource  otlpResource `json:"resource"`
		ScopeLogs []struct {
			Scope      otlpScope       `json:"scope"`
			LogRecords []otlpLogRecord `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

// logRequests are the requests made to /v1/logs, decoded.
func (r *logReceiver) logRequests(t *testing.T) []logsPayload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []logsPayload
	for i, path := range r.paths {
		if path != "/v1/logs" {
			continue
		}
		var payload logsPayload
		if err := json.Unmarshal(r.bodies[i], &payload); err != nil {
			t.Fatalf("a log export does not decode: %v\n%s", err, r.bodies[i])
		}
		out = append(out, payload)
	}
	return out
}

// logRecords are the records of every request made to /v1/logs.
func (r *logReceiver) logRecords(t *testing.T) []otlpLogRecord {
	t.Helper()
	var out []otlpLogRecord
	for _, payload := range r.logRequests(t) {
		for _, resource := range payload.ResourceLogs {
			for _, scope := range resource.ScopeLogs {
				out = append(out, scope.LogRecords...)
			}
		}
	}
	return out
}

// logExportServer is a server whose OTLP export, metrics and logs, goes to
// the receiver, with change made to its otlp block, and the logger that
// goes through its tee, which writes to stderr at --log.level info.
func logExportServer(t *testing.T, receiver *logReceiver, change func(*model.OTLPConfig)) (*Server, *slog.Logger, *lockedBuffer) {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(receiver.server.URL + "/v1/metrics")}
	cfg.OTLP.Compression = model.OTLPCompressionNone
	cfg.OTLP.Logs.Enabled = true
	if change != nil {
		change(&cfg.OTLP)
	}
	server := newStaticServer(t, cfg, nil)
	stderr := &lockedBuffer{}
	logs := NewOTLPLogs(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server.SetOTLPLogs(logs)
	logger := slog.New(logs.Handler())
	server.logger = logger
	return server, logger, stderr
}

func exportLogsNow(server *Server) {
	server.exportOTLPLogs(context.Background(), server.manager.Get().OTLP, time.Minute, false)
}

// attributesOf is a record's attributes by key.
func attributesOf(record otlpLogRecord) map[string]otlpAnyValue {
	out := map[string]otlpAnyValue{}
	for _, a := range record.Attributes {
		out[a.Key] = a.Value
	}
	return out
}

func text(v otlpAnyValue) string {
	if v.StringValue == nil {
		return "<not a string>"
	}
	return *v.StringValue
}

type logValuer struct{}

func (logValuer) LogValue() slog.Value { return slog.StringValue("resolved") }

// A line goes to the collector the metrics go to, at /v1/logs, under the
// exporter-wide resource, as a record with the line's time, the time it
// was taken, the OTLP severity of its level and the level's name, its
// message as the body, and its attributes, each of its type, a group's under
// the group's name and a dot. stderr has the line as before.
func TestTheExportersLogArrivesOverOTLP(t *testing.T) {
	receiver := newLogReceiver(t)
	server, logger, stderr := logExportServer(t, receiver, nil)
	before := time.Now()
	logger.With("component", "probe").WithGroup("trip").Warn("target answered slowly",
		"attempt", 3, "ratio", 0.5, "cached", false, "took", 1500*time.Millisecond, "error", errors.New("boom"),
		"big", uint64(1<<63), "empty", "", "resolved", logValuer{}, slog.Group("request", "method", "GET", slog.Group("", "inline", "x")))
	logger.Error("probe failed")
	logger.Debug("not taken at info")
	exportLogsNow(server)

	requests := receiver.logRequests(t)
	if len(requests) != 1 || len(requests[0].ResourceLogs) != 1 {
		t.Fatalf("the log export made %d requests, want 1 of one resource: %+v", len(requests), requests)
	}
	resource := requests[0].ResourceLogs[0]
	if got, want := resource.Resource.Attributes, defaultResourceIdentity(server.manager.Get().OTLP).attributes(); len(got) != 2 || got[0].Key != "service.name" || got[0].Value.StringValue != "prometheus-universal-exporter" || got[1] != want[1] {
		t.Errorf("the resource is %+v, want the metrics' %+v", got, want)
	}
	if len(resource.ScopeLogs) != 1 || resource.ScopeLogs[0].Scope.Name != "prometheus-universal-exporter" {
		t.Fatalf("the scope is %+v", resource.ScopeLogs)
	}
	records := resource.ScopeLogs[0].LogRecords
	if len(records) != 2 {
		t.Fatalf("%d records, want the warning and the error, not the debug line: %+v", len(records), records)
	}
	warning := records[0]
	if text(warning.Body) != "target answered slowly" || warning.SeverityNumber != 13 || warning.SeverityText != "WARN" {
		t.Errorf("the warning is %+v", warning)
	}
	if records[1].SeverityNumber != 17 || records[1].SeverityText != "ERROR" || text(records[1].Body) != "probe failed" {
		t.Errorf("the error is %+v", records[1])
	}
	logged, err1 := strconv.ParseInt(warning.TimeUnixNano, 10, 64)
	observed, err2 := strconv.ParseInt(warning.ObservedTimeUnixNano, 10, 64)
	if err1 != nil || err2 != nil || logged < before.UnixNano() || observed < logged || observed > time.Now().UnixNano() {
		t.Errorf("the record's times are %q and %q, want the line's time and the time it was taken, since %d", warning.TimeUnixNano, warning.ObservedTimeUnixNano, before.UnixNano())
	}
	attrs := attributesOf(warning)
	for key, want := range map[string]string{"component": "probe", "trip.took": "1.5s", "trip.error": "boom", "trip.big": "9223372036854775808", "trip.empty": "", "trip.resolved": "resolved", "trip.request.method": "GET", "trip.request.inline": "x"} {
		if got, ok := attrs[key]; !ok || text(got) != want {
			t.Errorf("attribute %s is %+v, want the string %q", key, got, want)
		}
	}
	if got := attrs["trip.attempt"]; got.IntValue != "3" || got.StringValue != nil {
		t.Errorf("trip.attempt is %+v, want the int 3", got)
	}
	if got := attrs["trip.ratio"]; got.DoubleValue == nil || *got.DoubleValue != 0.5 {
		t.Errorf("trip.ratio is %+v, want the double 0.5", got)
	}
	if got := attrs["trip.cached"]; got.BoolValue == nil || *got.BoolValue {
		t.Errorf("trip.cached is %+v, want the bool false", got)
	}
	if len(attrs) != 11 {
		t.Errorf("the warning has %d attributes, want 11: %+v", len(attrs), warning.Attributes)
	}
	lines := stderr.String()
	if !strings.Contains(lines, `"msg":"target answered slowly"`) || !strings.Contains(lines, `"msg":"probe failed"`) || strings.Contains(lines, "not taken at info") {
		t.Errorf("stderr is not what it was without the export:\n%s", lines)
	}
	if got := seriesValue(t, selfMetrics(t, server), "http_exporter_otlp_log_records_sent_total"); got != 2 {
		t.Errorf("http_exporter_otlp_log_records_sent_total is %v, want 2", got)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_otlp_log_exports_total{result="success"}`); got != 1 {
		t.Errorf("http_exporter_otlp_log_exports_total success is %v, want 1", got)
	}
	// Nothing waits, and nothing is sent when nothing waits.
	exportLogsNow(server)
	if got := len(receiver.logRequests(t)); got != 1 {
		t.Errorf("an export with nothing waiting made a request: %d", got)
	}
}

// otlp.logs.level is the least level exported, whatever --log.level writes
// to stderr: below it a line is not taken, and at debug a line stderr does
// not write is.
func TestOTLPLogsLevelIsTheLeastExported(t *testing.T) {
	for level, want := range map[string][]string{"debug": {"d", "i", "w", "e"}, "info": {"i", "w", "e"}, "warn": {"w", "e"}, "ERROR": {"e"}} {
		receiver := newLogReceiver(t)
		server, logger, stderr := logExportServer(t, receiver, func(o *model.OTLPConfig) { o.Logs.Level = level })
		logger.Debug("d")
		logger.Info("i")
		logger.Warn("w")
		logger.Error("e")
		exportLogsNow(server)
		var got []string
		for _, record := range receiver.logRecords(t) {
			got = append(got, text(record.Body))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("level %s exports %v, want %v", level, got, want)
		}
		if strings.Contains(stderr.String(), `"msg":"d"`) {
			t.Errorf("level %s: stderr at info has the debug line", level)
		}
	}
}

// A line that finds otlp.logs.max_pending_records waiting is dropped and
// counted, without waiting: the records kept are the first, and the self
// metric and the status page count the rest.
func TestOTLPLogsDropWhatFindsTheQueueFull(t *testing.T) {
	receiver := newLogReceiver(t)
	server, logger, stderr := logExportServer(t, receiver, func(o *model.OTLPConfig) { o.Logs.MaxPendingRecords = 2 })
	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		logger.Info(msg)
	}
	if got := strings.Count(stderr.String(), "\n"); got != 5 {
		t.Errorf("stderr has %d lines, want every one of the 5", got)
	}
	metrics := selfMetrics(t, server)
	if got := seriesValue(t, metrics, "http_exporter_otlp_log_records_dropped_total"); got != 3 {
		t.Errorf("http_exporter_otlp_log_records_dropped_total is %v, want 3", got)
	}
	logs := otlpDetails(t, server).Logs
	if !logs.Enabled || logs.Pending != 2 || logs.MaxPending != 2 || logs.Dropped != 3 || logs.DroppedBy.PendingCap != 3 {
		t.Errorf("the status page's log export is %+v", logs.OTLPLogsStatusDetails)
	}
	requireContains(t, getPage(t, server, "/static-targets/status"), `<h3 id="otlp-logs">Log export</h3>`, "log lines at info and above, exported every 5s", "2 log records of at most 2", "3 log records<br><span class=\"muted\">3 over the pending bound, 0 refused or rejected, 0 of the last export at shutdown")
	exportLogsNow(server)
	var got []string
	for _, record := range receiver.logRecords(t) {
		got = append(got, text(record.Body))
	}
	if strings.Join(got, ",") != "one,two" {
		t.Errorf("the export carried %v, want the two queued first", got)
	}
}

// The export's own lines are written to stderr alone, never queued: a
// collector that keeps failing, with the log export taking debug lines and
// every attempt retried, leaves only the lines the exporter logged
// otherwise waiting, however many exports fail; and the failure is logged
// once, then as a repeat, not at every export. When the collector is back,
// what it receives has none of the export's lines.
func TestTheOTLPLogExportDoesNotExportItsOwnLines(t *testing.T) {
	saved := otlpRetryBackoff
	otlpRetryBackoff = time.Millisecond
	t.Cleanup(func() { otlpRetryBackoff = saved })
	receiver := newLogReceiver(t)
	receiver.answer(http.StatusServiceUnavailable)
	server, logger, stderr := logExportServer(t, receiver, func(o *model.OTLPConfig) { o.Logs.Level = "debug" })
	logger.Info("the line to deliver")
	for range 5 {
		server.exportOTLPLogs(context.Background(), server.manager.Get().OTLP, 300*time.Millisecond, false)
		if got := len(server.otlpLogs.queue); got != 1 {
			t.Fatalf("after a failed export %d records wait, want the 1 logged", got)
		}
	}
	if got := strings.Count(stderr.String(), `"msg":"OTLP log export failed; its log records are kept for the next export; this line is not exported"`); got != 1 {
		t.Errorf("the failure is logged %d times at warn, want once:\n%s", got, stderr)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_otlp_log_exports_total{result="failure"}`); got != 5 {
		t.Errorf("http_exporter_otlp_log_exports_total failure is %v, want 5", got)
	}
	logs := otlpDetails(t, server).Logs
	if !logs.Failing || logs.Failures != 5 || !strings.Contains(logs.LastError, "503") || logs.LastResult != statusFailed {
		t.Errorf("the status page's log export is %+v", logs.OTLPLogsStatusDetails)
	}
	// Readiness does not depend on it.
	if code, _ := ready(t, server); code != http.StatusOK {
		t.Errorf("/ready answers %d while log exports fail", code)
	}
	receiver.answer(http.StatusOK)
	exportLogsNow(server)
	var got []string
	for _, record := range receiver.logRecords(t) {
		got = append(got, text(record.Body))
	}
	// Every request carried the one line, and nothing else.
	for _, body := range got {
		if body != "the line to deliver" {
			t.Errorf("the collector received %q, a line of the export's own", body)
		}
	}
	if !strings.Contains(stderr.String(), "OTLP log export recovered") {
		t.Errorf("the recovery is not logged:\n%s", stderr)
	}
	if server.otlpLogs.queue != nil {
		t.Errorf("%d records wait after the export that got through", len(server.otlpLogs.queue))
	}
}

// A request the collector refuses is dropped and counted, not sent again;
// one it accepts while rejecting some records counts those as dropped.
func TestOTLPLogsRefusedAndRejectedAreDropped(t *testing.T) {
	receiver := newLogReceiver(t)
	receiver.answer(http.StatusBadRequest)
	server, logger, _ := logExportServer(t, receiver, nil)
	logger.Info("refused")
	exportLogsNow(server)
	if got := len(server.otlpLogs.queue); got != 0 {
		t.Errorf("%d refused records wait", got)
	}
	if got := otlpDetails(t, server).Logs.DroppedBy.Refused; got != 1 {
		t.Errorf("%d records counted refused, want 1", got)
	}
	if got := partialSuccessOf([]byte(`{"partialSuccess":{"rejectedLogRecords":"2","errorMessage":"too old"}}`), "application/json", true); got.rejected != 2 || got.message != "too old" {
		t.Errorf("a log export's partial success reads as %+v", got)
	}
	if got := partialSuccessOf([]byte(`{"partialSuccess":{"rejectedDataPoints":"2"}}`), "application/json", true); got.rejected != 0 {
		t.Errorf("a log export's partial success reads rejected data points: %+v", got)
	}
}

// An export of more than otlp.batch_max_size records is sent in requests of
// at most that many, in order.
func TestOTLPLogsAreSentInBatches(t *testing.T) {
	receiver := newLogReceiver(t)
	server, logger, _ := logExportServer(t, receiver, func(o *model.OTLPConfig) { o.BatchMaxSize = 2 })
	for _, msg := range []string{"a", "b", "c", "d", "e"} {
		logger.Info(msg)
	}
	exportLogsNow(server)
	requests := receiver.logRequests(t)
	if len(requests) != 3 {
		t.Fatalf("%d requests, want 3 of at most 2 records", len(requests))
	}
	var got []string
	for _, record := range receiver.logRecords(t) {
		got = append(got, text(record.Body))
	}
	if strings.Join(got, "") != "abcde" {
		t.Errorf("the requests carried %v", got)
	}
}

// The last export at shutdown sends the records waiting after the metrics'
// last export, the line that announces it among them, within otlp.timeout;
// nothing is taken after it.
func TestTheLastOTLPExportSendsTheLogsWaiting(t *testing.T) {
	receiver := newLogReceiver(t)
	server, logger, _ := logExportServer(t, receiver, func(o *model.OTLPConfig) { o.Logs.Interval = model.Duration(time.Hour) })
	logger.Info("before the shutdown")
	server.FlushOTLP()
	var got []string
	for _, record := range receiver.logRecords(t) {
		got = append(got, text(record.Body))
	}
	if strings.Join(got, "|") != "before the shutdown|sending the last OTLP export before exiting" {
		t.Errorf("the last log export carried %v", got)
	}
	receiver.mu.Lock()
	metricsFirst := len(receiver.paths) == 2 && receiver.paths[0] == "/v1/metrics" && receiver.paths[1] == "/v1/logs"
	receiver.mu.Unlock()
	if !metricsFirst {
		t.Errorf("the last exports were made to %v, want the metrics' then the logs'", receiver.paths)
	}
	logger.Info("after the last export")
	if got := len(server.otlpLogs.queue); got != 0 {
		t.Errorf("%d records were taken after the last export", got)
	}
}

// A last export at shutdown that fails drops its records and counts them:
// the line logged, and the two the metrics' last export logged, announcing
// it and its failure.
func TestAFailedLastOTLPLogExportDropsItsRecords(t *testing.T) {
	receiver := newLogReceiver(t)
	receiver.answer(http.StatusServiceUnavailable)
	server, logger, stderr := logExportServer(t, receiver, func(o *model.OTLPConfig) { o.Timeout = model.Duration(200 * time.Millisecond) })
	logger.Info("lost")
	server.FlushOTLP()
	if got := otlpDetails(t, server).Logs.DroppedBy.Shutdown; got != 3 {
		t.Errorf("%d records counted dropped at shutdown, want the 3 logged", got)
	}
	if !strings.Contains(stderr.String(), "the last OTLP log export before exiting failed") {
		t.Errorf("the failure is not logged:\n%s", stderr)
	}
}

// A reload that switches otlp.logs off stops the tee taking lines and drops
// what waits; one that switches it on again starts it; the status page and
// the self-metrics follow.
func TestAReloadSwitchesTheOTLPLogExport(t *testing.T) {
	receiver := newLogReceiver(t)
	document := func(logs string) string {
		return "otlp:\n  enabled: true\n  endpoint: " + receiver.server.URL + "/v1/metrics\n  compression: none\n  logs:\n    enabled: " + logs + "\n" + testutil.MinimalConfig
	}
	r := newReloadable(t, document("true"), "")
	r.server = NewServer(r.manager, "python3", testutil.QuietLogger(t))
	logs := NewOTLPLogs(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	r.server.SetOTLPLogs(logs)
	logger := slog.New(logs.Handler())
	logger.Info("while on")
	if got := len(logs.queue); got != 1 {
		t.Fatalf("%d records wait while the log export is on, want 1", got)
	}
	r.write(r.path, document("false"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	logger.Info("while off")
	if logs.queue != nil || logs.wants(slog.LevelError) {
		t.Errorf("switched off, the tee still takes lines: %d wait", len(logs.queue))
	}
	if section := otlpSection(t, r.server); section.Logs.Enabled {
		t.Errorf("the status page says the log export is on: %+v", section.Logs)
	}
	requireContains(t, getPage(t, r.server, "/static-targets/status"), "Log export is off.")
	if strings.Contains(selfMetrics(t, r.server), "http_exporter_otlp_log_") {
		t.Errorf("the log export's self-metrics are served while it is off")
	}
	r.write(r.path, document("true"))
	if err := r.manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	logger.Info("on again")
	r.server.exportOTLPLogs(context.Background(), r.manager.Get().OTLP, time.Minute, false)
	var got []string
	for _, record := range receiver.logRecords(t) {
		got = append(got, text(record.Body))
	}
	if strings.Join(got, "|") != "on again" {
		t.Errorf("after switching it off and on the collector received %v, want the line logged since", got)
	}
}

// The configuration's otlp.logs is read as validated: the endpoint made of
// the metrics', which the loop and the page use.
func TestOTLPLogsGoToTheMetricsCollector(t *testing.T) {
	cfg := otlpConfig("http://collector.invalid:4318/v1/metrics")
	cfg.Logs.Enabled = true
	full := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: cfg}
	if err := config.Validate(full); err != nil {
		t.Fatal(err)
	}
	if got := full.OTLP.Logs.Endpoint; got != "http://collector.invalid:4318/v1/logs" {
		t.Errorf("the logs endpoint is %q", got)
	}
}
