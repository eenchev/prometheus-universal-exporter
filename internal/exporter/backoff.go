package exporter

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A target that is down fails every scrape, yet each of those scrapes still
// goes to it. A connection that hangs until its timeout holds a trip slot of
// the collector (max_concurrent_probes) and of the exporter
// (--probe.max-concurrent) the whole time. So a handful of dead endpoints
// can keep the slots full and turn away the probes of targets that answer.
// failure_backoff stops contacting such a target for a while. After `after`
// consecutive failed trips, its probes and static target scrapes fail at
// once, as a failed fetch, for `initial`. The wait doubles with each further
// failure, up to `max`.
//
// For backoff, a trip failed when its fetch failed: the connection, a
// timeout, TLS, a status the collector does not accept, or a request type's
// own fetch error. A target that answered with something the decoder or the
// transform could not read is up, and its trips go on. These trips tell
// nothing about the target and change nothing here:
//
//   - a trip the target policy refused;
//   - a trip a trip limit turned away before it started;
//   - a trip cancelled because every probe waiting for it went away or the
//     exporter shut down.
//
// The same holds for a trip that ran out of a budget below min_budget. A
// probe's budget comes from its caller, through Prometheus's scrape timeout
// header, which is not part of the key that identifies a target. Without
// the floor, a caller asking for less time than the target needs to answer
// would back off the target for every probe and static target scrape that
// shares its key. A target that does not answer at all still backs off,
// since the budgets Prometheus gives are above the floor. Failures other
// than the deadline (a refused connection, TLS, a status) count whatever
// the budget.
//
// A skipped trip fails in the fetch stage, exactly as the fetch it stands
// for would have: under error_handling on_fetch_error, with
// cache.stale_if_error answering for it, but not when that fetch was
// refused its credential, which no stale result answers. It takes no trip
// slot and is counted in http_exporter_backoff_skipped_total.
//
// When the wait has passed, the next trip goes to the target alone, one trip
// at a time, so the end of a wait does not let every waiting probe through
// at once. A probe identical to it shares it, as any probe shares a trip in
// flight (probeflight.go). Any other probe, such as one of a collector with
// coalesce: false, fails at once as during the wait. If this trip fails,
// the wait doubles. If it or any other trip succeeds, the target is
// forgotten.
//
// A target is identified the way the response cache and the shared trips
// identify probes (probeCacheKey): by the collector's definition, the
// target, the probe parameters and the forwarded headers. So probes with
// different param_ values back off separately, and a static target and the
// identical probe share their state. Probe targets come from the query
// string, so the remembered targets are bounded per collector
// (maxBackoffTargets), and the one that failed longest ago is forgotten
// first. A target is forgotten as soon as a trip to it succeeds. A reload
// that removes or changes a collector forgets its targets along with its
// other state (reconcile.go).
//
// A debug run neither asks nor tells. It always contacts the target, its
// outcome changes nothing here, and its report says when the target is
// backing off (probedebug.go).

// maxBackoffTargets bounds the targets a collector remembers failures of.
const maxBackoffTargets = 10000

// failureBackoffs is every collector's backoff state.
type failureBackoffs struct {
	mu         sync.Mutex
	collectors map[string]*collectorBackoffs
	// now is the clock the waits are read by; tests replace it.
	now func() time.Time
	// limit bounds each collector's targets: maxBackoffTargets, but for
	// tests.
	limit int
}

// collectorBackoffs is one collector's targets that have failed, by key,
// and the same in the order they last failed, the least recent first.
type collectorBackoffs struct {
	targets map[string]*list.Element
	order   list.List
}

// backoffTarget is a target that has failed its last trips.
type backoffTarget struct {
	key string
	// failures counts its consecutive failed trips.
	failures int
	// until is when its next trip may go to it, zero before it has failed
	// `after` times.
	until time.Time
	// trial is set while the trip let through when the wait had passed is
	// in progress.
	trial bool
	// unauthorized says its last failed trip was the target refusing the
	// credential: the trips kept from it meanwhile stand for that failure,
	// and are no more answered with a stale result than it was.
	unauthorized bool
}

func newFailureBackoffs() *failureBackoffs {
	return &failureBackoffs{collectors: map[string]*collectorBackoffs{}, now: time.Now, limit: maxBackoffTargets}
}

// backoffTurn is what admit decided for one trip: refusal when the trip must
// not contact the target, and otherwise what settle needs to record the
// outcome. A nil turn belongs to a trip of a collector without
// failure_backoff, or without a key, and records nothing.
type backoffTurn struct {
	b         *failureBackoffs
	collector string
	config    *model.FailureBackoffConfig
	key       string
	// trial is the target's entry when this trip is the one let through
	// after the wait.
	trial   *backoffTarget
	refusal error
	settled bool
	// unauthorized is, for a refused turn, whether the failure it stands
	// for was a refused credential, and for one let through, set by its
	// caller before settle, whether its own failure was.
	unauthorized bool
}

// backoffError is the failure of a skipped trip. Its text names the number
// of failures and the time of the next try, and both change from one skip
// to the next. The failure log recognises it by the target alone
// (model.SameFailureAs), so its repeats count as one failure.
func backoffError(display string, failures int, until time.Time, trial bool) error {
	if display == "" {
		// A localfile collector reading its own path has no target named.
		display = "the target"
	}
	var err error
	if trial {
		err = fmt.Errorf("%s is backing off after %d consecutive failed scrapes; its next try is in progress", display, failures)
	} else {
		err = fmt.Errorf("%s is backing off after %d consecutive failed scrapes; next try after %s", display, failures, until.UTC().Format(time.RFC3339))
	}
	return model.SameFailureAs(err, display+" is backing off after consecutive failed scrapes")
}

// admit decides whether a trip of collector c to the target key, shown as
// display, may contact it. It is nil for a collector without
// failure_backoff and for a trip without a key.
func (b *failureBackoffs) admit(c *model.Collector, key, display string) *backoffTurn {
	if c.FailureBackoff == nil || key == "" {
		return nil
	}
	turn := &backoffTurn{b: b, collector: c.Name, config: c.FailureBackoff, key: key}
	b.mu.Lock()
	defer b.mu.Unlock()
	targets := b.collectors[c.Name]
	if targets == nil {
		return turn
	}
	element := targets.targets[key]
	if element == nil {
		return turn
	}
	target := element.Value.(*backoffTarget)
	switch {
	case target.until.IsZero():
		// Not yet failed `after` times: every trip goes to the target.
	case target.trial:
		turn.refusal, turn.unauthorized = backoffError(display, target.failures, target.until, true), target.unauthorized
	case b.now().Before(target.until):
		turn.refusal, turn.unauthorized = backoffError(display, target.failures, target.until, false), target.unauthorized
	default:
		target.trial = true
		turn.trial = target
	}
	return turn
}

// backoffOutcome is what a trip says of its target.
type backoffOutcome int

const (
	// backoffUnknown is a trip that says nothing of the target: refused,
	// turned away, cancelled, skipped, or never made.
	backoffUnknown backoffOutcome = iota
	backoffFailed
	backoffSucceeded
)

// outcomeOf is what the trip that ended as r says about its target, for a
// collector whose min_budget is floor. A trip that ran out of a budget
// below floor says nothing. A budget of 0 is not a bound the caller set, so
// its timeout counts.
func outcomeOf(r collected, floor time.Duration) backoffOutcome {
	switch {
	case r.aborted, r.refused, r.skipped:
		return backoffUnknown
	case r.fetchFailed && r.outOfBudget && r.budget > 0 && r.budget < floor:
		return backoffUnknown
	case r.fetchFailed:
		return backoffFailed
	}
	return backoffSucceeded
}

// settle records the outcome of the trip that admit let through; read is
// how that trip read its collector. If a reload has since removed or
// changed the collector, settle records nothing: its targets were forgotten
// with it and must not be remembered again for a definition that is gone.
// started is set when this failure makes the target back off, together with
// the failures and the end of the wait for the log. A turn is settled once
// and later calls do nothing, so a caller can defer one with backoffUnknown
// for a trip that panicked or was turned away.
func (t *backoffTurn) settle(read configRead, outcome backoffOutcome) (started bool, failures int, until time.Time) {
	if t == nil || t.settled || t.refusal != nil {
		return false, 0, time.Time{}
	}
	t.settled = true
	b := t.b
	b.mu.Lock()
	defer b.mu.Unlock()
	targets := b.collectors[t.collector]
	var element *list.Element
	if targets != nil {
		element = targets.targets[t.key]
	}
	var target *backoffTarget
	if element != nil {
		target = element.Value.(*backoffTarget)
	}
	// The trial is over whatever came of it. An entry that is not the one
	// the trial was let through by, forgotten by a reload or a success
	// meanwhile, is not touched.
	if t.trial != nil {
		t.trial.trial = false
	}
	if outcome == backoffUnknown || !read.stands(t.collector) {
		return false, 0, time.Time{}
	}
	if outcome == backoffSucceeded {
		if element != nil {
			targets.order.Remove(element)
			delete(targets.targets, t.key)
			if len(targets.targets) == 0 {
				delete(b.collectors, t.collector)
			}
		}
		return false, 0, time.Time{}
	}
	if targets == nil {
		targets = &collectorBackoffs{targets: map[string]*list.Element{}}
		b.collectors[t.collector] = targets
	}
	if element == nil {
		limit := b.limit
		if limit <= 0 {
			limit = maxBackoffTargets
		}
		for len(targets.targets) >= limit {
			oldest := targets.order.Front()
			targets.order.Remove(oldest)
			delete(targets.targets, oldest.Value.(*backoffTarget).key)
		}
		target = &backoffTarget{key: t.key}
		element = targets.order.PushBack(target)
		targets.targets[t.key] = element
	} else {
		targets.order.MoveToBack(element)
	}
	target.failures++
	target.unauthorized = t.unauthorized
	after := *t.config.After
	if target.failures < after {
		return false, 0, time.Time{}
	}
	now := b.now()
	target.until = now.Add(backoffWait(t.config, target.failures-after))
	return target.failures == after, target.failures, target.until
}

// backoffWait is the wait after the failure that came doublings failures
// after the one that started the backoff: initial, doubled that many times,
// and no longer than max.
func backoffWait(c *model.FailureBackoffConfig, doublings int) time.Duration {
	wait, longest := time.Duration(*c.Initial), time.Duration(*c.Max)
	for range doublings {
		if wait >= longest/2 {
			return longest
		}
		wait *= 2
	}
	return min(wait, longest)
}

// backingOff counts the collector's targets whose trips are kept from them
// now: those within their wait, and those whose trip after it is in
// progress.
func (b *failureBackoffs) backingOff(collector string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	targets := b.collectors[collector]
	if targets == nil {
		return 0
	}
	now, n := b.now(), 0
	for _, element := range targets.targets {
		if target := element.Value.(*backoffTarget); target.trial || !target.until.IsZero() && now.Before(target.until) {
			n++
		}
	}
	return n
}

// describe says, for a debug run's report, whether the target key of the
// collector is backing off now: empty when it is not.
func (b *failureBackoffs) describe(collector, key string) string {
	if key == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	targets := b.collectors[collector]
	if targets == nil {
		return ""
	}
	element := targets.targets[key]
	if element == nil {
		return ""
	}
	target := element.Value.(*backoffTarget)
	switch {
	case target.trial:
		return fmt.Sprintf("The target is backing off after %d consecutive failed scrapes (failure_backoff), and its next try is in progress.", target.failures)
	case !target.until.IsZero() && b.now().Before(target.until):
		return fmt.Sprintf("The target is backing off after %d consecutive failed scrapes (failure_backoff): probes and scrapes fail without contacting it until %s.", target.failures, target.until.UTC().Format(time.RFC3339))
	}
	return ""
}

// backoffState is what failure_backoff holds about one target, for the
// status page: its consecutive failed trips, and whether its trips are held
// back now, and until when, or because the trip let through after the wait
// is in progress.
type backoffState struct {
	failures   int
	backingOff bool
	until      time.Time
	trial      bool
}

// state is what is held of the target key of the collector: the zero state
// when nothing is, as for a target whose last trip succeeded.
func (b *failureBackoffs) state(collector, key string) backoffState {
	if key == "" {
		return backoffState{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	targets := b.collectors[collector]
	if targets == nil {
		return backoffState{}
	}
	element := targets.targets[key]
	if element == nil {
		return backoffState{}
	}
	target := element.Value.(*backoffTarget)
	return backoffState{
		failures:   target.failures,
		backingOff: target.trial || !target.until.IsZero() && b.now().Before(target.until),
		until:      target.until,
		trial:      target.trial,
	}
}

// forgetLocked forgets the targets of the collectors named, under mu.
func (b *failureBackoffs) forgetLocked(collectors map[string]bool) {
	for name := range collectors {
		delete(b.collectors, name)
	}
}
