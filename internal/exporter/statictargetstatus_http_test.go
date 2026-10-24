//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// statusTargets is a server with a target in each state the status page
// tells apart, each scraped as the state needs, and one never scraped.
func statusTargets(t *testing.T) *Server {
	t.Helper()
	up := textTarget(t, "value=42\n")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	// The hostile target answers with a page whose value cannot be read,
	// which its collector fails on, quoting the page.
	hostile := textTarget(t, "value=<script>alert(1)</script>\n")
	dead := newDeadTarget(t)
	var flakyDown atomic.Bool
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if flakyDown.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("value=7\n"))
	}))
	t.Cleanup(flaky.Close)
	strict := testutil.Collector("strict", "text")
	strict.Metrics[0].ErrorMode = "fail"
	strict.Metrics[0].Expression = `value=(.+)`
	cfg := &model.Config{Collectors: []model.Collector{
		testutil.Collector("text", "text"),
		strict,
		backoffCollector("dead", 1, time.Minute, time.Minute),
		staleCollector(time.Millisecond, time.Hour),
	}}
	// A refused port: the trip fails at once, its error quoting the URL.
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := strings.Replace(refused.URL, "http://", "http://reader:hunter2@", 1) + "/?token=hunter3"
	refused.Close()
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "ok", Collector: "text", Target: up.URL, Labels: map[string]string{"team": `<b onmouseover="x">`}},
		{Name: "broken", Collector: "text", Target: down.URL},
		{Name: "hostile", Collector: "strict", Target: hostile.URL},
		{Name: "backing_off", Collector: "dead", Target: dead.URL},
		{Name: "stale", Collector: "flaky", Target: flaky.URL, ExportViaOTLP: true},
		{Name: "secret", Collector: "text", Target: refusedURL},
		{Name: "never", Collector: "text", Target: up.URL},
	}}
	cfg.OTLP = otlpConfig("http://collector.invalid/v1/metrics")
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	scrape := func(name string) {
		for _, target := range server.manager.StaticTargets() {
			if target.Name == name {
				server.scrapeTarget(t.Context(), server.manager.Get(), target)
				return
			}
		}
		t.Fatalf("no target %s", name)
	}
	for _, name := range []string{"ok", "broken", "hostile", "backing_off", "backing_off", "stale", "secret"} {
		scrape(name)
	}
	// The flaky target's result is in the cache; once it is out of date
	// and the target is down, the scrape serves it stale.
	ageEntries(server, time.Second)
	flakyDown.Store(true)
	scrape("stale")
	return server
}

// statusJSON reads the status page as JSON.
func statusJSON(t *testing.T, server *Server, path string) staticStatusPage {
	t.Helper()
	r := getPath(server, http.MethodGet, path+"?format=json")
	if r.Code != http.StatusOK || r.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("%s?format=json answered %d %q: %s", path, r.Code, r.Header().Get("Content-Type"), r.Body)
	}
	var page staticStatusPage
	if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
		t.Fatalf("the JSON does not decode: %v\n%s", err, r.Body)
	}
	return page
}

// Each target has a row saying how its last scrape went: success, failed
// with its error, skipped by failure_backoff while it backs off, failed with
// a stale result served, or not yet scraped. The failing targets come first,
// and the summary counts them.
func TestTheStatusPageShowsEachTargetsLastScrape(t *testing.T) {
	server := statusTargets(t)
	page := statusJSON(t, server, "/static-targets/status")
	if page.Total != 7 || page.Failing != 5 || page.NotScraped != 1 || page.Allocator {
		t.Errorf("the summary is %d targets, %d failing, %d not scraped, allocator %v; want 7, 5, 1, false", page.Total, page.Failing, page.NotScraped, page.Allocator)
	}
	rows := map[string]staticStatusRow{}
	var order []string
	for _, row := range page.Rows {
		rows[row.Name] = row
		order = append(order, row.Name)
	}
	if want := "backing_off broken hostile secret stale never ok"; strings.Join(order, " ") != want {
		t.Errorf("the rows are in the order %v, want %s: the failing first, each part by name", order, want)
	}
	ok := rows["ok"]
	if ok.Result != statusSuccess || ok.Failing || ok.Failures != 0 || ok.LastError != "" || ok.LastScrape == "" || ok.LastScrapeAge == nil || ok.Duration == nil || ok.Interval != "1m0s" || ok.Labels["team"] != `<b onmouseover="x">` || ok.Collector != "text" {
		t.Errorf("the successful target's row is %+v", ok)
	}
	if broken := rows["broken"]; broken.Result != statusFailed || !broken.Failing || broken.Failures != 1 || !strings.Contains(broken.LastError, "received HTTP status 500") || broken.LastErrorAt == "" {
		t.Errorf("the failed target's row is %+v", broken)
	}
	if b := rows["backing_off"]; b.Result != statusBackoff || b.Failures != 2 || b.BackoffUntil != "2026-10-23T09:01:00Z" || b.Backoff != "backing off until 2026-10-23T09:01:00Z" || b.BackoffFailure != 1 || !strings.Contains(b.LastError, "is backing off") {
		t.Errorf("the backed off target's row is %+v", b)
	}
	if stale := rows["stale"]; stale.Result != statusStale || !stale.Stale || stale.Failures != 1 || !stale.ExportViaOTLP {
		t.Errorf("the stale target's row is %+v", stale)
	}
	if never := rows["never"]; never.Result != statusNotScraped || never.Failing || never.LastScrape != "" || never.LastScrapeAge != nil {
		t.Errorf("the target never scraped has the row %+v", never)
	}
	if hostile := rows["hostile"]; hostile.Result != statusFailed || !strings.Contains(hostile.LastError, "<script>") {
		t.Errorf("the hostile target's row is %+v; want its error quoting the page", hostile)
	}
}

// A second scrape of a target served from the cache says so, and a success
// ends the run of failures while the last error stays shown.
func TestTheStatusPageSaysWhenTheCacheAnswered(t *testing.T) {
	target := textTarget(t, "value=1\n")
	c := testutil.Collector("cached", "text")
	c.Cache = model.CacheConfig{TTL: model.Duration(time.Hour)}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "c", Collector: "cached", Target: target.URL}}}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{c}}, file)
	static := server.manager.StaticTargets()[0]
	server.scrapeTarget(t.Context(), server.manager.Get(), static)
	if row := statusJSON(t, server, "/static-targets/status").Rows[0]; row.Cached {
		t.Errorf("the first scrape is shown as from the cache: %+v", row)
	}
	server.scrapeTarget(t.Context(), server.manager.Get(), static)
	if row := statusJSON(t, server, "/static-targets/status").Rows[0]; !row.Cached || row.Result != statusSuccess {
		t.Errorf("the scrape the cache answered is shown as %+v", row)
	}
	if page := getPage(t, server, "/static-targets/status"); !strings.Contains(page, "from the cache") {
		t.Errorf("the page does not say the cache answered:\n%s", page)
	}
}

// The HTML shows what the JSON holds, every value escaped: a label and an
// error that are markup show as text.
func TestTheStatusPageShowsTheJSONsValuesEscaped(t *testing.T) {
	server := statusTargets(t)
	page := statusJSON(t, server, "/static-targets/status")
	body := getPage(t, server, "/static-targets/status")
	if strings.Contains(body, "<script>alert") || strings.Contains(body, `<b onmouseover`) {
		t.Fatalf("the page has a value as markup:\n%s", body)
	}
	if !strings.Contains(body, "7 static targets, 5 failing, 1 not yet scraped") {
		t.Errorf("the page has no summary line:\n%s", body)
	}
	for _, row := range page.Rows {
		for _, value := range []string{row.Name, row.Collector, row.Target, row.Interval, row.Result, row.LastScrape, row.LastError, row.LastErrorAt, row.Backoff} {
			if value != "" && !strings.Contains(body, templateEscaped(value)) {
				t.Errorf("the page does not show %q of the row %s", value, row.Name)
			}
		}
		for name, value := range row.Labels {
			if !strings.Contains(body, templateEscaped(name+"="+value)) {
				t.Errorf("the page does not show the label %s=%q of the row %s", name, value, row.Name)
			}
		}
	}
	if !strings.Contains(body, `href="../"`) {
		t.Errorf("the page does not link the landing page relatively:\n%s", body)
	}
}

// templateEscaped is s as html/template escapes text, which writes some
// characters otherwise than html.EscapeString.
func templateEscaped(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;", "+", "&#43;", "\x00", "�").Replace(s)
}

// A credential in a target's address is shown neither in its target nor in
// the error of its failed scrape, which quotes the URL.
func TestTheStatusPageWithholdsTheTargetsCredentials(t *testing.T) {
	server := statusTargets(t)
	r := getPath(server, http.MethodGet, "/static-targets/status?format=json")
	body := getPage(t, server, "/static-targets/status")
	for _, text := range []string{r.Body.String(), body} {
		if strings.Contains(text, "hunter2") || strings.Contains(text, "hunter3") {
			t.Fatalf("the status shows a credential:\n%s", text)
		}
	}
	var secret staticStatusRow
	for _, row := range statusJSON(t, server, "/static-targets/status").Rows {
		if row.Name == "secret" {
			secret = row
		}
	}
	if secret.Result != statusFailed || secret.LastError == "" || !strings.Contains(secret.Target, "127.0.0.1") {
		t.Errorf("the target with a credential has the row %+v", secret)
	}
}

// The page is protected by the exporter's Basic Auth, as every endpoint
// but /health and /ready; it is read with GET or HEAD, and anything else is
// answered 405 with the methods allowed; a format it does not know is a
// 400.
func TestTheStatusPageIsReadOnlyAndAuthenticated(t *testing.T) {
	target := textTarget(t, "value=1\n")
	cfg := weatherConfig()
	cfg.Web = model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, Username: "u", Password: "p"}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "w", Collector: "weather", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	for _, path := range []string{"/static-targets/status", "/static-targets/status?format=json"} {
		if r := getPath(server, http.MethodGet, path); r.Code != http.StatusUnauthorized {
			t.Errorf("an unauthenticated %s answered %d, want 401", path, r.Code)
		}
	}
	send := func(method, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, nil)
		request.SetBasicAuth("u", "p")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if r := send(method, "/static-targets/status"); r.Code != http.StatusOK {
			t.Errorf("an authenticated %s answered %d", method, r.Code)
		}
	}
	if r := send(http.MethodPost, "/static-targets/status"); r.Code != http.StatusMethodNotAllowed || r.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("a POST answered %d with Allow %q, want 405 with GET, HEAD", r.Code, r.Header().Get("Allow"))
	}
	if r := send(http.MethodGet, "/static-targets/status?format=xml"); r.Code != http.StatusBadRequest {
		t.Errorf("an unknown format answered %d, want 400", r.Code)
	}
}

// Without static targets there is no status page: it is not found, and the
// landing page does not link it; with them, the landing page does.
func TestTheStatusPageIsThereOnlyWithStaticTargets(t *testing.T) {
	server := landingServer(t, weatherConfig())
	if r := getPath(server, http.MethodGet, "/static-targets/status"); r.Code != http.StatusNotFound {
		t.Errorf("without static targets the status page answered %d, want 404", r.Code)
	}
	if page := getPage(t, server, "/"); strings.Contains(page, "static-targets/status") {
		t.Errorf("the landing page links the status page without static targets:\n%s", page)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "w", Collector: "weather", Target: "http://127.0.0.1:1/"}}}
	server = newStaticServer(t, weatherConfig(), file)
	page := getPage(t, server, "/")
	requireContains(t, page, `<a href="static-targets/status">/static-targets/status</a>`)
}

// The page moves with --web.static-targets-path, and a self-metrics path
// where it would be is refused.
func TestTheStatusPageFollowsTheStaticTargetsPath(t *testing.T) {
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "w", Collector: "weather", Target: "http://127.0.0.1:1/"}}}
	server := newStaticServer(t, weatherConfig(), file)
	server.SetStaticTargetsPath("/static/targets")
	page := getPage(t, server, "/static/targets/status")
	requireContains(t, page, `href="../../"`, "<code>/static/targets</code>")
	if _, err := StaticTargetsPath("/static-targets", "/static-targets/status"); err == nil || !strings.Contains(err.Error(), "status page") {
		t.Errorf("a self-metrics path on the status page's was refused with %v", err)
	}
}

// In allocator mode the page says its targets are this replica's share.
func TestTheStatusPageSaysItShowsThisReplicasTargets(t *testing.T) {
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "w", Collector: "weather", Target: "http://127.0.0.1:1/"}}}
	server := newStaticServer(t, weatherConfig(), file)
	server.SetAllocatorStatus(func() AllocatorStatus { return AllocatorStatus{SyncErrors: map[string]uint64{}} })
	requireContains(t, getPage(t, server, "/static-targets/status"), "the static targets the allocator gave this replica")
	if !statusJSON(t, server, "/static-targets/status").Allocator {
		t.Error("the JSON does not say the exporter is in allocator mode")
	}
}
