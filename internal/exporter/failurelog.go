package exporter

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A target that is down fails every probe, and every probe logged its failure:
// one line per scrape interval per Prometheus replica per target, the same
// line again and again, burying everything else in the log. The self-metrics
// already count every failure exactly; the log is for learning that
// something broke, what, and when it recovered.
//
// So a failure is logged in full the first time. While the same thing keeps
// failing the same way — the same collector, target (and file, for a
// directory), stage and error — the repeats are logged at debug level only,
// and at the failure's own level once every failureRepeatInterval, with how
// many times it happened since the last line and since when it has been
// failing. A different stage or error is a new failure, logged in full. The
// first success after a failure is logged at info level, with how long it
// failed and how many times. With --log.level=debug every repeat is still
// visible.
//
// The same error is the same failure, not the same text: where in the
// response it happened and what it measured are left out of what is compared
// (model.SameFailureText), so an empty cell in row 3 and then in row 7, or a
// label 612 bytes long and then 640, is one failure that keeps happening.
// What is logged is the text in full, as it is each time.

const (
	// failureRepeatInterval is how often a failure that keeps happening is
	// logged again.
	failureRepeatInterval = 5 * time.Minute
	// failureLogMaxEntries bounds the failures remembered; past it, a new one
	// is logged in full every time rather than remembered.
	failureLogMaxEntries = 10000
	// failureLogForget is how long a failure that has stopped being reported,
	// such as that of a target no longer probed, is remembered.
	failureLogForget = time.Hour
)

type failureState struct {
	// err is the text the failure is recognised by (model.SameFailureText).
	stage, err           string
	first, logged, seen  time.Time
	failures, suppressed int
}

type failureLog struct {
	mu      sync.Mutex
	entries map[string]*failureState
	// ruleFailures is, for each trip or file whose rules have failures among
	// the entries, how many: what a scrape asks before it looks for the
	// rules that recovered (remembersRules). It is kept in step where an
	// entry is made and where one is dropped (putLocked, dropLocked), and
	// nowhere else is either done.
	ruleFailures map[string]int
	interval     time.Duration
	now          func() time.Time
	lastSweep    time.Time
}

func newFailureLog() *failureLog {
	return &failureLog{entries: map[string]*failureState{}, ruleFailures: map[string]int{}, interval: failureRepeatInterval, now: time.Now}
}

// ruleKeyMarker is what stands, in the key of a rule's failure, between the
// key of the trip or file the rule failed on and what tells the rule apart
// (ruleFailureKey).
const ruleKeyMarker = "\x00rule\x00"

// ruleKeyTrip is the part of key before the marker of a rule's failure, and
// whether key has one: the trip or file the rule failed on.
func ruleKeyTrip(key string) (string, bool) {
	trip, _, rule := strings.Cut(key, ruleKeyMarker)
	return trip, rule
}

// putLocked remembers st under key, in place of what was remembered there,
// and counts a rule's failure that was not, for its trip.
func (f *failureLog) putLocked(key string, st *failureState) {
	if _, known := f.entries[key]; !known {
		if trip, rule := ruleKeyTrip(key); rule {
			if _, counted := f.ruleFailures[trip]; !counted {
				// The count's key is not left a part of the first rule's
				// key, which would be held for as long as any is counted.
				trip = strings.Clone(trip)
			}
			f.ruleFailures[trip]++
		}
	}
	f.entries[key] = st
}

// dropLocked forgets what is remembered under key, if anything is, and
// counts a rule's failure no longer, for its trip. Every entry that goes,
// goes through here, so the counts are those of the entries there are.
func (f *failureLog) dropLocked(key string) {
	if _, known := f.entries[key]; !known {
		return
	}
	delete(f.entries, key)
	if trip, rule := ruleKeyTrip(key); rule {
		if f.ruleFailures[trip]--; f.ruleFailures[trip] <= 0 {
			delete(f.ruleFailures, trip)
		}
	}
}

// failureKey identifies what failed. file is empty but for a directory's file.
func failureKey(collector, target, file string) string {
	return collector + "\x00" + target + "\x00" + file
}

// failed logs a failure at level, or counts it as a repeat.
func (f *failureLog) failed(logger *slog.Logger, level slog.Level, key, msg, stage string, err error, attrs ...any) {
	f.failedFor(configRead{}, logger, level, key, msg, stage, err, attrs...)
}

// failedFor is failed for a trip that read its collector as read says. When
// the collector no longer stands (configRead) — a reload removed it, or
// changed its definition, since — the failure is of a collector that is gone:
// it is remembered nowhere, so it is neither taken for the first failure of
// the collector now under the name nor ends a run of that one's, and is
// logged at debug level only, as superseded. So is the failure of a static
// target's scrape whose target a reload removed or changed since
// (logStands).
func (f *failureLog) failedFor(read configRead, logger *slog.Logger, level slog.Level, key, msg, stage string, err error, attrs ...any) {
	errText := ""
	if err != nil {
		errText = model.SameFailureText(err)
		attrs = append(attrs, "error", err)
	}
	now := f.now()
	f.mu.Lock()
	if !read.logStands(keyCollector(key)) {
		f.mu.Unlock()
		logger.Log(context.Background(), slog.LevelDebug, msg, append(attrs, "superseded", true)...)
		return
	}
	f.sweepLocked(now)
	st := f.entries[key]
	// A failure not reported for failureLogForget is forgotten: the same
	// failure again, days later, is a new one, not a repeat failing since.
	if st != nil && now.Sub(st.seen) > failureLogForget {
		f.dropLocked(key)
		st = nil
	}
	if st == nil || st.stage != stage || st.err != errText {
		if st != nil || f.rememberLocked() {
			f.putLocked(key, &failureState{stage: stage, err: errText, first: now, logged: now, seen: now, failures: 1})
		}
		f.mu.Unlock()
		logger.Log(context.Background(), level, msg, attrs...)
		return
	}
	st.failures++
	st.seen = now
	if now.Sub(st.logged) < f.interval {
		st.suppressed++
		f.mu.Unlock()
		logger.Log(context.Background(), slog.LevelDebug, msg, append(attrs, "repeat", true)...)
		return
	}
	repeated, since := st.suppressed+1, st.first
	st.suppressed, st.logged = 0, now
	f.mu.Unlock()
	logger.Log(context.Background(), level, msg, append(attrs, "repeated", repeated, "failing_since", since.UTC().Format(time.RFC3339))...)
}

// rememberLocked reports whether there is room for a new entry.
func (f *failureLog) rememberLocked() bool {
	return len(f.entries) < failureLogMaxEntries
}

// sweepLocked forgets the failures not reported for failureLogForget — a
// target no longer probed — at most once a minute, whether or not the log is
// full.
func (f *failureLog) sweepLocked(now time.Time) {
	if now.Sub(f.lastSweep) < time.Minute {
		return
	}
	f.lastSweep = now
	for k, st := range f.entries {
		if now.Sub(st.seen) > failureLogForget {
			f.dropLocked(k)
		}
	}
}

// recovered logs the first success after a failure, and forgets the failure.
// One forgotten already, not reported for failureLogForget, is not logged
// as recovering.
func (f *failureLog) recovered(logger *slog.Logger, key, msg string, attrs ...any) {
	f.recoveredFor(configRead{}, logger, key, msg, attrs...)
}

// recoveredFor is recovered for a trip that read its collector as read says:
// the success of a collector that no longer stands (configRead) is no
// recovery of the collector now under the name, whose failure stays
// remembered, and neither is the success of a static target's scrape whose
// target no longer stands (logStands).
func (f *failureLog) recoveredFor(read configRead, logger *slog.Logger, key, msg string, attrs ...any) {
	f.mu.Lock()
	st := f.entries[key]
	// Asked only when there is a failure to forget: a trip that recovers
	// from nothing, as nearly every one does, pays nothing for it.
	if st == nil || !read.logStands(keyCollector(key)) {
		f.mu.Unlock()
		return
	}
	f.dropLocked(key)
	stale := f.now().Sub(st.seen) > failureLogForget
	f.mu.Unlock()
	if stale {
		return
	}
	logger.Info(msg, append(attrs, "stage", st.stage, "failed_for", f.now().Sub(st.first).Round(time.Second).String(), "failures", st.failures)...)
}

// remembers reports whether a failure is remembered under key, for a caller
// that makes its recovery's line only when there is something to recover
// from.
func (f *failureLog) remembers(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[key] != nil
}

// remembersRules reports whether a failure of any rule is remembered for the
// trip or file key is of, for a scrape that makes the rules' keys, to ask
// which of them recovered, only when one may have: as nearly none does. The
// trip is read from key as it is read from a rule's key when its failure is
// counted (ruleKeyTrip), so what is asked is what was counted.
func (f *failureLog) remembersRules(key string) bool {
	trip, _ := ruleKeyTrip(key)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ruleFailures[trip] > 0
}

// forget drops what is remembered about key without logging a recovery, for
// a failure whose subject is gone rather than fixed.
func (f *failureLog) forget(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropLocked(key)
}

// forgetCollectors drops what is remembered about collectors a reload removed
// or changed.
func (f *failureLog) forgetCollectors(names map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgetCollectorsLocked(names)
}

// forgetCollectorsLocked is forgetCollectors under the log's lock.
func (f *failureLog) forgetCollectorsLocked(names map[string]bool) {
	for key := range f.entries {
		if names[keyCollector(key)] {
			f.dropLocked(key)
		}
	}
}

// staticTargetKeyPrefix begins the target of the key of a static target's
// failure, before the target's name.
const staticTargetKeyPrefix = "static target "

// staticTargetKey is the target of the key (failureKey) under which the
// failures of the static target name are remembered.
func staticTargetKey(name string) string { return staticTargetKeyPrefix + name }

// forgetStaticTargetsLocked drops, under the log's lock, what is remembered
// under the names of the named static targets, which a reload removed or
// changed: under any collector, since the reload may have given the target
// another. What a scrape's trip remembers under the address it went to — bytes
// that were not UTF-8, sample lines left out, the files of a directory — is
// told apart by that address, as a probe's is, and stays; so does what the
// static targets endpoint remembers of a target's metric it left out, under
// no collector, which is the endpoint's to settle (settleStaticClashes).
func (f *failureLog) forgetStaticTargetsLocked(names map[string]bool) {
	if len(names) == 0 {
		return
	}
	for key := range f.entries {
		collector, rest, _ := strings.Cut(key, "\x00")
		target, _, _ := strings.Cut(rest, "\x00")
		if name, static := strings.CutPrefix(target, staticTargetKeyPrefix); static && collector != "" && names[name] {
			f.dropLocked(key)
		}
	}
}

// keyCollector is the collector of a key failureKey made.
func keyCollector(key string) string {
	collector, _, _ := strings.Cut(key, "\x00")
	return collector
}
