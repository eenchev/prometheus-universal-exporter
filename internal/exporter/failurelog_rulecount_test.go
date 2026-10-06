package exporter

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// reportedFailure is a failure as a test reports it to the failure log:
// under the bytes key, and for a rule's as that of a rule of the trip or
// file whose key is trip. One that is no rule's is that of trip itself.
type reportedFailure struct {
	trip subjectKey
	key  string
	rule bool
	// metric, expression and items are what tells a rule apart, which a
	// scrape reports its failure by: the log makes the rule's key, and key
	// is what the test holds that key to.
	metric, expression, items string
	// keyed says that a rule's failure is reported under key instead, as a
	// scrape reported one while it made the key itself
	// (ruleFailedUnderKey): to a log kept as an oracle, whose keys are
	// those of another time.
	keyed bool
}

// ruleFailureOf is the failure of the rule of the trip or file whose key is
// trip that metric, expression and items tell apart, as a scrape reports it,
// with the key the log is to remember it under.
func ruleFailureOf(trip subjectKey, key, metric, expression, items string) reportedFailure {
	return reportedFailure{rule: true, trip: trip, key: key, metric: metric, expression: expression, items: items}
}

// failureOf is the failure of the trip or file whose key is trip.
func failureOf(trip subjectKey) reportedFailure {
	return reportedFailure{trip: trip, key: trip.bytes}
}

// subject is the key the failure is remembered under, with whose it is.
func (r reportedFailure) subject() subjectKey { return r.trip.rule(r.key) }

// failed reports the failure to f as that of a trip that read its collector
// as read says, as a scrape reports a rule's failure and any other. The key
// the log says a rule's failure is remembered under is the key the test
// made of the rule, or none, when and only when no entry is under that key:
// anything else is the test's failure, said in a panic, since the tests
// that report by the thousand have no place to take it.
func (r reportedFailure) failed(f *failureLog, read configRead, logger *slog.Logger, stage string, err error) {
	switch {
	case !r.rule:
		f.failedFor(read, logger, slog.LevelWarn, r.trip, "failed", stage, err)
	case r.keyed:
		f.ruleFailedUnderKey(read, logger, slog.LevelWarn, r.trip, r.key, "failed", stage, err)
	default:
		under := f.ruleFailedFor(read, logger, slog.LevelWarn, r.trip, r.metric, r.expression, r.items, "failed", stage, err)
		if remembered := f.remembers(r.key); under != r.key && under != "" || remembered != (under != "") {
			panic(fmt.Sprintf("the failure of the rule %q, %q, %q of %q is said to be remembered under %q; its key is %q, and an entry is under it: %v", r.metric, r.expression, r.items, r.trip.bytes, under, r.key, remembered))
		}
	}
}

// ruleFailuresRecounted is what the failure log's counts of rule failures
// must be: the entries there are, counted again by the trip or file each was
// reported as a rule of, which the test that reported them kept in of by
// the rule's key, and is not found as the log finds it.
func ruleFailuresRecounted(f *failureLog, of map[string]string) map[string]int {
	counts := map[string]int{}
	for key := range f.entries {
		if trip, rule := of[key]; rule {
			counts[trip]++
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
// expression and items hold a NUL and the marker itself, and among the
// trips a directory's file named rule, whose key ends as the marker begins,
// and the files of a target named rule, whose keys hold it: a failure is
// counted for the trip it was reported as a rule of, which the test keeps
// beside each rule's key, whatever the keys read as. Under the race
// detector it is the first of the four sequences, which makes every step
// hundreds of times and fills the log as each of the others does.
func TestTheRuleFailuresCountedAreTheEntriesThereAre(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	collectors := []string{"web", "files", "api", "gone"}
	targets := []string{"http://a", "http://b", staticTargetWas + "store", staticTargetWas + "vault", "", "rule"}
	files := []string{"", "a.prom", "rule"}
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
		trip := func() subjectKey {
			now, _ := tripKeyOf(collectors[random.IntN(len(collectors))], targets[random.IntN(len(targets))], files[random.IntN(len(files))])
			return now
		}
		// of is the trip each rule's key was reported with: a rule's key
		// is of one trip.
		of := map[string]string{}
		ruleOf := func(trip subjectKey, metric, expression, items string) reportedFailure {
			t.Helper()
			key := ruleFailureKey(trip.bytes, metric, expression, items)
			if other, seen := of[key]; seen && other != trip.bytes {
				t.Fatalf("the trips %q and %q have the one key %q of a rule", other, trip.bytes, key)
			}
			of[key] = trip.bytes
			return ruleFailureOf(trip, key, metric, expression, items)
		}
		reported := func() reportedFailure {
			if random.IntN(3) == 0 {
				return failureOf(trip())
			}
			rule := rules[random.IntN(len(rules))]
			return ruleOf(trip(), rule[0], rule[1], rule[2])
		}
		check := func(what string) {
			t.Helper()
			f.mu.Lock()
			defer f.mu.Unlock()
			want := ruleFailuresRecounted(f, of)
			if !reflect.DeepEqual(f.ruleFailures, want) {
				t.Fatalf("seed %d, step %d, after %s: the rule failures counted are %v, and the entries recounted %v", seed, steps, what, f.ruleFailures, want)
			}
			counted += len(want)
		}
		told := func() {
			t.Helper()
			f.mu.Lock()
			recounted := ruleFailuresRecounted(f, of)
			f.mu.Unlock()
			for _, collector := range collectors {
				for _, target := range targets {
					for _, file := range files {
						trip, _ := tripKeyOf(collector, target, file)
						if got, remembered := f.remembersRules(trip, nil), recounted[trip.bytes] > 0; got != remembered {
							t.Fatalf("seed %d, step %d: a scrape of %q is told %v, and a rule's failure of it is remembered: %v", seed, steps, trip.bytes, got, remembered)
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
				f.recovered(logger, reported().subject(), "recovered")
				return "a recovery"
			case 3:
				f.forget(reported().subject())
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
					ruleOf(failureKey("gone", "http://a", ""), "m", ".a", "").failed(f, retired, logger, "metric", failures[1])
					return "the failure of a retired collector's rule"
				}
				gone := failureKey("gone", "http://a", "")
				f.recoveredFor(retired, logger, gone.rule(ruleFailureKey(gone.bytes, "m", ".a", "")), "recovered")
				return "the recovery of a retired collector's rule"
			case 8:
				if slow {
					break
				}
				// A failure turns an hour old less than a minute after a
				// sweep that found it younger, and happens again: it is
				// dropped and remembered anew where it is reported.
				again := reported()
				again.failed(f, configRead{}, logger, "metric", failures[2])
				now = now.Add(failureLogForget - 30*time.Second)
				f.failed(logger, slog.LevelWarn, failureKey("web", "http://a", ""), "failed", "http", failures[1])
				now = now.Add(45 * time.Second)
				again.failed(f, configRead{}, logger, "metric", failures[2])
				f.mu.Lock()
				if st := f.entries[again.key]; st == nil || st.failures != 1 || !st.first.Equal(now) {
					t.Fatalf("seed %d, step %d: the failure an hour old was not remembered anew: %+v", seed, steps, st)
				}
				f.mu.Unlock()
				replaced++
				return "a failure an hour old happening again"
			}
			failed := reported()
			stage := []string{"metric", "http"}[random.IntN(2)]
			failed.failed(f, configRead{}, logger, stage, failures[random.IntN(len(failures))])
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
				ruleOf(trip, "m", fmt.Sprintf(".e%d", i%3), "").failed(f, configRead{}, logger, "metric", failures[2])
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

// A rule's failure is counted for the trip or file its scrape reported it as
// a rule of, which is the key the scrape asks with, whatever the parts of
// that key are: the failure log reads the trip out of no key. Over every key
// made of a collector, a target and a file that are each nothing, a plain
// name, rule, a NUL, the marker of a rule's key, or a piece of it — rule
// after a NUL, as the key of a directory's file named rule ends; rule before
// one; the marker short of its last byte or of its first; the marker twice —
// and the keys of a file named rule, of a file of a target named rule and of
// a static target named rule, with a rule failing on every second trip and
// another, whose name is rule and whose expression and items hold the
// marker, on every third:
//
//   - each trip's count is the failures reported of its own rules, and no
//     trip is counted that has none, though the key of one trip is the
//     beginning of another's, or what another's reads as up to a marker;
//   - a scrape of each trip is told that a rule's failure is remembered
//     when, and only when, one of its own rules' is, beside those it names
//     as failing again, and a failure it names that is another trip's is
//     not taken from its count;
//   - when the failures recover, each count goes with them, and nothing is
//     counted in the end.
//
// The log that read the trip out of the keys (remembersRulesReadingKeys)
// told a scrape whose key neither held the marker nor ended as it begins of
// every failure it is told of now, and of those of other trips besides; and
// told one whose key ended in a NUL and rule, as that of a file named rule
// did, of none, whatever was remembered. That log is given the keys the
// trips and their rules had (failureKeyWas, ruleFailureKeyWas), and of the
// trips that had one key between them — a part with a NUL read as two — the
// first is taken, so that what is remembered now is what was then.
func TestARulesFailureIsCountedForTheTripItsScrapeAsksWith(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	parts := []string{"", "a.prom", "rule", "\x00", "\x00rule", "rule\x00", "\x00rule\x00", "\x00rul", "ule\x00", "\x00rule\x00rule\x00", "rule\x00rule"}
	// A trip has the key it has, which the log is asked with, and the key
	// it had, which the oracle reads.
	type tripKeys struct {
		now subjectKey
		was string
	}
	var trips []tripKeys
	add := func(collector, target, file string) {
		now, was := tripKeyOf(collector, target, file)
		trips = append(trips, tripKeys{now, was})
	}
	add("dir", "", "rule")
	add("dir", "/var/metrics", "rule")
	add("dir", "rule", "a.prom")
	add("dir", staticTargetWas+"rule", "")
	made := map[string]bool{}
	for _, collector := range parts {
		for _, target := range parts {
			for _, file := range parts {
				add(collector, target, file)
				made[trips[len(trips)-1].now.bytes] = true
			}
		}
	}
	if len(made) != len(parts)*len(parts)*len(parts) {
		t.Fatalf("the %d trips made of the parts have %d keys between them, want one each", len(parts)*len(parts)*len(parts), len(made))
	}
	slices.SortStableFunc(trips, func(a, b tripKeys) int { return strings.Compare(a.was, b.was) })
	trips = slices.CompactFunc(trips, func(a, b tripKeys) bool { return a.was == b.was })
	rules := [2][3]string{{"m", ".a", ""}, {"rule", ".b #\x00rule\x00", "\x00rule"}}
	// keys are the keys of the two rules of each trip: no two trips have
	// one in common, so an entry under one is of that trip's rule; nor had
	// they, by the keys the rules had, which wasKeys holds.
	keys, of, wasKeys, taken := make([][2]string, len(trips)), map[string]string{}, map[string]string{}, map[string]bool{}
	for i, trip := range trips {
		for j, rule := range rules {
			key, wasKey := ruleFailureKey(trip.now.bytes, rule[0], rule[1], rule[2]), ruleFailureKeyWas(trip.was, rule[0], rule[1], rule[2])
			if other, seen := of[key]; seen || taken[wasKey] {
				t.Fatalf("the trips %q and %q have the one key %q of the rule %q, or had the one key %q", other, trip.was, key, rule, wasKey)
			}
			keys[i][j], of[key], wasKeys[key], taken[wasKey] = key, trip.was, wasKey, true
		}
	}
	was := func(key string) string { return wasKeys[key] }
	f := newFailureLog()
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return now }
	// failing says which of the two rules of the trip at i has a failure
	// remembered.
	failing := func(i int) [2]bool { return [2]bool{i%2 == 0, i%3 == 0} }
	var short, untold, shared int
	check := func(what string, remembered func(i int) [2]bool) {
		t.Helper()
		want := map[string]int{}
		for i, trip := range trips {
			for _, is := range remembered(i) {
				if is {
					want[trip.now.bytes]++
				}
			}
		}
		f.mu.Lock()
		for _, trip := range trips {
			if f.ruleFailures[trip.now.bytes] != want[trip.now.bytes] {
				t.Fatalf("%s %d failures of rules are counted for %q, want %d", what, f.ruleFailures[trip.now.bytes], trip.now.bytes, want[trip.now.bytes])
			}
		}
		if len(f.ruleFailures) != len(want) {
			t.Fatalf("%s rule failures are counted for %d trips, want %d: some for a key that is no trip's", what, len(f.ruleFailures), len(want))
		}
		f.mu.Unlock()
		counted := ruleFailuresReadFromKeys(f, was)
		for i, keyed := range trips {
			trip := keyed.was
			is := remembered(i)
			for _, asked := range []struct {
				again [2]bool
				want  bool
			}{
				{[2]bool{false, false}, is[0] || is[1]},
				{[2]bool{true, false}, is[1]},
				{[2]bool{false, true}, is[0]},
				{[2]bool{true, true}, false},
			} {
				again := map[string]bool{}
				for j, fails := range asked.again {
					if fails {
						again[keys[i][j]] = true
					}
				}
				got := f.remembersRules(keyed.now, again)
				if got != asked.want {
					t.Fatalf("%s a scrape of %q, of whose rules %v have a failure remembered and %v fail again, is told %v that another rule's failure is remembered, want %v", what, trip, is, asked.again, got, asked.want)
				}
				// The first trip's rule, which fails whenever any does,
				// is no rule of this trip.
				if i > 0 {
					again[keys[0][0]] = true
					if got := f.remembersRules(keyed.now, again); got != asked.want {
						t.Fatalf("%s a scrape of %q, of whose rules %v have a failure remembered and %v fail again, is told %v when it names the failure of a rule of %q too, want %v", what, trip, is, asked.again, got, trips[0].was, asked.want)
					}
					delete(again, keys[0][0])
				}
				was := remembersRulesReadingKeys(f, counted, trip, again)
				_, marked := tripReadFromKey(trip)
				switch {
				case !marked && strings.HasSuffix(trip, "\x00rule"):
					if was {
						t.Fatalf("%s the oracle tells a scrape of %q, whose key ended as the marker begins, of a rule's failure: it does not read a rule's key as the log read it", what, trip)
					}
					if got {
						untold++
					}
				case got && !was:
					t.Fatalf("%s a scrape of %q, of whose rules %v have a failure remembered and %v fail again, is told of a rule's failure that it was not told of", what, trip, is, asked.again)
				case was && !got:
					shared++
				}
			}
		}
	}
	check("With nothing failing", func(int) [2]bool { return [2]bool{} })
	for i, trip := range trips {
		if read, _ := tripReadFromKey(was(keys[i][0])); read != trip.was {
			short++
		}
		for j, fails := range failing(i) {
			if fails {
				ruleFailureOf(trip.now, keys[i][j], rules[j][0], rules[j][1], rules[j][2]).failed(f, configRead{}, logger, "metric", errors.New("value is missing"))
			}
		}
	}
	check("With the rules failing", failing)
	// The failures happen again, and one of each trip's with another text:
	// each is counted once still.
	for i, trip := range trips {
		for j, fails := range failing(i) {
			if fails {
				ruleFailureOf(trip.now, keys[i][j], rules[j][0], rules[j][1], rules[j][2]).failed(f, configRead{}, logger, "metric", []error{errors.New("value is missing"), errors.New("not a number")}[j])
			}
		}
	}
	check("With the rules failing again", failing)
	for i, trip := range trips {
		f.recovered(logger, trip.now.rule(keys[i][0]), "recovered")
	}
	check("With the first rule recovered", func(i int) [2]bool { return [2]bool{false, failing(i)[1]} })
	// What is left is swept when it is an hour old.
	now = now.Add(failureLogForget + 2*time.Minute)
	f.failed(logger, slog.LevelWarn, failureKey("web", "http://a", ""), "failed", "http", errors.New("connection refused"))
	check("With everything forgotten", func(int) [2]bool { return [2]bool{} })
	if len(f.entries) != 1 {
		t.Errorf("after the sweep %d entries are remembered, want the one of the trip that failed last", len(f.entries))
	}
	if len(trips) < 1000 || short < 200 || untold < 200 || shared < 200 {
		t.Errorf("of %d trips the key of a rule of %d did not read as the trip's; %d times a scrape whose key ended as the marker begins was told of a failure it was not told of, and %d times one was told of another trip's: the table shows too little", len(trips), short, untold, shared)
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
	trip := failureKey("nul", "http://target.example", "").bytes
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
			if rest, found := strings.CutPrefix(key, trip+ruleKeyMarker); !found || !strings.HasPrefix(rest, strconv.Itoa(len(r.metric))+"\x00"+r.metric) {
				t.Fatalf("the key %q of %q does not begin with its trip, the marker and the metric after its length", key, r)
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
