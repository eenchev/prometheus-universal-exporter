package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// max_concurrent_probes bounds a collector's trips to its targets
// (triplimit.go).

// heldTarget answers every request only once released, and counts them.
type heldTarget struct {
	server  *httptest.Server
	release chan struct{}
	mu      sync.Mutex
	arrived int
}

func newHeldTarget(t *testing.T) *heldTarget {
	t.Helper()
	h := &heldTarget{release: make(chan struct{})}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.arrived++
		h.mu.Unlock()
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(func() {
		h.open()
		h.server.Close()
	})
	return h
}

func (h *heldTarget) open() {
	select {
	case <-h.release:
	default:
		close(h.release)
	}
}

func (h *heldTarget) requests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.arrived
}

func TestMaxConcurrentProbesDefaultsAndValidation(t *testing.T) {
	c := testutil.Collector("limited", "text")
	if got := maxConcurrentProbes(&c); got != DefaultMaxConcurrentProbes || DefaultMaxConcurrentProbes != 32 {
		t.Fatalf("default limit %d", got)
	}
	c.MaxConcurrentProbes = 3
	if got := maxConcurrentProbes(&c); got != 3 {
		t.Fatalf("limit %d", got)
	}
	c.MaxConcurrentProbes = -1
	if err := config.Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), "max_concurrent_probes must not be negative") {
		t.Fatalf("err=%v", err)
	}
}

// Over the limit, a probe is answered 503 at once and counted; the probes in
// progress are unaffected, and the slots come back as they finish.
func TestProbesOverTheLimitAreRejected(t *testing.T) {
	target := newHeldTarget(t)
	c := testutil.Collector("limited", "text")
	c.MaxConcurrentProbes = 2
	server := verboseServer(t, false, c)
	probe := func(path string) *httptest.ResponseRecorder {
		return probeOnce(t, server, "/probe?collector=limited&target="+url.QueryEscape(target.server.URL+path), nil)
	}
	results := make(chan int, 2)
	for _, path := range []string{"/a", "/b"} {
		go func() { results <- probe(path).Code }()
	}
	testutil.WaitFor(t, "two probes at the target", func() bool { return target.requests() == 2 })
	if got := server.trips.count("limited"); got != 2 {
		t.Fatalf("in flight %d", got)
	}
	start := time.Now()
	rejected := probe("/c")
	if rejected.Code != http.StatusServiceUnavailable || !strings.Contains(rejected.Body.String(), "max_concurrent_probes") {
		t.Fatalf("third probe: %d %s", rejected.Code, rejected.Body)
	}
	if time.Since(start) > time.Second || target.requests() != 2 {
		t.Fatal("the rejected probe waited or reached the target")
	}
	exposition := selfMetrics(t, server)
	if got := seriesValue(t, exposition, `http_exporter_probes_rejected_total{collector="limited"}`); got != 1 {
		t.Errorf("rejected=%v", got)
	}
	if got := seriesValue(t, exposition, `http_exporter_probes_in_flight{collector="limited"}`); got != 2 {
		t.Errorf("in flight gauge=%v", got)
	}

	target.open()
	for i := 0; i < 2; i++ {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("a probe within the limit got %d", code)
		}
	}
	testutil.WaitFor(t, "the slots to be freed", func() bool { return server.trips.count("limited") == 0 })
	if recorder := probe("/d"); recorder.Code != http.StatusOK {
		t.Fatalf("after the burst: %d %s", recorder.Code, recorder.Body)
	}
}

// Probes that make no trip — sharing another probe's request, or answered from
// the cache — take no slot.
func TestSharedAndCachedProbesTakeNoSlot(t *testing.T) {
	target := newHeldTarget(t)
	c := testutil.Collector("single", "text")
	c.MaxConcurrentProbes = 1
	c.Cache.TTL = model.Duration(time.Minute)
	server := verboseServer(t, false, c)
	probe := func(path string) int {
		return probeOnce(t, server, "/probe?collector=single&target="+url.QueryEscape(target.server.URL+path), nil).Code
	}
	results := make(chan int, 2)
	go func() { results <- probe("/same") }()
	testutil.WaitFor(t, "the first probe at the target", func() bool { return target.requests() == 1 })
	go func() { results <- probe("/same") }()
	testutil.WaitFor(t, "the second probe to join", func() bool {
		return seriesValue(t, selfMetrics(t, server), `http_exporter_scrapes_total{collector="single"}`) == 2
	})
	target.open()
	for i := 0; i < 2; i++ {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("identical probes: %d", code)
		}
	}
	// A cache hit is answered even while another target holds the only slot.
	held := newHeldTarget(t)
	busy := make(chan int, 1)
	go func() {
		busy <- probeOnce(t, server, "/probe?collector=single&target="+url.QueryEscape(held.server.URL), nil).Code
	}()
	testutil.WaitFor(t, "the slot to be taken", func() bool { return server.trips.count("single") == 1 })
	if code := probe("/same"); code != http.StatusOK {
		t.Fatalf("a cache hit was refused: %d", code)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_probes_rejected_total{collector="single"}`); got != 0 {
		t.Fatalf("rejected=%v", got)
	}
	held.open()
	<-busy
}

// A static target waits for a slot within its budget, and fails in the
// concurrency stage when none frees up in time.
func TestStaticTargetsWaitForASlot(t *testing.T) {
	target := newHeldTarget(t)
	scheduled := textTarget(t, "value=42\n")
	c := testutil.Collector("shared_limit", "text")
	c.MaxConcurrentProbes = 1
	cfg := &model.Config{Collectors: []model.Collector{c}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "scheduled", Collector: "shared_limit", Target: scheduled.URL}}}
	server := newStaticServer(t, cfg, file)

	busy := make(chan int, 1)
	go func() {
		busy <- probeOnce(t, server, "/probe?collector=shared_limit&target="+url.QueryEscape(target.server.URL), nil).Code
	}()
	testutil.WaitFor(t, "the probe to take the slot", func() bool { return server.trips.count("shared_limit") == 1 })

	up := func() float64 {
		for _, resource := range server.drainOTLP() {
			if m := metricByName(resource.Set, "http_exporter_target_up"); m != nil {
				return m.Value
			}
		}
		return -1
	}
	// No slot frees within the budget.
	server.scrapeStaticTargets(context.Background(), 50*time.Millisecond)
	if got := up(); got != 0 {
		t.Fatalf("up=%v while the only slot was held", got)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_probes_rejected_total{collector="shared_limit"}`); got != 1 {
		t.Fatalf("rejected=%v", got)
	}

	// The slot frees while the static target scrape waits.
	go func() {
		time.Sleep(50 * time.Millisecond)
		target.open()
	}()
	server.scrapeStaticTargets(context.Background(), 5*time.Second)
	if got := up(); got != 1 {
		t.Fatalf("up=%v after the slot was freed", got)
	}
	<-busy
}

func TestTripLimiterAcquireHonoursItsContext(t *testing.T) {
	limiter := newTripLimiter()
	if !limiter.tryAcquire("c", 1) || limiter.tryAcquire("c", 1) {
		t.Fatal("the limit of one was not enforced")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.acquire(ctx, "c", 1); err == nil || !strings.Contains(err.Error(), "max_concurrent_probes") {
		t.Fatalf("err=%v", err)
	}
	limiter.release("c")
	if err := limiter.acquire(context.Background(), "c", 1); err != nil {
		t.Fatal(err)
	}
	if limiter.count("c") != 1 || limiter.tryAcquire("d", 1) != true {
		t.Fatal("collectors do not have limits of their own")
	}
}
