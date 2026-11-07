//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
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
		// The interval, Go's 1m0s in the JSON, is written as the page
		// writes a duration, 1m.
		for _, value := range []string{row.Name, row.Collector, row.Target, pagekit.DurationText(row.Interval), row.Result, row.LastScrape, row.LastError, row.LastErrorAt, row.BackoffUntil} {
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
	requireContains(t, getPage(t, server, "/static-targets/status"), "the static targets the allocator gave this replica", "No bundle from the allocator is in force yet.")
	if !statusJSON(t, server, "/static-targets/status").Allocator {
		t.Error("the JSON does not say the exporter is in allocator mode")
	}
}

// In allocator mode the page names the member this replica is, linking the
// allocator's view of it, and says which bundle is in force and since
// when, and when the allocator was last heard from; the time the bundle
// came is said only when the client's bundle is the manager's.
func TestTheStatusPageLinksTheAllocatorsViewAndSaysTheBundleInForce(t *testing.T) {
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "w", Collector: "weather", Target: "http://127.0.0.1:1/"}}}
	server := newStaticServer(t, weatherConfig(), file)
	server.manager.SetGeneration("g7")
	now := time.Now()
	st := AllocatorStatus{SyncErrors: map[string]uint64{}, URL: "http://pue-allocator:9116/", Member: "pod 0&1", Generation: "g7", InForceAt: now.Add(-90 * time.Second), LastSync: now.Add(-5 * time.Second)}
	server.SetAllocatorStatus(func() AllocatorStatus { return st })
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page,
		`as the member <a href="http://pue-allocator:9116/?member=pod&#43;0%261" title="the allocator's view of pod 0&amp;1">pod 0&amp;1</a>:`,
		`<p class="meta" id="bundle">The bundle in force is generation <code>g7</code>, in force since <time datetime="`+st.InForceAt.UTC().Format(time.RFC3339)+`">`+st.InForceAt.UTC().Format(time.RFC3339)+`</time> (1m30s ago). The allocator was last heard from at <time datetime="`)
	b := statusJSON(t, server, "/static-targets/status").Bundle
	if b == nil || b.Member != "pod 0&1" || b.MemberView != "http://pue-allocator:9116/?member=pod+0%261" || b.Generation != "g7" || b.InForceSince != st.InForceAt.UTC().Format(time.RFC3339) || b.LastSync == "" {
		t.Errorf("the JSON's allocator is %+v", b)
	}
	// The client has seen a bundle the manager has not put in force yet.
	st.Generation = "g8"
	if b := statusJSON(t, server, "/static-targets/status").Bundle; b.Generation != "g7" || b.InForceSince != "" {
		t.Errorf("with the client a bundle ahead, the JSON's allocator is %+v; want g7 with no time", b)
	}
	if strings.Contains(getPage(t, server, "/static-targets/status"), "in force since") {
		t.Error("the page says when a bundle other than the one in force came")
	}
	if strings.Contains(newStaticServerJSON(t), `"allocator"`) {
		t.Error("outside allocator mode the JSON has an allocator")
	}
}

// newStaticServerJSON is the status page's JSON of an exporter outside
// allocator mode.
func newStaticServerJSON(t *testing.T) string {
	t.Helper()
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "w", Collector: "weather", Target: "http://127.0.0.1:1/"}}}
	return getPath(newStaticServer(t, weatherConfig(), file), http.MethodGet, "/static-targets/status?format=json").Body.String()
}

// Each row's collector links its section on the collectors page, relative
// to the page, so the link holds under a prefix and follows the static
// targets path.
func TestATargetsCollectorLinksItsSection(t *testing.T) {
	server := statusTargets(t)
	requireContains(t, getPage(t, server, "/static-targets/status"),
		`<span class="muted">collector</span> <a href="../collectors#collector-text" title="the collector text on the collectors page">text</a>`)
	requireContains(t, getPage(t, server, "/collectors"), `id="collector-text"`)
	server.SetStaticTargetsPath("/a/b/targets")
	requireContains(t, getPage(t, server, "/a/b/targets/status"), `href="../../../collectors#collector-text"`)
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

// A scraped target's name links to the endpoint narrowed to it,
// ?targets=, relative to the page, which serves that target's series and
// no other's; a target not yet scraped, which the endpoint has nothing of,
// has no link. Under --web.debug=full the row links the target's debug
// scrape too, and below it does not. The links follow
// --web.static-targets-path.
func TestATargetsNameLinksItsSeries(t *testing.T) {
	server := statusTargets(t)
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page, `<a href="../static-targets?targets=ok" title="the series the endpoint serves of ok"><strong>ok</strong></a>`)
	if strings.Contains(page, "targets=never") || !strings.Contains(page, "<strong>never</strong>") {
		t.Errorf("the target not yet scraped is linked")
	}
	if strings.Contains(page, "?debug=") {
		t.Errorf("below --web.debug=full the page links a debug scrape")
	}
	base, _ := url.Parse("http://exporter/static-targets/status")
	link, _ := base.Parse("../static-targets?targets=ok")
	series := getPath(server, http.MethodGet, link.RequestURI()).Body.String()
	if !strings.Contains(series, `static_target="ok"`) || strings.Contains(series, `static_target="broken"`) {
		t.Errorf("%s serves:\n%s", link.RequestURI(), series)
	}
	server.SetDebug(DebugFull)
	requireContains(t, getPage(t, server, "/static-targets/status"), `<a class="muted" href="../static-targets?debug=ok" title="scrape ok once now, and report each stage">debug</a>`)
	server.SetStaticTargetsPath("/static/targets")
	requireContains(t, getPage(t, server, "/static/targets/status"), `href="../targets?targets=ok"`)
}

// Each row has a strip of the target's last scrapes, oldest first, marked
// as the recent probes' are, so that a target failing now and then reads
// apart from one that failed once. The strip keeps the last ten.
func TestATargetsRowShowsItsLastScrapes(t *testing.T) {
	server := statusTargets(t)
	results := func(row staticStatusRow) string {
		var got []string
		for _, r := range row.Recent {
			if r.At == "" {
				t.Errorf("a scrape of %s has no time", row.Name)
			}
			got = append(got, r.Result)
		}
		return strings.Join(got, " ")
	}
	for name, want := range map[string]string{
		"ok":          statusSuccess,
		"backing_off": statusFailed + " " + statusBackoff,
		"stale":       statusSuccess + " " + statusStale,
		"never":       "",
	} {
		if got := results(statusRow(t, server, name)); got != want {
			t.Errorf("the strip of %s is %q, want %q", name, got, want)
		}
	}
	r := getPath(server, http.MethodGet, "/static-targets/status?format=json")
	requireContains(t, r.Body.String(), `"name":"never"`)
	if !strings.Contains(r.Body.String(), `"recent":[]`) {
		t.Errorf("the target never scraped has no empty recent list in the JSON")
	}
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page, `<th data-sort="text">Result</th><th data-sort="num">Recent</th><th data-sort="num">Failures</th>`)
	requireContains(t, page, `">✗</span><span class="bad" data-at="`)
	requireContains(t, page, ` backoff">…</span></span></td>`)
	for _, target := range server.manager.StaticTargets() {
		if target.Name == "ok" {
			for range 12 {
				server.scrapeTarget(t.Context(), server.manager.Get(), target)
			}
		}
	}
	if got := len(statusRow(t, server, "ok").Recent); got != recentProbesPerKey {
		t.Errorf("after 13 scrapes the strip has %d, want the last %d", got, recentProbesPerKey)
	}
}

// The static targets table sorts by a column and filters its rows as the
// probe jobs table of the allocator's page does, with the same script:
// the rows carry their order and the text the filter matches, a number's
// cell its value, and the filter's form is hidden until the script shows
// it. The script keeps the order and the filter through a refresh's swap.
func TestTheStaticTargetsTableSortsAndFilters(t *testing.T) {
	server := statusTargets(t)
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page,
		`<form class="filter table" id="static-filter-form" role="search" hidden>`,
		`<input id="static-filter" type="search" autocomplete="off" spellcheck="false">`,
		`<p class="note muted" data-rows-of="static-targets-table" role="status" aria-live="polite"></p>`,
		`<table id="static-targets-table">`+"\n"+`<thead><tr><th data-sort="text">Target</th><th data-sort="num">Interval</th><th data-sort="num">Last scrape</th>`,
		`<tr data-order="0" data-match="backing_off`+"\ndead\n",
		`<td data-value="60">1m</td>`,
		`<td class="strip" data-value="2">`,
		`<td data-value="0">`+"<span class=\"muted\">none yet</span></td>",
		`const arrangeTargets = sortable(content, "static-targets-table", "static-filter", "static targets");`,
		`arrangeTargets(() => { content.innerHTML = fragment; });`,
		`box.setSelectionRange(typing[0], typing[1]);`,
		`table.sortable th:not(.num) button::after { content: "\00a0\2195";`)
	_, table, _ := strings.Cut(page, `id="static-targets-table"`)
	table, _, _ = strings.Cut(table, "</table>")
	if strings.Contains(table, "<button") || strings.Contains(table, "aria-sort") {
		t.Errorf("the table's headings are buttons without the script:\n%s", table)
	}
	if !strings.Contains(table, `data-match="ok`+"\ntext\n") || !strings.Contains(table, "team=&lt;b onmouseover=&#34;x&#34;&gt;\nsuccess\"") {
		t.Errorf("the row of ok does not match its name, collector, labels and result:\n%s", table)
	}
}

// Every moment the status page shows is a <time> of RFC 3339 in UTC, which
// a page without JavaScript shows as it is; the page's script shows it in
// the browser's time with UTC and how long ago in its tooltip, after a
// refresh too, and writes a strip mark's moment the same way.
func TestTheStatusPageWritesEachMomentAsATime(t *testing.T) {
	server := statusTargets(t)
	page := getPage(t, server, "/static-targets/status")
	row := statusRow(t, server, "ok")
	b := statusRow(t, server, "backing_off")
	requireContains(t, page,
		`<td data-value="`, `<time datetime="`+row.LastScrape+`">`+row.LastScrape+`</time><br><span class="muted">`,
		`backing off until <time datetime="`+b.BackoffUntil+`">`+b.BackoffUntil+`</time>`,
		`at <time datetime="`+b.LastErrorAt+`">`+b.LastErrorAt+`</time>`,
		`data-at="`+row.Recent[0].At+`" title="`+row.Recent[0].At+` success"`,
		"function localTimes(root) {", "localTimes(content);\n  function swap(fragment) {", "arrangeTargets(() => { content.innerHTML = fragment; });\n    localTimes(content);")
	if strings.Contains(page, `<td data-value="`+fmt.Sprint(row.ScrapedAt)+`">`+row.LastScrape) {
		t.Error("a last scrape's moment is no <time>")
	}
}

// A strip's marks, read one symbol at a time, say nothing of what they
// stand for: a screen reader is told the strip as its counts and its
// latest result, and not the marks, which stay for the eye.
func TestAStripIsToldToAScreenReaderAsItsCounts(t *testing.T) {
	for _, c := range []struct {
		results []string
		want    string
	}{
		{nil, ""},
		{[]string{statusSuccess}, "last 1 scrape: 1 success; the latest success"},
		{[]string{statusFailed, statusSuccess, statusFailed, statusBackoff}, "last 4 scrapes, oldest first: 2 failed, 1 success, 1 skipped by backoff; the latest skipped by backoff"},
	} {
		if got := stripText(c.results, "scrape"); got != c.want {
			t.Errorf("stripText(%v) = %q, want %q", c.results, got, c.want)
		}
	}
	server := statusTargets(t)
	probeOnce(t, server, probeOf("text", textTarget(t, "value=1\n").URL), nil)
	page := getPage(t, server, "/static-targets/status")
	requireContains(t, page,
		`<td class="strip" data-value="2"><span class="sr-only">last 2 scrapes, oldest first: 1 failed, 1 skipped by backoff; the latest skipped by backoff</span><span aria-hidden="true"><span class="bad" data-at="`,
		`<td class="strip"><span class="sr-only">last 1 probe: 1 success; the latest success</span><span aria-hidden="true">`,
		`.sr-only { position: absolute; width: 1px; height: 1px;`)
}
