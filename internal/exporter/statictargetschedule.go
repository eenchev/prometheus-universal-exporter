package exporter

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"reflect"
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
// next one later. Its cadence is offset within its interval by a hash of its
// name, so targets sharing an interval are spread over it rather than all
// scraped at once. A target new to the schedule is first scraped sooner, within
// firstScrapeWindow, again by the hash: otherwise a target with a long interval
// would be missing from the static targets endpoint for up to that interval
// after a start or a reload, which looks the same as a target nobody
// configured. Its cadence then starts at the next point of it at least half an
// interval after that first scrape, so the two are never close together. A scrape is bounded by its interval, and one still running
// when the next is due makes that one skipped rather than overlapping it.

// scheduleCheckInterval is the longest the scheduler sleeps, so a reloaded
// target file takes effect within it.
const scheduleCheckInterval = time.Second

// firstScrapeWindow is the time over which targets new to the schedule are
// first scraped, spread by name; a target whose interval is shorter uses its
// interval.
const firstScrapeWindow = 10 * time.Second

// targetSchedule is when each static target is next due.
type targetSchedule struct {
	states map[string]*staticTargetState
}

// staticTargetState is one target's place in the schedule.
type staticTargetState struct {
	// target is the definition the state was made for; a reload that
	// changes it starts the target again (plan).
	target   model.StaticTarget
	interval time.Duration
	next     time.Time
	// cadence is where the target's regular cadence starts, until its first,
	// earlier scrape has been made; zero after.
	cadence time.Time
	// running is set while a scrape of the target is in flight. A target
	// a reload changed starts a new state that shares it, so
	// a scrape begun on the old definition still keeps the next one from
	// starting beside it, where the older could publish after the newer.
	running *atomic.Bool
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
// scrape, and when the next one is due. A target new to the schedule, or one
// a reload changed — its interval, its address, its request, its params —
// starts again: first scraped soon, within firstScrapeWindow, then on a
// cadence of its own. A fixed address or credential therefore shows within
// seconds rather than after the old definition's next turn, which for a long
// interval could be an hour away. One no longer in targets is forgotten.
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
		if state == nil || !reflect.DeepEqual(state.target, target) {
			running := &atomic.Bool{}
			if state != nil {
				running = state.running
			}
			state = &staticTargetState{
				target:   target,
				interval: interval,
				next:     now.Add(scheduleOffset(target.Name, min(interval, firstScrapeWindow))),
				cadence:  now.Add(scheduleOffset(target.Name, interval)),
				running:  running,
			}
			s.states[target.Name] = state
		}
		if !now.Before(state.next) {
			if state.running.Load() {
				skipped = append(skipped, dueTarget{target, state})
			} else {
				due = append(due, dueTarget{target, state})
			}
			if !state.cadence.IsZero() {
				// The first scrape: the cadence takes over, at least half an
				// interval on.
				state.next = state.cadence
				state.cadence = time.Time{}
				for state.next.Sub(now) < interval/2 {
					state.next = state.next.Add(interval)
				}
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
	errNoSlot       = errors.New("no scrape slot came free within the interval; other static target scrapes held them all, so raise the file's concurrency or lengthen the interval")
	// errShuttingDown is why AbortStaticScrapes cancels the scrapes in flight.
	errShuttingDown = errors.New("the exporter is shutting down")
)

// A shutdown stops the static targets in two steps. When the loop's context
// ends, no scrape starts, and a scrape still waiting for a slot gives up,
// without a word of advice about the file's concurrency; the scrapes in
// flight go on, under their own context, and the loop waits for them. When
// that wait has to end — --web.shutdown-timeout running out —
// AbortStaticScrapes cancels them. A scrape cut short that way publishes
// nothing and logs no failure: the target did not fail, the exporter stopped,
// and its last result stands. Without this every restart published each
// target in flight as down, and sent that over OTLP on the way out.

// staticScrapes is the context static target scrapes run under, and
// AbortStaticScrapes cancels it.
func (s *Server) staticScrapes() context.Context {
	s.scrapesOnce.Do(s.initStaticScrapes)
	return s.scrapesCtx
}

func (s *Server) initStaticScrapes() {
	s.scrapesCtx, s.abortScrapes = context.WithCancelCause(context.Background())
}

// AbortStaticScrapes cancels the static target scrapes in flight, as a
// shutdown whose wait has run out does. They publish nothing.
func (s *Server) AbortStaticScrapes() {
	s.scrapesOnce.Do(s.initStaticScrapes)
	s.abortScrapes(errShuttingDown)
}

// shuttingDown reports whether ctx ended because the exporter is shutting down.
func shuttingDown(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errShuttingDown)
}

// StaticScrapeLoop scrapes every static target on its interval until ctx
// ends, and then waits for the scrapes in flight, which run until they end or
// AbortStaticScrapes cancels them. The results are published for the static
// targets endpoint, and queued for the OTLP export loop for a target with
// export_via_otlp. At most the file's concurrency are scraped at once; a
// reload that changes it applies to the scrapes that start after it.
func (s *Server) StaticScrapeLoop(ctx context.Context) {
	schedule := newTargetSchedule()
	var slots chan struct{}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		if limit := s.manager.StaticTargetConcurrency(); slots == nil || cap(slots) != limit {
			slots = make(chan struct{}, limit)
		}
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
			// Each scrape frees the slot of the limit it took one under.
			slots := slots
			go func() {
				defer wg.Done()
				defer d.state.running.Store(false)
				scrapeCtx, cancel := context.WithTimeout(s.staticScrapes(), d.state.interval)
				defer cancel()
				// Waiting for a slot counts against the scrape's interval.
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					// The loop is stopping: a scrape not yet begun is not
					// begun, and is no one's failure.
					return
				case <-scrapeCtx.Done():
					if shuttingDown(scrapeCtx) {
						return
					}
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
