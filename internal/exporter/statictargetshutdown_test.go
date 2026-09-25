package exporter

import (
	"context"
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

// A shutdown stops the static targets without reporting them down
// (statictargetschedule.go), and a reload does not let a target's scrapes
// overlap or a removed target publish.

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
	case <-time.After(5 * time.Second):
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
func TestAStoppingLoopDropsScrapesWaitingForASlot(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	waiting := make(chan string, 4)
	hook := func(name string) { waiting <- name }
	slotWaitHook.Store(&hook)
	t.Cleanup(func() { slotWaitHook.Store(nil) })
	held, heldHits, release := holdingTarget(t, 0)
	queued, queuedHits := countingTarget(func(*http.Request) string { return "value=1\n" })
	defer queued.Close()
	_, stop, done := loopServer(t, 1,
		model.StaticTarget{Name: "held50", Collector: "text", Target: held.URL, Interval: model.Duration(time.Minute)},
		model.StaticTarget{Name: "queued3", Collector: "text", Target: queued.URL, Interval: model.Duration(time.Minute)},
	)
	testutil.WaitFor(t, "the held scrape to take the only slot", func() bool { return heldHits.Load() >= 1 })
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
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not return")
	}
	if n := queuedHits.Load(); n != 0 {
		t.Fatalf("a scrape waiting for a slot was begun after the loop stopped: %d requests", n)
	}
	if strings.Contains(logs.String(), "no scrape slot came free") {
		t.Fatalf("stopping was logged as a lack of slots:\n%s", logs)
	}
}

// A target given a new interval while its scrape is in flight is not scraped
// again until that scrape ends, so two scrapes of it never overlap.
func TestANewIntervalDoesNotOverlapTheScrapeInFlight(t *testing.T) {
	schedule := newTargetSchedule()
	now := time.Unix(1_000_000, 0)
	schedule.plan([]model.StaticTarget{scheduled("a", time.Second)}, now)
	now = now.Add(time.Second)
	due, _, _ := schedule.plan([]model.StaticTarget{scheduled("a", time.Second)}, now)
	if len(due) != 1 {
		t.Fatalf("due=%d", len(due))
	}
	due[0].state.running.Store(true)
	// The reload changes the interval; the new cadence, first due within
	// firstScrapeWindow, comes due while the scrape begun on the old one
	// still runs.
	schedule.plan([]model.StaticTarget{scheduled("a", 30*time.Second)}, now)
	now = now.Add(firstScrapeWindow)
	due, skipped, _ := schedule.plan([]model.StaticTarget{scheduled("a", 30*time.Second)}, now)
	if len(due) != 0 || len(skipped) != 0 {
		t.Fatalf("while the old scrape runs: due=%d skipped=%d", len(due), len(skipped))
	}
	schedule.states["a"].running.Store(false)
	// The first scrape on the new interval waited for the old one, and is
	// made at the next check after it ended.
	now = now.Add(scheduleCheckInterval)
	if due, _, _ = schedule.plan([]model.StaticTarget{scheduled("a", 30*time.Second)}, now); len(due) != 1 {
		t.Fatalf("once it ended: due=%d", len(due))
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
