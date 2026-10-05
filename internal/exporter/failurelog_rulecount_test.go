package exporter

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// ruleFailuresRecounted is what the failure log's counts of rule failures
// must be: the entries there are, counted again by the trip or file before
// the first marker of a rule's key, which is found here by itself and not as
// the log finds it.
func ruleFailuresRecounted(f *failureLog) map[string]int {
	counts := map[string]int{}
	for key := range f.entries {
		if at := strings.Index(key, "\x00rule\x00"); at >= 0 {
			counts[key[:at]]++
		}
	}
	return counts
}

// The failure log's count of the rule failures it remembers for a trip is
// what a scrape asks before it looks for rules that recovered, so a count at
// zero while a rule's failure is remembered would leave that rule's recovery
// unlogged. It cannot be: over long generated sequences of everything that
// makes or drops an entry — failures of rules and of trips, the same again
// and with another error, of collectors that stand and of one a reload
// retired; recoveries; forgetting a key; the clock moving seconds, minutes
// and past the hour after which a failure is forgotten, where the sweep
// drops it and where, the sweep not being due, it is dropped and remembered
// anew when it happens again; a reload forgetting a collector's
// entries or a static target's; and the log filled to its 10,000 entries,
// where a new failure is not remembered — the counts are, after every step
// while the log is small and every hundred steps when it is full, exactly
// the entries recounted, and what a scrape is told of each trip is whether
// an entry of a rule of it exists. Among the rules are ones whose
// expression and items hold a NUL and the marker itself. Under the race
// detector it is the first of the four sequences, which makes every step
// hundreds of times and fills the log as each of the others does.
func TestTheRuleFailuresCountedAreTheEntriesThereAre(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	collectors := []string{"web", "files", "api", "gone"}
	targets := []string{"http://a", "http://b", staticTargetKey("store"), staticTargetKey("vault"), ""}
	files := []string{"", "a.prom"}
	rules := [][3]string{
		{"m", ".a", ""},
		{"m", ".b", ".items[]"},
		{"n", ".a", ".items[]"},
		{"", "^app_jobs", ""},
		{"m", ".a #", ".i[] #\x00.j[]"},
		{"m", ".a #\x00.i[] #", ".j[]"},
		{"m", ".a #\x00rule\x00m", ".k[]"},
	}
	failures := []error{nil, errors.New("connection refused"), errors.New("value is missing"), errors.New("not a number")}
	// retired is how a trip reads a collector a reload removed since: what it
	// tells the log is remembered nowhere.
	var followed atomic.Pointer[followedConfig]
	followed.Store(&followedConfig{generation: 2, defined: map[string]uint64{"web": 1, "files": 1, "api": 1}})
	retired := configRead{followed: &followed, generation: 1}

	var steps, counted, full, superseded, replaced int
	seeds := alloctest.UnlessRaced(4, 1)
	for seed := range uint64(seeds) {
		random := rand.New(rand.NewPCG(seed, 16))
		f := newFailureLog()
		now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
		f.now = func() time.Time { return now }
		trip := func() string {
			return failureKey(collectors[random.IntN(len(collectors))], targets[random.IntN(len(targets))], files[random.IntN(len(files))])
		}
		key := func() string {
			if random.IntN(3) == 0 {
				return trip()
			}
			rule := rules[random.IntN(len(rules))]
			return ruleFailureKey(trip(), rule[0], rule[1], rule[2])
		}
		check := func(what string) {
			t.Helper()
			f.mu.Lock()
			defer f.mu.Unlock()
			want := ruleFailuresRecounted(f)
			if !reflect.DeepEqual(f.ruleFailures, want) {
				t.Fatalf("seed %d, step %d, after %s: the rule failures counted are %v, and the entries recounted %v", seed, steps, what, f.ruleFailures, want)
			}
			counted += len(want)
		}
		told := func() {
			t.Helper()
			f.mu.Lock()
			recounted := ruleFailuresRecounted(f)
			f.mu.Unlock()
			for _, collector := range collectors {
				for _, target := range targets {
					for _, file := range files {
						trip := failureKey(collector, target, file)
						if got, remembered := f.remembersRules(trip), recounted[trip] > 0; got != remembered {
							t.Fatalf("seed %d, step %d: a scrape of %q is told %v, and a rule's failure of it is remembered: %v", seed, steps, trip, got, remembered)
						}
					}
				}
			}
		}
		// While the log is full its clock moves slowly, so that it stays
		// full: nothing is an hour old before the last sweep.
		slow := false
		step := func() string {
			steps++
			switch random.IntN(20) {
			case 0, 1, 2:
				f.recovered(logger, key(), "recovered")
				return "a recovery"
			case 3:
				f.forget(key())
				return "forgetting a key"
			case 4:
				if slow {
					now = now.Add(time.Second)
					return "a second passing"
				}
				now = now.Add(time.Duration(1+random.IntN(90)) * time.Second)
				return "seconds passing"
			case 5:
				if slow {
					now = now.Add(time.Duration(1+random.IntN(20)) * time.Second)
					return "seconds passing"
				}
				if random.IntN(6) == 0 {
					now = now.Add(failureLogForget + time.Duration(random.IntN(120))*time.Second)
					return "more than an hour passing"
				}
				now = now.Add(failureRepeatInterval + time.Minute)
				return "minutes passing"
			case 6:
				if random.IntN(4) == 0 {
					f.forgetCollectors(map[string]bool{collectors[random.IntN(len(collectors))]: true})
					return "a reload forgetting a collector"
				}
				f.mu.Lock()
				f.forgetStaticTargetsLocked(map[string]bool{[]string{"store", "vault"}[random.IntN(2)]: true})
				f.mu.Unlock()
				return "a reload forgetting a static target"
			case 7:
				superseded++
				if random.IntN(2) == 0 {
					f.failedFor(retired, logger, slog.LevelWarn, ruleFailureKey(failureKey("gone", "http://a", ""), "m", ".a", ""), "failed", "metric", failures[1])
					return "the failure of a retired collector's rule"
				}
				f.recoveredFor(retired, logger, ruleFailureKey(failureKey("gone", "http://a", ""), "m", ".a", ""), "recovered")
				return "the recovery of a retired collector's rule"
			case 8:
				if slow {
					break
				}
				// A failure turns an hour old less than a minute after a
				// sweep that found it younger, and happens again: it is
				// dropped and remembered anew where it is reported.
				again := key()
				f.failed(logger, slog.LevelWarn, again, "failed", "metric", failures[2])
				now = now.Add(failureLogForget - 30*time.Second)
				f.failed(logger, slog.LevelWarn, failureKey("web", "http://a", ""), "failed", "http", failures[1])
				now = now.Add(45 * time.Second)
				f.failed(logger, slog.LevelWarn, again, "failed", "metric", failures[2])
				f.mu.Lock()
				if st := f.entries[again]; st == nil || st.failures != 1 || !st.first.Equal(now) {
					t.Fatalf("seed %d, step %d: the failure an hour old was not remembered anew: %+v", seed, steps, st)
				}
				f.mu.Unlock()
				replaced++
				return "a failure an hour old happening again"
			}
			f.failed(logger, slog.LevelWarn, key(), "failed", []string{"metric", "http"}[random.IntN(2)], failures[random.IntN(len(failures))])
			return "a failure"
		}
		for range 6000 {
			check(step())
			if steps%50 == 0 {
				told()
			}
		}
		// The log is filled: with the failures of rules and of trips of
		// targets of their own, of a collector no step forgets, past what
		// it remembers.
		slow = true
		for i := range failureLogMaxEntries + 500 {
			trip := failureKey("filling", fmt.Sprintf("http://t%d", i/3), "")
			if i%3 == 0 {
				f.failed(logger, slog.LevelWarn, trip, "failed", "http", failures[1])
			} else {
				f.failed(logger, slog.LevelWarn, ruleFailureKey(trip, "m", fmt.Sprintf(".e%d", i%3), ""), "failed", "metric", failures[2])
			}
		}
		check("filling the log")
		f.mu.Lock()
		if len(f.entries) != failureLogMaxEntries {
			t.Fatalf("seed %d: the filled log remembers %d entries, want %d", seed, len(f.entries), failureLogMaxEntries)
		}
		f.mu.Unlock()
		for range 3000 {
			what := step()
			f.mu.Lock()
			if len(f.entries) == failureLogMaxEntries {
				full++
			}
			f.mu.Unlock()
			if steps%100 == 0 {
				check(what)
				told()
			}
		}
		check("the steps of a full log")
		// Everything is forgotten in the end, and nothing is counted.
		now = now.Add(failureLogForget + 2*time.Minute)
		f.failed(logger, slog.LevelWarn, failureKey("web", "http://a", ""), "failed", "http", failures[1])
		check("the sweep of everything")
		f.mu.Lock()
		if len(f.entries) != 1 || len(f.ruleFailures) != 0 {
			t.Fatalf("seed %d: after the sweep %d entries are remembered and rule failures are counted for %d trips, want 1 and 0", seed, len(f.entries), len(f.ruleFailures))
		}
		f.mu.Unlock()
	}
	if counted < 2500*seeds || full < 250*seeds || superseded < 125*seeds || replaced < 125*seeds {
		t.Fatalf("%d counts compared, %d steps of a full log, %d of a retired collector and %d of a failure an hour old happening again: the generator shows too little", counted, full, superseded, replaced)
	}
}

// Two rules have one key in the failure log only when they are one rule:
// alike in metric name, expression and items. A jq or css expression may
// hold a NUL, in a comment or a string, and the reviewer's pair of rules —
// `items: ".i[] #\0.j[]"` with `expression: ".a #"`, and `items: ".j[]"`
// with `expression: ".a #\0.i[] #"` — had one key while the key joined the
// three with a NUL between them, so one rule's failure was the other's
// repeat and its recovery. They have two now, and so has every pair of
// generated rules that differ, with NULs and without; rules without a NUL
// are told apart exactly as they were.
func TestARulesKeyIsItsOwnWhateverItsExpressionHolds(t *testing.T) {
	trip := failureKey("nul", "http://target.example", "")
	if a, b := ruleFailureKey(trip, "m", ".a #", ".i[] #\x00.j[]"), ruleFailureKey(trip, "m", ".a #\x00.i[] #", ".j[]"); a == b {
		t.Errorf("two rules that differ have the one key %q", a)
	}
	// was is the key as it was made.
	was := func(key, metric, expression, items string) string {
		return key + "\x00rule\x00" + metric + "\x00" + expression + "\x00" + items
	}
	random := rand.New(rand.NewPCG(2, 16))
	part := func(pieces []string) string {
		var b strings.Builder
		for range random.IntN(4) {
			b.WriteString(pieces[random.IntN(len(pieces))])
		}
		return b.String()
	}
	type rule struct{ metric, expression, items string }
	for _, pieces := range [][]string{{".a", ".b", " #", "1", "0", ""}, {".a", ".b", " #", "\x00", "1", "0", "2\x00", ""}} {
		plain := !strings.Contains(strings.Join(pieces, ""), "\x00")
		keys, wasKeys := map[string]rule{}, map[string]rule{}
		differ := 0
		for range 20000 {
			r := rule{[]string{"m", "n", ""}[random.IntN(3)], part(pieces), part(pieces)}
			key, wasKey := ruleFailureKey(trip, r.metric, r.expression, r.items), was(trip, r.metric, r.expression, r.items)
			if known, seen := keys[key]; seen && known != r {
				t.Fatalf("the rules %q and %q have the one key %q", known, r, key)
			}
			if _, seen := keys[key]; !seen {
				differ++
			}
			keys[key] = r
			if known, seen := wasKeys[wasKey]; plain && seen && known != r {
				t.Fatalf("the rules %q and %q, without a NUL, had the one key %q", known, r, wasKey)
			}
			wasKeys[wasKey] = r
			if rest, found := strings.CutPrefix(key, trip+ruleKeyMarker); !found || !strings.HasPrefix(rest, r.metric+"\x00") {
				t.Fatalf("the key %q of %q does not begin with its trip, the marker and the metric", key, r)
			}
		}
		if plain && len(keys) != len(wasKeys) {
			t.Errorf("%d rules without a NUL are told apart, and %d were", len(keys), len(wasKeys))
		}
		if !plain && len(wasKeys) >= len(keys) {
			t.Errorf("rules with a NUL had %d keys and have %d: the generator shows no pair that had one key", len(wasKeys), len(keys))
		}
		if differ < 200 {
			t.Errorf("only %d rules that differ were generated", differ)
		}
	}
}
