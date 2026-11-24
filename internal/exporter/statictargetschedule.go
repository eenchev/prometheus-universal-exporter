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

// Static targets are scraped on their own intervals, set in the static
// targets file, as Prometheus scrapes a probe on its scrape_interval. Neither
// Prometheus's scrapes of the static targets endpoint nor the OTLP export
// (which delivers on otlp.interval what the scrapes queued) decides when a
// target is scraped.
//
// Each target runs on a fixed cadence measured from its first scrape, so a
// slow scrape does not delay the next one. The cadence is offset within the
// interval by a hash of the target's name, so targets that share an interval
// are spread over it instead of all being scraped at once. A target new to
// the schedule is first scraped sooner, within firstScrapeWindow, again by
// the hash; otherwise a target with a long interval would be missing from
// the static targets endpoint for up to that interval after a start or a
// reload, which looks like a target nobody configured. The cadence then
// starts at its next point at least one whole interval after the first
// scrape was due. The first scrape, like every scrape, has its interval to
// end in, so it has ended when the cadence starts, and the cadence's first
// turn starts on time with a full interval; a slow target's first scrape is
// not cut short.
//
// The interval is measured from when the first scrape was due, not from when
// the loop actually started it, a moment later. With an interval of
// firstScrapeWindow or less, the first scrape is due at a point of the
// cadence itself, so the second scrape comes one interval after it (measured
// from when the loop woke, the next point would be just under an interval
// away and would be skipped). Only a first scrape that was delayed because a
// scrape on the target's old definition was still running has the interval
// measured from when it began.
//
// A scrape is bounded by its interval, and two scrapes of a target never run
// at once. A turn that comes while the target's last scrape is still running
// (one that used its whole interval, as every scrape of a target that never
// answers does, ends just after the next turn has come) is not given up: it
// waits for that scrape, starts as soon as it ends, and must itself end by
// the following turn. The cadence therefore stays at one scrape per interval,
// and a target that answers again is seen within one interval. A turn is
// skipped, and
// logged, only when it is really lost: the scrape it waited for had not
// ended when the following turn came, or no scrape slot became free within
// the interval. That budget, an interval minus the small overrun of the
// previous scrape, belongs to the steady cadence: the first scrape overruns
// into the cadence only by how late the loop was in starting it.

// scheduleCheckInterval is the longest the scheduler sleeps, so a reloaded
// target file takes effect within it.
const scheduleCheckInterval = time.Second

// firstScrapeWindow is the time over which targets new to the schedule are
// first scraped, spread by name; a target whose interval is shorter uses its
// interval.
const firstScrapeWindow = 10 * time.Second

// lateFirstScrapeShare is the share of its interval, one in so many, by
// which the loop may be late in making a target's first scrape and the
// cadence still be counted from when that scrape was due. A loop that sleeps
// until a time wakes a little after it, far inside this; one later than
// this was held up, and the cadence is counted from when the scrape began,
// so that it has ended, within its interval, when the cadence starts.
const lateFirstScrapeShare = 10

// targetSchedule is when each static target is next due.
type targetSchedule struct {
	states map[string]*staticTargetState
	// config and targets are what the schedule was last brought in step
	// with. Neither changes once in force, so while a plan is given the
	// same ones no target is compared with its state again.
	config  *model.Config
	targets []model.StaticTarget
}

// staticTargetState is one target's place in the schedule.
type staticTargetState struct {
	// target is the definition the state was made for, and collector the
	// fingerprint of its collector's definition (collectorFingerprint). A
	// reload that changes either restarts the target (plan). The fingerprint
	// is only compared with the collector's fingerprint in the configuration
	// planned with, read where the reload computed it for that configuration
	// (planFollowed, followedConfig.fingerprintOf), so the schedule never
	// encodes a definition a second time. since is the generation at which
	// the target's stay had begun when the state was made
	// (followedConfig.stay): reloads that changed either and changed it
	// back, or removed either and restored it, between two examinations of
	// the schedule leave both unchanged, but begin another stay, which also
	// restarts the target.
	target    model.StaticTarget
	collector string
	since     uint64
	interval  time.Duration
	next      time.Time
	// cadence is where the target's regular cadence starts, until its first,
	// earlier scrape has been made; zero after. putOff says that first
	// scrape came due while a scrape of the target still ran, and waits for
	// it.
	cadence time.Time
	putOff  bool
	// running is set while a scrape of the target is in flight. A target
	// that a reload changed starts a new state that shares it, so a scrape
	// begun on the old definition still prevents the next one from starting
	// beside it, and so does a target that began another stay with an
	// unchanged definition (since). A target that a reload removed and
	// another restored, with the schedule examined in between, gets a state
	// that shares nothing with the forgotten one; a scrape of the old one
	// cannot publish after the new one's because it publishes nothing
	// (configRead.targetStands).
	running *atomic.Bool
	// awaited, shared as running is, is set when the schedule waits for
	// the scrape in flight to end, so the loop is woken when it does.
	awaited *atomic.Bool
	// waiting is set while a turn of the cadence has come and its scrape
	// has not started, because the last scrape still runs; until is when
	// the scrape of that turn must end, the turn after it.
	waiting bool
	until   time.Time
}

// awaitScrape reports whether a scrape of the target still runs, and if so
// asks for the loop to be woken when it ends (scrapeEnded). It looks again
// after asking: a scrape that ended in between has not seen the request.
func (st *staticTargetState) awaitScrape() bool {
	if !st.running.Load() {
		return false
	}
	st.awaited.Store(true)
	return st.running.Load()
}

// scrapeEnded marks the target's scrape as ended, and reports whether the
// schedule waits for that.
func (st *staticTargetState) scrapeEnded() bool {
	st.running.Store(false)
	return st.awaited.Swap(false)
}

// dueTarget is a target to scrape now, with its state, the configuration
// that was in force with it, which its scrape uses, and when the scrape must
// end. For a skipped one only the target and the state are set.
type dueTarget struct {
	target   model.StaticTarget
	state    *staticTargetState
	config   *model.Config
	deadline time.Time
}

func newTargetSchedule() *targetSchedule {
	return &targetSchedule{states: map[string]*staticTargetState{}}
}

// plan brings the schedule in line with targets, the ones in force with cfg,
// and returns the targets to scrape at now, those whose turn was lost
// because their last scrape is still running, and when the next one is due.
// A target new to the schedule, or one that a reload changed (its interval,
// address, request, params, or anything in its collector's definition),
// restarts: first scraped soon, within firstScrapeWindow, then on its own
// cadence. A fixed address, credential or metric rule therefore takes effect
// within seconds, not at the old definition's next turn, which for a long
// interval could be an hour away, with the endpoint serving the old
// definition's results until then. A target whose collector did not change
// keeps its cadence through a configuration reload. A target no longer in
// targets is forgotten.
func (s *targetSchedule) plan(cfg *model.Config, targets []model.StaticTarget, now time.Time) (due, skipped []dueTarget, next time.Time) {
	return s.planFollowed(cfg, targets, nil, now)
}

// planFollowed is plan for the scrape loop, which reads cfg and targets as
// followed holds them (Server.reconcile), and so knows the generation since
// which each target and its collector have been unchanged (stay). A target
// whose stay began after its state was made restarts like a changed one,
// even though its definition matches its state: since the schedule last
// looked, reloads changed it or its collector and changed it back, or
// removed it and restored it. A scrape of it still in flight read the ended
// stay and publishes nothing (targetStands), so without its own first scrape
// the target, including its http_exporter_target_up, would be missing from
// the static targets endpoint until its next turn, an hour for a long
// interval. That scrape waits for the one in flight to end, as a changed
// target's does. A reload that left the target and its collector unchanged
// began no stay and restarts nothing.
func (s *targetSchedule) planFollowed(cfg *model.Config, targets []model.StaticTarget, followed *followedConfig, now time.Time) (due, skipped []dueTarget, next time.Time) {
	// Only a reload puts another configuration or list in force; until then,
	// every state matches its target and no stay began. The collectors'
	// fingerprints are requested only for another configuration or for a
	// target that restarts, and then once per collector, however many targets
	// share it. They are read where the reload that put cfg in force computed
	// them, through followed (followedConfig.fingerprintOf). The schedule
	// encodes a definition itself only when it plans with a configuration
	// other than followed's, as a caller that follows none does (plan).
	reloaded := cfg != s.config
	changed := reloaded || len(targets) != len(s.targets) || len(targets) > 0 && &targets[0] != &s.targets[0]
	s.config, s.targets = cfg, targets
	fingerprints := map[string]string{}
	fingerprint := func(collector string) string {
		value, known := fingerprints[collector]
		if !known {
			if cfg != nil {
				value = followed.fingerprintOf(cfg, collector)
			}
			fingerprints[collector] = value
		}
		return value
	}
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
		if state == nil || changed && (followed.stay(target.Name, target.Collector) > state.since || !model.SameStaticTarget(state.target, target) || reloaded && state.collector != fingerprint(target.Collector)) {
			running, awaited := &atomic.Bool{}, &atomic.Bool{}
			if state != nil {
				running, awaited = state.running, state.awaited
			}
			state = &staticTargetState{
				target:    target,
				collector: fingerprint(target.Collector),
				since:     followed.stay(target.Name, target.Collector),
				interval:  interval,
				next:      now.Add(scheduleOffset(target.Name, min(interval, firstScrapeWindow))),
				cadence:   now.Add(scheduleOffset(target.Name, interval)),
				running:   running,
				awaited:   awaited,
			}
			s.states[target.Name] = state
		}
		// look is when the target is to be looked at again; came says a
		// turn of its cadence came at this very look.
		look, came := state.next, false
		switch {
		case now.Before(state.next):
		case !state.cadence.IsZero() && state.awaitScrape():
			// The first scrape of a target a reload changed, due while a
			// scrape begun on its old definition still runs: it is not
			// skipped, which would put it off to its cadence, but stays
			// due, and is made as soon as that scrape ends: the loop
			// plans again when it does, and at every check.
			look, state.putOff = now.Add(scheduleCheckInterval), true
		case !state.cadence.IsZero():
			// The first scrape, with the whole interval to end in. The
			// cadence takes over at least a whole interval later, when this
			// scrape has ended, so its first turn waits for nothing and also
			// gets a whole interval. The interval is counted from when the
			// scrape was due, which is always slightly before now: counted
			// from now, the cadence point one interval after a first scrape
			// made at such a point was never far enough. A scrape that was
			// delayed begins when it is made, and so does one that the loop
			// reached more than a tenth of the interval late, as after a
			// process stall: counted from a time that far back, the cadence
			// would start while this scrape still ran, or in the past.
			began := state.next
			if state.putOff || now.Sub(state.next) > interval/lateFirstScrapeShare {
				began = now
			}
			due = append(due, dueTarget{target: target, state: state, config: cfg, deadline: now.Add(interval)})
			state.next = state.cadence
			state.cadence = time.Time{}
			for state.next.Sub(began) < interval {
				state.next = state.next.Add(interval)
			}
			look = state.next
		default:
			// A turn of the cadence. One that still waits from the turn
			// before is lost: the scrape it waited for outlasted it.
			if state.waiting {
				skipped = append(skipped, dueTarget{target: target, state: state})
			}
			for !state.next.After(now) {
				state.next = state.next.Add(interval)
			}
			look, came = state.next, true
			state.waiting, state.until = true, state.next
		}
		if state.waiting && !state.awaitScrape() {
			// The turn's scrape starts. Starting when its turn comes, as it
			// nearly always does, it has the whole interval. One that had to
			// wait for the last scrape to end has the rest of its turn, so it
			// ends before the following turn and the cadence holds. The
			// scrape it waited for overran its turn only slightly: one of the
			// cadence, or the first scrape, by how late the loop was in
			// starting it.
			deadline := state.until
			if came {
				deadline = now.Add(interval)
			}
			state.waiting = false
			due = append(due, dueTarget{target: target, state: state, config: cfg, deadline: deadline})
		}
		if next.IsZero() || look.Before(next) {
			next = look
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
	errStillRunning = errors.New("the scrape before it was still running when the turn after it came, so it was never started; a scrape is ended with its interval, so the one still running is held by something its deadline does not end, or the exporter is short of CPU")
	errNoSlot       = errors.New("no scrape slot came free within the interval; other static target scrapes held them all, so raise the file's concurrency or lengthen the interval")
	// errShuttingDown is why AbortStaticScrapes cancels the scrapes in flight.
	errShuttingDown = errors.New("the exporter is shutting down")
)

// A shutdown stops the static targets in two steps. When the loop's context
// ends, no scrape starts, and a scrape still waiting for a slot gives up
// without logging advice about the file's concurrency. The scrapes in flight
// continue under their own context, and the loop waits for them.
// AbortStaticScrapes cancels them, which the exporter's shutdown does
// immediately (main.go): what a scrape would publish is served only by this
// process, which is stopping, so a target that does not answer would only
// delay the shutdown for --web.shutdown-timeout. A scrape cut short this way
// publishes nothing and logs no failure: the target did not fail, the
// exporter stopped, and the target's last result stands.

// staticTargetsOf is the targets of a static target file, none without one.
func staticTargetsOf(file *model.StaticTargetFile) []model.StaticTarget {
	if file == nil {
		return nil
	}
	return file.Targets
}

// staticScrapes is the context static target scrapes run under, and
// AbortStaticScrapes cancels it.
func (s *Server) staticScrapes() context.Context {
	s.scrapesOnce.Do(s.initStaticScrapes)
	return s.scrapesCtx
}

func (s *Server) initStaticScrapes() {
	s.scrapesCtx, s.abortScrapes = context.WithCancelCause(context.Background())
}

// AbortStaticScrapes cancels the static target scrapes in flight, as the
// shutdown does as it begins. They publish nothing.
func (s *Server) AbortStaticScrapes() {
	s.scrapesOnce.Do(s.initStaticScrapes)
	s.abortScrapes(errShuttingDown)
}

// shuttingDown reports whether ctx ended because the exporter is shutting down.
func shuttingDown(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errShuttingDown)
}

// slotWaitHook, set by tests, is called with a target's name as its scrape
// begins to wait for a slot, so a test can stop the loop while one waits.
var slotWaitHook atomic.Pointer[func(string)]

// turnSkipped logs the turn that the schedule gave up for d, a target read
// in force with its collector at generation. Repeats are logged sparingly,
// like failed scrapes, and the turn is held to what was read, like a scrape
// that finds no slot (logStands). The loop reports the turn in the
// examination that read generation, where only a reload made at that very
// instant can have retired the target; what the turn leaves after such a
// reload is shown by a call with a generation read before the reload.
func (s *Server) turnSkipped(generation uint64, d dueTarget) {
	s.failures.failedFor(context.Background(), s.readTargetAt(generation, d.target.Name), s.logger, slog.LevelWarn, staticTargetKey(d.target.Collector, d.target.Name).aspect(scheduleAspect),
		"static target scrape skipped", "schedule", errStillRunning, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
}

// staticCollectors is the number of collectors targets name.
func staticCollectors(targets []model.StaticTarget) int {
	names := map[string]bool{}
	for _, t := range targets {
		names[t.Collector] = true
	}
	return len(names)
}

// StaticScrapeLoop scrapes every static target on its interval until ctx
// ends, and then waits for the scrapes in flight, which run until they end
// or AbortStaticScrapes cancels them. The results are published for the
// static targets endpoint, and queued for the OTLP export loop for a target
// with export_via_otlp. At most the file's concurrency are scraped at once,
// with the slots shared between the collectors (staticslots.go); a reload
// that changes it applies to the waiting scrapes and to those that start
// after it.
func (s *Server) StaticScrapeLoop(ctx context.Context) {
	schedule := newTargetSchedule()
	slots := newStaticSlots()
	var wg sync.WaitGroup
	defer wg.Wait()
	// ended wakes the loop when a scrape the schedule waits for ends, so
	// the turn waiting for it starts then rather than at the next check.
	ended := make(chan struct{}, 1)
	for {
		// The configuration and the targets are read once, as one reload
		// left them, and each scrape uses the configuration that its target
		// was read with: read separately, a reload in between could give a
		// scrape a collector that its target was not checked against.
		// generation records when: a scrape that begins after a reload
		// removed its collector does not count in a collector restored under
		// the name (statsSince).
		followed := s.followedInForce()
		cfg, file, generation := followed.config, followed.targets, followed.generation
		targets := staticTargetsOf(file)
		slots.shape(file.ScrapeConcurrency(), staticCollectors(targets))
		now := time.Now()
		due, skipped, next := schedule.planFollowed(cfg, targets, followed, now)
		for _, d := range skipped {
			s.turnSkipped(generation, d)
		}
		for _, d := range due {
			d.state.running.Store(true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if d.state.scrapeEnded() {
						select {
						case ended <- struct{}{}:
						default:
						}
					}
				}()
				scrapeCtx, cancel := context.WithDeadline(s.staticScrapes(), d.deadline)
				defer cancel()
				if hook := slotWaitHook.Load(); hook != nil {
					(*hook)(d.target.Name)
				}
				// Waiting for a slot counts against the scrape's interval;
				// the loop's stop ends the wait too.
				waitCtx, stopWait := context.WithCancel(scrapeCtx)
				unhook := context.AfterFunc(ctx, stopWait)
				err := slots.acquire(waitCtx, d.target.Collector)
				unhook()
				stopWait()
				if err != nil {
					// The loop is stopping: a scrape not yet begun is not
					// begun, and is no one's failure.
					if ctx.Err() != nil || shuttingDown(scrapeCtx) {
						return
					}
					s.failures.failedFor(scrapeCtx, s.readTargetAt(generation, d.target.Name), s.logger, slog.LevelWarn, staticTargetKey(d.target.Collector, d.target.Name).aspect(scheduleAspect),
						"static target scrape skipped", "schedule", errNoSlot, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
					return
				}
				defer slots.release(d.target.Collector)
				// A slot free as the loop stops leaves the select above two
				// ways to go, and it takes either: the scrape that got the
				// slot is not begun any more than the one that saw the stop.
				if ctx.Err() != nil {
					return
				}
				// A scrape that starts ends a run of skipped ones.
				s.failures.recoveredFor(scrapeCtx, s.readTargetAt(generation, d.target.Name), s.logger, staticTargetKey(d.target.Collector, d.target.Name).aspect(scheduleAspect),
					"static target scrapes on schedule again", "target", d.target.Name, "collector", d.target.Collector)
				s.scrapeTargetSince(scrapeCtx, d.config, generation, d.target)
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
		case <-ended:
			timer.Stop()
		case <-timer.C:
		}
	}
}
