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

// Static targets are scraped on their own intervals, set in the static targets
// file, as Prometheus scrapes a probe on its scrape_interval. Neither
// Prometheus's scrapes of the static targets endpoint nor the OTLP export,
// which delivers on otlp.interval what the scrapes queued, decides when a
// target is scraped. Each target runs on a fixed
// cadence measured from its first scrape, so a slow scrape does not push the
// next one later; its first scrape is offset within its interval by a hash of
// its name, so targets sharing an interval are spread over it rather than all
// scraped at once. A scrape is bounded by its interval, and one still running
// when the next is due makes that one skipped rather than overlapping it.

// scheduleCheckInterval is the longest the scheduler sleeps, so a reloaded
// target file takes effect within it.
const scheduleCheckInterval = time.Second

// targetSchedule is when each static target is next due.
type targetSchedule struct {
	states map[string]*staticTargetState
}

// staticTargetState is one target's place in the schedule.
type staticTargetState struct {
	interval time.Duration
	next     time.Time
	// running is set while a scrape of the target is in flight.
	running atomic.Bool
}

// dueTarget is a target to scrape now, with its state.
type dueTarget struct {
	target model.StaticTarget
	state  *staticTargetState
}

func newTargetSchedule() *targetSchedule {
	return &targetSchedule{states: map[string]*staticTargetState{}}
}

// plan brings the schedule in step with targets, the ones in force, and
// returns those due at now, those due but still running from their last
// scrape, and when the next one is due. A target new to the schedule, or whose
// interval changed, starts a cadence of its own; one no longer in targets is
// forgotten.
func (s *targetSchedule) plan(targets []model.StaticTarget, now time.Time) (due, skipped []dueTarget, next time.Time) {
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		interval := time.Duration(target.Interval)
		if interval <= 0 {
			// Validation sets every loaded target's interval; this only
			// keeps a target built some other way from spinning.
			interval = time.Minute
		}
		seen[target.Name] = true
		state := s.states[target.Name]
		if state == nil || state.interval != interval {
			state = &staticTargetState{interval: interval, next: now.Add(scheduleOffset(target.Name, interval))}
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
	errNoSlot       = errors.New("no scrape slot came free within the interval; other static target scrapes held them all")
)

// StaticScrapeLoop scrapes every static target on its interval until
// ctx ends, and then waits for the scrapes in flight. The results are queued
// for the OTLP export loop, which delivers them on otlp.interval.
func (s *Server) StaticScrapeLoop(ctx context.Context) {
	schedule := newTargetSchedule()
	slots := make(chan struct{}, staticTargetConcurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		now := time.Now()
		due, skipped, next := schedule.plan(s.manager.StaticTargets(), now)
		for _, d := range skipped {
			// Repeats are logged sparingly, as failed scrapes are.
			s.failures.failed(s.logger, slog.LevelWarn, failureKey(d.target.Collector, "static target "+d.target.Name, "schedule"),
				"static target scrape skipped", "schedule", errStillRunning, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
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
					s.failures.failed(s.logger, slog.LevelWarn, failureKey(d.target.Collector, "static target "+d.target.Name, "schedule"),
						"static target scrape skipped", "schedule", errNoSlot, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
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
