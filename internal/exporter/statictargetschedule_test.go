package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Static targets are scraped on their own intervals, not on the OTLP
// export's (statictargetschedule.go).

func scheduled(name string, interval time.Duration) model.StaticTarget {
	return model.StaticTarget{Name: name, Collector: "text", Target: "http://t.invalid", Interval: model.Duration(interval)}
}

// A target is first due at its offset within its interval, then every
// interval from there, however late the scheduler looks.
func TestAScheduleKeepsItsCadence(t *testing.T) {
	schedule := newTargetSchedule()
	start := time.Unix(1_000_000, 0)
	target := scheduled("a", 10*time.Second)
	offset := scheduleOffset("a", 10*time.Second)
	if offset < 0 || offset >= 10*time.Second || offset != scheduleOffset("a", 10*time.Second) {
		t.Fatalf("offset %s is not a stable place within the interval", offset)
	}
	due, _, next := schedule.plan([]model.StaticTarget{target}, start)
	if len(due) != 0 && offset > 0 || !next.Equal(start.Add(offset)) {
		t.Fatalf("due=%d next=%s, want the first scrape at the offset %s", len(due), next.Sub(start), offset)
	}
	first := start.Add(offset)
	due, _, next = schedule.plan([]model.StaticTarget{target}, first)
	if len(due) != 1 || !next.Equal(first.Add(10*time.Second)) {
		t.Fatalf("at the offset: due=%d next=%s", len(due), next.Sub(first))
	}
	// Looking 3.5 intervals late scrapes once, and keeps the cadence.
	late := first.Add(35 * time.Second)
	due, _, next = schedule.plan([]model.StaticTarget{target}, late)
	if len(due) != 1 || !next.Equal(first.Add(40*time.Second)) {
		t.Fatalf("late: due=%d next=+%s, want the cadence kept at +40s", len(due), next.Sub(first))
	}
}

// A target with a long interval is first scraped within firstScrapeWindow,
// not up to an interval later, and its cadence, spread over the interval by
// name, starts at least half an interval after that first scrape.
func TestALongIntervalIsFirstScrapedPromptly(t *testing.T) {
	const interval = time.Hour
	for _, name := range []string{"a", "b", "payments", "legacy_eu", "nightly_backup"} {
		schedule := newTargetSchedule()
		start := time.Unix(1_000_000, 0)
		target := scheduled(name, interval)
		_, _, first := schedule.plan([]model.StaticTarget{target}, start)
		if wait := first.Sub(start); wait < 0 || wait >= firstScrapeWindow {
			t.Fatalf("%s: first scrape after %s, want within %s", name, wait, firstScrapeWindow)
		}
		due, _, second := schedule.plan([]model.StaticTarget{target}, first)
		if len(due) != 1 {
			t.Fatalf("%s: due=%d at the first scrape", name, len(due))
		}
		gap := second.Sub(first)
		if gap < interval/2 || gap >= interval*3/2 {
			t.Fatalf("%s: second scrape %s after the first, want between half and one and a half intervals", name, gap)
		}
		if offset := second.Sub(start) % interval; offset != scheduleOffset(name, interval) {
			t.Fatalf("%s: cadence at %s into the interval, want its offset %s", name, offset, scheduleOffset(name, interval))
		}
		due, _, third := schedule.plan([]model.StaticTarget{target}, second)
		if len(due) != 1 || third.Sub(second) != interval {
			t.Fatalf("%s: then due=%d every %s, want every interval", name, len(due), third.Sub(second))
		}
	}
}

// A target still running when it is due again is skipped, not overlapped; a
// changed interval starts a new cadence; a removed target is forgotten.
func TestAScheduleFollowsItsTargets(t *testing.T) {
	schedule := newTargetSchedule()
	now := time.Unix(1_000_000, 0)
	target := scheduled("a", time.Second)
	schedule.plan([]model.StaticTarget{target}, now)
	now = now.Add(time.Second)
	due, _, _ := schedule.plan([]model.StaticTarget{target}, now)
	if len(due) != 1 {
		t.Fatalf("due=%d", len(due))
	}
	due[0].state.running.Store(true)
	now = now.Add(time.Second)
	due, skipped, _ := schedule.plan([]model.StaticTarget{target}, now)
	if len(due) != 0 || len(skipped) != 1 {
		t.Fatalf("while running: due=%d skipped=%d", len(due), len(skipped))
	}
	state := skipped[0].state
	schedule.plan([]model.StaticTarget{scheduled("a", 5*time.Second)}, now)
	if schedule.states["a"] == state || schedule.states["a"].interval != 5*time.Second {
		t.Fatal("a changed interval kept the old cadence")
	}
	schedule.plan(nil, now)
	if len(schedule.states) != 0 {
		t.Fatal("a removed target is still scheduled")
	}
}

// The loop scrapes each target on its own interval and queues the results,
// with no export running: the export does not decide when targets are scraped.
func TestStaticTargetsAreScrapedOnTheirOwnIntervals(t *testing.T) {
	var fast, slow atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			fast.Add(1)
		} else {
			slow.Add(1)
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	// Built directly: validation holds intervals to at least a second, which
	// would make this test slow.
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "fast", Collector: "text", Target: target.URL + "/fast", Interval: model.Duration(40 * time.Millisecond)},
		{Name: "slow", Collector: "text", Target: target.URL + "/slow", Interval: model.Duration(time.Hour)},
	}}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	manager.SetTargets("", file)
	server := NewServer(manager, "python3", slog.Default())
	server.logger = testutil.QuietLogger(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.StaticScrapeLoop(ctx)
	}()
	testutil.WaitFor(t, "the fast target to be scraped several times", func() bool { return fast.Load() >= 4 })
	cancel()
	<-done
	if got := slow.Load(); got > 1 {
		t.Errorf("the hourly target was scraped %d times", got)
	}
	if results := server.staticTargetResults(); len(results) == 0 || results[0].name != "fast" {
		t.Errorf("the scrapes published %+v, want the fast target's result for the static targets endpoint", results)
	}
	// Neither target sets export_via_otlp, so nothing waits for an export.
	if _, ok := pendingValue(server, "http_exporter_target_up"); ok {
		t.Error("a target without export_via_otlp was queued for OTLP")
	}
}

// The export loop only delivers: it scrapes no target.
func TestTheExportLoopDoesNotScrape(t *testing.T) {
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	var exports atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readOTLPBody(t, r)
		exports.Add(1)
	}))
	t.Cleanup(endpoint.Close)
	otlp := otlpConfig(endpoint.URL)
	otlp.Interval = model.Duration(20 * time.Millisecond)
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlp},
		&model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "text", Target: target.URL}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.OTLPExportLoop(ctx)
	}()
	testutil.WaitFor(t, "a few exports", func() bool { return exports.Load() >= 3 })
	cancel()
	<-done
	if got := requests.Load(); got != 0 {
		t.Fatalf("the export loop scraped the target %d times", got)
	}
}

// The scrape loop scrapes at most the file's concurrency at once.
func TestTheScrapeLoopKeepsToTheFilesConcurrency(t *testing.T) {
	var inFlight, most, scraped atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		for {
			seen := most.Load()
			if now <= seen || most.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		scraped.Add(1)
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	// Built directly, with intervals under the least validation allows, so
	// every target is first due within 300ms; each scrape outlasts the
	// handler's 50ms, so a scrape in the handler is one the loop is running.
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Concurrency: 2}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		file.Targets = append(file.Targets, model.StaticTarget{Name: name, Collector: "text", Target: target.URL, Interval: model.Duration(300 * time.Millisecond)})
	}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	manager.SetTargets("", file)
	server := NewServer(manager, "python3", testutil.QuietLogger(t))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.StaticScrapeLoop(ctx)
	}()
	testutil.WaitFor(t, "every target to be scraped", func() bool { return scraped.Load() >= 5 })
	cancel()
	<-done
	if got := most.Load(); got != 2 {
		t.Fatalf("at most %d scrapes ran at once, want the file's concurrency, 2", got)
	}
}

func TestStaticTargetConcurrencyDefaultsToEight(t *testing.T) {
	for concurrency, want := range map[int]int{0: 8, 3: 3} {
		if got := (&model.StaticTargetFile{Concurrency: concurrency}).ScrapeConcurrency(); got != want {
			t.Errorf("concurrency %d scrapes %d at once, want %d", concurrency, got, want)
		}
	}
	if got := (*model.StaticTargetFile)(nil).ScrapeConcurrency(); got != 8 {
		t.Errorf("without a file: %d", got)
	}
}
