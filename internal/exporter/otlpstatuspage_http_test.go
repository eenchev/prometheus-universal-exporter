//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// otlpSection is the status page's OTLP section, read as JSON.
func otlpSection(t *testing.T, server *Server) otlpStatusSection {
	t.Helper()
	return statusJSON(t, server, "/static-targets/status").OTLP
}

// otlpDetails is the OTLP section of a server that exports.
func otlpDetails(t *testing.T, server *Server) *OTLPStatusDetails {
	t.Helper()
	section := otlpSection(t, server)
	if !section.Enabled || section.OTLPStatusDetails == nil {
		t.Fatalf("the OTLP section says export is off: %+v", section)
	}
	return section.OTLPStatusDetails
}

// rawOTLPSection is the status page's otlp object as the JSON has it.
func rawOTLPSection(t *testing.T, server *Server) map[string]json.RawMessage {
	t.Helper()
	r := getPath(server, http.MethodGet, "/static-targets/status?format=json")
	var page struct {
		OTLP map[string]json.RawMessage `json:"otlp"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
		t.Fatalf("the JSON does not decode: %v\n%s", err, r.Body)
	}
	return page.OTLP
}

// With OTLP export off the section says so in one line, and the JSON's otlp
// object is {"enabled": false} alone.
func TestTheStatusPageSaysOTLPExportIsOff(t *testing.T) {
	server := newStaticServer(t, weatherConfig(), nil)
	if raw := rawOTLPSection(t, server); len(raw) != 1 || string(raw["enabled"]) != "false" {
		t.Errorf("the otlp object with OTLP export off is %v, want enabled false alone", raw)
	}
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page, `<h2 id="otlp">OTLP export</h2>`, "OTLP export is off.")
	if strings.Contains(page, "OTLP export is failing") || strings.Contains(page, "Endpoint") {
		t.Errorf("the page with OTLP export off shows more than that:\n%s", page)
	}
}

// exportedPoints is how many data points the endpoint's k'th request
// carried.
func exportedPoints(t *testing.T, e *otlpEndpoint, k int) int {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	var payload otlpPayload
	if err := json.Unmarshal(e.bodies[k], &payload); err != nil {
		t.Fatalf("the export does not decode: %v", err)
	}
	return otlpPayloadPoints(payload)
}

// An export that gets through is shown with when it began, how long ago,
// how long it took, its result, its requests and the data points they
// carried; the last success is its time, nothing fails, and nothing is
// pending until a probe queues more. What the exports carry is said: the
// probes, the static targets with export_via_otlp, and the self-metrics.
func TestTheStatusPageShowsOTLPExportsGettingThrough(t *testing.T) {
	endpoint := newOTLPEndpoint(t)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig(endpoint.server.URL + "/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "a", Collector: "text", Target: "http://127.0.0.1:1/", ExportViaOTLP: true},
		{Name: "b", Collector: "text", Target: "http://127.0.0.1:1/"},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	before := otlpDetails(t, server)
	if before.LastExport != nil || before.LastSuccess != "" || before.Failing || before.Failures != 0 || before.LastError != "" {
		t.Errorf("before any export the section is %+v", before)
	}
	if before.Endpoint != endpoint.server.URL+"/v1/metrics" || before.Protocol != "http/json" || before.Interval != "1m0s" || before.MaxPending != model.DefaultOTLPMaxPendingPoints {
		t.Errorf("the section shows the settings as %+v", before)
	}
	if before.Exported != (otlpExportedKind{Probes: true, StaticTargets: 1, SelfMetrics: true}) {
		t.Errorf("the section says the exports carry %+v", before.Exported)
	}
	requireContains(t, getPage(t, server, "/static-targets/status"), "Last export</th><td><span class=\"muted\">none yet</span>", "<span class=\"muted\">never</span>", "1 static target with <code>export_via_otlp</code>")

	queueProbeMetric(server, "first_value", 1)
	queueProbeMetric(server, "second_value", 2)
	if got := otlpDetails(t, server).Pending; got != 2 {
		t.Errorf("%d data points are shown pending, want the 2 queued", got)
	}
	began := time.Now()
	server.exportOTLP(context.Background(), time.Minute)
	ended := time.Now()

	d := otlpDetails(t, server)
	last := d.LastExport
	if last == nil {
		t.Fatalf("the export is not shown: %+v", d)
	}
	started, err := time.Parse(time.RFC3339, last.Started)
	if err != nil || started.Before(began.Truncate(time.Second)) || started.After(ended) {
		t.Errorf("the export is shown to have begun at %q, between %v and %v: %v", last.Started, began, ended, err)
	}
	if want := exportedPoints(t, endpoint, 0); last.Result != statusSuccess || last.Requests != 1 || last.DataPoints != want || last.Rejected != 0 || last.Retries != 0 || last.Duration <= 0 || last.Age < 0 {
		t.Errorf("the export that got through is shown as %+v; want a success of 1 request and %d data points", last, want)
	}
	if last.DataPoints <= 2 {
		t.Errorf("the export is shown with %d data points, want the 2 queued and the self-metrics", last.DataPoints)
	}
	if d.LastSuccess == "" || d.LastSuccess < last.Started || d.Failing || d.Failures != 0 || d.LastError != "" || d.Pending != 0 || d.Succeeded != 1 || d.FailedAll != 0 || d.Readiness != "" {
		t.Errorf("after an export that got through the section is %+v", d)
	}
	queueProbeMetric(server, "third_value", 3)
	if got := otlpDetails(t, server).Pending; got != 1 {
		t.Errorf("%d data points are shown pending, want the 1 queued since", got)
	}
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page, last.Started, `<span class="good">success</span>`, "1 request, "+strconv.Itoa(last.DataPoints)+" data points, 0 retries", d.LastSuccess, "1 data point of at most 100000", "1 export succeeded, 0 failed")
	if strings.Contains(page, "OTLP export is failing") {
		t.Errorf("the page says OTLP export is failing:\n%s", page)
	}
}

// flakyOTLPEndpoint is an endpoint that drops the connection of every
// export while down is set, which the exporter retries and gives up on as a
// network error quoting the URL it posted to, and takes every export
// otherwise.
func flakyOTLPEndpoint(t *testing.T, down *atomic.Bool) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readOTLPBody(t, r)
		if down.Load() {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// An export that fails is shown with its error, as the log has it, with
// the endpoint's credentials — the URL's user and password, a token in its
// query — withheld in the endpoint shown and in the error, which quotes the
// URL; the error is kept after an export gets through again, which ends
// the failures in a row. The summary at the top says OTLP export is failing
// while it is. Every value is escaped: the endpoint's tenant is markup.
func TestTheStatusPageShowsAFailingOTLPExportWithItsErrorRedacted(t *testing.T) {
	fastRetries(t)
	var down atomic.Bool
	down.Store(true)
	endpoint := flakyOTLPEndpoint(t, &down)
	url := strings.Replace(endpoint.URL, "http://", "http://reader:hunter2@", 1) + "/v1/metrics?token=hunter3&tenant=<b>acme</b>"
	server := otlpServer(t, endpoint.URL)
	server.manager.Get().OTLP.Endpoint = url
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	queueProbeMetric(server, "first_value", 1)
	began := time.Now()
	server.exportOTLP(context.Background(), 300*time.Millisecond)

	d := otlpDetails(t, server)
	if d.LastExport == nil || d.LastExport.Result != statusFailed || !d.Failing || d.Failures != 1 || d.LastSuccess != "" || d.FailedAll != 1 {
		t.Fatalf("after a failed export the section is %+v, last export %+v", d, d.LastExport)
	}
	if !strings.Contains(d.LastError, "EOF") || !strings.Contains(d.LastError, "/v1/metrics") || d.LastErrorAt != d.LastExport.Started {
		t.Errorf("the failed export's error is %q at %q, want the network error quoting the URL, at %q", d.LastError, d.LastErrorAt, d.LastExport.Started)
	}
	if d.LastExport.Retries == 0 || d.Retries != d.LastExport.Retries {
		t.Errorf("the failed export is shown with %d retries, %d since start; want the retries it made", d.LastExport.Retries, d.Retries)
	}
	if !strings.Contains(d.Endpoint, "redacted:redacted@") || !strings.Contains(d.Endpoint, "token=<redacted>") || !strings.Contains(d.Endpoint, "tenant=<b>acme</b>") {
		t.Errorf("the endpoint is shown as %q", d.Endpoint)
	}
	raw := getPath(server, http.MethodGet, "/static-targets/status?format=json").Body.String()
	page := getPage(t, server, "/static-targets/status")
	for what, text := range map[string]string{"the JSON": raw, "the page": page, "the log": logs.String()} {
		if strings.Contains(text, "hunter2") || strings.Contains(text, "hunter3") {
			t.Errorf("%s shows a credential of the endpoint:\n%s", what, text)
		}
	}
	if strings.Contains(page, "<b>acme") {
		t.Errorf("the page has the endpoint's tenant as markup:\n%s", page)
	}
	requireContains(t, page, templateEscaped(d.Endpoint), templateEscaped(d.LastError), "OTLP export is failing: the last 1 export failed.", `<span class="bad">failed</span>`)

	// It gets through again: the failures end, the error stays.
	time.Sleep(time.Until(began.Truncate(time.Second).Add(time.Second)))
	down.Store(false)
	server.exportOTLP(context.Background(), time.Minute)
	after := otlpDetails(t, server)
	if after.LastExport.Result != statusSuccess || after.Failing || after.Failures != 0 || after.LastSuccess == "" || after.Succeeded != 1 || after.FailedAll != 1 {
		t.Errorf("after an export got through again the section is %+v, last export %+v", after, after.LastExport)
	}
	if after.LastError != d.LastError || after.LastErrorAt != d.LastErrorAt || after.LastErrorAt == after.LastExport.Started {
		t.Errorf("after an export got through again the error is %q at %q, want %q at %q kept", after.LastError, after.LastErrorAt, d.LastError, d.LastErrorAt)
	}
	if page := getPage(t, server, "/static-targets/status"); strings.Contains(page, "OTLP export is failing") {
		t.Errorf("the page says OTLP export is failing after it recovered:\n%s", page)
	}
}

// The points pending and dropped, the retries and the exports are the
// numbers the self-metrics have, and the dropped are shown by why: the
// refused, the rejected in a partialSuccess, and the oldest past
// otlp.max_pending_points.
func TestTheStatusPagesOTLPNumbersAreTheSelfMetrics(t *testing.T) {
	fastRetries(t)
	var status atomic.Int32
	status.Store(http.StatusBadRequest)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readOTLPBody(t, r)
		switch code := int(status.Load()); code {
		case http.StatusOK:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedDataPoints":"2","errorMessage":"too long"}}`))
		default:
			w.WriteHeader(code)
		}
	}))
	t.Cleanup(endpoint.Close)
	server := otlpServer(t, endpoint.URL)
	server.manager.Get().OTLP.MaxPendingPoints = 10

	// Refused: the probe's 3 points and the self-metrics are dropped.
	for _, name := range []string{"a", "b", "c"} {
		queueProbeMetric(server, name, 1)
	}
	server.exportOTLP(context.Background(), time.Minute)
	// Retried and out of time: the points wait; the 11th queued is past
	// the bound of 10, and the 2 oldest are dropped, down to 9.
	status.Store(http.StatusServiceUnavailable)
	for i := range 12 {
		queueProbeMetric(server, "p"+strconv.Itoa(i), 1)
	}
	server.exportOTLP(context.Background(), 300*time.Millisecond)
	// Accepted, 2 of its points rejected.
	status.Store(http.StatusOK)
	server.exportOTLP(context.Background(), time.Minute)
	queueProbeMetric(server, "waiting", 1)
	queueProbeMetric(server, "waiting_too", 1)

	d := otlpDetails(t, server)
	exposition := selfMetrics(t, server)
	server.otlpMu.Lock()
	pending := server.otlpPoints
	server.otlpMu.Unlock()
	if d.Pending != pending || d.Pending != 2 || d.MaxPending != 10 {
		t.Errorf("%d of at most %d points are shown pending; %d are, of 10", d.Pending, d.MaxPending, pending)
	}
	for series, shown := range map[string]float64{
		"http_exporter_otlp_points_dropped_total":            float64(d.Dropped),
		"http_exporter_otlp_export_retries_total":            float64(d.Retries),
		`http_exporter_otlp_exports_total{result="success"}`: float64(d.Succeeded),
		`http_exporter_otlp_exports_total{result="failure"}`: float64(d.FailedAll),
	} {
		if want := seriesValue(t, exposition, series); shown != want {
			t.Errorf("the page shows %v for %s, which is %v", shown, series, want)
		}
	}
	by := d.DroppedBy
	if by.Refused < 3 || by.Rejected != 2 || by.PendingCap != 2 || by.Shutdown != 0 || by.Refused+by.Rejected+by.PendingCap+by.Shutdown != d.Dropped {
		t.Errorf("the points dropped are shown as %d, by why %+v; want at least 3 refused, 2 rejected and 2 over the bound, adding up", d.Dropped, by)
	}
	if d.Retries == 0 || d.Succeeded != 1 || d.FailedAll != 2 {
		t.Errorf("the section counts %d retries, %d exports succeeded and %d failed", d.Retries, d.Succeeded, d.FailedAll)
	}
	if last := d.LastExport; last.Result != statusPartialSuccess || last.Rejected != 2 {
		t.Errorf("the export of which points were rejected is shown as %+v", last)
	}
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page, "partial success</span>, 2 data points rejected", "2 data points of at most 10", strconv.Itoa(by.Refused)+" refused, 2 rejected, 2 over the pending bound, 0 of the last export at shutdown")
}

// With otlp.unready_after_failures set the section shows the readiness it
// makes: ready, and unready since the failed export that made it so, as
// /ready answers.
func TestTheStatusPageShowsTheReadinessOTLPFailuresMake(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	server := otlpServer(t, endpoint.server.URL)
	server.manager.Get().OTLP.UnreadyAfterFailures = 2
	if d := otlpDetails(t, server); d.UnreadyAfter != 2 || d.Readiness != "ready" || d.UnreadySince != "" {
		t.Errorf("before any export the readiness is shown as %+v", d)
	}
	server.exportOTLP(context.Background(), 20*time.Millisecond)
	if d := otlpDetails(t, server); d.Readiness != "ready" || d.Failures != 1 {
		t.Errorf("after 1 failed export of 2 the readiness is shown as %q with %d failures", d.Readiness, d.Failures)
	}
	began := time.Now()
	server.exportOTLP(context.Background(), 20*time.Millisecond)
	ended := time.Now()
	d := otlpDetails(t, server)
	since, err := time.Parse(time.RFC3339, d.UnreadySince)
	if d.Readiness != "unready" || err != nil || since.Before(began.Truncate(time.Second)) || since.After(ended) {
		t.Errorf("after 2 failed exports the readiness is shown as %q since %q", d.Readiness, d.UnreadySince)
	}
	if code, _ := ready(t, server); code != http.StatusServiceUnavailable {
		t.Errorf("/ready answers %d while the page says unready", code)
	}
	requireContains(t, getPage(t, server, "/static-targets/status"), `<span class="bad">unready</span> since `+d.UnreadySince, "OTLP export is failing: the last 2 exports failed, and the exporter is unready.")

	endpoint.mu.Lock()
	endpoint.statuses = []int{http.StatusOK}
	endpoint.mu.Unlock()
	server.exportOTLP(context.Background(), time.Minute)
	if d := otlpDetails(t, server); d.Readiness != "ready" || d.UnreadySince != "" {
		t.Errorf("after an export got through the readiness is shown as %q since %q", d.Readiness, d.UnreadySince)
	}
}

// A reload that changes otlp.unready_after_failures during a run of
// failures moves when the exporter is unready since to the end of the
// failure the new threshold counts to: one under the run's length shows
// unready since that failure, at once, and one over it ready, as /ready
// answers.
func TestTheStatusPageFollowsAReloadOfTheOTLPUnreadyThreshold(t *testing.T) {
	fastRetries(t)
	endpoint := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	server := otlpServer(t, endpoint.server.URL)
	cfg := &server.manager.Get().OTLP
	cfg.UnreadyAfterFailures = 3
	start := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	var ends atomic.Int64
	// The nth failure of the run ends n minutes after start.
	server.otlp.now = func() time.Time { return start.Add(time.Duration(ends.Add(1)) * time.Minute) }
	for range 4 {
		server.exportOTLP(context.Background(), 20*time.Millisecond)
	}
	at := func(n int) string { return start.Add(time.Duration(n) * time.Minute).Format(time.RFC3339) }
	for _, step := range []struct {
		unreadyAfter    int
		readiness, from string
		code            int
	}{
		{3, "unready", at(3), http.StatusServiceUnavailable},
		{2, "unready", at(2), http.StatusServiceUnavailable},
		{1, "unready", at(1), http.StatusServiceUnavailable},
		{4, "unready", at(4), http.StatusServiceUnavailable},
		{5, "ready", "", http.StatusOK},
		{model.MaxOTLPUnreadyAfterFailures, "ready", "", http.StatusOK},
	} {
		cfg.UnreadyAfterFailures = step.unreadyAfter
		d := otlpDetails(t, server)
		if d.Failures != 4 || d.Readiness != step.readiness || d.UnreadySince != step.from {
			t.Errorf("after 4 failed exports, reloaded with unready_after_failures %d, the readiness is shown as %q since %q, want %q since %q", step.unreadyAfter, d.Readiness, d.UnreadySince, step.readiness, step.from)
		}
		if code, _ := ready(t, server); code != step.code {
			t.Errorf("unready_after_failures %d: /ready answers %d, want %d", step.unreadyAfter, code, step.code)
		}
	}
	// A fifth failure under 5 makes the exporter unready since its end.
	cfg.UnreadyAfterFailures = 5
	server.exportOTLP(context.Background(), 20*time.Millisecond)
	if d := otlpDetails(t, server); d.Readiness != "unready" || d.UnreadySince != at(5) {
		t.Errorf("after a fifth failed export under 5 the readiness is shown as %q since %q, want unready since %s", d.Readiness, d.UnreadySince, at(5))
	}
}

// A reload that points OTLP at another endpoint shows the new endpoint at
// once, with none of the old one's exports, failures or error, until its own
// export; the counts since the start, as the self-metrics have them, go on.
func TestTheStatusPageFollowsAReloadOfTheOTLPEndpoint(t *testing.T) {
	fastRetries(t)
	dead := newOTLPEndpoint(t, http.StatusServiceUnavailable)
	alive := newOTLPEndpoint(t)
	server := otlpServer(t, dead.server.URL)
	server.exportOTLP(context.Background(), 20*time.Millisecond)
	if d := otlpDetails(t, server); !d.Failing || d.LastError == "" {
		t.Fatalf("the failed export to the first endpoint is shown as %+v", d)
	}
	server.manager.Get().OTLP.Endpoint = alive.server.URL + "/v1/metrics"
	d := otlpDetails(t, server)
	if d.Endpoint != alive.server.URL+"/v1/metrics" || d.LastExport != nil || d.Failing || d.Failures != 0 || d.LastError != "" || d.LastErrorAt != "" || d.LastSuccess != "" {
		t.Errorf("after the endpoint changed the section is %+v", d)
	}
	if d.FailedAll != 1 {
		t.Errorf("after the endpoint changed %d failed exports are counted since start, want 1", d.FailedAll)
	}
	server.exportOTLP(context.Background(), time.Minute)
	if d := otlpDetails(t, server); d.LastExport == nil || d.LastExport.Result != statusSuccess || d.LastError != "" || d.Succeeded != 1 || d.FailedAll != 1 {
		t.Errorf("after an export to the new endpoint the section is %+v", d)
	}
}

// The otlp object has the fields the documentation lists, and the last
// export's object its own; in allocator mode the section says the exports
// are this replica's.
func TestTheStatusPagesOTLPJSON(t *testing.T) {
	endpoint := newOTLPEndpoint(t)
	server := otlpServer(t, endpoint.server.URL)
	server.manager.Get().OTLP.UnreadyAfterFailures = 3
	server.exportOTLP(context.Background(), time.Minute)
	raw := rawOTLPSection(t, server)
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	// last_error, last_error_at and unready_since are left out when empty.
	want := []string{"allocator_mode", "consecutive_failures", "enabled", "endpoint", "exported", "exports_failed", "exports_succeeded", "failing", "interval", "last_export", "last_success", "max_pending_points", "pending_points", "points_dropped", "points_dropped_by_reason", "protocol", "readiness", "retries", "unready_after_failures"}
	if !slices.Equal(keys, want) {
		t.Errorf("the otlp object has the fields\n%v\nwant\n%v", keys, want)
	}
	var last map[string]json.RawMessage
	if err := json.Unmarshal(raw["last_export"], &last); err != nil {
		t.Fatal(err)
	}
	keys = keys[:0]
	for key := range last {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if want := []string{"age_seconds", "data_points", "duration_seconds", "rejected_points", "requests", "result", "retries", "started"}; !slices.Equal(keys, want) {
		t.Errorf("the last export has the fields %v, want %v", keys, want)
	}
	if got := string(raw["points_dropped_by_reason"]); got != `{"refused":0,"rejected":0,"over_max_pending_points":0,"last_export_at_shutdown":0}` {
		t.Errorf("points_dropped_by_reason is %s", got)
	}
	if got := string(raw["exported"]); got != `{"probes":true,"static_targets":0,"self_metrics":true}` {
		t.Errorf("exported is %s", got)
	}

	server.SetAllocatorStatus(func() AllocatorStatus { return AllocatorStatus{SyncErrors: map[string]uint64{}} })
	if !otlpDetails(t, server).Allocator {
		t.Error("the OTLP section does not say the exporter is in allocator mode")
	}
	requireContains(t, getPage(t, server, "/static-targets/status"), "These are the exports of this replica.")
}
