package exporter

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Scheduled targets are scraped on their own intervals, set in the targets
// file, as Prometheus scrapes a probe on its scrape_interval: the exporter's
// OTLP export only delivers what the scrapes queued, on otlp.interval, and
// does not decide when a target is scraped. Each target runs on a fixed
// cadence measured from its first scrape, so a slow scrape does not push the
// next one later; its first scrape is offset within its interval by a hash of
// its name, so targets sharing an interval are spread over it rather than all
// scraped at once. A scrape is bounded by its interval, and one still running
// when the next is due makes that one skipped rather than overlapping it.

// scheduleCheckInterval is the longest the scheduler sleeps, so a reloaded
// target file takes effect within it.
const scheduleCheckInterval = time.Second

// targetSchedule is when each scheduled target is next due.
type targetSchedule struct {
	states map[string]*scheduledState
}

// scheduledState is one target's place in the schedule.
type scheduledState struct {
	interval time.Duration
	next     time.Time
	// running is set while a scrape of the target is in flight.
	running atomic.Bool
}

// dueTarget is a target to scrape now, with its state.
type dueTarget struct {
	target model.ScheduledTarget
	state  *scheduledState
}

func newTargetSchedule() *targetSchedule {
	return &targetSchedule{states: map[string]*scheduledState{}}
}

// plan brings the schedule in step with targets, the ones in force, and
// returns those due at now, those due but still running from their last
// scrape, and when the next one is due. A target new to the schedule, or whose
// interval changed, starts a cadence of its own; one no longer in targets is
// forgotten.
func (s *targetSchedule) plan(targets []model.ScheduledTarget, now time.Time) (due, skipped []dueTarget, next time.Time) {
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		interval := time.Duration(target.Interval)
		if interval <= 0 {
			interval = time.Duration(model.DefaultScheduledTargetInterval)
		}
		seen[target.Name] = true
		state := s.states[target.Name]
		if state == nil || state.interval != interval {
			state = &scheduledState{interval: interval, next: now.Add(scheduleOffset(target.Name, interval))}
			s.states[target.Name] = state
		}
		if !now.Before(state.next) {
			if state.running.Load() {
				skipped = append(skipped, dueTarget{target, state})
			} else {
				due = append(due, dueTarget{target, state})
			}
			for !state.next.After(now) {
				state.next = state.next.Add(interval)
			}
		}
		if next.IsZero() || state.next.Before(next) {
			next = state.next
		}
	}
	for name := range s.states {
		if !seen[name] {
			delete(s.states, name)
		}
	}
	return due, skipped, next
}

// scheduleOffset spreads targets over their interval by a hash of the name,
// the same for a name every time the exporter starts.
func scheduleOffset(name string, interval time.Duration) time.Duration {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(name))
	return time.Duration(hash.Sum64() % uint64(interval)) //nolint:gosec // G115: the remainder is below interval, a positive time.Duration
}

var (
	errStillRunning = errors.New("the previous scrape is still running")
	errNoSlot       = errors.New("no scrape slot came free within the interval; other scheduled scrapes held them all")
)

// ScheduledScrapeLoop scrapes every scheduled target on its interval until
// ctx ends, and then waits for the scrapes in flight. The results are queued
// for the OTLP export loop, which delivers them on otlp.interval.
func (s *Server) ScheduledScrapeLoop(ctx context.Context) {
	schedule := newTargetSchedule()
	slots := make(chan struct{}, scheduledTargetConcurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		now := time.Now()
		due, skipped, next := schedule.plan(s.manager.Targets(), now)
		for _, d := range skipped {
			// Repeats are logged sparingly, as failed scrapes are.
			s.failures.failed(s.logger, slog.LevelWarn, failureKey(d.target.Collector, "scheduled target "+d.target.Name, "schedule"),
				"scheduled target scrape skipped", "schedule", errStillRunning, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
		}
		for _, d := range due {
			d.state.running.Store(true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer d.state.running.Store(false)
				scrapeCtx, cancel := context.WithTimeout(ctx, d.state.interval)
				defer cancel()
				// Waiting for a slot counts against the scrape's interval.
				select {
				case slots <- struct{}{}:
				case <-scrapeCtx.Done():
					s.failures.failed(s.logger, slog.LevelWarn, failureKey(d.target.Collector, "scheduled target "+d.target.Name, "schedule"),
						"scheduled target scrape skipped", "schedule", errNoSlot, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
					return
				}
				defer func() { <-slots }()
				s.scrapeTarget(scrapeCtx, d.target)
			}()
		}
		wait := scheduleCheckInterval
		if !next.IsZero() {
			wait = min(max(time.Until(next), 0), scheduleCheckInterval)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
