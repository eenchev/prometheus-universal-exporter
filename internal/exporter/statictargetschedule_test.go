package exporter

import (
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
	due, _, next := schedule.plan(nil, []model.StaticTarget{target}, start)
	if len(due) != 0 && offset > 0 || !next.Equal(start.Add(offset)) {
		t.Fatalf("due=%d next=%s, want the first scrape at the offset %s", len(due), next.Sub(start), offset)
	}
	first := start.Add(offset)
	due, _, next = schedule.plan(nil, []model.StaticTarget{target}, first)
	if len(due) != 1 || !next.Equal(first.Add(10*time.Second)) {
		t.Fatalf("at the offset: due=%d next=%s", len(due), next.Sub(first))
	}
	// Looking 3.5 intervals late scrapes once, and keeps the cadence.
	late := first.Add(35 * time.Second)
	due, _, next = schedule.plan(nil, []model.StaticTarget{target}, late)
	if len(due) != 1 || !next.Equal(first.Add(40*time.Second)) {
		t.Fatalf("late: due=%d next=+%s, want the cadence kept at +40s", len(due), next.Sub(first))
	}
}

// A target with a long interval is first scraped within firstScrapeWindow,
// not up to an interval later, and its cadence, spread over the interval by
// name, starts at least a whole interval after that first scrape, and less
// than two.
func TestALongIntervalIsFirstScrapedPromptly(t *testing.T) {
	const interval = time.Hour
	for _, name := range []string{"a", "b", "payments", "legacy_eu", "nightly_backup"} {
		schedule := newTargetSchedule()
		start := time.Unix(1_000_000, 0)
		target := scheduled(name, interval)
		_, _, first := schedule.plan(nil, []model.StaticTarget{target}, start)
		if wait := first.Sub(start); wait < 0 || wait >= firstScrapeWindow {
			t.Fatalf("%s: first scrape after %s, want within %s", name, wait, firstScrapeWindow)
		}
		due, _, second := schedule.plan(nil, []model.StaticTarget{target}, first)
		if len(due) != 1 {
			t.Fatalf("%s: due=%d at the first scrape", name, len(due))
		}
		gap := second.Sub(first)
		if gap < interval || gap >= 2*interval {
			t.Fatalf("%s: second scrape %s after the first, want between one and two intervals", name, gap)
		}
		if offset := second.Sub(start) % interval; offset != scheduleOffset(name, interval) {
			t.Fatalf("%s: cadence at %s into the interval, want its offset %s", name, offset, scheduleOffset(name, interval))
		}
		due, _, third := schedule.plan(nil, []model.StaticTarget{target}, second)
		if len(due) != 1 || third.Sub(second) != interval {
			t.Fatalf("%s: then due=%d every %s, want every interval", name, len(due), third.Sub(second))
		}
	}
}

// A target still running when it is due again is not scraped beside that
// scrape: its turn waits for it; a changed interval starts a new cadence; a
// removed target is forgotten.
func TestAScheduleFollowsItsTargets(t *testing.T) {
	schedule := newTargetSchedule()
	now := time.Unix(1_000_000, 0)
	target := scheduled("a", time.Second)
	schedule.plan(nil, []model.StaticTarget{target}, now)
	now = now.Add(time.Second)
	due, _, turn := schedule.plan(nil, []model.StaticTarget{target}, now)
	if len(due) != 1 {
		t.Fatalf("due=%d", len(due))
	}
	due[0].state.running.Store(true)
	now = turn
	due, skipped, _ := schedule.plan(nil, []model.StaticTarget{target}, now)
	if len(due) != 0 || len(skipped) != 0 || !schedule.states["a"].waiting {
		t.Fatalf("while running: due=%d skipped=%d, want the turn waiting", len(due), len(skipped))
	}
	state := schedule.states["a"]
	schedule.plan(nil, []model.StaticTarget{scheduled("a", 5*time.Second)}, now)
	if schedule.states["a"] == state || schedule.states["a"].interval != 5*time.Second {
		t.Fatal("a changed interval kept the old cadence")
	}
	schedule.plan(nil, nil, now)
	if len(schedule.states) != 0 {
		t.Fatal("a removed target is still scheduled")
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

// A target a reload changed in anything but its interval — its address here —
// starts again, first scraped within firstScrapeWindow instead of on the old
// definition's cadence an hour away; an unchanged one keeps its cadence, and
// a scrape still running on the old definition keeps the new one from
// starting beside it.
func TestAChangedTargetStartsAgain(t *testing.T) {
	schedule := newTargetSchedule()
	now := time.Unix(1_000_000, 0)
	target := scheduled("a", time.Hour)
	_, _, first := schedule.plan(nil, []model.StaticTarget{target}, now)
	due, _, _ := schedule.plan(nil, []model.StaticTarget{target}, first)
	if len(due) != 1 {
		t.Fatalf("due=%d", len(due))
	}
	now = first.Add(time.Minute)
	state := schedule.states["a"]
	if _, _, next := schedule.plan(nil, []model.StaticTarget{scheduled("a", time.Hour)}, now); schedule.states["a"] != state || next.Sub(now) < 10*time.Minute {
		t.Fatalf("an unchanged target started again, next in %s", next.Sub(now))
	}
	due[0].state.running.Store(true)
	fixed := target
	fixed.Target = "http://fixed.invalid"
	_, _, next := schedule.plan(nil, []model.StaticTarget{fixed}, now)
	if schedule.states["a"] == state || next.Sub(now) >= firstScrapeWindow {
		t.Fatalf("a changed target keeps its old cadence: next in %s", next.Sub(now))
	}
	// While the old definition's scrape runs, the new one's first scrape
	// waits for it, neither made beside it nor put off to its cadence.
	due, skipped, again := schedule.plan(nil, []model.StaticTarget{fixed}, next)
	if len(due) != 0 || len(skipped) != 0 || again.Sub(next) != scheduleCheckInterval {
		t.Fatalf("while the old definition's scrape runs: due=%d skipped=%d, next in %s", len(due), len(skipped), again.Sub(next))
	}
	// It ends, and the first scrape is made at the next check.
	schedule.states["a"].running.Store(false)
	due, skipped, after := schedule.plan(nil, []model.StaticTarget{fixed}, again)
	if len(due) != 1 || len(skipped) != 0 || after.Sub(again) < time.Hour || !due[0].deadline.Equal(again.Add(time.Hour)) {
		t.Fatalf("after the old scrape ended: due=%d skipped=%d, then next in %s", len(due), len(skipped), after.Sub(again))
	}
}
