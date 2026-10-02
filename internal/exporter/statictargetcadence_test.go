//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A scrape that uses its whole interval ends just after the next turn has
// come. That turn is kept, a reload rescrapes the targets of a collector it
// changed, and a scrape uses the configuration its target was read with
// (statictargetschedule.go).

// lockedBuffer is a log buffer the scrape loop's goroutines write and the
// test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// soonScraped is count target names whose first scrape, and whose place in an
// interval of within, comes in the first 50ms, so a test of the loop need
// not wait out the first-scrape window.
func soonScraped(t *testing.T, within time.Duration, count int) []string {
	t.Helper()
	var names []string
	for i := 0; len(names) < count; i++ {
		if i > 1_000_000 {
			t.Fatal("too few names are scheduled that soon")
		}
		if name := fmt.Sprintf("app_%d", i); scheduleOffset(name, min(within, firstScrapeWindow)) < 50*time.Millisecond {
			names = append(names, name)
		}
	}
	return names
}

// A turn that comes while the target's last scrape still runs is not given
// up: it waits, starts once that scrape has ended, and must end by the turn
// after it. Only when the scrape has still not ended at that turn is the
// waiting one lost, and reported as skipped; the newer turn waits in its
// place.
func TestATurnWaitsForTheScrapeStillRunning(t *testing.T) {
	const interval = 10 * time.Second
	schedule := newTargetSchedule()
	target := scheduled("a", interval)
	targets := []model.StaticTarget{target}
	start := time.Unix(1_000_000, 0)
	_, _, first := schedule.plan(nil, targets, start)
	due, _, turn := schedule.plan(nil, targets, first)
	if len(due) != 1 || !due[0].deadline.Equal(first.Add(interval)) {
		t.Fatalf("first scrape: due=%d, want one with the whole interval", len(due))
	}
	state := due[0].state

	// A turn of the cadence with nothing running: scraped at once, with the
	// whole interval from when it starts, though the loop looks a little late.
	late := turn.Add(30 * time.Millisecond)
	due, skipped, next := schedule.plan(nil, targets, late)
	if len(due) != 1 || len(skipped) != 0 || !due[0].deadline.Equal(late.Add(interval)) || !next.Equal(turn.Add(interval)) {
		t.Fatalf("an on-time turn: due=%d skipped=%d deadline=+%s next=+%s", len(due), len(skipped), due[0].deadline.Sub(turn), next.Sub(turn))
	}

	// That scrape hangs, and is still running when the next turn comes: the
	// turn waits, and asks to be woken when the scrape ends.
	state.running.Store(true)
	second := turn.Add(interval)
	due, skipped, next = schedule.plan(nil, targets, second)
	if len(due) != 0 || len(skipped) != 0 || !next.Equal(second.Add(interval)) {
		t.Fatalf("a turn finding its target still scraped: due=%d skipped=%d next=+%s, want it waiting", len(due), len(skipped), next.Sub(second))
	}
	if !state.scrapeEnded() {
		t.Fatal("the schedule did not ask to be woken when the scrape ends")
	}
	// It has ended: the waiting turn's scrape starts, and must end with
	// its turn, so the turn after it finds nothing running.
	ended := second.Add(40 * time.Millisecond)
	due, skipped, next = schedule.plan(nil, targets, ended)
	if len(due) != 1 || len(skipped) != 0 || !due[0].deadline.Equal(second.Add(interval)) || !next.Equal(second.Add(interval)) {
		t.Fatalf("after the scrape ended: due=%d skipped=%d next=+%s, want the waiting turn scraped until the turn after it", len(due), len(skipped), next.Sub(second))
	}
	// Nothing more is due until then.
	if due, skipped, _ = schedule.plan(nil, targets, ended.Add(time.Second)); len(due) != 0 || len(skipped) != 0 {
		t.Fatalf("between turns: due=%d skipped=%d", len(due), len(skipped))
	}

	// A scrape that outlasts a whole further turn loses the turn that
	// waited for it; the newer one waits in its place.
	state.running.Store(true)
	third := second.Add(interval)
	if due, skipped, _ = schedule.plan(nil, targets, third); len(due) != 0 || len(skipped) != 0 {
		t.Fatalf("the third turn: due=%d skipped=%d, want it waiting", len(due), len(skipped))
	}
	fourth := third.Add(interval)
	due, skipped, _ = schedule.plan(nil, targets, fourth)
	if len(due) != 0 || len(skipped) != 1 || skipped[0].target.Name != "a" {
		t.Fatalf("a turn whose wait outlasted it: due=%d skipped=%d, want it reported lost", len(due), len(skipped))
	}
	state.scrapeEnded()
	due, skipped, _ = schedule.plan(nil, targets, fourth.Add(time.Second))
	if len(due) != 1 || len(skipped) != 0 || !due[0].deadline.Equal(fourth.Add(interval)) {
		t.Fatalf("after the stuck scrape ended: due=%d skipped=%d, want the fourth turn scraped", len(due), len(skipped))
	}
}

// A healthy target whose scrapes take most of its interval — 26 seconds of
// 30 — was cut short once after every start, and reported down. Its first
// scrape is made early, and the cadence started as little as half an interval
// after it: the cadence's first turn came while the first scrape still ran,
// waited for it, and was left what remained of its turn, less than the 26
// seconds the scrape takes. The cadence now starts a whole interval after
// the first scrape began or later, so whatever the target's name, which sets
// the offsets, and however much of its interval the first scrape takes, the
// first turn of the cadence finds nothing running and has a whole interval.
// The same holds after a reload that changed the target, whose first scrape
// waits for the one begun on the old definition.
func TestTheFirstTurnOfACadenceHasAWholeIntervalWhateverTheFirstScrapeTook(t *testing.T) {
	const interval = 30 * time.Second
	start := time.Unix(1_000_000, 0)
	for _, takes := range []time.Duration{26 * time.Second, interval} {
		for i := 0; i < 400; i++ {
			name := fmt.Sprintf("app_%d", i)
			schedule := newTargetSchedule()
			targets := []model.StaticTarget{scheduled(name, interval)}
			_, _, first := schedule.plan(nil, targets, start)
			if wait := first.Sub(start); wait < 0 || wait >= firstScrapeWindow {
				t.Fatalf("%s: first scrape after %s, want within %s", name, wait, firstScrapeWindow)
			}
			due, _, turn := schedule.plan(nil, targets, first)
			if len(due) != 1 || !due[0].deadline.Equal(first.Add(interval)) {
				t.Fatalf("%s: the first scrape: due=%d", name, len(due))
			}
			state := due[0].state
			state.running.Store(true)
			ends := first.Add(takes)
			// The cadence keeps the place in the interval the name gives
			// it, and starts when the first scrape has ended.
			if gap := turn.Sub(first); gap < interval || gap >= 2*interval || turn.Before(ends) || turn.Sub(start)%interval != scheduleOffset(name, interval) {
				t.Fatalf("%s: the cadence starts %s after the first scrape began, which takes %s, at %s into the interval; want one to two intervals after it, at %s", name, gap, takes, turn.Sub(start)%interval, scheduleOffset(name, interval))
			}
			// Nothing is due, skipped or left waiting while it runs.
			if due, skipped, _ := schedule.plan(nil, targets, ends.Add(-time.Millisecond)); len(due) != 0 || len(skipped) != 0 || state.waiting {
				t.Fatalf("%s: while the first scrape runs: due=%d skipped=%d waiting=%v", name, len(due), len(skipped), state.waiting)
			}
			state.scrapeEnded()
			due, skipped, next := schedule.plan(nil, targets, turn)
			if len(due) != 1 || len(skipped) != 0 || !next.Equal(turn.Add(interval)) {
				t.Fatalf("%s: the first turn of the cadence: due=%d skipped=%d next=+%s", name, len(due), len(skipped), next.Sub(turn))
			}
			if budget := due[0].deadline.Sub(turn); budget != interval {
				t.Fatalf("%s: the first turn of the cadence must end within %s, and the scrape takes %s of an interval of %s", name, budget, takes, interval)
			}

			// A reload changes the target while that scrape runs: the new
			// definition's first scrape waits for it, starts when it has
			// ended with a whole interval, and its cadence starts a whole
			// interval after that.
			state.running.Store(true)
			changed := targets[0]
			changed.Target = "http://changed.invalid"
			reloaded := []model.StaticTarget{changed}
			at := turn.Add(time.Second)
			schedule.plan(nil, reloaded, at)
			over := turn.Add(takes)
			if due, skipped, _ := schedule.plan(nil, reloaded, over.Add(-time.Millisecond)); len(due) != 0 || len(skipped) != 0 {
				t.Fatalf("%s: while the old definition's scrape runs: due=%d skipped=%d", name, len(due), len(skipped))
			}
			schedule.states[name].scrapeEnded()
			due, skipped, turn = schedule.plan(nil, reloaded, over)
			if len(due) != 1 || len(skipped) != 0 || !due[0].deadline.Equal(over.Add(interval)) || turn.Sub(over) < interval || turn.Sub(over) >= 2*interval {
				t.Fatalf("%s: the changed target's first scrape: due=%d skipped=%d, its cadence starting %s after it", name, len(due), len(skipped), turn.Sub(over))
			}
			due[0].state.running.Store(true)
			due[0].state.scrapeEnded()
			if due, skipped, _ = schedule.plan(nil, reloaded, turn); len(due) != 1 || len(skipped) != 0 || due[0].deadline.Sub(turn) != interval {
				t.Fatalf("%s: the first turn of the changed target's cadence: due=%d skipped=%d", name, len(due), len(skipped))
			}
		}
	}
}

// A target that never answers is tried once per interval, each scrape ending
// with its interval and the next starting as it ends, rather than every
// second interval; and no scrape is logged as skipped, since no turn is lost.
func TestAHangingTargetIsScrapedOncePerInterval(t *testing.T) {
	const interval = 500 * time.Millisecond
	const scrapes = 6
	var mu sync.Mutex
	var started []time.Time
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		started = append(started, time.Now())
		mu.Unlock()
		<-r.Context().Done()
	}))
	t.Cleanup(target.Close)
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(started)
	}
	logs := &lockedBuffer{}
	// The server is built here rather than by loopServer, whose logger
	// could only be replaced while the loop already logs.
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	// Built directly: validation holds intervals to at least a second.
	manager.SetTargets("", &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: soonScraped(t, interval, 1)[0], Collector: "text", Target: target.URL, Interval: model.Duration(interval)},
	}})
	server := NewServer(manager, "python3", slog.New(slog.NewJSONHandler(logs, nil)))
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
	deadline := time.Now().Add(20 * time.Second)
	for count() < scrapes {
		if time.Now().After(deadline) {
			t.Fatalf("only %d scrapes of the hanging target were started", count())
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	took := started[scrapes-1].Sub(started[0])
	mu.Unlock()
	// Five intervals lie between the first scrape and the sixth; every
	// second turn given up made that ten.
	if most := (scrapes - 1) * interval * 3 / 2; took > most {
		t.Errorf("%d scrapes of a hanging target took %s, want one per interval of %s", scrapes, took, interval)
	}
	if text := logs.String(); strings.Contains(text, "static target scrape skipped") || !strings.Contains(text, "static target scrape failed") {
		t.Errorf("want the failed scrapes logged and none skipped:\n%s", text)
	}
}

// A reload that changes a collector starts the targets using it again, as
// one that changes a target does, while the targets of an unchanged
// collector keep their place in the schedule.
func TestAChangedCollectorStartsItsTargetsAgain(t *testing.T) {
	configuration := func(rule string) *model.Config {
		changing, steady := testutil.Collector("changing", "text"), testutil.Collector("steady", "text")
		changing.Metrics[0].Name = rule
		cfg := &model.Config{Collectors: []model.Collector{changing, steady}}
		if err := config.Validate(cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	targets := []model.StaticTarget{
		{Name: "a", Collector: "changing", Target: "http://a.invalid", Interval: model.Duration(time.Hour)},
		{Name: "b", Collector: "steady", Target: "http://b.invalid", Interval: model.Duration(time.Hour)},
	}
	schedule := newTargetSchedule()
	before := configuration("old_name")
	now := time.Unix(1_000_000, 0)
	schedule.plan(before, targets, now)
	now = now.Add(firstScrapeWindow)
	if due, _, _ := schedule.plan(before, targets, now); len(due) != 2 || due[0].config != before {
		t.Fatalf("first scrapes: due=%d, want both, each with the configuration planned with", len(due))
	}
	a, b := schedule.states["a"], schedule.states["b"]
	now = now.Add(time.Minute)
	// The same collectors read again, as a reload that changed nothing
	// leaves them, start nothing again.
	schedule.plan(configuration("old_name"), targets, now)
	if schedule.states["a"] != a || schedule.states["b"] != b {
		t.Fatal("a reload that changed no collector started targets again")
	}
	after := configuration("new_name")
	_, _, next := schedule.plan(after, targets, now)
	if schedule.states["a"] == a {
		t.Fatal("the target of a changed collector kept its place in the schedule")
	}
	if schedule.states["b"] != b {
		t.Fatal("the target of an unchanged collector started again")
	}
	if wait := next.Sub(now); wait < 0 || wait >= firstScrapeWindow {
		t.Fatalf("the changed collector's target is next scraped in %s, want within %s", wait, firstScrapeWindow)
	}
	due, _, _ := schedule.plan(after, targets, now.Add(firstScrapeWindow))
	if len(due) != 1 || due[0].target.Name != "a" || due[0].config != after {
		t.Fatalf("after the reload: due=%+v, want only the changed collector's target, with the new configuration", due)
	}
}

// With the loop running, a reload that renames a collector's metric shows on
// the static targets endpoint within the first-scrape window, although the
// target's interval is an hour and its own definition did not change.
func TestAReloadedCollectorShowsOnTheEndpointPromptly(t *testing.T) {
	target := textTarget(t, "value=1\n")
	dir := t.TempDir()
	configPath, targetsPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "targets.yaml")
	writeConfig := func(metric string) {
		body := "collectors:\n  - name: text\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: " + metric + "\n        expression: 'value=(\\d+)'\n"
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("old_name")
	name := soonScraped(t, time.Hour, 1)[0]
	if err := os.WriteFile(targetsPath, []byte("interval: 1h\ntargets:\n  - name: "+name+"\n    collector: text\n    target: "+target.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadStaticTargets(targetsPath)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, configPath, testutil.QuietLogger(t))
	manager.SetTargets(targetsPath, file)
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
	testutil.WaitFor(t, "the first scrape to be served", func() bool {
		return strings.Contains(getStaticTargets(t, server, "/static-targets"), "old_name{")
	})
	writeConfig("new_name")
	if err := manager.Reload(config.ReloadTriggerHTTP); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, "the reloaded collector's metric to be served", func() bool {
		body := getStaticTargets(t, server, "/static-targets")
		return strings.Contains(body, "new_name{") && !strings.Contains(body, "old_name{")
	})
}

// The endpoint serves what each target's last scrape left. A failed scrape
// leaves the health series alone, so values the target no longer gives are
// not presented as current; with cache.stale_if_error it leaves the last
// good result, marked stale; and a turn that publishes nothing — one skipped,
// or cut short by a shutdown — leaves the last result standing.
func TestAFailedScrapeLeavesOnlyTheHealthSeries(t *testing.T) {
	for _, stale := range []bool{false, true} {
		var down atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if down.Load() {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte("value=1\n"))
		}))
		t.Cleanup(target.Close)
		collector := testutil.Collector("text", "text")
		if stale {
			collector.Cache.StaleIfError = model.Duration(time.Hour)
		}
		file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "app", Collector: "text", Target: target.URL}}}
		server := newStaticServer(t, &model.Config{Collectors: []model.Collector{collector}}, file)
		server.logger = testutil.QuietLogger(t)
		server.scrapeStaticTargets(t.Context(), 10*time.Second)
		body := getStaticTargets(t, server, "/static-targets")
		if !strings.Contains(body, `demo_value{static_target="app"} 1`) || !strings.Contains(body, "http_exporter_target_up{") {
			t.Fatalf("stale_if_error %t, after a good scrape:\n%s", stale, body)
		}
		down.Store(true)
		server.scrapeStaticTargets(t.Context(), 10*time.Second)
		body = getStaticTargets(t, server, "/static-targets")
		if !strings.Contains(body, `http_exporter_target_up{collector="text",static_target="app",target="`+target.URL+`"} 0`) ||
			strings.Contains(body, "http_exporter_target_last_success_timestamp_seconds{collector=\"text\",static_target=\"app\",target=\""+target.URL+"\"} 0\n") {
			t.Fatalf("stale_if_error %t, after a failed scrape, want the target down and its last success kept:\n%s", stale, body)
		}
		if served := strings.Contains(body, `demo_value{static_target="app"} 1`); served != stale {
			t.Fatalf("stale_if_error %t, after a failed scrape: the last values served is %t:\n%s", stale, served, body)
		}
		if marked := strings.Contains(body, `http_exporter_result_stale{static_target="app"} 1`); marked != stale {
			t.Fatalf("stale_if_error %t, after a failed scrape: marked stale is %t:\n%s", stale, marked, body)
		}
		// A scrape the shutdown cuts short publishes nothing: what the
		// failed scrape left is still what is served.
		server.AbortStaticScrapes()
		server.scrapeStaticTargets(server.staticScrapes(), 10*time.Second)
		if again := getStaticTargets(t, server, "/static-targets"); stale && !strings.Contains(again, `demo_value{static_target="app"} 1`) || !strings.Contains(again, "http_exporter_target_up{") {
			t.Fatalf("stale_if_error %t, after a scrape that published nothing:\n%s", stale, again)
		}
	}
}

// A scrape uses the configuration its target was read with. One that waits
// for a scrape slot while a reload renames its collector in both files still
// scrapes with its collector, rather than looking the old name up in the new
// configuration; the renamed collector's targets are then scraped again.
func TestAScrapeUsesTheConfigurationItsTargetWasReadWith(t *testing.T) {
	waiting := make(chan string, 16)
	hook := func(name string) { waiting <- name }
	slotWaitHook.Store(&hook)
	t.Cleanup(func() { slotWaitHook.Store(nil) })
	held, hits, release := holdingTarget(t, 0)
	names := soonScraped(t, time.Minute, 2)
	dir := t.TempDir()
	configPath, targetsPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "targets.yaml")
	write := func(collector string) {
		t.Helper()
		body := "collectors:\n  - name: " + collector + "\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var targets strings.Builder
		targets.WriteString("interval: 1m\nconcurrency: 1\ntargets:\n")
		for _, name := range names {
			fmt.Fprintf(&targets, "  - name: %s\n    collector: %s\n    target: %s\n", name, collector, held.URL)
		}
		if err := os.WriteFile(targetsPath, []byte(targets.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("before")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadStaticTargets(targetsPath)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	manager := config.NewManager(cfg, configPath, testutil.QuietLogger(t))
	manager.SetTargets(targetsPath, file)
	server := NewServer(manager, "python3", slog.New(slog.NewJSONHandler(logs, nil)))
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
	// One scrape holds the only slot, at the target; the other waits for it.
	testutil.WaitFor(t, "a scrape to take the only slot", func() bool { return hits.Load() >= 1 })
	for begun := map[string]bool{}; len(begun) < 2; {
		select {
		case name := <-waiting:
			begun[name] = true
		case <-time.After(15 * time.Second):
			t.Fatal("the second scrape never began to wait for a slot")
		}
	}
	write("after")
	if err := manager.Reload(config.ReloadTriggerHTTP); err != nil {
		t.Fatal(err)
	}
	close(release)
	testutil.WaitFor(t, "the renamed collector's targets to be scraped again", func() bool {
		return strings.Count(getStaticTargets(t, server, "/static-targets"), `http_exporter_target_up{collector="after"`) == 2
	})
	// Both scrapes begun before the reload reached the target, and both
	// targets were scraped again after it.
	if got := hits.Load(); got != 4 {
		t.Errorf("the target was asked %d times, want 4: each target before the reload and after it", got)
	}
	if text := logs.String(); strings.Contains(text, "unknown collector") {
		t.Fatalf("a scrape looked its collector up in a configuration its target was not read with:\n%s", text)
	}
}
