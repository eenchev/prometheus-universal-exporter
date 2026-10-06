package exporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// ruleFailureKey is the key of a rule's failure as a string, of the bytes
// the failure log writes to look for the rule (appendRuleFailureKey): what
// the log makes when it first remembers the failure, and what a scrape made
// to report every failure while it made the key itself. It is for the tests
// that name an entry, and for the oracles that report under a key.
func ruleFailureKey(key, metric, expression, items string) string {
	return string(appendRuleFailureKey(nil, key, metric, expression, items))
}

// ruleFailedUnderKey is ruleFailedFor as it was while the scrape that
// reported a rule's failure made the rule's key, for every failure it
// reported that was not remembered, and gave it with the trip: kept for the
// oracles, which report under the keys of their time.
func (f *failureLog) ruleFailedUnderKey(read configRead, logger *slog.Logger, level slog.Level, trip subjectKey, key, msg, stage string, err error, attrs ...any) {
	f.failedUnderKey(read, logger, level, true, trip.bytes, trip.rule(key), msg, stage, err, attrs...)
}

// failedUnderKey is failedOf as it was while a rule's failure came with its
// key, to the letter: the oracle of
// TestARulesFailureIsLoggedAndRememberedByItsPartsAsUnderItsKey.
func (f *failureLog) failedUnderKey(read configRead, logger *slog.Logger, level slog.Level, rule bool, trip string, key subjectKey, msg, stage string, err error, attrs ...any) {
	errText := ""
	if err != nil {
		errText = model.SameFailureText(err)
		attrs = append(attrs, "error", err)
	}
	now := f.now()
	f.mu.Lock()
	if !read.logStands(key.collector) {
		f.mu.Unlock()
		logger.Log(context.Background(), slog.LevelDebug, msg, append(attrs, "superseded", true)...)
		return
	}
	f.sweepLocked(now)
	st := f.entries[key.bytes]
	if st != nil && now.Sub(st.seen) > failureLogForget {
		f.dropLocked(key.bytes)
		st = nil
	}
	if st == nil || st.stage != stage || st.err != errText {
		if st != nil || f.rememberLocked() {
			f.putLocked(key, &failureState{rule: rule, trip: trip, stage: stage, err: errText, first: now, logged: now, seen: now, failures: 1})
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

// rememberedRuleThenFailedUnderKey reports a rule's failure as a scrape did
// while it asked the log for the key the failure was remembered under, made
// the key when it was under none, and reported the failure under it
// (failedUnderKey): two askings of the log where there is one. It returns the
// key the scrape then held the rule to have failed by, remembered or not.
func rememberedRuleThenFailedUnderKey(f *failureLog, read configRead, logger *slog.Logger, level slog.Level, trip subjectKey, metric, expression, items, msg, stage string, err error, attrs ...any) string {
	key, remembered := f.rememberedRule(trip, metric, expression, items)
	if !remembered {
		key = ruleFailureKey(trip.bytes, metric, expression, items)
	}
	f.ruleFailedUnderKey(read, logger, level, trip, key, msg, stage, err, attrs...)
	return key
}

// sameFailuresRemembered reports whether two failure logs remember the same
// failures, each with the key, the trip, the stage, the text, the times and
// the counts the other has it with, and count the same failures of rules
// for the same trips. Of the entries it compares those under keys, and all
// of them when keys is nil; the two logs remember as many either way.
func sameFailuresRemembered(a, b *failureLog, keys []string) bool {
	if len(a.entries) != len(b.entries) || !reflect.DeepEqual(a.ruleFailures, b.ruleFailures) {
		return false
	}
	same := func(key string) bool {
		st, other := a.entries[key], b.entries[key]
		return st == other || st != nil && other != nil && *st == *other
	}
	for _, key := range keys {
		if !same(key) {
			return false
		}
	}
	if keys != nil {
		return true
	}
	for key := range a.entries {
		if !same(key) {
			return false
		}
	}
	return true
}

// A rule's failure reported by what tells the rule apart, for the log to
// make its key when it remembers it, is logged and remembered exactly as one
// that came with its key was. Two logs are told the same generated things,
// one each way (failedUnderKey is the former, to the letter, asked as a
// scrape asked it): failures of rules and of trips, the same again and with
// another stage or text, of a collector that stands and of one a reload
// retired; recoveries; a key forgotten; a reload forgetting a collector;
// seconds and minutes passing, and more than the hour after which a failure
// is forgotten, where the sweep drops it and where, the sweep having run
// less than a minute before, it is dropped as it is reported again. After
// every step the two logs read line for line the same, hold as many
// entries, hold the entries of the trips and of their rules with the same
// times and counts — every entry, each two hundredth step and at the end —
// and count the same failures for the same trips; and the key the log
// returns is the one the scrape made when, and only when, an entry is under
// that key afterwards, and none otherwise.
//
// In every other sequence the logs are filled, a quarter of the way in, to
// their 10,000 entries with the failures of other targets, and filled again
// every twenty steps: a failure
// that starts is logged in full every time and remembered by neither, with
// no key returned; those remembered before stay, repeat and change; and
// when a recovery, a forgotten key, the sweep or a reload makes room, a
// failure that was not remembered is, by both, at its next report. The rules
// are few, so that each fails again and again, and among them are ones
// whose expression and items hold a NUL and the marker of a rule's key, two
// of 2,000 bytes that differ in their last, and a trip that is a
// directory's file named rule. Under the race detector it is two sequences
// of 1,500 steps for the four of 3,000.
func TestARulesFailureIsLoggedAndRememberedByItsPartsAsUnderItsKey(t *testing.T) {
	trips := []subjectKey{
		failureKey("web", "http://a", ""), failureKey("web", "http://b", ""), failureKey("files", "/var/metrics", "rule"),
		staticTargetKey("web", "store"), failureKey("gone", "http://a", ""),
	}
	long := strings.Repeat("x", 2000)
	rules := [][3]string{
		{"m", ".a", ""}, {"m", ".b", ".items[]"}, {"n", ".a", ".items[]"}, {"", "^app_jobs", ""}, {"", "", ""},
		{"m", ".a #", ".i[] #\x00.j[]"}, {"m", ".a #\x00.i[] #", ".j[]"}, {"m", ".a #\x00rule\x00m", ".k[]"},
		{"m", ".long | " + long + "a", ""}, {"m", ".long | " + long + "b", ""},
	}
	failures := []error{nil, errors.New("value is missing"), model.Errorf("value %d is no number", model.Position(4)), errors.New("connection refused")}
	stages := []string{"metric", "metric", "http"}
	var followed atomic.Pointer[followedConfig]
	followed.Store(&followedConfig{generation: 2, defined: map[string]uint64{"web": 2, "files": 1}})
	retired := configRead{followed: &followed, generation: 1}
	quiet := slog.New(slog.DiscardHandler)
	// subjects are the keys the steps report under: the trips', their
	// rules' and that of the trip that only makes the sweep run.
	subjects := []string{failureKey("web", "http://other", "").bytes}
	for _, trip := range trips {
		subjects = append(subjects, trip.bytes)
		for _, rule := range rules {
			subjects = append(subjects, ruleFailureKey(trip.bytes, rule[0], rule[1], rule[2]))
		}
	}

	kinds := map[string]int{`"level":"WARN"`: 0, `"repeat":true`: 0, `"repeated":`: 0, `"msg":"recovered"`: 0, `"superseded":true`: 0}
	var compared, unremembered, late, started, replaced, swept, whileFull int
	seeds, steps := alloctest.UnlessRaced(4, 2), alloctest.UnlessRaced(3000, 1500)
	for seed := range uint64(seeds) {
		random := rand.New(rand.NewPCG(seed, 33))
		clock := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
		logs := func() (*failureLog, *bytes.Buffer, *slog.Logger) {
			f, out := newFailureLog(), &bytes.Buffer{}
			f.now = func() time.Time { return clock }
			return f, out, slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: withoutLineTime}))
		}
		now, nowOut, nowLogger := logs()
		was, wasOut, wasLogger := logs()
		// missed are the rules' failures a full log did not remember, by
		// key: late counts those remembered at a later report.
		missed := map[string]bool{}
		filler, filled := 0, false
		for step := range steps {
			if seed%2 == 1 && step == steps/4 {
				filled = true
			}
			// A full log is filled again, when room was made in it, every
			// twentieth step: in between a failure finds room or does not.
			if filled && step%20 == 0 {
				for len(now.entries) < failureLogMaxEntries {
					filler++
					for _, f := range []*failureLog{now, was} {
						f.failed(quiet, slog.LevelError, failureKey("filling", fmt.Sprintf("http://t%d", filler), ""), "failed", "http", failures[3])
					}
				}
			}
			full := len(now.entries) == failureLogMaxEntries
			trip := trips[random.IntN(len(trips))]
			which := random.IntN(len(rules))
			rule := rules[which]
			key := ruleFailureKey(trip.bytes, rule[0], rule[1], rule[2])
			attrs := []any{"trip", trip.bytes, "rule", which}
			switch op := random.IntN(30); {
			case op < 3:
				now.recovered(nowLogger, trip.rule(key), "recovered", attrs...)
				was.recovered(wasLogger, trip.rule(key), "recovered", attrs...)
			case op == 3:
				now.forget(trip.rule(key))
				was.forget(trip.rule(key))
			case op == 4:
				clock = clock.Add(time.Duration(1+random.IntN(90)) * time.Second)
			case op == 5:
				clock = clock.Add(failureRepeatInterval + time.Minute)
			case op == 6 && random.IntN(5) == 0:
				// More than an hour, and at times with the sweep having
				// just run: a failure of another trip is reported half a
				// minute before the hour is over.
				clock = clock.Add(failureLogForget - 30*time.Second)
				if random.IntN(2) == 0 {
					for _, f := range []*failureLog{now, was} {
						f.failed(quiet, slog.LevelError, failureKey("web", "http://other", ""), "failed", "http", failures[3])
					}
				}
				clock = clock.Add(45 * time.Second)
				swept++
			case op == 7 && random.IntN(4) == 0:
				names := map[string]bool{[]string{"web", "files"}[random.IntN(2)]: true}
				now.forgetCollectors(names)
				was.forgetCollectors(names)
			case op == 8:
				// A failure that is the trip's own, between the rules'.
				now.failedFor(configRead{}, nowLogger, slog.LevelError, trip, "failed", "http", failures[3], attrs...)
				was.failedFor(configRead{}, wasLogger, slog.LevelError, trip, "failed", "http", failures[3], attrs...)
			default:
				read := configRead{}
				if op == 9 || trip.collector == "gone" && op < 20 {
					read = retired
				}
				stage, err := stages[random.IntN(len(stages))], failures[random.IntN(len(failures))]
				before := was.remembers(key)
				under := now.ruleFailedFor(read, nowLogger, slog.LevelWarn, trip, rule[0], rule[1], rule[2], "failed", stage, err, attrs...)
				made := rememberedRuleThenFailedUnderKey(was, read, wasLogger, slog.LevelWarn, trip, rule[0], rule[1], rule[2], "failed", stage, err, attrs...)
				if made != key {
					t.Fatalf("seed %d, step %d: the scrape that made the key reports the rule %q of %q under %q, want %q", seed, step, rule, trip.bytes, made, key)
				}
				switch after := was.remembers(key); {
				case after && under != key, !after && under != "":
					t.Fatalf("seed %d, step %d: the failure of the rule %q of %q is said to be remembered under %q; an entry is under its key %q: %v", seed, step, rule, trip.bytes, under, key, after)
				case !after && read.followed == nil:
					unremembered++
					missed[key] = true
				case after && !before && missed[key]:
					late++
					delete(missed, key)
				case after && !before:
					started++
				}
				if under != "" && under == key && before {
					replaced++
				}
				if full {
					whileFull++
				}
			}
			if nowOut.String() != wasOut.String() {
				t.Fatalf("seed %d, step %d, of the rule %q of %q: the log reads\n%s\nwant as the log given the key read\n%s", seed, step, rule, trip.bytes, nowOut, wasOut)
			}
			every := subjects
			if step%200 == 0 || step == steps-1 {
				every = nil
			}
			if !sameFailuresRemembered(now, was, every) {
				t.Fatalf("seed %d, step %d, of the rule %q of %q: the log remembers %d failures and counts %v; the log given the key remembers %d and counts %v, or others", seed, step, rule, trip.bytes, len(now.entries), now.ruleFailures, len(was.entries), was.ruleFailures)
			}
			for kind := range kinds {
				kinds[kind] += strings.Count(nowOut.String(), kind)
			}
			nowOut.Reset()
			wasOut.Reset()
			compared++
		}
	}
	for kind, lines := range kinds {
		if lines < compared/100 {
			t.Errorf("only %d lines with %s were compared in %d steps", lines, kind, compared)
		}
	}
	if unremembered < compared/50 || late < compared/400 || started < compared/100 || replaced < compared/20 || swept < compared/400 || whileFull < compared/8 {
		t.Errorf("in %d steps %d failures of rules were not remembered by a full log and %d of those were at a later report, %d started with room, %d were of a rule remembered already, the clock passed the hour %d times and %d failures were reported to a full log: the generator shows too little", compared, unremembered, late, started, replaced, swept, whileFull)
	}
}
