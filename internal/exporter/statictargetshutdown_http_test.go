//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// holdingTarget answers value=<n> to its first answers requests, then holds every
// later one until release is closed. hits counts the requests.
func holdingTarget(t *testing.T, answers int64) (server *httptest.Server, hits *atomic.Int64, release chan struct{}) {
	t.Helper()
	hits, release = &atomic.Int64{}, make(chan struct{})
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > answers {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		server.Close()
	})
	return server, hits, release
}

// loopServer runs the scrape loop over targets, which are not validated so
// their intervals may be short, and returns the server, a function that
// stops the loop, and a channel closed when it has returned.
func loopServer(t *testing.T, concurrency int, targets ...model.StaticTarget) (*Server, context.CancelFunc, chan struct{}) {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	manager.SetTargets("", &model.StaticTargetFile{Interval: model.Duration(time.Minute), Concurrency: concurrency, Targets: targets})
	server := NewServer(manager, "python3", testutil.QuietLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.StaticScrapeLoop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		server.AbortStaticScrapes()
		<-done
	})
	return server, cancel, done
}

func targetUp(server *Server, name string) (float64, bool) {
	for _, result := range server.staticTargetResults() {
		if result.name != name {
			continue
		}
		for _, m := range result.set.Metrics {
			if m.Name == "http_exporter_target_up" {
				return m.Value, true
			}
		}
	}
	return 0, false
}

// When the loop stops, a scrape in flight is finished, and publishes as usual.
func TestAStoppingLoopFinishesTheScrapesInFlight(t *testing.T) {
	target, hits, release := holdingTarget(t, 0)
	server, stop, done := loopServer(t, 0, model.StaticTarget{Name: "slow", Collector: "text", Target: target.URL, Interval: model.Duration(300 * time.Millisecond)})
	testutil.WaitFor(t, "the scrape to reach the target", func() bool { return hits.Load() >= 1 })
	stop()
	select {
	case <-done:
		t.Fatal("the loop returned with a scrape in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done
	if up, ok := targetUp(server, "slow"); !ok || up != 1 {
		t.Fatalf("the finished scrape published up=%v, %v; want 1", up, ok)
	}
	if hits.Load() != 1 {
		t.Fatalf("the stopped loop started %d scrapes", hits.Load())
	}
}

// A scrape the shutdown cuts short publishes nothing, over OTLP neither, and
// logs no failure: the target's last result stands.
func TestAbortedScrapesPublishNothing(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target, hits, _ := holdingTarget(t, 1)
	server, stop, done := loopServer(t, 0, model.StaticTarget{Name: "slow", Collector: "text", Target: target.URL, Interval: model.Duration(300 * time.Millisecond), ExportViaOTLP: true})
	testutil.WaitFor(t, "a second scrape to be held", func() bool { return hits.Load() >= 2 })
	if up, ok := targetUp(server, "slow"); !ok || up != 1 {
		t.Fatalf("the first scrape published up=%v, %v", up, ok)
	}
	stop()
	server.AbortStaticScrapes()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop did not return once its scrapes were cut short")
	}
	if up, ok := targetUp(server, "slow"); !ok || up != 1 {
		t.Fatalf("after the abort the endpoint serves up=%v, %v; want the last result, 1", up, ok)
	}
	if up, ok := pendingValue(server, "http_exporter_target_up"); !ok || up != 1 {
		t.Fatalf("queued for OTLP: up=%v, %v; want the last result, 1", up, ok)
	}
	if strings.Contains(logs.String(), "static target scrape failed") {
		t.Fatalf("the abort was logged as a failure:\n%s", logs)
	}
}

// A scrape still waiting for a slot when the loop stops is not begun, and no
// advice to raise the file's concurrency is logged for it. The interval is
// long, so neither scrape can time out while the test runs, and the names
// are ones whose first scrapes are due early, held50 at 5ms and queued3 at
// 106ms (scheduleOffset), so the test does not wait out firstScrapeWindow.
//
// Which scrape holds the slot and which waits is the test's doing, not the
// clock's: the scrape of queued3 is held where it would begin to wait until
// that of held50 has reached its target, with the slot. On a machine too
// busy to run held50's scrape in the 101ms before queued3's came due, the
// two otherwise raced for the slot.
func TestAStoppingLoopDropsScrapesWaitingForASlot(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	waiting := make(chan string, 4)
	slotTaken := make(chan struct{})
	hook := func(name string) {
		if name == "queued3" {
			<-slotTaken
		}
		waiting <- name
	}
	slotWaitHook.Store(&hook)
	t.Cleanup(func() { slotWaitHook.Store(nil) })
	// However the test ends, the scrape it holds is let go, or the loop
	// would never return.
	letGo := sync.OnceFunc(func() { close(slotTaken) })
	defer letGo()
	held, heldHits, release := holdingTarget(t, 0)
	queued, queuedHits := countingTarget(func(*http.Request) string { return "value=1\n" })
	defer queued.Close()
	_, stop, done := loopServer(t, 1,
		model.StaticTarget{Name: "held50", Collector: "text", Target: held.URL, Interval: model.Duration(time.Minute)},
		model.StaticTarget{Name: "queued3", Collector: "text", Target: queued.URL, Interval: model.Duration(time.Minute)},
	)
	testutil.WaitFor(t, "the held scrape to take the only slot", func() bool { return heldHits.Load() >= 1 })
	letGo()
	for name := ""; name != "queued3"; {
		select {
		case name = <-waiting:
		case <-time.After(15 * time.Second):
			t.Fatal("the second scrape never waited for the slot")
		}
	}
	stop()
	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop did not return")
	}
	if n := queuedHits.Load(); n != 0 {
		t.Fatalf("a scrape waiting for a slot was begun after the loop stopped: %d requests", n)
	}
	if strings.Contains(logs.String(), "no scrape slot came free") {
		t.Fatalf("stopping was logged as a lack of slots:\n%s", logs)
	}
}

// A scrape that comes to take its slot when the loop has already stopped is
// not begun either, though the slot is free: it is held here just before it
// would take one until the loop has been stopped, so it finds the slot free
// and the loop stopped at once. Go takes either way out of a select with
// both ready, so the scrape used to begin every second time; the rounds
// make a loop that still does that fail all but once in a thousand runs,
// and one that does not never.
func TestAStoppedLoopDoesNotBeginAScrapeThatFindsASlotFree(t *testing.T) {
	t.Cleanup(func() { slotWaitHook.Store(nil) })
	target, hits := countingTarget(func(*http.Request) string { return "value=1\n" })
	defer target.Close()
	round := func() {
		arrived, stopped := make(chan struct{}), make(chan struct{})
		hook := func(string) {
			close(arrived)
			<-stopped
		}
		slotWaitHook.Store(&hook)
		// However the round ends, the scrape it holds is let go, or the
		// loop would never return.
		letGo := sync.OnceFunc(func() { close(stopped) })
		defer letGo()
		_, stop, done := loopServer(t, 1, model.StaticTarget{Name: "held50", Collector: "text", Target: target.URL, Interval: model.Duration(time.Minute)})
		select {
		case <-arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("the scrape never came due")
		}
		stop()
		letGo()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("the loop did not return")
		}
	}
	for n := 1; n <= 10; n++ {
		round()
		if begun := hits.Load(); begun != 0 {
			t.Fatalf("round %d: a scrape was begun after the loop stopped: %d requests", n, begun)
		}
	}
}

// A target a reload removed while its scrape was in flight publishes nothing,
// on the endpoint or over OTLP.
func TestARemovedTargetPublishesNothing(t *testing.T) {
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	kept := model.StaticTarget{Name: "kept", Collector: "text", Target: "http://a.invalid", ExportViaOTLP: true}
	removed := model.StaticTarget{Name: "removed", Collector: "text", Target: "http://b.invalid", ExportViaOTLP: true}
	server := newStaticServer(t, cfg, &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{kept}})
	set := model.MetricSet{Metrics: []model.Metric{{Name: "demo_value", Type: model.GaugeMetricType, Value: 1}}}
	server.publishStaticTarget(removed, otlpResourceIdentity{}, set)
	if _, ok := pendingValue(server, "demo_value"); ok {
		t.Fatal("a removed target was queued for OTLP")
	}
	server.staticMu.Lock()
	_, stored := server.staticResults["removed"]
	server.staticMu.Unlock()
	if stored {
		t.Fatal("a removed target's result was kept")
	}
	server.publishStaticTarget(kept, otlpResourceIdentity{}, set)
	if _, ok := pendingValue(server, "demo_value"); !ok {
		t.Fatal("a target in force was not queued")
	}
}
