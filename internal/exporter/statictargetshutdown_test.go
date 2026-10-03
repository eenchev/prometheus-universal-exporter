package exporter

import (
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A shutdown stops the static targets without reporting them down
// (statictargetschedule.go), and a reload does not let a target's scrapes
// overlap or a removed target publish.

// A target given a new interval while its scrape is in flight is not scraped
// again until that scrape ends, so two scrapes of it never overlap.
func TestANewIntervalDoesNotOverlapTheScrapeInFlight(t *testing.T) {
	schedule := newTargetSchedule()
	now := time.Unix(1_000_000, 0)
	schedule.plan(nil, []model.StaticTarget{scheduled("a", time.Second)}, now)
	now = now.Add(time.Second)
	due, _, _ := schedule.plan(nil, []model.StaticTarget{scheduled("a", time.Second)}, now)
	if len(due) != 1 {
		t.Fatalf("due=%d", len(due))
	}
	due[0].state.running.Store(true)
	// The reload changes the interval; the new cadence, first due within
	// firstScrapeWindow, comes due while the scrape begun on the old one
	// still runs.
	schedule.plan(nil, []model.StaticTarget{scheduled("a", 30*time.Second)}, now)
	now = now.Add(firstScrapeWindow)
	due, skipped, _ := schedule.plan(nil, []model.StaticTarget{scheduled("a", 30*time.Second)}, now)
	if len(due) != 0 || len(skipped) != 0 {
		t.Fatalf("while the old scrape runs: due=%d skipped=%d", len(due), len(skipped))
	}
	schedule.states["a"].running.Store(false)
	// The first scrape on the new interval waited for the old one, and is
	// made at the next check after it ended.
	now = now.Add(scheduleCheckInterval)
	if due, _, _ = schedule.plan(nil, []model.StaticTarget{scheduled("a", 30*time.Second)}, now); len(due) != 1 {
		t.Fatalf("once it ended: due=%d", len(due))
	}
}
