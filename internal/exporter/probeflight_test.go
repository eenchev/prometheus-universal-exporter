package exporter

import (
	"context"
	"fmt"
	"log/slog"
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

// Identical probes that arrive while one is in flight share its request to the
// target (probeflight.go).

// gatedTarget answers every request only once release is closed, so a test can
// hold a probe in flight while others arrive. It counts the requests it gets
// and the ones whose context was cancelled before they were answered.
type gatedTarget struct {
	*httptest.Server
	requests  atomic.Int64
	cancelled atomic.Int64
	release   chan struct{}
	once      sync.Once
	status    int
	body      string
}

func newGatedTarget(t *testing.T, status int, body string) *gatedTarget {
	t.Helper()
	g := &gatedTarget{release: make(chan struct{}), status: status, body: body}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		select {
		case <-g.release:
		case <-r.Context().Done():
			g.cancelled.Add(1)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(g.status)
		_, _ = fmt.Fprint(w, g.body)
	}))
	t.Cleanup(func() {
		g.open()
		g.Close()
	})
	return g
}

func (g *gatedTarget) open() { g.once.Do(func() { close(g.release) }) }

type probeOutcome struct {
	code int
	body string
}

// probeAsync starts a probe and returns its outcome on a channel.
func probeAsync(ctx context.Context, server *Server, path string, header http.Header) <-chan probeOutcome {
	out := make(chan probeOutcome, 1)
	go func() {
		request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		for name, values := range header {
			request.Header[name] = values
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		out <- probeOutcome{recorder.Code, recorder.Body.String()}
	}()
	return out
}

// waitForWaiters blocks until the probes in flight have n callers between
// them, so a test knows every probe it started has joined before it lets the
// target answer.
func waitForWaiters(t *testing.T, server *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		server.flights.mu.Lock()
		total := 0
		for _, flight := range server.flights.flights {
			total += flight.waiters
		}
		server.flights.mu.Unlock()
		if total == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d probes to be in flight", n)
}

func flightServer(t *testing.T, collectors ...model.Collector) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: collectors}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

func probePath(collector, target string, extra string) string {
	return "/probe?collector=" + collector + "&target=" + url.QueryEscape(target) + extra
}

func TestConcurrentIdenticalProbesShareOneRequest(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := flightServer(t, testutil.Collector("shared", "text"))

	const probes = 5
	var outcomes []<-chan probeOutcome
	for i := 0; i < probes; i++ {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("shared", target.URL, ""), nil))
	}
	waitForWaiters(t, server, probes)
	target.open()

	var first string
	for i, outcome := range outcomes {
		got := <-outcome
		if got.code != http.StatusOK || !strings.Contains(got.body, "demo_value 42") {
			t.Fatalf("probe %d: %d %q", i, got.code, got.body)
		}
		if i == 0 {
			first = got.body
		} else if got.body != first {
			t.Fatalf("probe %d got a different answer:\n%s\nvs\n%s", i, got.body, first)
		}
	}
	if n := target.requests.Load(); n != 1 {
		t.Fatalf("the target was asked %d times for %d identical probes, want once", n, probes)
	}
	exposition := selfMetrics(t, server)
	for _, want := range []string{
		`http_exporter_probes_coalesced_total{collector="shared"} 4`,
		`http_exporter_scrapes_total{collector="shared"} 5`,
		`http_exporter_scrape_success_total{collector="shared"} 5`,
		// The trip to the target is counted once.
		`http_exporter_decode_success_total{collector="shared"} 1`,
	} {
		if !strings.Contains(exposition, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if server.flights.inFlight() != 0 {
		t.Fatal("a finished probe was left in flight")
	}
}

// A probe that finished is not shared with one that arrives later: without a
// cache, the next probe goes to the target again.
func TestProbesThatDoNotOverlapAreNotShared(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	target.open()
	server := flightServer(t, testutil.Collector("sequential", "text"))
	for i := 0; i < 2; i++ {
		if got := probeOnce(t, server, probePath("sequential", target.URL, ""), nil); got.Code != http.StatusOK {
			t.Fatalf("status=%d", got.Code)
		}
	}
	if n := target.requests.Load(); n != 2 {
		t.Fatalf("two sequential probes made %d requests, want 2", n)
	}
}

// Probes that could get different answers never share: a different target, a
// different probe parameter, or different forwarded credentials.
func TestDifferentProbesAreNotShared(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	other := newGatedTarget(t, http.StatusOK, "value=7\n")
	c := testutil.Collector("distinct", "text")
	c.Request.ForwardAuthorization = true
	server := flightServer(t, c)

	probes := []struct {
		target string
		extra  string
		auth   string
	}{
		{target.URL, "", "Bearer alice"},
		{other.URL, "", "Bearer alice"},
		{target.URL, "&timeout=5s", "Bearer alice"},
		{target.URL, "", "Bearer bob"},
	}
	var outcomes []<-chan probeOutcome
	for _, p := range probes {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("distinct", p.target, p.extra), http.Header{"Authorization": {p.auth}}))
	}
	waitForWaiters(t, server, len(probes))
	target.open()
	other.open()
	for _, outcome := range outcomes {
		if got := <-outcome; got.code != http.StatusOK {
			t.Fatalf("status=%d body=%q", got.code, got.body)
		}
	}
	if n := target.requests.Load() + other.requests.Load(); n != int64(len(probes)) {
		t.Fatalf("%d distinct probes made %d requests, want one each", len(probes), n)
	}
	if exposition := selfMetrics(t, server); !strings.Contains(exposition, `http_exporter_probes_coalesced_total{collector="distinct"} 0`+"\n") {
		t.Fatal("distinct probes were counted as coalesced")
	}
}

// Probes differing only in parameters that make no request — one no request
// type knows, a header_ parameter for a header the collector does not
// forward — are identical probes, and share one trip.
func TestParametersThatDoNotMakeTheRequestDoNotKeepProbesApart(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := flightServer(t, testutil.Collector("loose", "text"))
	extras := []string{"", "&x=1", "&x=2", "&header_X-Other=a"}
	var outcomes []<-chan probeOutcome
	for _, extra := range extras {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("loose", target.URL, extra), nil))
	}
	waitForWaiters(t, server, len(extras))
	target.open()
	for _, outcome := range outcomes {
		if got := <-outcome; got.code != http.StatusOK {
			t.Fatalf("status=%d body=%q", got.code, got.body)
		}
	}
	if n := target.requests.Load(); n != 1 {
		t.Fatalf("%d probes made %d requests, want one", len(extras), n)
	}
}

// A collector whose definition could not be fingerprinted has no key to
// cache or share by: its probes each make their own trip rather than all
// share the one keyed "".
func TestProbesWithoutAKeyAreNotShared(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := flightServer(t, testutil.Collector("unkeyed", "text"))
	// The fingerprint every probe of this configuration gets: none.
	generation := newFingerprintGeneration(server.manager.Get())
	for i := range generation.once {
		generation.once[i].Do(func() {})
	}
	server.fingerprints.current.Store(generation)
	other := newGatedTarget(t, http.StatusOK, "value=7\n")

	first := probeAsync(context.Background(), server, probePath("unkeyed", target.URL, ""), nil)
	second := probeAsync(context.Background(), server, probePath("unkeyed", other.URL, ""), nil)
	deadline := time.Now().Add(5 * time.Second)
	for target.requests.Load()+other.requests.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the probes made %d requests, want one each", target.requests.Load()+other.requests.Load())
		}
		time.Sleep(time.Millisecond)
	}
	target.open()
	other.open()
	if got := <-first; got.code != http.StatusOK || !strings.Contains(got.body, "demo_value 42") {
		t.Fatalf("first: %d %q", got.code, got.body)
	}
	if got := <-second; got.code != http.StatusOK || !strings.Contains(got.body, "demo_value 7") {
		t.Fatalf("second: %d %q", got.code, got.body)
	}
}

// A failure is shared like a success: every waiting probe gets the same error,
// and it is logged once.
func TestASharedFailureReachesEveryProbe(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusInternalServerError, "upstream broke\n")
	server := flightServer(t, testutil.Collector("failing", "text"))

	var outcomes []<-chan probeOutcome
	for i := 0; i < 3; i++ {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("failing", target.URL, ""), nil))
	}
	waitForWaiters(t, server, 3)
	target.open()
	for i, outcome := range outcomes {
		got := <-outcome
		if got.code != http.StatusBadGateway || !strings.Contains(got.body, "received HTTP status 500") {
			t.Fatalf("probe %d: %d %q", i, got.code, got.body)
		}
	}
	if n := strings.Count(logs.String(), `"msg":"probe failed"`); n != 1 {
		t.Fatalf("the failure was logged %d times, want once:\n%s", n, logs.String())
	}
	if exposition := selfMetrics(t, server); !strings.Contains(exposition, `http_exporter_scrape_success_total{collector="failing"} 0`+"\n") {
		t.Fatal("a shared failure was counted as a success")
	}
}

// A metric rule's error_mode fail answers every waiting probe with the JSON
// error.
func TestASharedMetricFailureKeepsItsJSONBody(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "nothing to see\n")
	c := testutil.Collector("strict", "text")
	c.Metrics[0].ErrorMode = model.ErrorModeFail
	server := flightServer(t, c)
	var outcomes []<-chan probeOutcome
	for i := 0; i < 2; i++ {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("strict", target.URL, ""), nil))
	}
	waitForWaiters(t, server, 2)
	target.open()
	for _, outcome := range outcomes {
		got := <-outcome
		if got.code != http.StatusBadGateway || !strings.Contains(got.body, `"stage":"metric"`) {
			t.Fatalf("%d %q", got.code, got.body)
		}
	}
}

// The probe that started the request going away does not fail the others,
// and does not cancel the request they are waiting on.
func TestTheFirstProbeLeavingDoesNotFailTheOthers(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := flightServer(t, testutil.Collector("leader", "text"))

	leaderCtx, leave := context.WithCancel(context.Background())
	leader := probeAsync(leaderCtx, server, probePath("leader", target.URL, ""), nil)
	waitForWaiters(t, server, 1)
	followers := []<-chan probeOutcome{
		probeAsync(context.Background(), server, probePath("leader", target.URL, ""), nil),
		probeAsync(context.Background(), server, probePath("leader", target.URL, ""), nil),
	}
	waitForWaiters(t, server, 3)
	leave()
	<-leader
	target.open()
	for _, outcome := range followers {
		if got := <-outcome; got.code != http.StatusOK || !strings.Contains(got.body, "demo_value 42") {
			t.Fatalf("%d %q", got.code, got.body)
		}
	}
	if target.cancelled.Load() != 0 || target.requests.Load() != 1 {
		t.Fatalf("requests=%d cancelled=%d", target.requests.Load(), target.cancelled.Load())
	}
}

// When every waiting probe has gone, the request is cancelled, and a probe
// arriving afterwards starts afresh rather than joining the cancelled one.
func TestTheRequestIsCancelledWhenEveryProbeHasGone(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := flightServer(t, testutil.Collector("abandoned", "text"))

	ctx, leave := context.WithCancel(context.Background())
	first := probeAsync(ctx, server, probePath("abandoned", target.URL, ""), nil)
	second := probeAsync(ctx, server, probePath("abandoned", target.URL, ""), nil)
	waitForWaiters(t, server, 2)
	// Leave only once the request has reached the target, so there is a
	// request there to be cancelled.
	for deadline := time.Now().Add(5 * time.Second); target.requests.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	leave()
	<-first
	<-second
	deadline := time.Now().Add(5 * time.Second)
	for target.cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if target.cancelled.Load() != 1 {
		t.Fatal("the abandoned request was not cancelled")
	}
	target.open()
	if got := probeOnce(t, server, probePath("abandoned", target.URL, ""), nil); got.Code != http.StatusOK {
		t.Fatalf("a probe after the abandoned one: %d %q", got.Code, got.Body.String())
	}
}

// coalesce: false gives every probe its own request.
func TestCoalescingCanBeTurnedOff(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	c := testutil.Collector("independent", "text")
	off := false
	c.Coalesce = &off
	server := flightServer(t, c)
	var outcomes []<-chan probeOutcome
	for i := 0; i < 3; i++ {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("independent", target.URL, ""), nil))
	}
	deadline := time.Now().Add(5 * time.Second)
	for target.requests.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	target.open()
	for _, outcome := range outcomes {
		if got := <-outcome; got.code != http.StatusOK {
			t.Fatalf("%d %q", got.code, got.body)
		}
	}
	if n := target.requests.Load(); n != 3 {
		t.Fatalf("three probes with coalesce: false made %d requests", n)
	}
}

// With a cache, the probes in flight share one request and fill the cache
// once; the next probe is answered from it.
func TestSharedProbesFillTheCacheOnce(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	c := testutil.Collector("cached_shared", "text")
	c.Cache.TTL = model.Duration(time.Minute)
	server := flightServer(t, c)
	var outcomes []<-chan probeOutcome
	for i := 0; i < 3; i++ {
		outcomes = append(outcomes, probeAsync(context.Background(), server, probePath("cached_shared", target.URL, ""), nil))
	}
	waitForWaiters(t, server, 3)
	target.open()
	for _, outcome := range outcomes {
		<-outcome
	}
	if got := probeOnce(t, server, probePath("cached_shared", target.URL, ""), nil); got.Code != http.StatusOK {
		t.Fatalf("status=%d", got.Code)
	}
	if n := target.requests.Load(); n != 1 {
		t.Fatalf("made %d requests, want 1", n)
	}
	exposition := selfMetrics(t, server)
	for _, want := range []string{
		// One probe went to the target; the two that shared it are
		// counted as coalesced, not as misses.
		`http_exporter_cache_misses_total{collector="cached_shared"} 1`,
		`http_exporter_cache_hits_total{collector="cached_shared"} 1`,
		`http_exporter_probes_coalesced_total{collector="cached_shared"} 2`,
	} {
		if !strings.Contains(exposition, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
}

// The shared work runs on its own goroutine, so a panic in it must answer the
// waiting probes with an error instead of crashing the exporter.
func TestAPanicInSharedWorkIsContained(t *testing.T) {
	testutil.CaptureLogs(t)
	flights := newProbeFlights()
	result, shared, err := flights.do(context.Background(), "key", func(context.Context) *probeResult {
		panic("boom")
	})
	if err != nil || shared || result.status != http.StatusInternalServerError || !strings.Contains(string(result.body), "internal error: boom") || result.ok {
		t.Fatalf("result=%+v shared=%t err=%v", result, shared, err)
	}
	if flights.inFlight() != 0 {
		t.Fatal("the panicked flight was left behind")
	}
}
