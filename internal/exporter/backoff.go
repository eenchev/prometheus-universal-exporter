package exporter

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A target that is down fails every scrape, and each of those scrapes goes
// to it all the same: a connection that hangs until its timeout holds a
// trip slot of the collector (max_concurrent_probes) and of the exporter
// (--probe.max-concurrent) for the whole of it, so a handful of dead
// endpoints can keep the slots full and turn away the probes of targets
// that answer. failure_backoff stops contacting such a target for a while:
// after `after` consecutive failed trips, its probes and static target
// scrapes fail at once, as a failed fetch, for `initial`, the wait doubling
// with each further failure up to `max`.
//
// A trip failed for backoff's purposes when its fetch did: the connection,
// a timeout, TLS, a status the collector does not accept, a request type's
// own fetch error. A target that answered with something the decoder or the
// transform could not read is up, and its trips go on. Nothing is known of
// the target from a trip the target policy refused, one a trip limit turned
// away before it started, or one cancelled because every probe waiting for
// it went away or the exporter shut down: such a trip changes nothing here.
//
// Nor from a trip that ran out of a budget below min_budget. A probe's
// budget comes from its caller, from Prometheus's scrape timeout header,
// which is no part of the key a target is told apart by: without the floor,
// a caller asking for less time than the target needs to answer would back
// the target off for every probe and static target scrape sharing its key.
// A target that does not answer at all still backs off, since the budgets
// Prometheus gives are above the floor. Failures that are not the deadline's
// — a refused connection, TLS, a status — count whatever the budget.
//
// A skipped trip fails in the fetch stage, under error_handling
// on_fetch_error and with cache.stale_if_error answering for it, exactly as
// the fetch it stands for would have; it takes no trip slot, and is counted
// in http_exporter_backoff_skipped_total. When the wait has passed, the
// next trip goes to the target alone: one trip at a time, so that a wait
// running out does not let every probe waiting for it through at once. A
// probe identical to it shares it as any probe shares a trip in flight
// (probeflight.go); any other, such as one of a collector with coalesce:
// false, fails at once as during the wait. Its failure doubles the wait,
// and its success, or any trip's, forgets the target.
//
// A target is told apart as the response cache and the shared trips tell
// probes apart (probeCacheKey): by the collector's definition, the target,
// the probe parameters and the forwarded headers, so probes with different
// param_ values back off apart, and a static target and the identical probe
// share their state. Probe targets come from the query string, so the
// targets remembered are bounded per collector (maxBackoffTargets), the one
// that failed longest ago forgotten first, and a target is forgotten as
// soon as a trip to it succeeds. A reload that removes or changes a
// collector forgets its targets with its other state (reconcile.go).
//
// A debug run neither asks nor tells: it always contacts the target, its
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
}

func newFailureBackoffs() *failureBackoffs {
	return &failureBackoffs{collectors: map[string]*collectorBackoffs{}, now: time.Now, limit: maxBackoffTargets}
}

// backoffTurn is what admit decided for one trip: refusal, when the trip
// must not contact the target, and otherwise what settle needs to record its
// outcome. A nil turn is a trip of a collector without failure_backoff, or
// without a key, and records nothing.
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
}

// backoffError is the failure of a skipped trip. Its text names the number
// of failures and the time of the next try, both of which move from one
// skip to the next: the failure log recognises it by the target alone
// (model.SameFailureAs), so its repeats are one failure.
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
		turn.refusal = backoffError(display, target.failures, target.until, true)
	case b.now().Before(target.until):
		turn.refusal = backoffError(display, target.failures, target.until, false)
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

// outcomeOf is what the trip that ended as r says of its target, for a
// collector whose min_budget is floor: a trip that ran out of a budget
// below it says nothing (a budget of 0 is no bound the caller set, and its
// timeout counts).
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

// settle records the outcome of the trip admit let through, for a trip
// that read its collector as read says: nothing, once a reload has removed
// or changed the collector since, whose targets were forgotten with it and
// must not be remembered again for a definition that is gone. started is
// set when this failure is the one that makes the target back off, with
// the failures and the end of the wait for the log. A turn is settled once;
// a later call does nothing, so a caller can defer one with backoffUnknown
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

// forgetLocked forgets the targets of the collectors named, under mu.
func (b *failureBackoffs) forgetLocked(collectors map[string]bool) {
	for name := range collectors {
		delete(b.collectors, name)
	}
}
