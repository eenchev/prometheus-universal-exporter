package exporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Delivering OTLP exports: compression, retries, what happens to data that
// did not get through, the export status self-metrics, the last export at
// shutdown, readiness and the proxy from the environment (otlp.go,
// otlpstatus.go, readiness.go, fetch/transport.go).

// readOTLPBody reads an export request, gunzipping it when it says it is
// gzipped.
func readOTLPBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading the export: %v", err)
		return nil
	}
	if r.Header.Get("Content-Encoding") != "gzip" {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Errorf("the export says it is gzipped but is not: %v", err)
		return nil
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Errorf("gunzipping the export: %v", err)
	}
	return out
}

// otlpEndpoint answers each export with the next status of statuses, the last
// one repeating, and records what it received.
type otlpEndpoint struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	statuses []int
	header   http.Header
	requests int
	bodies   [][]byte
	encoding []string
}

func newOTLPEndpoint(t *testing.T, statuses ...int) *otlpEndpoint {
	t.Helper()
	e := &otlpEndpoint{t: t, statuses: statuses, header: http.Header{}}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readOTLPBody(t, r)
		e.mu.Lock()
		status := http.StatusOK
		if len(e.statuses) > 0 {
			status = e.statuses[min(e.requests, len(e.statuses)-1)]
		}
		e.requests++
		e.bodies = append(e.bodies, body)
		e.encoding = append(e.encoding, r.Header.Get("Content-Encoding"))
		for name, values := range e.header {
			w.Header()[name] = values
		}
		e.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *otlpEndpoint) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests
}

// delivered reports whether any export that got a 2xx carried the metric.
func (e *otlpEndpoint) received(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, body := range e.bodies {
		if strings.Contains(string(body), `"name":"`+name+`"`) {
			return true
		}
	}
	return false
}

func otlpServer(t *testing.T, endpoint string) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint + "/v1/metrics")}
	server := newScheduledServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	return server
}

// fastRetries makes the retry backoff short for the test.
func fastRetries(t *testing.T) {
	t.Helper()
	previous := otlpRetryBackoff
	otlpRetryBackoff = 10 * time.Millisecond
	t.Cleanup(func() { otlpRetryBackoff = previous })
}

func queueProbeMetric(server *Server, name string, value float64) {
	server.queueOTLP(model.MetricSet{Metrics: []model.Metric{{Name: name, Type: model.GaugeMetricType, Value: value}}})
}

func pendingValue(server *Server, name string) (float64, bool) {
	server.otlpMu.Lock()
	defer server.otlpMu.Unlock()
	for _, batch := range server.otlpPending {
		for _, pending := range batch.metrics {
			if pending.metric.Name == name {
				return pending.metric.Value, true
			}
		}
	}
	return 0, false
}

func TestOTLPExportsAreGzippedUnlessCompressionIsNone(t *testing.T) {
	endpoint := newOTLPEndpoint(t)
	server := otlpServer(t, endpoint.server.URL)
	if got := server.manager.Get().OTLP.Compression; got != model.OTLPCompressionGzip {
		t.Fatalf("otlp.compression defaults to %q, want gzip", got)
	}
	queueProbeMetric(server, "probe_value", 1)
	server.exportOTLP(context.Background(), 5*time.Second)
	server.manager.Get().OTLP.Compression = model.OTLPCompressionNone
	server.exportOTLP(context.Background(), 5*time.Second)

	if endpoint.count() != 2 || endpoint.encoding[0] != "gzip" || endpoint.encoding[1] != "" {
		t.Fatalf("Content-Encoding of the exports: %q", endpoint.encoding)
	}
	for i, body := range endpoint.bodies {
		var payload otlpPayload
		if err := json.Unmarshal(body, &payload); err != nil || len(payload.ResourceMetrics) == 0 {
			t.Fatalf("export %d does not decode: %v\n%s", i, err, body)
		}
	}
	if !endpoint.received("probe_value") {
		t.Fatal("the gzipped export lost the probe's metric")
	}
}

func TestOTLPCompressionIsValidated(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://otel:4318/v1/metrics")}
	cfg.OTLP.Compression = "zstd"
	if err := config.Validate(cfg); err == nil || !strings.Contains(err.Error(), "otlp.compression") {
		t.Fatalf("otlp.compression zstd: %v", err)
	}
}

// 429, 502, 503, 504 and network errors are retried; the export that gets
// through is one success, its retries counted.
func TestOTLPExportsRetryTransientFailures(t *testing.T) {
	fastRetries(t)
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		endpoint := newOTLPEndpoint(t, status, status, http.StatusOK)
		server := otlpServer(t, endpoint.server.URL)
		queueProbeMetric(server, "probe_value", 1)
		server.exportOTLP(context.Background(), 5*time.Second)
		if endpoint.count() != 3 {
			t.Fatalf("%d: %d attempts, want 3", status, endpoint.count())
		}
		exposition := selfMetrics(t, server)
		for series, want := range map[string]float64{
			`http_exporter_otlp_exports_total{result="success"}`: 1,
			`http_exporter_otlp_exports_total{result="failure"}`: 0,
			`http_exporter_otlp_export_retries_total`:            2,
			`http_exporter_otlp_points_dropped_total`:            0,
		} {
			if got := seriesValue(t, exposition, series); got != want {
				t.Errorf("%d: %s = %v, want %v", status, series, got, want)
			}
		}
		if seriesValue(t, exposition, "http_exporter_otlp_last_export_success_timestamp_seconds") < float64(time.Now().Add(-time.Minute).Unix()) {
			t.Errorf("%d: the last success timestamp was not set", status)
		}
		if _, left := pendingValue(server, "probe_value"); left {
			t.Errorf("%d: a delivered metric is still pending", status)
		}
	}
}

// Retry-After is honoured, in seconds or as a date.
func TestOTLPRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":                              0,
		"3":                             3 * time.Second,
		"-1":                            0,
		"soon":                          0,
		"Tue, 01 Sep 2026 12:00:07 GMT": 7 * time.Second,
		"Tue, 01 Sep 2026 11:00:00 GMT": 0,
	} {
		if got := retryAfter(value, now); got != want {
			t.Errorf("Retry-After %q = %s, want %s", value, got, want)
		}
	}
	if otlpBackoff(0) != otlpRetryBackoff || otlpBackoff(1) != 2*otlpRetryBackoff || otlpBackoff(30) != otlpMaxBackoff {
		t.Errorf("backoff %s %s %s", otlpBackoff(0), otlpBackoff(1), otlpBackoff(30))
	}
}

// A Retry-After longer than the budget ends the export rather than waiting
// past it.
func TestOTLPRetriesStayWithinTheBudget(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	server := otlpServer(t, endpoint.server.URL)
	start := time.Now()
	server.exportOTLP(context.Background(), 300*time.Millisecond)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("an export with a 300ms budget took %s", took)
	}
	if endpoint.count() < 2 {
		t.Fatalf("%d attempts within the budget, want retries", endpoint.count())
	}

	endpoint.mu.Lock()
	endpoint.header.Set("Retry-After", "60")
	endpoint.mu.Unlock()
	before := endpoint.count()
	start = time.Now()
	server.exportOTLP(context.Background(), time.Second)
	if took := time.Since(start); took > 2*time.Second || endpoint.count() != before+1 {
		t.Fatalf("Retry-After 60 with a 1s budget: %d attempts in %s, want one at once", endpoint.count()-before, took)
	}
}

// Data that did not get through for a retryable reason is kept for the next
// export, unless a newer value replaced it; the failure is counted.
func TestUndeliveredOTLPDataIsKeptForTheNextExport(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	server := otlpServer(t, endpoint.server.URL)
	queueProbeMetric(server, "kept_value", 1)
	queueProbeMetric(server, "replaced_value", 1)
	server.exportOTLP(context.Background(), 50*time.Millisecond)
	if v, ok := pendingValue(server, "kept_value"); !ok || v != 1 {
		t.Fatalf("an undelivered metric was not kept: %v %v", v, ok)
	}
	if _, ok := pendingValue(server, "http_exporter_scrapes_total"); ok {
		t.Fatal("the self-metric snapshot was queued again")
	}
	exposition := selfMetrics(t, server)
	if seriesValue(t, exposition, `http_exporter_otlp_exports_total{result="failure"}`) != 1 || seriesValue(t, exposition, "http_exporter_otlp_points_dropped_total") != 0 {
		t.Fatalf("a failed export was not counted as one:\n%s", exposition)
	}

	// A newer value queued while an export fails wins over the requeued one.
	server.requeueOTLP(nil)
	queueProbeMetric(server, "replaced_value", 2)
	pending := server.drainOTLP()
	queueProbeMetric(server, "replaced_value", 3)
	server.requeueOTLP(pending)
	if v, _ := pendingValue(server, "replaced_value"); v != 3 {
		t.Fatalf("the requeued value %v replaced the newer 3", v)
	}

	endpoint.mu.Lock()
	endpoint.statuses = []int{http.StatusOK}
	endpoint.mu.Unlock()
	server.exportOTLP(context.Background(), time.Second)
	if !endpoint.received("kept_value") {
		t.Fatal("the kept metric was not sent once the endpoint recovered")
	}
	if _, ok := pendingValue(server, "kept_value"); ok {
		t.Fatal("the delivered metric is still pending")
	}
}

// A status that is not retried drops the data, once, and counts it.
func TestRefusedOTLPDataIsDropped(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusBadRequest)
	server := otlpServer(t, endpoint.server.URL)
	queueProbeMetric(server, "first_value", 1)
	queueProbeMetric(server, "second_value", 1)
	server.exportOTLP(context.Background(), 5*time.Second)
	if endpoint.count() != 1 {
		t.Fatalf("a 400 was tried %d times, want once", endpoint.count())
	}
	if _, ok := pendingValue(server, "first_value"); ok {
		t.Fatal("refused data was kept")
	}
	exposition := selfMetrics(t, server)
	if got := seriesValue(t, exposition, "http_exporter_otlp_points_dropped_total"); got != 2 {
		t.Fatalf("dropped %v points, want the 2 probe metrics", got)
	}
	if seriesValue(t, exposition, `http_exporter_otlp_exports_total{result="failure"}`) != 1 {
		t.Fatal("the refused export was not a failure")
	}
}

// Unreachable is retried and kept.
func TestUnreachableOTLPEndpoint(t *testing.T) {
	fastRetries(t)
	endpoint := httptest.NewServer(http.NotFoundHandler())
	url := endpoint.URL
	endpoint.Close()
	server := otlpServer(t, url)
	queueProbeMetric(server, "probe_value", 1)
	server.exportOTLP(context.Background(), 200*time.Millisecond)
	exposition := selfMetrics(t, server)
	if seriesValue(t, exposition, `http_exporter_otlp_exports_total{result="failure"}`) != 1 || seriesValue(t, exposition, "http_exporter_otlp_export_retries_total") < 1 {
		t.Fatalf("an unreachable endpoint was not retried and counted:\n%s", exposition)
	}
	if _, ok := pendingValue(server, "probe_value"); !ok {
		t.Fatal("data for an unreachable endpoint was dropped")
	}
}

// The export status families exist only while OTLP is enabled.
func TestOTLPStatusMetricsOnlyWithOTLP(t *testing.T) {
	server := verboseServer(t, false, testutil.Collector("text", "text"))
	if strings.Contains(selfMetrics(t, server), "http_exporter_otlp_") {
		t.Fatal("OTLP status families without OTLP")
	}
	server = otlpServer(t, "http://otel.invalid:4318")
	exposition := selfMetrics(t, server)
	for _, family := range []string{"http_exporter_otlp_exports_total", "http_exporter_otlp_export_retries_total", "http_exporter_otlp_points_dropped_total", "http_exporter_otlp_export_duration_seconds", "http_exporter_otlp_last_export_success_timestamp_seconds"} {
		if !strings.Contains(exposition, "# TYPE "+family+" ") {
			t.Errorf("%s is missing", family)
		}
	}
	if seriesValue(t, exposition, "http_exporter_otlp_last_export_success_timestamp_seconds") != 0 {
		t.Error("the last success timestamp is not 0 before the first export")
	}
}

// At shutdown the loop stops without exporting, an export it was making is
// cut short with its data kept, and FlushOTLP sends everything pending with a
// last self-metric snapshot.
func TestTheLastOTLPExportAtShutdown(t *testing.T) {
	block := make(chan struct{})
	var requests atomic.Int64
	var mu sync.Mutex
	var bodies []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readOTLPBody(t, r)
		if requests.Add(1) == 1 {
			<-block
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
	}))
	t.Cleanup(endpoint.Close)
	t.Cleanup(func() { close(block) })
	server := otlpServer(t, endpoint.URL)
	server.manager.Get().OTLP.Interval = model.Duration(10 * time.Millisecond)
	queueProbeMetric(server, "before_shutdown", 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.OTLPExportLoop(ctx)
	}()
	testutil.WaitFor(t, "the loop to start an export", func() bool { return requests.Load() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the export loop did not stop when its context ended")
	}
	if _, ok := pendingValue(server, "before_shutdown"); !ok {
		t.Fatal("the export cut short by shutdown lost its data")
	}
	if strings.Contains(selfMetrics(t, server), `http_exporter_otlp_exports_total{result="failure"} 1`) {
		t.Fatal("an export cut short by shutdown was counted as a failure")
	}

	queueProbeMetric(server, "during_shutdown", 1)
	server.FlushOTLP()
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("the last export made %d requests, want 1", len(bodies))
	}
	for _, name := range []string{"before_shutdown", "during_shutdown", "http_exporter_scrapes_total"} {
		if !strings.Contains(bodies[0], `"name":"`+name+`"`) {
			t.Errorf("the last export is missing %s", name)
		}
	}
}

// Without OTLP there is no last export.
func TestNoLastExportWithoutOTLP(t *testing.T) {
	server := verboseServer(t, false, testutil.Collector("text", "text"))
	server.FlushOTLP()
}

func ready(t *testing.T, server *Server) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))
	return recorder.Code, recorder.Body.String()
}

// Not ready while the last reload of a file was rejected, ready again once one
// is accepted.
func TestNotReadyWhileTheConfigurationIsRejected(t *testing.T) {
	r := newReloadable(t, testutil.CollectorsDocument("first"), "")
	if code, body := ready(t, r.server); code != http.StatusOK || body != "ready\n" {
		t.Fatalf("a fresh exporter: %d %q", code, body)
	}
	r.write(r.path, "collectors: [\n")
	if err := r.manager.Reload("http"); err == nil {
		t.Fatal("a broken configuration was accepted")
	}
	code, body := ready(t, r.server)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "the last reload of the configuration was rejected") {
		t.Fatalf("after a rejected reload: %d %q", code, body)
	}
	if strings.Contains(body, "yaml") || strings.Contains(body, r.path) {
		t.Fatalf("/ready is unauthenticated and quotes the error: %q", body)
	}
	r.write(r.path, testutil.CollectorsDocument("first"))
	if err := r.manager.Reload("http"); err != nil {
		t.Fatal(err)
	}
	if code, _ := ready(t, r.server); code != http.StatusOK {
		t.Fatalf("still not ready after an accepted reload: %d", code)
	}
}

// Failing exports leave readiness alone by default. With
// otlp.unready_after_failures, the exporter is not ready after that many in a
// row, and ready at the next that gets through.
func TestNotReadyWhileOTLPExportsFail(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	server := otlpServer(t, endpoint.server.URL)
	for range 5 {
		server.exportOTLP(context.Background(), 20*time.Millisecond)
	}
	if code, body := ready(t, server); code != http.StatusOK {
		t.Fatalf("failing exports made the exporter unready without unready_after_failures: %d %q", code, body)
	}

	server = otlpServer(t, endpoint.server.URL)
	server.manager.Get().OTLP.UnreadyAfterFailures = 2
	for i := 1; i <= 2; i++ {
		if code, _ := ready(t, server); code != http.StatusOK {
			t.Fatalf("not ready after %d failed exports", i-1)
		}
		server.exportOTLP(context.Background(), 20*time.Millisecond)
	}
	code, body := ready(t, server)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "the last 2 OTLP exports failed") {
		t.Fatalf("after 2 failed exports: %d %q", code, body)
	}
	endpoint.mu.Lock()
	endpoint.statuses = []int{http.StatusOK}
	endpoint.mu.Unlock()
	server.exportOTLP(context.Background(), time.Second)
	if code, _ := ready(t, server); code != http.StatusOK {
		t.Fatalf("not ready after an export got through: %d", code)
	}

	// Failures do not count with OTLP disabled.
	server.manager.Get().OTLP.Enabled = false
	server.otlp.consecutiveFailures = 5
	if code, _ := ready(t, server); code != http.StatusOK {
		t.Fatal("not ready over OTLP failures with OTLP disabled")
	}
}

// A reload that points OTLP at another endpoint starts the failure count
// again, so the new endpoint is not blamed for the old one's failures.
func TestAnotherOTLPEndpointStartsReadinessAfresh(t *testing.T) {
	fastRetries(t)
	dead := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	alive := newOTLPEndpoint(t)
	server := otlpServer(t, dead.server.URL)
	server.manager.Get().OTLP.UnreadyAfterFailures = 1
	server.exportOTLP(context.Background(), 20*time.Millisecond)
	if code, _ := ready(t, server); code != http.StatusServiceUnavailable {
		t.Fatal("not unready after a failed export")
	}
	server.manager.Get().OTLP.Endpoint = alive.server.URL + "/v1/metrics"
	if code, body := ready(t, server); code != http.StatusOK {
		t.Fatalf("a new endpoint is blamed for the old one's failures: %d %q", code, body)
	}
}

func TestOTLPSettingsAreValidated(t *testing.T) {
	for key, change := range map[string]func(c *model.OTLPConfig){
		"max_pending_points":     func(c *model.OTLPConfig) { c.MaxPendingPoints = -1 },
		"unready_after_failures": func(c *model.OTLPConfig) { c.UnreadyAfterFailures = -1 },
	} {
		cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://otel:4318/v1/metrics")}
		change(&cfg.OTLP)
		if err := config.Validate(cfg); err == nil || !strings.Contains(err.Error(), "otlp."+key) {
			t.Errorf("a negative %s: %v", key, err)
		}
	}
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://otel:4318/v1/metrics")}
	if err := config.Validate(cfg); err != nil || cfg.OTLP.MaxPendingPoints != model.DefaultOTLPMaxPendingPoints || cfg.OTLP.UnreadyAfterFailures != 0 {
		t.Fatalf("defaults: %v %+v", err, cfg.OTLP)
	}
}

// While exports fail, the data points waiting stay within
// otlp.max_pending_points: past it the oldest go, counted and logged, and a
// point queued again after a failed export is older than any queued since.
func TestPendingOTLPPointsAreCapped(t *testing.T) {
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	server := otlpServer(t, endpoint.server.URL)
	server.manager.Get().OTLP.MaxPendingPoints = 10
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()

	for i := range 6 {
		queueProbeMetric(server, "early_"+strconv.Itoa(i), 1)
	}
	failed := server.drainOTLP()
	for i := range 6 {
		queueProbeMetric(server, "late_"+strconv.Itoa(i), 1)
	}
	server.requeueOTLP(failed)
	// 12 points against a limit of 10: down to 9, the three oldest going,
	// which are the requeued ones.
	if server.otlpPoints != 9 {
		t.Fatalf("%d points pending, want 9", server.otlpPoints)
	}
	for i := range 6 {
		if _, ok := pendingValue(server, "late_"+strconv.Itoa(i)); !ok {
			t.Errorf("late_%d, queued last, was dropped", i)
		}
	}
	early := 0
	for i := range 6 {
		if _, ok := pendingValue(server, "early_"+strconv.Itoa(i)); ok {
			early++
		}
	}
	if early != 3 {
		t.Fatalf("%d of the requeued points are left, want 3", early)
	}
	if got := seriesValue(t, selfMetrics(t, server), "http_exporter_otlp_points_dropped_total"); got != 3 {
		t.Fatalf("dropped %v, want 3", got)
	}
	if !strings.Contains(logs.String(), "reached otlp.max_pending_points") {
		t.Fatalf("the drop was not logged:\n%s", logs.String())
	}
	// Replacing a pending series is not a new point.
	queueProbeMetric(server, "late_0", 2)
	if server.otlpPoints != 9 {
		t.Fatalf("replacing a series changed the count to %d", server.otlpPoints)
	}
	server.drainOTLP()
	if server.otlpPoints != 0 {
		t.Fatal("draining did not reset the count")
	}
}

// freshTransports gives the test transports built from the environment it
// sets.
func freshTransports(t *testing.T) {
	t.Helper()
	previous := fetch.Transports
	fetch.Transports = fetch.NewTransportCache()
	t.Cleanup(func() { fetch.Transports = previous })
}

// forwardProxy answers every request it is sent as a proxy, and records the
// URLs asked for.
func forwardProxy(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=7\n"))
	}))
	t.Cleanup(proxy.Close)
	return proxy, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// Target requests and OTLP exports go through the proxy the environment names,
// and a host NO_PROXY names goes direct.
func TestRequestsUseTheProxyFromTheEnvironment(t *testing.T) {
	proxy, seen := forwardProxy(t)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "direct.example")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("no_proxy", "")
	freshTransports(t)

	server := otlpServer(t, "http://otel.example:4318")
	recorder := probeOnce(t, server, "/probe?collector=text&target=http://target.example:8080/status", nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "demo_value 7") {
		t.Fatalf("a probe through the proxy: %d %s", recorder.Code, recorder.Body.String())
	}
	server.exportOTLP(context.Background(), 5*time.Second)
	got := seen()
	if len(got) != 2 || got[0] != "http://target.example:8080/status" || got[1] != "http://otel.example:4318/v1/metrics" {
		t.Fatalf("the proxy was asked for %q", got)
	}

	probeOnce(t, server, "/probe?collector=text&timeout=1s&target=http://direct.example:8080/status", nil)
	if got := seen(); len(got) != 2 {
		t.Fatalf("a NO_PROXY host went through the proxy: %q", got)
	}
}
