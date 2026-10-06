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
// configured. Its cadence then starts at the next point of it at least one
// whole interval after that first scrape was due. The first scrape has its
// interval to end in, as every scrape has, so it has ended when the cadence
// starts, and the first turn of the cadence starts on time with an interval
// of its own. Started sooner, half an interval on, the cadence's first turn
// found the first scrape of a slow target still running, waited for it, and
// was left what remained of its turn: less than the scrape takes, so a
// healthy target whose scrapes take most of their interval was cut short and
// reported down once after every start and every reload that changed it.
//
// The interval is measured from when the first scrape was due, and not from
// when the loop came to start it, a moment later. With an interval of
// firstScrapeWindow or less the first scrape is due at a point of the
// cadence itself, so the second scrape comes one interval after it. Measured
// from the moment the loop woke, always a little past that point, the next
// point was a hair less than an interval away and was passed over: every
// such target was scraped a second time only two intervals after the first,
// after every start and every reload that changed it. Only a first scrape
// that was put off, because a scrape begun on the target's old definition
// still ran, has the interval measured from when it began: that can be the
// old scrape's whole interval after it was due.
//
// A scrape is bounded by its interval, and two scrapes of a target never run
// at once. A turn that comes while the target's last scrape still runs —
// one that used its whole interval, as a target that never answers makes
// every scrape do, ends just after the next turn has come — is not given up:
// it waits for that scrape, starts as soon as it has ended, and must itself
// end by the turn after it, so the cadence stays at one scrape per interval
// and a target that answers again is seen within one. Giving the turn up
// instead scraped such a target every second interval, and logged a skipped
// scrape beside every failed one. A turn is skipped, and logged, only when it
// is really lost: the scrape it waited for had not ended when the turn after
// it came, or no scrape slot came free within the interval. That budget, less
// than an interval by the little the scrape before it ran over, is one of the
// steady cadence: the first scrape runs into the cadence only by the moment
// the loop was late in starting it.

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
	// fingerprint of its collector's definition (collectorFingerprint); a
	// reload that changes either starts the target again (plan). The
	// fingerprint is only compared, with the one the collector has in the
	// configuration planned with, and is read where the reload made it for
	// that configuration, the one the following of the reload told the
	// changed collectors by (planFollowed, followedConfig.fingerprintOf):
	// the schedule encodes no definition a second time. since is
	// the generation the target's stay had begun at when the state was made
	// (followedConfig.stay): reloads that changed either and changed it
	// back, or removed either and brought it back, between two looks of the
	// schedule leave both as the state has them, and have begun another
	// stay, which starts the target again too.
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
	// a reload changed starts a new state that shares it, so
	// a scrape begun on the old definition still keeps the next one from
	// starting beside it, and so does a target that began another stay
	// with its definition as it was (since). A target a reload removed and
	// another brought back, the schedule having looked between the two,
	// has a state that shares nothing with the one forgotten: what keeps a
	// scrape of the old one from publishing after the new one's is that it
	// publishes nothing (configRead.targetStands).
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

// plan brings the schedule in step with targets, the ones in force together
// with cfg, and returns those to scrape at now, those whose turn was lost
// because their last scrape still runs, and when the next one is due. A
// target new to the schedule, or one a reload changed — its interval, its
// address, its request, its params, or anything in the definition of its
// collector — starts again: first scraped soon, within firstScrapeWindow,
// then on a cadence of its own. A fixed address, credential or metric rule
// therefore shows within seconds rather than after the old definition's next
// turn, which for a long interval could be an hour away, with the endpoint
// serving what the old definition made until then. A target whose collector
// did not change keeps its cadence through a reload of the configuration.
// One no longer in targets is forgotten.
func (s *targetSchedule) plan(cfg *model.Config, targets []model.StaticTarget, now time.Time) (due, skipped []dueTarget, next time.Time) {
	return s.planFollowed(cfg, targets, nil, now)
}

// planFollowed is plan for the scrape loop, which reads cfg and targets as
// followed has them (Server.reconcile), and so knows from which generation
// each target has been as it is, with its collector as it is (stay). A
// target whose stay began after its state was made starts again as one a
// reload changed does, though it is defined as its state has it: reloads
// changed it, or its collector, and changed it back, or removed it and
// brought it back, since the schedule last looked. A scrape of it still in
// flight read the stay that ended and publishes nothing (targetStands), so
// without a first scrape of its own the target would be missing from the
// static targets endpoint, its http_exporter_target_up too, until its next
// turn: for a long interval, an hour. That scrape waits for the one in
// flight to end, as that of a changed target does. A reload that left the
// target and its collector as they were began no stay, and starts nothing.
func (s *targetSchedule) planFollowed(cfg *model.Config, targets []model.StaticTarget, followed *followedConfig, now time.Time) (due, skipped []dueTarget, next time.Time) {
	// A reload puts another configuration or another list in force; until
	// one does, every state is that of its target as it is, and no stay
	// began. The collectors' fingerprints are asked for only for another
	// configuration, or for a target that starts, and then once per
	// collector, however many targets share it. They are read where the
	// reload that put cfg in force made them, with followed
	// (followedConfig.fingerprintOf): the schedule encodes a definition
	// itself only when it plans with a configuration that is not the one
	// followed gives it, as a caller that follows none does (plan).
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
		if state == nil || changed && (followed.stay(target.Name, target.Collector) > state.since || !reflect.DeepEqual(state.target, target) || reloaded && state.collector != fingerprint(target.Collector)) {
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
			// The first scrape, with the whole interval to end in: the
			// cadence takes over a whole interval on or later, when this
			// scrape has ended, so its first turn waits for nothing and has
			// a whole interval too. The interval is counted from when the
			// scrape was due, which now is always a little past: counted
			// from now, the point of the cadence one interval after a first
			// scrape made at such a point was never far enough. A scrape
			// that was put off began when it is made, and so did one the
			// loop came to more than a tenth of the interval late, as
			// after a stall of the process: counted from a time that far
			// back, the cadence would start while this scrape still ran,
			// or in the past.
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
			// The turn's scrape starts. It has the whole interval when it
			// starts as its turn comes, as it nearly always does; one that
			// had to wait for the last scrape to end has what is left of
			// its turn, so it ends before the turn after it and the
			// cadence holds. The scrape waited for ran over its turn by
			// little: one of the cadence, or the first scrape, by as much
			// as the loop was late in starting it.
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
// ends, no scrape starts, and a scrape still waiting for a slot gives up,
// without a word of advice about the file's concurrency; the scrapes in
// flight go on, under their own context, and the loop waits for them. When
// that wait has to end — --web.shutdown-timeout running out —
// AbortStaticScrapes cancels them. A scrape cut short that way publishes
// nothing and logs no failure: the target did not fail, the exporter stopped,
// and its last result stands. Without this every restart published each
// target in flight as down, and sent that over OTLP on the way out.

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

// slotWaitHook, set by tests, is called with a target's name as its scrape
// begins to wait for a slot, so a test can stop the loop while one waits.
var slotWaitHook atomic.Pointer[func(string)]

// turnSkipped logs the turn the schedule gave up for d, a target read in
// force with its collector at generation. Repeats are logged sparingly, as
// failed scrapes are, and the turn is held to what was read, as the scrape
// that finds no slot is (logStands). The loop reports the turn in the look
// that read generation, where only a reload made in that very instant has
// retired the target: what the turn leaves once one has is shown by a call
// with a generation read before a reload.
func (s *Server) turnSkipped(generation uint64, d dueTarget) {
	s.failures.failedFor(s.readTargetAt(generation, d.target.Name), s.logger, slog.LevelWarn, staticTargetKey(d.target.Collector, d.target.Name).aspect(scheduleAspect),
		"static target scrape skipped", "schedule", errStillRunning, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
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
	// ended wakes the loop when a scrape the schedule waits for ends, so
	// the turn waiting for it starts then rather than at the next check.
	ended := make(chan struct{}, 1)
	for {
		// The configuration and the targets are read once, as one reload
		// left them, and each scrape uses the configuration its target was
		// read with: read apart, a reload between the two could give a
		// scrape a collector its target was not checked against. generation
		// says when: a scrape that begins after a reload removed its
		// collector does not count in a collector brought back under the name
		// (statsSince).
		followed := s.followedInForce()
		cfg, file, generation := followed.config, followed.targets, followed.generation
		if limit := file.ScrapeConcurrency(); slots == nil || cap(slots) != limit {
			slots = make(chan struct{}, limit)
		}
		now := time.Now()
		due, skipped, next := schedule.planFollowed(cfg, staticTargetsOf(file), followed, now)
		for _, d := range skipped {
			s.turnSkipped(generation, d)
		}
		for _, d := range due {
			d.state.running.Store(true)
			wg.Add(1)
			// Each scrape frees the slot of the limit it took one under.
			slots := slots
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
					s.failures.failedFor(s.readTargetAt(generation, d.target.Name), s.logger, slog.LevelWarn, staticTargetKey(d.target.Collector, d.target.Name).aspect(scheduleAspect),
						"static target scrape skipped", "schedule", errNoSlot, "target", d.target.Name, "collector", d.target.Collector, "interval", d.state.interval.String())
					return
				}
				defer func() { <-slots }()
				// A slot free as the loop stops leaves the select above two
				// ways to go, and it takes either: the scrape that got the
				// slot is not begun any more than the one that saw the stop.
				if ctx.Err() != nil {
					return
				}
				// A scrape that starts ends a run of skipped ones.
				s.failures.recoveredFor(s.readTargetAt(generation, d.target.Name), s.logger, staticTargetKey(d.target.Collector, d.target.Name).aspect(scheduleAspect),
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
