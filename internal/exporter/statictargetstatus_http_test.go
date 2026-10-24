//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
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
	if b := rows["backing_off"]; b.Result != statusBackoff || b.Failures != 1 || b.BackoffUntil != "2026-10-23T09:01:00Z" || b.Backoff != "backing off until 2026-10-23T09:01:00Z" || b.BackoffFailure != 1 || !strings.Contains(b.LastError, "received HTTP status 500") {
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

// A scrape failure_backoff skips is shown as skipped, and is no failed
// scrape: the error shown stays that of the scrape that failed, with when it
// began, and the consecutive failures are the failed scrapes alone, however
// many are skipped after them, while the backoff shows its own count.
func TestTheStatusPageKeepsTheRealErrorWhileBackingOff(t *testing.T) {
	dead := newDeadTarget(t)
	cfg := &model.Config{Collectors: []model.Collector{backoffCollector("dead", 2, time.Minute, time.Minute)}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "d", Collector: "dead", Target: dead.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	static := server.manager.StaticTargets()[0]
	server.scrapeTarget(t.Context(), server.manager.Get(), static)
	server.scrapeTarget(t.Context(), server.manager.Get(), static)
	failed := statusJSON(t, server, "/static-targets/status").Rows[0]
	if failed.Result != statusFailed || failed.Failures != 2 || !strings.Contains(failed.LastError, "received HTTP status 500") || failed.BackoffFailure != 2 {
		t.Fatalf("after two failed scrapes the row is %+v", failed)
	}
	// The second scrape's time is kept apart from the skipped ones' by
	// waiting past the second the page shows.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	for range 3 {
		server.scrapeTarget(t.Context(), server.manager.Get(), static)
	}
	if dead.total() != 2 {
		t.Fatalf("the target was sent %d requests, want 2: the scrapes after were to be skipped", dead.total())
	}
	row := statusJSON(t, server, "/static-targets/status").Rows[0]
	if row.Result != statusBackoff || !row.Failing || row.Failures != 2 || row.LastError != failed.LastError || row.LastErrorAt != failed.LastErrorAt || row.LastScrape == failed.LastScrape || row.BackoffFailure != 2 || row.Backoff == "" {
		t.Errorf("after three scrapes skipped by backoff the row is %+v; want skipped by backoff with the failed scrape's error, its time and 2 failures", row)
	}
}

// statusRow is the status page's row of the target name, as JSON.
func statusRow(t *testing.T, server *Server, name string) staticStatusRow {
	t.Helper()
	for _, row := range statusJSON(t, server, "/static-targets/status").Rows {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("the status page has no row %s", name)
	return staticStatusRow{}
}

// checkedTargets is targets as a file the exporter has read and checked.
func checkedTargets(t *testing.T, cfg *model.Config, targets ...model.StaticTarget) *model.StaticTargetFile {
	t.Helper()
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: targets}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatal(err)
	}
	return file
}

// A target removed and brought back under its name, its collector and its
// address as they were, starts again: nothing having read the static
// targets endpoint between, the page shows it not yet scraped, with no
// failures and no error, and not the last scrape of the one removed.
func TestATargetBroughtBackIsNotYetScraped(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	up := textTarget(t, "value=1\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	broken := model.StaticTarget{Name: "broken", Collector: "text", Target: down.URL}
	kept := model.StaticTarget{Name: "kept", Collector: "text", Target: up.URL}
	server := newStaticServer(t, cfg, checkedTargets(t, cfg, broken, kept))
	server.logger = testutil.QuietLogger(t)
	server.reconcile()
	server.scrapeTarget(t.Context(), server.manager.Get(), server.manager.StaticTargets()[0])
	if row := statusRow(t, server, "broken"); row.Result != statusFailed || row.Failures != 1 {
		t.Fatalf("the failed scrape is shown as %+v", row)
	}
	// Each file is followed as a reload follows the one it puts in force.
	server.manager.SetTargets("", checkedTargets(t, cfg, kept))
	server.reconcile()
	server.manager.SetTargets("", checkedTargets(t, cfg, broken, kept))
	server.reconcile()
	row := statusRow(t, server, "broken")
	if row.Result != statusNotScraped || row.Failures != 0 || row.LastError != "" || row.LastErrorAt != "" || row.LastScrape != "" || row.Failing {
		t.Errorf("the target brought back is shown as %+v; want not yet scraped, with no failures and no error", row)
	}
}

// What is kept of a static target, its result, its last success and its
// status page record, is forgotten when a reload or a bundle from the
// allocator removes it, though nothing reads the static targets endpoint,
// as nothing does where the target is exported over OTLP alone.
func TestARemovedTargetIsForgottenWithoutAReadOfTheEndpoint(t *testing.T) {
	up := textTarget(t, "value=1\n")
	document := testutil.CollectorsDocument("text") + "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\n"
	targets := func(names ...string) string {
		var doc strings.Builder
		doc.WriteString("interval: 1m\ntargets:\n")
		for _, name := range names {
			fmt.Fprintf(&doc, "  - name: %s\n    collector: text\n    target: %s\n    export_via_otlp: true\n", name, up.URL)
		}
		return doc.String()
	}
	r := newReloadable(t, document, targets("otlp_only", "other"))
	r.server.logger = testutil.QuietLogger(t)
	kept := func() (results, successes, statuses []string) {
		r.server.staticMu.Lock()
		defer r.server.staticMu.Unlock()
		return model.SortedKeys(r.server.staticResults), model.SortedKeys(r.server.staticLastSuccess), model.SortedKeys(r.server.staticStatus)
	}
	scrapeAll := func() {
		for _, target := range r.manager.StaticTargets() {
			r.server.scrapeTarget(t.Context(), r.manager.Get(), target)
		}
	}
	check := func(when string, want ...string) {
		t.Helper()
		results, successes, statuses := kept()
		for what, names := range map[string][]string{"results": results, "last successes": successes, "status records": statuses} {
			if strings.Join(names, " ") != strings.Join(want, " ") {
				t.Errorf("%s, the %s kept are of %v, want of %v", when, what, names, want)
			}
		}
	}
	r.server.reconcile()
	scrapeAll()
	check("after the scrapes", "other", "otlp_only")
	r.reloadBoth(document, targets("other"))
	check("after a reload removed a target", "other")

	r.reloadBoth(document, targets("otlp_only", "other"))
	scrapeAll()
	check("after it was brought back and scraped", "other", "otlp_only")
	err := r.manager.ReloadReplacing("allocator", "g2", func() (func() error, error) {
		r.write(r.targets, targets("otlp_only"))
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	check("after a bundle from the allocator moved a target away", "otlp_only")
}
