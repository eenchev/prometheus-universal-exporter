package exporter

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// remembers reports whether a failure is remembered under key: what a scrape
// asked of the key of each of its rules while it made those keys, kept for
// the oracles that log the rules as they were logged then.
func (f *failureLog) remembers(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries[key] != nil
}

// failureKeyWas is the bytes of a key as failureKey made them while a key
// was its collector, its target and its file with a NUL between them, and
// the target of a static target's own failures the words in
// staticTargetWas before its name: kept for the oracles that read a key as
// the failure log read one then, which are given the keys it had.
func failureKeyWas(collector, target, file string) string {
	return collector + "\x00" + target + "\x00" + file
}

// staticTargetWas began the target of the key of a static target's own
// failures, before its name, while a key was read to tell whose it was.
const staticTargetWas = "static target "

// tripKeyOf is the key of the trip a table of the tests names by a
// collector, a target and a file, as the key was made then
// (failureKeyWas), and as it is made now: of the static target whose name
// follows staticTargetWas in target, when no file is named, as such a key
// was that target's own, and else of what a trip found at target.
func tripKeyOf(collector, target, file string) (now subjectKey, was string) {
	was = failureKeyWas(collector, target, file)
	if name, static := strings.CutPrefix(target, staticTargetWas); static && file == "" {
		return staticTargetKey(collector, name), was
	}
	return failureKey(collector, target, file), was
}

// tripReadFromKey is the trip or file a rule's failure was counted for while
// the failure log read it back out of the rule's key, and whether key reads
// as a rule's: what stands before the first marker of a rule's key in it.
// The key of a trip was read so too by the scrape that asked about its
// rules. It reads the key of a rule of a directory's file named rule a part
// short — the file's name and the NUL before it are, with the NUL that
// begins the marker, a marker themselves — which is why the log is told the
// trip now (ruleFailedFor); kept for the oracles that count as the log
// counted. key is a key as it was made then (failureKeyWas,
// ruleFailureKeyWas).
func tripReadFromKey(key string) (string, bool) {
	trip, _, rule := strings.Cut(key, "\x00rule\x00")
	return trip, rule
}

// ruleFailuresReadFromKeys is what the failure log counted while it read
// the trip of a rule's failure out of the key it was remembered under: its
// entries, counted by what stands before the first marker of a rule's key
// in the key of each, whatever the entry is of — a key with the marker in it
// was taken for a rule's. was gives, for the bytes an entry is under, the
// key it was under then.
func ruleFailuresReadFromKeys(f *failureLog, was func(string) string) map[string]int {
	counts := map[string]int{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.entries {
		if trip, rule := tripReadFromKey(was(key)); rule {
			counts[trip]++
		}
	}
	return counts
}

// remembersRulesReadingKeys is remembersRules as it was while the trip was
// read out of the keys: the oracle of what a scrape was told, over counted,
// the counts the log had then (ruleFailuresReadFromKeys). key is the trip's
// as it was then, and failing the bytes the failures are under now.
func remembersRulesReadingKeys(f *failureLog, counted map[string]int, key string, failing map[string]bool) bool {
	trip, _ := tripReadFromKey(key)
	remembered := counted[trip]
	if remembered == 0 {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for failed := range failing {
		if f.entries[failed] != nil {
			remembered--
		}
	}
	return remembered > 0
}

// ruleFailureKeyWas is ruleFailureKey as it was while a rule's name stood
// before a NUL and only its expression after its length: of a trip's key as
// it was then, the key the failure log read the trip back out of
// (tripReadFromKey), and what the logging that made every rule's key made
// (logRuleFailuresMakingEveryKey).
func ruleFailureKeyWas(key, metric, expression, items string) string {
	var length [20]byte
	return key + "\x00rule\x00" + metric + "\x00" + string(strconv.AppendInt(length[:0], int64(len(expression)), 10)) + "\x00" + expression + "\x00" + items
}

// ruleFailureKeyJoined is the bytes of ruleFailureKey, of its parts joined
// in one expression: the bytes of the trip's key, the marker, the name and
// the expression each after its length and a NUL, and the items. It is the
// oracle of TestARulesKeyIsItsPartsJoinedHoweverItIsMade and of the tests
// that look for a rule's entry without asking the log for its key.
func ruleFailureKeyJoined(key, metric, expression, items string) string {
	return key + "\x00rule\x00" + strconv.Itoa(len(metric)) + "\x00" + metric + strconv.Itoa(len(expression)) + "\x00" + expression + items
}

// A rule's key is, byte for byte, the bytes of its trip's key, the marker,
// its name and its expression each after its length and a NUL, and its
// items, as they are when joined in one expression: the key the failure log
// makes when it first remembers the rule's failure, which it returns; and so
// are the bytes a lookup writes in place of a key: after whatever its buffer
// held, and with nothing left of a longer key written there before. The log
// makes the key in one allocation, of the bytes it wrote to look for the
// rule, where the scrape made it in two, the room of its size and the
// string: remembering a failure allocates the entry and the key, at every
// length, and less than two keys' bytes; under
// the race detector, which changes what is allocated, they are not counted. It is so over 30,000 generated rules
// whose trip, name, expression and items hold NULs, the marker of a rule's
// key and digits, with names and expressions of every length at which the
// count of the length's digits changes, up to 12,000 bytes.
//
// The bytes are not those a rule's key had (ruleFailureKeyWas), which held
// the name before a NUL: two rules whose names hold a NUL had one key then
// — the name "a\x003\x00xyz" without an expression or items, and the name
// "a" with the expression "xyz" and the items "0\x00\x00" — and have two now, as every two of the generated rules that differ have.
// Of the generated rules whose names hold no NUL, two have one key now when,
// and only when, they had one then.
func TestARulesKeyIsItsPartsJoinedHoweverItIsMade(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 30))
	pieces := []string{".a", ".b", " #", "\x00", "\x00rule\x00", "rule", "0", "1", "12\x00", "é", ""}
	part := func() string {
		var b strings.Builder
		for range random.IntN(5) {
			b.WriteString(pieces[random.IntN(len(pieces))])
		}
		return b.String()
	}
	trip := failureKey("web", "http://a", "").bytes
	if a, b := ruleFailureKey(trip, "a\x003\x00xyz", "", ""), ruleFailureKey(trip, "a", "xyz", "0\x00\x00"); a == b {
		t.Errorf("two rules whose names differ have the one key %q", a)
	}
	if a, b := ruleFailureKeyWas(trip, "a\x003\x00xyz", "", ""), ruleFailureKeyWas(trip, "a", "xyz", "0\x00\x00"); a != b {
		t.Errorf("the two rules had the keys %q and %q: they show no rules that had one key", a, b)
	}
	lengths := []int{0, 1, 9, 10, 11, 99, 100, 101, 999, 1000, 1001, 9999, 10000, 12000}
	type rule struct{ trip, metric, expression, items string }
	var buffer []byte
	// remembered is a failure log that remembers the failure of one rule
	// at a time: under is the key it made of the rule.
	remembered, quiet := newFailureLog(), slog.New(slog.DiscardHandler)
	underOf := func(trip, metric, expression, items string) string {
		under := remembered.ruleFailedFor(configRead{}, quiet, slog.LevelWarn, subjectKey{bytes: trip}, metric, expression, items, "failed", "metric", nil)
		remembered.forget(subjectKey{bytes: under})
		return under
	}
	distinct, long := map[string]bool{}, 0
	// rules are the rules by the key each has, wasRules by the key each
	// had; shared counts the rules that had the key of another.
	rules, wasRules, shared, plain := map[string]rule{}, map[string]rule{}, 0, 0
	// twin is the rule that had the key of the one before it.
	var twin rule
	for i := range alloctest.UnlessRaced(30000, 6000) {
		trip := failureKey(part(), part(), part()).bytes
		if i%3 == 0 {
			// Rules of one trip, which differ only in what tells a rule
			// apart.
			trip = failureKey("web", "http://a", "").bytes
		}
		metric, expression, items := part(), part(), part()
		switch i % 20 {
		case 0:
			// An expression of a length whose digits are one more or one
			// fewer than its neighbour's.
			expression = strings.Repeat("x", lengths[random.IntN(len(lengths))])
			long++
		case 1:
			// And a name of such a length.
			metric = strings.Repeat("m", lengths[random.IntN(len(lengths))])
			long++
		case 2:
			// A rule without an expression whose name reads as another
			// rule's name, the length of its expression and the
			// expression, and then that other rule.
			twin = rule{trip, metric, expression, "0\x00\x00" + items}
			metric, expression = metric+"\x00"+strconv.Itoa(len(expression))+"\x00"+expression, ""
		case 3:
			trip, metric, expression, items = twin.trip, twin.metric, twin.expression, twin.items
		}
		want := ruleFailureKeyJoined(trip, metric, expression, items)
		made := rule{trip, metric, expression, items}
		if other, seen := rules[want]; seen && other != made {
			t.Fatalf("the rules %q and %q have the one key %q", other, made, want)
		}
		rules[want] = made
		wasKey := ruleFailureKeyWas(trip, metric, expression, items)
		if other, seen := wasRules[wasKey]; seen && other != made {
			if !strings.Contains(other.metric+made.metric, "\x00") {
				t.Fatalf("the rules %q and %q, whose names hold no NUL, had the one key %q and have two", other, made, wasKey)
			}
			shared++
		}
		wasRules[wasKey] = made
		if !strings.Contains(metric, "\x00") {
			plain++
		}
		if got := underOf(trip, metric, expression, items); got != want {
			t.Fatalf("the key of the rule %q, %q, %q of %q is\n%q\nwant its parts joined\n%q", metric, expression, items, trip, got, want)
		}
		if got := ruleFailureKey(trip, metric, expression, items); got != want {
			t.Fatalf("the key the tests name the rule %q, %q, %q of %q by is\n%q\nwant its parts joined\n%q", metric, expression, items, trip, got, want)
		}
		buffer = appendRuleFailureKey(buffer[:0], trip, metric, expression, items)
		if string(buffer) != want {
			t.Fatalf("the bytes a lookup of the rule %q, %q, %q of %q writes are\n%q\nwant its key\n%q", metric, expression, items, trip, buffer, want)
		}
		if after := appendRuleFailureKey([]byte("kept"), trip, metric, expression, items); string(after) != "kept"+want {
			t.Fatalf("written after other bytes, the key of the rule %q, %q, %q of %q is\n%q\nwant them and then\n%q", metric, expression, items, trip, after, want)
		}
		distinct[want] = true
	}
	if len(distinct) < 4000 || long < 500 || shared < 250 || plain < 2000 {
		t.Errorf("%d keys that differ and %d of long names and expressions were compared, %d rules had the key of another and %d have names without a NUL: the generator shows too little", len(distinct), long, shared, plain)
	}
	if alloctest.RaceDetector {
		// The race detector changes what is allocated.
		return
	}
	// A key is made once, a string of the bytes written to look for the
	// rule, whatever the digits of its lengths: with the entry it is the
	// two allocations of a failure remembered, and its bytes are there
	// once.
	var kept string
	longest := lengths[len(lengths)-1]
	for _, length := range lengths {
		name, expression := strings.Repeat("m", length), strings.Repeat("x", longest-length)
		remember := func() { kept = underOf(trip, name, expression, ".items[]") }
		if allocs := alloctest.AllocsAtMost(20, 2, remember); allocs > 2 {
			t.Errorf("the failure of a rule with a name of %d bytes and an expression of %d is remembered in %v allocations, want 2: the entry and the key", len(name), len(expression), allocs)
		}
		if size := alloctest.BytesAtMost(20, uint64(2*longest), remember); size > uint64(2*longest) || len(kept) < longest {
			t.Errorf("the failure of a rule with a name of %d bytes and an expression of %d is remembered in %d bytes under a key of %d, want less than two keys of %d and more", len(name), len(expression), size, len(kept), longest)
		}
	}
}

// A scrape is told of a rule's failure without the rule's key exactly what
// it was told with it. Over generated sequences of everything that makes or
// drops an entry of the failure log — failures of rules and of trips, the
// same again and with another error, of a collector a reload retired;
// recoveries; forgetting a key; minutes and more than an hour passing; a
// reload forgetting a collector's entries or a static target's — and, every
// few steps, for every rule of every trip:
//
//   - the rule is found remembered when, and only when, an entry is under
//     its key, and the key it is found with is that key;
//   - with some of the trip's rules failing, or none, the scrape is told
//     that another rule's failure is remembered when, and only when, an
//     entry is under the key of a rule of the trip other than theirs, which
//     is found here by asking for the key of each of the trip's rules;
//   - and that is what it was told while the trip of a rule's failure was
//     read back out of its key (remembersRulesReadingKeys), for every trip
//     whose key then neither held the marker of a rule's key nor ended as
//     the marker begins. The keys are made otherwise now, so the oracle is
//     given, for each entry and each trip, the key it had then
//     (failureKeyWas, ruleFailureKeyWas): no two of the trips and rules here
//     had one key.
//
// Among the trips are the two kinds told otherwise then. A directory's file
// named rule, whose key ended as the marker begins, was told of no failure
// whatever was remembered, so its rules' recoveries went unlogged, and is
// told as any other trip is. A target named rule, whose key held the
// marker, was told of the failures of every trip of the target, a trip's
// own among them, and is told of its rules' alone: of nothing that it was
// not told of then. Among the rules are ones whose expression and items
// hold a NUL and the marker, and two of 3,000 bytes that differ in their
// last. Every entry holds the key it is under. Under the race detector it
// is the first of the three sequences, of 1,500 steps for the 4,000.
func TestARuleIsFoundRememberedWithoutItsKeyAsWithIt(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	collectors := []string{"web", "gone"}
	targets := []string{"http://a", "rule", staticTargetWas + "store", ""}
	files := []string{"", "a.prom", "rule"}
	rules := [][3]string{
		{"m", ".a", ""},
		{"m", ".b", ".items[]"},
		{"n", ".a", ".items[]"},
		{"", "^app_jobs", ""},
		{"m", ".a #", ".i[] #\x00.j[]"},
		{"m", ".a #\x00.i[] #", ".j[]"},
		{"m", ".a #\x00rule\x00m", ".k[]"},
		{"m", ".long | " + strings.Repeat("x", 3000), ""},
		{"m", ".long | " + strings.Repeat("x", 2999) + "y", ""},
	}
	failures := []error{errors.New("connection refused"), errors.New("value is missing"), errors.New("not a number")}
	var followed atomic.Pointer[followedConfig]
	followed.Store(&followedConfig{generation: 2, defined: map[string]uint64{"web": 1}})
	retired := configRead{followed: &followed, generation: 1}

	// A rule's key is of one trip: no two trips here have a rule's key in
	// common, so an entry under a rule's key is of that trip's rule. Nor
	// had two of them, or a trip and a rule, one key: wasKeys are the keys
	// the trips and their rules had, by the bytes of those they have.
	of, wasKeys, taken := map[string]string{}, map[string]string{}, map[string]string{}
	had := func(key, wasKey string) {
		t.Helper()
		if other, seen := taken[wasKey]; seen && other != key {
			t.Fatalf("%q and %q had the one key %q", other, key, wasKey)
		}
		wasKeys[key], taken[wasKey] = wasKey, key
	}
	for _, collector := range collectors {
		for _, target := range targets {
			for _, file := range files {
				trip, wasTrip := tripKeyOf(collector, target, file)
				had(trip.bytes, wasTrip)
				for _, rule := range rules {
					key := ruleFailureKeyJoined(trip.bytes, rule[0], rule[1], rule[2])
					if other, seen := of[key]; seen && other != trip.bytes {
						t.Fatalf("the trips %q and %q have the one key %q of the rule %q", other, trip.bytes, key, rule)
					}
					of[key] = trip.bytes
					had(key, ruleFailureKeyWas(wasTrip, rule[0], rule[1], rule[2]))
				}
			}
		}
	}
	was := func(key string) string { return wasKeys[key] }

	var found, absent, others, onlyFailing, alike, untold, fewer int
	seeds, steps := alloctest.UnlessRaced(3, 1), alloctest.UnlessRaced(4000, 1500)
	for seed := range uint64(seeds) {
		random := rand.New(rand.NewPCG(seed, 30))
		f := newFailureLog()
		now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
		f.now = func() time.Time { return now }
		trip := func() subjectKey {
			now, _ := tripKeyOf(collectors[random.IntN(len(collectors))], targets[random.IntN(len(targets))], files[random.IntN(len(files))])
			return now
		}
		reported := func() reportedFailure {
			if random.IntN(4) == 0 {
				return failureOf(trip())
			}
			rule := rules[random.IntN(len(rules))]
			of := trip()
			return ruleFailureOf(of, ruleFailureKeyJoined(of.bytes, rule[0], rule[1], rule[2]), rule[0], rule[1], rule[2])
		}
		step := func() {
			switch random.IntN(16) {
			case 0, 1, 2:
				f.recovered(logger, reported().subject(), "recovered")
			case 3:
				f.forget(reported().subject())
			case 4:
				now = now.Add(failureRepeatInterval + time.Minute)
			case 5:
				if random.IntN(8) == 0 {
					now = now.Add(failureLogForget + time.Minute)
				}
			case 6:
				if random.IntN(6) == 0 {
					f.forgetCollectors(map[string]bool{collectors[random.IntN(len(collectors))]: true})
					return
				}
				f.mu.Lock()
				f.forgetStaticTargetsLocked(map[string]bool{"store": true})
				f.mu.Unlock()
			case 7:
				of := failureKey("gone", "http://a", "")
				ruleFailureOf(of, ruleFailureKeyJoined(of.bytes, "m", ".a", ""), "m", ".a", "").failed(f, retired, logger, "metric", failures[1])
			default:
				reported().failed(f, configRead{}, logger, "metric", failures[random.IntN(len(failures))])
			}
		}
		for at := range steps {
			step()
			if at%5 != 0 {
				continue
			}
			f.mu.Lock()
			entries := make(map[string]bool, len(f.entries))
			for under, st := range f.entries {
				entries[under] = true
				if st.key.bytes != under {
					t.Fatalf("seed %d, step %d: the entry under %q holds the key %q", seed, at, under, st.key.bytes)
				}
			}
			f.mu.Unlock()
			counted := ruleFailuresReadFromKeys(f, was)
			for _, collector := range collectors {
				for _, target := range targets {
					for _, file := range files {
						trip, wasTrip := tripKeyOf(collector, target, file)
						failing, beside, some := map[string]bool{}, false, false
						var keys []string
						for _, rule := range rules {
							want := ruleFailureKeyJoined(trip.bytes, rule[0], rule[1], rule[2])
							got, remembered := f.rememberedRule(trip, rule[0], rule[1], rule[2])
							if remembered != entries[want] || remembered != f.remembers(want) || (remembered && got != want) {
								t.Fatalf("seed %d, step %d: the rule %q of %q is found remembered: %v, under %q; an entry is under its key %q: %v", seed, at, rule, trip.bytes, remembered, got, want, entries[want])
							}
							if remembered {
								found++
							} else {
								absent++
							}
							if random.IntN(3) == 0 {
								failing[want] = true
							}
							keys = append(keys, want)
						}
						for _, key := range keys {
							some = some || entries[key]
							beside = beside || entries[key] && !failing[key]
						}
						if got := f.remembersRules(trip, failing); got != beside {
							t.Fatalf("seed %d, step %d: a scrape of %q on which the rules under %v fail is told %v that another rule's failure is remembered, want %v; the entries: %v", seed, at, trip.bytes, failing, got, beside, entries)
						}
						if got := f.remembersRules(trip, nil); got != some {
							t.Fatalf("seed %d, step %d: a scrape of %q on which no rule fails is told %v that a rule's failure is remembered, want %v; the entries: %v", seed, at, trip.bytes, got, some, entries)
						}
						was, wasSome := remembersRulesReadingKeys(f, counted, wasTrip, failing), remembersRulesReadingKeys(f, counted, wasTrip, nil)
						switch _, marked := tripReadFromKey(wasTrip); {
						case marked:
							// The trip was told of every failure counted
							// before the marker in its key, which the
							// failures of its rules are among.
							if beside && !was || some && !wasSome {
								t.Fatalf("seed %d, step %d: a scrape of %q, whose key held the marker, is told of a rule's failure (%v beside those under %v, %v at all) that it was not told of (%v, %v); the entries: %v", seed, at, wasTrip, beside, failing, some, was, wasSome, entries)
							}
							if was && !beside {
								fewer++
							}
						case strings.HasSuffix(wasTrip, "\x00rule"):
							if was || wasSome {
								t.Fatalf("seed %d, step %d: the oracle tells a scrape of %q, whose key ended as the marker begins, of a rule's failure: it does not read a rule's key as the log read it; the entries: %v", seed, at, wasTrip, entries)
							}
							if beside {
								untold++
							}
						default:
							if was != beside || wasSome != some {
								t.Fatalf("seed %d, step %d: a scrape of %q on which the rules under %v fail is told %v, and with none failing %v, and was told %v and %v; the entries: %v", seed, at, wasTrip, failing, beside, some, was, wasSome, entries)
							}
							alike++
						}
						if beside {
							others++
						} else if some {
							onlyFailing++
						}
					}
				}
			}
		}
	}
	if lookups := seeds * steps; found < lookups || absent < lookups || others < lookups/2 || onlyFailing < lookups/20 || alike < lookups || untold < lookups/20 || fewer < lookups/20 {
		t.Fatalf("%d rules found remembered and %d not, %d scrapes told of another rule and %d told of none but those failing; %d scrapes told what they were, %d of a file named rule told of a failure they were not told of, and %d of a target named rule no longer told of another trip's: the generator shows too little", found, absent, others, onlyFailing, alike, untold, fewer)
	}
}

// Rules are looked up from many scrapes at once, of one trip and of others,
// with keys of a few bytes and of thousands, while failures are remembered
// and forgotten: each lookup finds the rule under its own key or not at all,
// never under the bytes another lookup wrote, which the race detector also
// holds the one buffer to.
func TestRulesAreLookedUpFromManyScrapesAtOnce(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	f := newFailureLog()
	failed := errors.New("value is missing")
	var scrapes sync.WaitGroup
	var wrong atomic.Pointer[string]
	var hits atomic.Int64
	for scrape := range 6 {
		scrapes.Go(func() {
			trip := failureKey("web", fmt.Sprintf("http://t%d", scrape%2), "")
			for round := range alloctest.UnlessRaced(3000, 600) {
				rule := (scrape + round) % 5
				expression := fmt.Sprintf(".e%d | %s", rule, strings.Repeat("x", []int{0, 40, 4000}[rule%3]))
				want := ruleFailureKeyJoined(trip.bytes, "m", expression, "")
				got, remembered := f.rememberedRule(trip, "m", expression, "")
				if remembered && got != want {
					said := fmt.Sprintf("the rule %d of %q was found under a key of %d bytes, want its own of %d", rule, trip.bytes, len(got), len(want))
					wrong.Store(&said)
					return
				}
				if remembered {
					hits.Add(1)
				}
				switch round % 3 {
				case 0:
					if under := f.ruleFailedFor(configRead{}, logger, slog.LevelWarn, trip, "m", expression, "", "failed", "metric", failed); under != want {
						said := fmt.Sprintf("the failure of the rule %d of %q is remembered under a key of %d bytes, want its own of %d", rule, trip.bytes, len(under), len(want))
						wrong.Store(&said)
						return
					}
				case 1:
					f.remembersRules(trip, map[string]bool{want: true})
				default:
					f.recovered(logger, trip.rule(want), "recovered")
				}
			}
		})
	}
	scrapes.Wait()
	if said := wrong.Load(); said != nil {
		t.Fatal(*said)
	}
	if hits.Load() == 0 {
		t.Error("no lookup found its rule remembered")
	}
}
