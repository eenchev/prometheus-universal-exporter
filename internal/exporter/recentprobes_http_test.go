//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// probeRows is the status page's recent probes, as JSON, by collector and
// target.
func probeRows(t *testing.T, server *Server) map[string]probeStatusRow {
	t.Helper()
	rows := map[string]probeStatusRow{}
	for _, row := range statusJSON(t, server, "/static-targets/status").Probes {
		rows[row.Collector+" "+row.Target] = row
	}
	return rows
}

// Every probe answered is recorded under its collector and target with its
// result, the status it was answered with, its series and its error:
// success, failed, refused by the target policy (403), rejected by the
// concurrency limit (503), cached, stale result served and backing off. The
// failing keys come first, and the summary counts them.
func TestTheStatusPageShowsTheRecentProbes(t *testing.T) {
	up := textTarget(t, "value=42\n")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	held := newHeldTarget(t)
	dead := newDeadTarget(t)
	flaky := newDeadTarget(t)
	flaky.failing.Store(false)
	guarded := testutil.Collector("guarded", "text")
	guarded.Request.AllowedTargets = []string{"example.invalid"}
	single := testutil.Collector("single", "text")
	single.MaxConcurrentProbes = 1
	cached := testutil.Collector("cached", "text")
	cached.Cache.TTL = model.Duration(time.Hour)
	server := modeServer(t, testutil.Collector("text", "text"), guarded, single, cached,
		staleCollector(time.Millisecond, time.Hour), backoffCollector("dead", 1, time.Minute, time.Minute))
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	expect := func(collector, target string, code int) {
		t.Helper()
		if r := probeOnce(t, server, probeOf(collector, target), nil); r.Code != code {
			t.Fatalf("the probe of %s through %s answered %d, want %d: %s", target, collector, r.Code, code, r.Body)
		}
	}
	expect("text", up.URL, http.StatusOK)
	expect("text", down.URL, http.StatusBadGateway)
	expect("guarded", up.URL, http.StatusForbidden)
	busy := make(chan int, 1)
	go func() { busy <- probeOnce(t, server, probeOf("single", held.server.URL+"/a"), nil).Code }()
	testutil.WaitFor(t, "the collector's slot to be taken", func() bool { return server.trips.count("single") == 1 })
	expect("single", held.server.URL+"/b", http.StatusServiceUnavailable)
	held.open()
	<-busy
	expect("cached", up.URL, http.StatusOK)
	expect("cached", up.URL, http.StatusOK)
	expect("flaky", flaky.URL, http.StatusOK)
	ageEntries(server, time.Second)
	flaky.failing.Store(true)
	expect("flaky", flaky.URL, http.StatusOK)
	expect("dead", dead.URL, http.StatusBadGateway)
	expect("dead", dead.URL, http.StatusBadGateway)

	page := statusJSON(t, server, "/static-targets/status")
	if page.ProbeKeys != 8 || page.FailingProbes != 5 || page.Total != 0 || len(page.Rows) != 0 {
		t.Errorf("the summary is %d keys, %d failing, %d static targets; want 8, 5, 0", page.ProbeKeys, page.FailingProbes, page.Total)
	}
	var order []string
	for _, row := range page.Probes {
		order = append(order, row.Collector)
	}
	if want := "dead flaky guarded single text cached single text"; strings.Join(order, " ") != want {
		t.Errorf("the keys are in the order %v, want %s: the failing first, each part by collector and target", order, want)
	}
	rows := probeRows(t, server)
	for key, want := range map[string]struct {
		result string
		status int
		series int
		probes int
		err    string
	}{
		"text " + up.URL:                   {probeSuccess, 200, 1, 1, ""},
		"text " + down.URL:                 {probeFailed, 502, 0, 1, "received HTTP status 500"},
		"guarded " + up.URL:                {probeRefused, 403, 0, 1, "allowed_targets"},
		"single " + held.server.URL + "/b": {probeRejected, 503, 0, 1, "max_concurrent_probes"},
		"single " + held.server.URL + "/a": {probeSuccess, 200, 1, 1, ""},
		"cached " + up.URL:                 {probeCached, 200, 1, 2, ""},
		"flaky " + flaky.URL:               {probeStale, 200, 0, 2, "received HTTP status 500"},
		"dead " + dead.URL:                 {probeBackoff, 502, 0, 2, "backing off"},
	} {
		row, found := rows[key]
		if !found {
			t.Errorf("no row for %s; the rows are %v", key, model.SortedKeys(rows))
			continue
		}
		if row.Result != want.result || row.Status != want.status || len(row.Probes) != want.probes || row.Failing != probeFailing(want.result) || row.LastProbe == "" {
			t.Errorf("the row of %s is %+v; want %s, HTTP %d and %d probes", key, row, want.result, want.status, want.probes)
		}
		if want.series > 0 && row.Series < want.series {
			t.Errorf("the row of %s has %d series, want at least %d", key, row.Series, want.series)
		}
		if want.err == "" && row.LastError != "" || !strings.Contains(row.LastError, want.err) {
			t.Errorf("the row of %s has the error %q, want one with %q", key, row.LastError, want.err)
		}
	}
	if probes := rows["cached "+up.URL].Probes; len(probes) != 2 || probes[0].Result != probeSuccess || probes[1].Result != probeCached {
		t.Errorf("the cached collector's probes are %+v; want success, then cached, oldest first", probes)
	}
	if probes := rows["dead "+dead.URL].Probes; len(probes) != 2 || probes[0].Result != probeFailed || probes[0].Error == "" || probes[1].Result != probeBackoff {
		t.Errorf("the backed off target's probes are %+v; want failed, then backing off", probes)
	}
	body := getPage(t, server, "/static-targets/status")
	requireContains(t, body, "Recent probes", "8 collector and targets probed, 5 failing their latest probe", "No static targets are configured.", "rejected by limit", "refused by policy", "backing off", ">✓</span>", ">✗</span>", ">~</span>", ">…</span>")
}

// A probe that joined another's trip is recorded, marked shared.
func TestTheStatusPageMarksASharedProbe(t *testing.T) {
	target := newDeadTarget(t)
	target.failing.Store(false)
	target.hold, target.arrived = make(chan struct{}), make(chan struct{}, 1)
	server := modeServer(t, testutil.Collector("text", "text"))
	codes := make(chan int, 2)
	go func() { codes <- probeOnce(t, server, probeOf("text", target.URL), nil).Code }()
	<-target.arrived
	go func() { codes <- probeOnce(t, server, probeOf("text", target.URL), nil).Code }()
	testutil.WaitFor(t, "the second probe to join the trip", func() bool {
		server.flights.mu.Lock()
		defer server.flights.mu.Unlock()
		for _, flight := range server.flights.flights {
			if flight.waiters == 2 {
				return true
			}
		}
		return false
	})
	close(target.hold)
	<-codes
	<-codes
	row := probeRows(t, server)["text "+target.URL]
	if len(row.Probes) != 2 || row.Probes[0].Shared == row.Probes[1].Shared || row.Result != probeSuccess {
		t.Errorf("the probes are %+v; want two successes, one of them shared", row.Probes)
	}
}

// A key keeps its last recentProbesPerKey probes, and no more than
// recentProbeKeys keys are kept, the one probed least recently forgotten
// first.
func TestTheRecentProbesAreBounded(t *testing.T) {
	server := modeServer(t, testutil.Collector("text", "text"))
	read := server.readAt(server.reconcile().generation)
	start := time.Now()
	key := func(i int) recentProbeKey {
		return recentProbeKey{collector: "text", target: fmt.Sprintf("http://t%d.example/", i)}
	}
	for i := range recentProbesPerKey + 5 {
		server.noteProbe(read, key(0), recentProbe{at: start.Add(time.Duration(i) * time.Second), result: probeSuccess, status: 200, series: i})
	}
	for i := 1; i < recentProbeKeys; i++ {
		server.noteProbe(read, key(i), recentProbe{at: start, result: probeSuccess, status: 200})
	}
	// Key 0 is probed again, so key 1 is the one probed least recently.
	server.noteProbe(read, key(0), recentProbe{at: start.Add(time.Minute), result: probeFailed, status: 502, failure: "boom"})
	server.noteProbe(read, key(recentProbeKeys), recentProbe{at: start, result: probeSuccess, status: 200})
	rows := probeRows(t, server)
	if len(rows) != recentProbeKeys {
		t.Fatalf("%d keys are kept, want %d", len(rows), recentProbeKeys)
	}
	if _, kept := rows["text "+key(1).target]; kept {
		t.Error("the key probed least recently was kept")
	}
	first := rows["text "+key(0).target]
	if len(first.Probes) != recentProbesPerKey || first.Probes[0].Series != 6 || first.Result != probeFailed || first.LastError != "boom" {
		t.Errorf("the key probed most has the probes %+v, the latest %s %q; want %d, from the 7th, the latest failed boom", first.Probes, first.Result, first.LastError, recentProbesPerKey)
	}
	if _, kept := rows["text "+key(recentProbeKeys).target]; !kept {
		t.Error("the new key was not kept")
	}
}

// What the recent probes hold of a key whose target and parameters a caller
// made 8 KiB long, failing, is the target cut to 512 bytes, the parameters
// cut and the error, within the 3.5 KiB a key may take with the target,
// the parameters and the error at their bounds (512, 512 and 2000 bytes)
// and what holds them: neither the target, the parameter nor the line of
// the request that named them stays with the key, which would be over
// 8 KiB. Measured as what
// forgetting the keys frees, with the server kept alive through both
// measurements and what the probes left settled by a collection before the
// first, over the 500 keys kept of 1,000 probed, 200 under the race
// detector.
func TestTheRecentProbesHoldLittleOfLongTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer target.Close()
	app := testutil.Collector("app", "text")
	app.Request.Path = "/{{param_tenant}}"
	server := modeServer(t, app)
	server.logger = testutil.QuietLogger(t)
	handler := server.Handler()
	pad := strings.Repeat("a", 8000)
	keys := alloctest.UnlessRaced(1000, 200)
	for i := range keys {
		query := url.Values{"collector": {"app"}, "target": {target.URL + "/" + strconv.Itoa(i) + pad}, "param_tenant": {pad}}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe?"+query.Encode(), nil))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("probe %d answered %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	server.recent.mu.Lock()
	kept := len(server.recent.entries)
	for key, entry := range server.recent.entries {
		if len(key.target) > recentProbeTargetBytes || len(key.params) > recentProbeParamsBytes || len(entry.probes) != 1 || len(entry.probes[0].failure) > model.MaxFailureBytes || entry.probes[0].failure == "" {
			t.Errorf("a key is kept with a target of %d bytes, parameters of %d and %d probes", len(key.target), len(key.params), len(entry.probes))
			break
		}
	}
	server.recent.mu.Unlock()
	if kept != min(keys, recentProbeKeys) {
		t.Fatalf("%d keys are kept of %d probed", kept, keys)
	}
	var remembered, forgotten runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&remembered)
	server.recent.mu.Lock()
	server.recent.entries = map[recentProbeKey]*recentProbeEntry{}
	server.recent.order.Init()
	server.recent.mu.Unlock()
	runtime.GC()
	runtime.ReadMemStats(&forgotten)
	runtime.KeepAlive(server)
	held := int64(remembered.HeapAlloc) - int64(forgotten.HeapAlloc)
	if perKey := held / int64(kept); perKey > 3584 {
		t.Errorf("the recent probes hold %d bytes for each of %d keys of 8 KiB targets, want at most 3584", perKey, kept)
	}
}

// The key shows the target as the log does, with its credentials withheld,
// and the probe parameters cut, a credential's withheld; the error is masked
// and every value is escaped on the page.
func TestTheRecentProbesWithholdCredentialsAndAreEscaped(t *testing.T) {
	hostile := textTarget(t, "value=<script>alert(1)</script>\n")
	strict := testutil.Collector("strict", "text")
	strict.Metrics[0].ErrorMode = "fail"
	strict.Metrics[0].Expression = `value=(.+)`
	strict.Request.Path = "/{{param_tenant}}/{{param_token}}"
	refused := httptest.NewServer(http.NotFoundHandler())
	secretURL := strings.Replace(refused.URL, "http://", "http://reader:hunter2@", 1) + "/?token=hunter3"
	refused.Close()
	server := modeServer(t, strict, testutil.Collector("text", "text"))
	server.logger = testutil.QuietLogger(t)
	long := strings.Repeat("x", 300)
	if r := probeOnce(t, server, probeOf("strict", hostile.URL)+"&param_tenant="+url.QueryEscape("<b>"+long)+"&param_token=hunter4", nil); r.Code != http.StatusBadGateway {
		t.Fatalf("the hostile probe answered %d: %s", r.Code, r.Body)
	}
	probeOnce(t, server, probeOf("text", secretURL), nil)
	r := getPath(server, http.MethodGet, "/static-targets/status?format=json")
	body := getPage(t, server, "/static-targets/status")
	for _, text := range []string{r.Body.String(), body} {
		for _, secret := range []string{"hunter2", "hunter3", "hunter4"} {
			if strings.Contains(text, secret) {
				t.Fatalf("the status shows the credential %s:\n%s", secret, text)
			}
		}
	}
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<b>x") {
		t.Fatalf("the page has a value as markup:\n%s", body)
	}
	rows := probeRows(t, server)
	row := rows["strict "+hostile.URL]
	if !strings.Contains(row.Params, "param_tenant=<b>xx") || strings.Contains(row.Params, long) || !strings.Contains(row.Params, "param_token=<redacted>") || !strings.Contains(row.LastError, "<script>") {
		t.Errorf("the hostile probe's row is %+v; want its parameters cut, the token withheld, and its error quoting the page", row)
	}
	for _, value := range []string{row.Params, row.LastError} {
		if !strings.Contains(body, templateEscaped(value)) {
			t.Errorf("the page does not show %q escaped", value)
		}
	}
	var secret probeStatusRow
	for _, row := range rows {
		if row.Collector == "text" {
			secret = row
		}
	}
	if secret.Result != probeFailed || secret.LastError == "" || !strings.Contains(secret.Target, "127.0.0.1") {
		t.Errorf("the probe of a target with credentials has the row %+v", secret)
	}
}

// A debug probe is counted nowhere and is not recorded; a probe refused for
// its parameters (400) is not either.
func TestADebugProbeIsNotARecentProbe(t *testing.T) {
	up := textTarget(t, "value=1\n")
	server := modeServer(t, testutil.Collector("text", "text"))
	server.SetDebug(DebugFull)
	if r := probeOnce(t, server, probeOf("text", up.URL)+"&debug=true", nil); r.Code != http.StatusOK {
		t.Fatalf("the debug probe answered %d", r.Code)
	}
	probeOnce(t, server, probeOf("nope", up.URL), nil)
	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(up.URL)+"&timeout=x", nil)
	if page := statusJSON(t, server, "/static-targets/status"); page.ProbeKeys != 0 || len(page.Probes) != 0 {
		t.Errorf("the recent probes are %+v; want none", page.Probes)
	}
}

// A reload forgets the recent probes of a collector it removes or changes,
// and keeps those of one it leaves as it was; a probe of the removed
// collector that ends after the reload records nothing.
func TestAReloadForgetsTheRecentProbesOfARemovedCollector(t *testing.T) {
	up := textTarget(t, "value=1\n")
	r := newReloadable(t, testutil.CollectorsDocument("kept", "removed", "changed"), "")
	for _, name := range []string{"kept", "removed", "changed"} {
		probeOnce(t, r.server, probeOf(name, up.URL), nil)
	}
	stale := r.server.readAt(r.server.reconcile().generation)
	changed := strings.Replace(testutil.CollectorsDocument("kept", "changed"), "changed_value", "changed_other", 1)
	r.reloadTo(changed)
	r.server.noteProbe(stale, recentProbeKey{collector: "removed", target: up.URL}, recentProbe{at: time.Now(), result: probeSuccess})
	var collectors []string
	for _, row := range statusJSON(t, r.server, "/static-targets/status").Probes {
		collectors = append(collectors, row.Collector)
	}
	if strings.Join(collectors, " ") != "kept" {
		t.Errorf("after the reload the recent probes are of %v, want of kept alone", collectors)
	}
}

// The page is served without static targets, for its recent probes, and
// the landing page links it.
func TestTheStatusPageIsServedWithoutStaticTargets(t *testing.T) {
	server := landingServer(t, weatherConfig())
	body := getPage(t, server, "/static-targets/status")
	requireContains(t, body, "No static targets are configured.", "Recent probes", "No probe has been answered")
	page := statusJSON(t, server, "/static-targets/status")
	if page.Total != 0 || page.Rows == nil || page.Probes == nil {
		t.Errorf("the JSON without static targets or probes is %+v; want empty arrays", page)
	}
	requireContains(t, getPage(t, server, "/"), `<a href="static-targets/status">/static-targets/status</a>`)
}

// The JSON has the recent probes as an array of objects, each with its
// probes, under the names the documentation gives.
func TestTheRecentProbesJSONShape(t *testing.T) {
	up := textTarget(t, "value=1\n")
	server := modeServer(t, testutil.Collector("text", "text"))
	probeOnce(t, server, probeOf("text", up.URL), nil)
	body := getPath(server, http.MethodGet, "/static-targets/status?format=json").Body.String()
	for _, field := range []string{`"probe_keys":1`, `"failing_probe_keys":0`, `"probes":[{"collector":"text","target":"` + up.URL + `","result":"success","failing":false,"shared":false,"last_probe":"`, `"last_probe_age_seconds":`, `"duration_seconds":`, `"status":200,"series":1,"probes":[{"at":"`} {
		if !strings.Contains(body, field) {
			t.Errorf("the JSON has no %s:\n%s", field, body)
		}
	}
}

// A probe that panicked is shown with a fixed error, as a static target's
// scrape that panicked is: the panic says whatever its code had at hand, a
// URL with its password here, and only the log has it.
func TestAPanickedProbeShowsAFixedError(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	fetch.RequestTypes["panics"] = &fetch.RequestType{
		Name:         "panics",
		Fields:       []string{"path"},
		TargetFields: []string{"path"},
		Requester: fetchOnly(func(context.Context, string, *model.Collector, fetch.RequestOverrides, http.Header) (*fetch.HTTPResponse, error) {
			panic("boom at https://reader:hunter2@db.example/x")
		}),
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "panics") })
	broken := testutil.Collector("broken", "text")
	broken.Request = model.RequestConfig{Type: "panics", Path: "/data"}
	server := modeServer(t, broken)
	if r := probeOnce(t, server, probeOf("broken", "panics://data"), nil); r.Code != http.StatusInternalServerError {
		t.Fatalf("the panicking probe answered %d: %s", r.Code, r.Body)
	}
	row := probeRows(t, server)["broken panics://data"]
	if row.Result != probeFailed || row.LastError != errScrapePanicked.Error() {
		t.Errorf("the panicked probe's row is %+v; want failed with %q", row, errScrapePanicked)
	}
	body := getPage(t, server, "/static-targets/status")
	for _, text := range []string{body, statusJSONText(t, server)} {
		if strings.Contains(text, "boom") || strings.Contains(text, "hunter2") {
			t.Fatalf("the status shows the panic:\n%s", text)
		}
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Fatalf("the panic is not in the log:\n%s", logs.String())
	}
}

// A probe that panics is answered a fixed text, since its caller keeps the
// answer as the scrape's error out of the exporter's hands: the panic says
// whatever its code had at hand, a URL with its password here. A debug
// report, an operator's tool, shows the panic with the URL's userinfo
// withheld, and the exporter's log has it as it was.
func TestAPanickedProbeAnswersAFixedError(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	fetch.RequestTypes["panics"] = &fetch.RequestType{
		Name:         "panics",
		Fields:       []string{"path"},
		TargetFields: []string{"path"},
		Requester: fetchOnly(func(context.Context, string, *model.Collector, fetch.RequestOverrides, http.Header) (*fetch.HTTPResponse, error) {
			panic("boom at https://user:hunter2@host/x")
		}),
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "panics") })
	broken := testutil.Collector("broken", "text")
	broken.Request = model.RequestConfig{Type: "panics", Path: "/data"}
	static := model.StaticTarget{Name: "broken", Collector: "broken", Target: "panics://data"}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{broken}}, &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{static}})
	server.SetDebug(DebugFull)
	r := probeOnce(t, server, probeOf("broken", "panics://data"), nil)
	if r.Code != http.StatusInternalServerError || r.Body.String() != "probe failed: "+errScrapePanicked.Error()+"\n" {
		t.Fatalf("the panicking probe answered %d: %q", r.Code, r.Body)
	}
	for _, report := range []string{
		debugProbeGet(t, server, "collector=broken&debug=true&target=panics://data").Body.String(),
		getPath(server, http.MethodGet, "/static-targets?debug=broken").Body.String(),
	} {
		if strings.Contains(report, "hunter2") || !strings.Contains(report, "boom at https://redacted:redacted@host/x") {
			t.Errorf("the debug report shows the panic so:\n%s", report)
		}
	}
	if !strings.Contains(logs.String(), "boom at https://user:hunter2@host/x") {
		t.Fatalf("the panic is not in the log:\n%s", logs.String())
	}
}

// statusJSONText is the status page as JSON, as served.
func statusJSONText(t *testing.T, server *Server) string {
	t.Helper()
	return getPath(server, http.MethodGet, "/static-targets/status?format=json").Body.String()
}

// The userinfo of a URL is withheld wherever the page shows text the
// exporter did not write: in an error that quotes one, a probe's or a static
// target's alike, and in a probe parameter's value.
func TestTheRecentProbesWithholdAURLsUserinfo(t *testing.T) {
	quoting := textTarget(t, "value=https://reader:hunter2@db.example/x\n")
	strict := testutil.Collector("strict", "text")
	strict.Metrics[0].ErrorMode = "fail"
	strict.Metrics[0].Expression = `value=(.+)`
	strict.Request.Path = "/{{param_tenant}}"
	server := modeServer(t, strict)
	server.logger = testutil.QuietLogger(t)
	tenant := url.QueryEscape("https://writer:hunter3@other.example/t")
	if r := probeOnce(t, server, probeOf("strict", quoting.URL)+"&param_tenant="+tenant, nil); r.Code != http.StatusBadGateway {
		t.Fatalf("the probe answered %d: %s", r.Code, r.Body)
	}
	body := getPage(t, server, "/static-targets/status")
	for _, text := range []string{body, statusJSONText(t, server)} {
		for _, secret := range []string{"hunter2", "hunter3"} {
			if strings.Contains(text, secret) {
				t.Fatalf("the status shows the password %s:\n%s", secret, text)
			}
		}
	}
	row := probeRows(t, server)["strict "+quoting.URL]
	if !strings.Contains(row.LastError, "db.example") || !strings.Contains(row.Params, "other.example") {
		t.Errorf("the row is %+v; want its error and parameter shown, the passwords withheld", row)
	}
	for _, err := range []error{
		errors.New("fetching https://reader:hunter2@db.example/x: refused"),
		&url.Error{Op: "Get", URL: "https://reader:hunter2@db.example/x?token=abc", Err: errors.New("refused")},
	} {
		for _, level := range []DebugLevel{DebugOff, DebugFull} {
			server.SetDebug(level)
			if shown := server.shownStatusError(err); strings.Contains(shown, "hunter2") || strings.Contains(shown, "abc") || !strings.Contains(shown, "db.example") {
				t.Errorf("with --web.debug=%s the error %q is shown as %q", level, err, shown)
			}
		}
	}
}

// noteProbeLikeOneBefore records, as a probe does, a probe of collector c
// with two parameters, like the probe before it.
func noteProbeLikeOneBefore(server *Server, read configRead, c *model.Collector) func() {
	overrides := fetch.RequestOverrides{Params: map[string]string{"tenant": "acme", "region": "eu-west-1"}}
	target := "http://db.internal/x"
	query := probeKeyQuery(c.Name, target, overrides)
	key := probeCacheKey(c, target, query, nil)
	p := recentProbe{at: time.Now(), result: probeCached, status: http.StatusOK, series: 3}
	return func() { server.noteProbe(read, server.recent.keyOf(key, c, target, query), p) }
}

// Recording a probe like one before it, as nearly every probe is, a cache
// hit as much as any, costs a lookup of the key it is recorded under, not
// the sorting and the redaction of its parameters again: those were two
// dozen allocations and some microseconds on every probe. The key is the
// one made from the parameters, and what is remembered of the keys is
// bounded.
func TestRecordingAProbeLikeOneBeforeItCostsALookup(t *testing.T) {
	c := testutil.Collector("text", "text")
	server := modeServer(t, c)
	read := server.readAt(server.reconcile().generation)
	record := noteProbeLikeOneBefore(server, read, &c)
	record()
	if allocs := alloctest.AllocsAtMost(100, 2, record); allocs > 2 {
		t.Errorf("recording a probe like one before it allocates %v times, want at most 2", allocs)
	}
	row := probeRows(t, server)["text http://db.internal/x"]
	if row.Params != "region=eu-west-1 tenant=acme" || len(row.Probes) != recentProbesPerKey {
		t.Errorf("the probes are recorded as %+v", row)
	}
	for i := range recentProbeKeysShown + 10 {
		target := fmt.Sprintf("http://t%d.example/?password=hunter2", i)
		query := probeKeyQuery(c.Name, target, fetch.RequestOverrides{Params: map[string]string{"password": "hunter2"}})
		key := server.recent.keyOf(probeCacheKey(&c, target, query, nil), &c, target, query)
		if want := recentProbeKeyFor(&c, target, query); key != want || key.params != "password="+fetch.Redacted {
			t.Fatalf("the probe is recorded under %+v, want %+v", key, want)
		}
	}
	if n := len(server.recent.shown); n > recentProbeKeysShown {
		t.Errorf("%d keys are remembered, more than %d", n, recentProbeKeysShown)
	}
}

// BenchmarkNoteProbe is the recording of a probe with two parameters like
// the probe before it, for the status page, which every probe answered
// pays.
func BenchmarkNoteProbe(b *testing.B) {
	c := testutil.Collector("text", "text")
	server := modeServer(b, c)
	record := noteProbeLikeOneBefore(server, server.readAt(server.reconcile().generation), &c)
	b.ReportAllocs()
	for b.Loop() {
		record()
	}
}
