//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// backoffClock is the clock failure_backoff reads its waits by, moved by
// the test rather than by time.
type backoffClock struct {
	mu  sync.Mutex
	now time.Time
}

func newBackoffClock(server *Server) *backoffClock {
	clock := &backoffClock{now: time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)}
	server.backoffs.now = clock.read
	return clock
}

func (c *backoffClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *backoffClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// at is the clock's time plus d as the error of a skipped trip writes it.
func (c *backoffClock) at(d time.Duration) string {
	return c.read().Add(d).UTC().Format(time.RFC3339)
}

// deadTarget answers value=1, or 500 while failing, and counts the
// requests it was sent, by path.
type deadTarget struct {
	*httptest.Server
	failing atomic.Bool
	mu      sync.Mutex
	hits    map[string]int
	// hold, when set, is waited for before a request is answered, and
	// arrived told of the request first.
	hold, arrived chan struct{}
}

func newDeadTarget(t *testing.T) *deadTarget {
	t.Helper()
	target := &deadTarget{hits: map[string]int{}}
	target.failing.Store(true)
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.mu.Lock()
		target.hits[r.URL.Path]++
		hold, arrived := target.hold, target.arrived
		target.mu.Unlock()
		if hold != nil {
			arrived <- struct{}{}
			<-hold
		}
		if target.failing.Load() || strings.HasSuffix(r.URL.Path, "/down") {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	return target
}

func (f *deadTarget) sent(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *deadTarget) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, hits := range f.hits {
		n += hits
	}
	return n
}

func backoffCollector(name string, after int, initial, longest time.Duration) model.Collector {
	c := testutil.Collector(name, "text")
	in, most := model.Duration(initial), model.Duration(longest)
	c.FailureBackoff = &model.FailureBackoffConfig{After: &after, Initial: &in, Max: &most}
	return c
}

func probeOf(collector, target string) string {
	return "/probe?collector=" + collector + "&target=" + url.QueryEscape(target)
}

// A target that fails `after` scrapes in a row is not contacted for
// `initial`: its probe fails at once with the failures and the time of the
// next try, takes no trip slot, and counts in
// http_exporter_backoff_skipped_total while
// http_exporter_targets_backing_off counts the target. When the wait has
// passed one probe goes to the target; its failure doubles the wait, and a
// success forgets the target. A collector without failure_backoff has both
// series at 0.
func TestAFailingTargetIsBackedOffFrom(t *testing.T) {
	target := newDeadTarget(t)
	dead := backoffCollector("dead", 2, time.Minute, 10*time.Minute)
	dead.MaxConcurrentProbes = 1
	server := modeServer(t, dead, testutil.Collector("plain", "text"))
	server.logger = testutil.QuietLogger(t)
	clock := newBackoffClock(server)
	probeURL := probeOf("dead", target.URL)
	for range 2 {
		if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "received HTTP status 500") {
			t.Fatalf("a probe of the failing target is answered %d: %s", r.Code, r.Body)
		}
	}
	if sent := target.total(); sent != 2 {
		t.Fatalf("the target was sent %d requests, want 2", sent)
	}
	// The collector's one slot is taken: a probe that went to the target
	// would be answered 503, and one backoff keeps from it needs none.
	if full := server.trips.tryAcquire("dead", 1); full != nil {
		t.Fatal(full.message)
	}
	want := target.URL + " is backing off after 2 consecutive failed scrapes; next try after " + clock.at(time.Minute)
	if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "collector dead http failed: "+want) {
		t.Fatalf("a probe of the target backed off from is answered %d: %s; want %q", r.Code, r.Body, want)
	}
	server.trips.release("dead")
	if sent := target.total(); sent != 2 {
		t.Fatalf("the target backed off from was sent %d requests, want still 2", sent)
	}
	metrics := selfMetrics(t, server)
	for series, want := range map[string]float64{
		`http_exporter_backoff_skipped_total{collector="dead"}`:   1,
		`http_exporter_targets_backing_off{collector="dead"}`:     1,
		`http_exporter_scrapes_total{collector="dead"}`:           3,
		`http_exporter_scrape_success_total{collector="dead"}`:    0,
		`http_exporter_scrape_http_status_code{collector="dead"}`: 500,
		`http_exporter_backoff_skipped_total{collector="plain"}`:  0,
		`http_exporter_targets_backing_off{collector="plain"}`:    0,
	} {
		if got := seriesValue(t, metrics, series); got != want {
			t.Errorf("%s is %v, want %v", series, got, want)
		}
	}

	// The wait has passed: the next probe goes, fails, and doubles it.
	clock.advance(time.Minute)
	if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusBadGateway || target.total() != 3 {
		t.Fatalf("the probe after the wait is answered %d with %d requests sent, want it sent: %s", r.Code, target.total(), r.Body)
	}
	clock.advance(90 * time.Second)
	want = "after 3 consecutive failed scrapes; next try after " + clock.at(30*time.Second)
	if r := probeOnce(t, server, probeURL, nil); !strings.Contains(r.Body.String(), want) || target.total() != 3 {
		t.Fatalf("a probe within the doubled wait is answered %d with %d requests sent: %s; want %q", r.Code, target.total(), r.Body, want)
	}

	// The next try succeeds, and the target is forgotten.
	clock.advance(30 * time.Second)
	target.failing.Store(false)
	for i := range 2 {
		if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusOK || target.total() != 4+i {
			t.Fatalf("probe %d after the target came back is answered %d with %d requests sent: %s", i+1, r.Code, target.total(), r.Body)
		}
	}
	if n := server.backoffs.remembered("dead"); n != 0 {
		t.Errorf("%d targets are remembered after the success, want none", n)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_targets_backing_off{collector="dead"}`); got != 0 {
		t.Errorf("http_exporter_targets_backing_off is %v after the success, want 0", got)
	}
}

// Only a failed fetch counts towards backing off. A target whose answer the
// transform cannot read is up, and a target the target policy refuses is
// not reached: neither is remembered, and each probe goes on. A fetch that
// failed under on_fetch_error log counts though the probe carried on, and
// the probe backoff keeps from the target carries on too.
func TestOnlyAFailedFetchCountsTowardsBackingOff(t *testing.T) {
	target := newDeadTarget(t)
	target.failing.Store(false)
	unreadable := backoffCollector("unreadable", 1, time.Minute, time.Minute)
	unreadable.Metrics[0].Expression = `missing=(\d+)`
	unreadable.Metrics[0].ErrorMode = model.ErrorModeFail
	refusing := backoffCollector("refusing", 1, time.Minute, time.Minute)
	refusing.Request.DeniedTargets = []string{"127.0.0.0/8"}
	carrying := backoffCollector("carrying", 1, time.Minute, time.Minute)
	carrying.ErrorHandling.OnFetchError = model.ErrorPolicyLog
	server := modeServer(t, unreadable, refusing, carrying)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	for range 2 {
		if r := probeOnce(t, server, probeOf("unreadable", target.URL+"/unreadable"), nil); r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), "demo_value") {
			t.Fatalf("a probe of the unreadable answer is answered %d: %s", r.Code, r.Body)
		}
		if r := probeOnce(t, server, probeOf("refusing", target.URL+"/refused"), nil); r.Code != http.StatusForbidden {
			t.Fatalf("a probe of the refused target is answered %d: %s", r.Code, r.Body)
		}
	}
	if sent := target.sent("/unreadable"); sent != 2 {
		t.Errorf("the target whose answer the transform could not read was sent %d requests, want 2", sent)
	}
	for _, name := range []string{"unreadable", "refusing"} {
		if n := server.backoffs.remembered(name); n != 0 {
			t.Errorf("collector %s remembers %d targets, want none", name, n)
		}
	}

	target.failing.Store(true)
	for i := range 2 {
		r := probeOnce(t, server, probeOf("carrying", target.URL+"/carrying"), nil)
		if r.Code != http.StatusOK {
			t.Fatalf("probe %d under on_fetch_error log is answered %d: %s", i+1, r.Code, r.Body)
		}
	}
	if sent := target.sent("/carrying"); sent != 1 {
		t.Errorf("the target failing under on_fetch_error log was sent %d requests, want 1: the second probe backed off", sent)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_backoff_skipped_total{collector="carrying"}`); got != 1 {
		t.Errorf("http_exporter_backoff_skipped_total is %v under on_fetch_error log, want 1", got)
	}
}

// A probe backoff keeps from the target is answered as its failed fetch
// would be: with cache.stale_if_error, by the last good result marked
// stale, counted in http_exporter_cache_stale_served_total.
func TestABackedOffProbeIsAnsweredWithTheLastGoodResult(t *testing.T) {
	target := newDeadTarget(t)
	target.failing.Store(false)
	stale := backoffCollector("stale", 1, time.Minute, time.Minute)
	stale.Cache.StaleIfError = model.Duration(time.Hour)
	server := modeServer(t, stale)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	probeURL := probeOf("stale", target.URL)
	if r := probeOnce(t, server, probeURL, nil); r.Code != http.StatusOK {
		t.Fatalf("the first probe is answered %d: %s", r.Code, r.Body)
	}
	target.failing.Store(true)
	for i := range 2 {
		r := probeOnce(t, server, probeURL, nil)
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "http_exporter_result_stale 1") || !strings.Contains(r.Body.String(), "demo_value 1") {
			t.Fatalf("failed probe %d is answered %d: %s; want the last good result", i+1, r.Code, r.Body)
		}
	}
	if sent := target.total(); sent != 2 {
		t.Errorf("the target was sent %d requests, want 2: the third probe backed off", sent)
	}
	metrics := selfMetrics(t, server)
	if got := seriesValue(t, metrics, `http_exporter_cache_stale_served_total{collector="stale"}`); got != 2 {
		t.Errorf("http_exporter_cache_stale_served_total is %v, want 2", got)
	}
	if got := seriesValue(t, metrics, `http_exporter_backoff_skipped_total{collector="stale"}`); got != 1 {
		t.Errorf("http_exporter_backoff_skipped_total is %v, want 1", got)
	}
}

// Probes that differ in a param_ value are different trips, and back off
// apart: the tenant that fails is skipped while the one that answers is
// still contacted.
func TestProbesWithOtherParametersBackOffApart(t *testing.T) {
	target := newDeadTarget(t)
	target.failing.Store(false)
	tenants := backoffCollector("tenants", 1, time.Minute, time.Minute)
	tenants.Request.Path = "/stats/{{param_tenant}}"
	server := modeServer(t, tenants)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	of := func(tenant string) string { return probeOf("tenants", target.URL) + "&param_tenant=" + tenant }
	for range 3 {
		probeOnce(t, server, of("down"), nil)
		if r := probeOnce(t, server, of("up"), nil); r.Code != http.StatusOK {
			t.Fatalf("the tenant that answers is answered %d: %s", r.Code, r.Body)
		}
	}
	if down, up := target.sent("/stats/down"), target.sent("/stats/up"); down != 1 || up != 3 {
		t.Errorf("the failing tenant was sent %d requests and the other %d, want 1 and 3", down, up)
	}
	if n := server.backoffs.remembered("tenants"); n != 1 {
		t.Errorf("%d targets are remembered, want the failing tenant alone", n)
	}
}

// When the wait has passed, one trip goes to the target at a time: a probe
// of a collector that shares no trip, arriving while the try is in
// progress, fails at once without contacting the target. The try's failure
// doubles the wait.
func TestOneTripAtATimeTriesTheTargetAgain(t *testing.T) {
	target := newDeadTarget(t)
	solo := backoffCollector("solo", 1, time.Minute, 10*time.Minute)
	off := false
	solo.Coalesce = &off
	server := modeServer(t, solo)
	server.logger = testutil.QuietLogger(t)
	clock := newBackoffClock(server)
	probeURL := probeOf("solo", target.URL)
	probeOnce(t, server, probeURL, nil)
	clock.advance(time.Minute)

	target.mu.Lock()
	target.hold, target.arrived = make(chan struct{}), make(chan struct{}, 1)
	hold, arrived := target.hold, target.arrived
	target.mu.Unlock()
	tried := make(chan *httptest.ResponseRecorder, 1)
	go func() { tried <- probeOnce(t, server, probeURL, nil) }()
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the try after the wait never reached the target")
	}
	target.mu.Lock()
	target.hold, target.arrived = nil, nil
	target.mu.Unlock()
	r := probeOnce(t, server, probeURL, nil)
	if !strings.Contains(r.Body.String(), "is backing off after 1 consecutive failed scrapes; its next try is in progress") {
		t.Errorf("a probe during the try is answered %d: %s; want it refused for the try in progress", r.Code, r.Body)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_targets_backing_off{collector="solo"}`); got != 1 {
		t.Errorf("http_exporter_targets_backing_off is %v during the try, want 1", got)
	}
	close(hold)
	if r := <-tried; r.Code != http.StatusBadGateway {
		t.Fatalf("the try is answered %d: %s", r.Code, r.Body)
	}
	if sent := target.total(); sent != 2 {
		t.Errorf("the target was sent %d requests, want 2", sent)
	}
	want := "after 2 consecutive failed scrapes; next try after " + clock.at(2*time.Minute)
	if r := probeOnce(t, server, probeURL, nil); !strings.Contains(r.Body.String(), want) {
		t.Errorf("a probe after the failed try is answered %d: %s; want %q", r.Code, r.Body, want)
	}
}

// A reload that changes or removes the collector forgets the targets it was
// backing off from, and a trip of the collector as it was before the reload
// records nothing once the reload has come.
func TestAReloadForgetsTheTargetsBackedOffFrom(t *testing.T) {
	target := newDeadTarget(t)
	dead := backoffCollector("dead", 1, time.Hour, time.Hour)
	server := modeServer(t, dead)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	probeURL := probeOf("dead", target.URL)
	probeOnce(t, server, probeURL, nil)
	probeOnce(t, server, probeURL, nil)
	if sent, n := target.total(), server.backoffs.remembered("dead"); sent != 1 || n != 1 {
		t.Fatalf("the target was sent %d requests with %d targets remembered, want 1 and 1", sent, n)
	}
	before := server.reconcile().generation
	turn := server.backoffs.admit(&server.manager.Get().Collectors[0], "another", "another")

	changed := backoffCollector("dead", 1, 2*time.Hour, 2*time.Hour)
	reloadTo(t, server, changed)
	// The state follows the reload when it is next used.
	server.reconcile()
	if n := server.backoffs.remembered("dead"); n != 0 {
		t.Fatalf("%d targets of the changed collector are remembered, want none", n)
	}
	if turn.settle(server.readAt(before), backoffFailed); server.backoffs.remembered("dead") != 0 {
		t.Errorf("a trip that read the collector before the reload remembered its target after it")
	}
	probeOnce(t, server, probeURL, nil)
	if sent := target.total(); sent != 2 {
		t.Errorf("the target was sent %d requests, want the changed collector's probe sent", sent)
	}

	reloadTo(t, server, testutil.Collector("other", "text"))
	server.reconcile()
	if n := server.backoffs.remembered("dead"); n != 0 {
		t.Errorf("%d targets of the removed collector are remembered, want none", n)
	}
}

// A collector remembers a bounded number of targets, forgetting the one
// that failed longest ago first.
func TestTheTargetsBackedOffFromAreBounded(t *testing.T) {
	target := newDeadTarget(t)
	server := modeServer(t, backoffCollector("dead", 1, time.Hour, time.Hour))
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	server.backoffs.limit = 3
	for _, path := range []string{"/1", "/2", "/3", "/4"} {
		probeOnce(t, server, probeOf("dead", target.URL+path), nil)
	}
	if n := server.backoffs.remembered("dead"); n != 3 {
		t.Fatalf("%d targets are remembered, want the bound, 3", n)
	}
	// The newest first: a probe of the one forgotten goes to the target and
	// fails, and is remembered in place of the oldest then.
	for _, path := range []string{"/4", "/3", "/2", "/1"} {
		probeOnce(t, server, probeOf("dead", target.URL+path), nil)
	}
	for path, want := range map[string]int{"/1": 2, "/2": 1, "/3": 1, "/4": 1} {
		if sent := target.sent(path); sent != want {
			t.Errorf("%s was sent %d requests, want %d", path, sent, want)
		}
	}
}

// A debug probe contacts a target the collector is backing off from, says
// so in its report, and changes nothing of it: its success does not forget
// the target.
func TestADebugProbeContactsATargetBackedOffFrom(t *testing.T) {
	target := newDeadTarget(t)
	server := modeServer(t, backoffCollector("dead", 1, time.Minute, time.Minute))
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	clock := newBackoffClock(server)
	probeURL := probeOf("dead", target.URL)
	probeOnce(t, server, probeURL, nil)
	target.failing.Store(false)
	report := probeOnce(t, server, probeURL+"&debug=true", nil).Body.String()
	want := "The target is backing off after 1 consecutive failed scrapes (failure_backoff): probes and scrapes fail without contacting it until " + clock.at(time.Minute) + ". This run contacted it all the same, and changes nothing of that.\n"
	if !strings.Contains(report, want) || !strings.Contains(report, "A probe would have answered 200 with 1 series") {
		t.Errorf("the debug report is:\n%s\nwant the line %q", report, want)
	}
	if sent := target.total(); sent != 2 {
		t.Errorf("the target was sent %d requests, want the debug probe's too", sent)
	}
	if r := probeOnce(t, server, probeURL, nil); !strings.Contains(r.Body.String(), "is backing off after 1 consecutive failed scrapes") || target.total() != 2 {
		t.Errorf("a probe after the debug probe is answered %d: %s; want it still backed off", r.Code, r.Body)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_backoff_skipped_total{collector="dead"}`); got != 1 {
		t.Errorf("http_exporter_backoff_skipped_total is %v, want the probe's 1 and nothing of the debug probe's", got)
	}
}

// A static target's scrape backs off as a probe does, without waiting for a
// trip slot, and shares its state with the identical probe.
func TestAStaticTargetBacksOffAndSharesTheProbesState(t *testing.T) {
	target := newDeadTarget(t)
	dead := backoffCollector("dead", 1, time.Minute, time.Minute)
	dead.MaxConcurrentProbes = 1
	cfg := &model.Config{Collectors: []model.Collector{dead}}
	file := &model.StaticTargetFile{Interval: model.Duration(10 * time.Second), Targets: []model.StaticTarget{{Name: "s", Collector: "dead", Target: target.URL}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	newBackoffClock(server)
	static := server.manager.StaticTargets()[0]
	server.scrapeTarget(t.Context(), server.manager.Get(), static)
	if full := server.trips.tryAcquire("dead", 1); full != nil {
		t.Fatal(full.message)
	}
	// The schedule bounds a scrape's wait for a slot by its turn; a scrape
	// that waited would take the whole of this.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	server.scrapeTarget(ctx, server.manager.Get(), static)
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("the scrape backed off from took %s: it waited for a slot", took)
	}
	server.trips.release("dead")
	if sent := target.total(); sent != 1 {
		t.Fatalf("the static target was sent %d requests, want 1", sent)
	}
	up := -1.0
	for _, m := range server.staticResults["s"].Metrics {
		if m.Name == "http_exporter_target_up" {
			up = m.Value
		}
	}
	if up != 0 {
		t.Errorf("the scrape backed off from published http_exporter_target_up %v, want 0", up)
	}
	if r := probeOnce(t, server, probeOf("dead", target.URL), nil); !strings.Contains(r.Body.String(), "is backing off after 1 consecutive failed scrapes") || target.total() != 1 {
		t.Errorf("the probe identical to the static target is answered %d: %s; want it backed off", r.Code, r.Body)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_backoff_skipped_total{collector="dead"}`); got != 2 {
		t.Errorf("http_exporter_backoff_skipped_total is %v, want 2", got)
	}
}

// The wait doubles from initial with each failure after the one that
// started the backoff, and stops at max.
func TestTheBackoffWaitDoublesUpToMax(t *testing.T) {
	initial, longest := model.Duration(30*time.Second), model.Duration(5*time.Minute)
	c := &model.FailureBackoffConfig{Initial: &initial, Max: &longest}
	for doublings, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		if got := backoffWait(c, doublings); got != want {
			t.Errorf("after %d doublings the wait is %s, want %s", doublings, got, want)
		}
	}
	if got := backoffWait(c, 1000); got != 5*time.Minute {
		t.Errorf("after 1000 doublings the wait is %s, want max", got)
	}
}

// In verbose mode the skipped trips are counted per request as well, and
// the targets backing off, which are the collector's, have no per-request
// series.
func TestTheSkippedTripsAreCountedPerRequest(t *testing.T) {
	target := newDeadTarget(t)
	cfg := &model.Config{Collectors: []model.Collector{backoffCollector("dead", 1, time.Minute, time.Minute)}, Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	newBackoffClock(server)
	probeOnce(t, server, probeOf("dead", target.URL), nil)
	probeOnce(t, server, probeOf("dead", target.URL), nil)
	metrics := selfMetrics(t, server)
	perRequest := ""
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, `http_exporter_backoff_skipped_total{collector="dead",http_method="GET"`) {
			perRequest = line
		}
		if strings.HasPrefix(line, `http_exporter_targets_backing_off{collector="dead",http_method=`) {
			t.Errorf("http_exporter_targets_backing_off has a per-request series: %s", line)
		}
	}
	if !strings.HasSuffix(perRequest, " 1") {
		t.Errorf("the per-request http_exporter_backoff_skipped_total is %q, want 1 in:\n%s", perRequest, metrics)
	}
}

// remembered is how many targets of the collector are remembered. It is
// here, beside the only tests that ask, so no build lacks a use of it.
func (b *failureBackoffs) remembered(collector string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if targets := b.collectors[collector]; targets != nil {
		return len(targets.targets)
	}
	return 0
}
