//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// ruleFailedUnderItsKey is Server.ruleFailed as it was while the scrape made
// the key of the rule's failure and reported the failure under it: to the
// failure log (ruleFailedUnderKey), or for a debug probe to its report
// alone, where the key went unread.
func ruleFailedUnderItsKey(ctx context.Context, s *Server, read configRead, level slog.Level, trip subjectKey, key, msg, stage string, err error, attrs ...any) {
	if probeTraceFrom(ctx) != nil {
		s.tripFailed(ctx, read, level, trip.rule(key), msg, stage, err, attrs...)
		return
	}
	s.failures.ruleFailedUnderKey(read, s.logger, level, trip, key, msg, stage, err, attrs...)
}

// ruleFailedReadingItsKey reports a rule's failure as it was reported while
// the failure log read the trip or file to count it for back out of the
// rule's key (tripReadFromKey): the log is told that trip, read here, as
// one of whose the scrape's trip, of, is.
func ruleFailedReadingItsKey(ctx context.Context, s *Server, read configRead, of subjectKey, key string, err error, attrs ...any) {
	trip, _ := tripReadFromKey(key)
	ruleFailedUnderItsKey(ctx, s, read, slog.LevelWarn, of.rule(trip), key, "metric extraction failed", "metric", err, attrs...)
}

// remembersAnyRule is remembersRules as it was while a scrape asked only
// whether any rule of its trip had a failure remembered, whichever rules
// failed on it, and while the trip was read out of the scrape's key. It
// asks a log whose rule failures were reported with the trip read out of
// each rule's key (ruleFailedReadingItsKey), as a log then counted them.
func remembersAnyRule(f *failureLog, key string) bool {
	trip, _ := tripReadFromKey(key)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ruleFailures[trip] > 0
}

// logRuleFailuresMakingEveryKey is logRuleFailures as it was while a scrape
// made a rule's key to report its failure and, once any rule of the trip had
// a failure remembered, the key of every rule under log to ask whether that
// rule recovered, and while the trip a failure was counted for and the trip
// a scrape asked with were read out of the keys (ruleFailedReadingItsKey,
// remembersAnyRule), a rule's key being made as it was then
// (ruleFailureKeyWas) of the bytes of l's: the oracle of
// TestRulesAreLoggedAsTheyWereWhenEveryKeyWasMade, which gives it each
// trip's key as it was then, and what BenchmarkLogRuleFailures compares
// with as /every_key.
func logRuleFailuresMakingEveryKey(ctx context.Context, s *Server, read configRead, c *model.Collector, failures []transform.RuleFailure, l collectLog, complete bool) {
	failing := map[string]bool{}
	var shared sharedRuleNames
	for _, f := range failures {
		if !f.Logged {
			continue
		}
		key := ruleFailureKeyWas(l.key.bytes, f.Metric, f.Expression, f.Items)
		failing[key] = true
		attrs := append(append([]any{}, l.attrs...), "metric", f.Metric)
		attrs = append(shared.telling(attrs, c, f.Metric, f.Expression, f.Items), "error_mode", model.ErrorModeLog, "failures", f.Failures)
		ruleFailedReadingItsKey(ctx, s, read, l.key, key, f.First, attrs...)
	}
	if !complete || !remembersAnyRule(s.failures, l.key.bytes) {
		return
	}
	for _, rule := range c.Metrics {
		if rule.ErrorMode != model.ErrorModeLog {
			continue
		}
		key := ruleFailureKeyWas(l.key.bytes, rule.Name, rule.Expression, rule.Items)
		if failing[key] || !s.failures.remembers(key) {
			continue
		}
		attrs := append(append([]any{}, l.attrs...), "metric", rule.Name)
		s.tripRecovered(ctx, read, l.key.rule(key), "metric extraction recovered", shared.telling(attrs, c, rule.Name, rule.Expression, rule.Items)...)
	}
}

// retiredRead is how a trip reads a collector of the name that a reload
// removed since: what it tells the failure log is remembered nowhere, and
// its success is no recovery.
func retiredRead() configRead {
	followed := &atomic.Pointer[followedConfig]{}
	followed.Store(&followedConfig{generation: 2, defined: map[string]uint64{}})
	return configRead{followed: followed, generation: 1}
}

// sameEntriesAs reports whether the failure log now remembers the failures
// that was, a log told the trip of each rule's failure as it was read out of
// the rule's key, remembers: each under the key that as gives for its own —
// the key it had while a key was its parts with a NUL between them — with
// the stage, the text, the times and the counts of the other's, as the same
// collector's, and a rule's as one of the trip its key under was reads as;
// and whether now counts the rules' failures that was counts, each trip's
// under what a key of a rule of the trip under was reads as. Every entry of
// now is compared when among is nil. Otherwise it is the entries that now
// has, and that was has, of the keys in among, each given by the key it has
// in now, and the two logs remember as many: which is every entry too when
// the others are those a test filled the two logs with alike, ten thousand
// of them, and tells what a scrape changed without reading them all.
func sameEntriesAs(now, was *failureLog, as func(key string) string, among map[string]string) bool {
	if len(now.entries) != len(was.entries) {
		return false
	}
	// read is the trip that was read out of a key of a rule of trip.
	read := func(trip string) string {
		read, _ := tripReadFromKey(as(trip) + "\x00rule\x00")
		return read
	}
	same := func(key string, st *failureState) bool {
		want := *st
		want.key.bytes = as(key)
		if st.rule {
			want.trip = read(st.trip)
		}
		other := was.entries[want.key.bytes]
		return other != nil && *other == want
	}
	for key := range among {
		if st := now.entries[key]; st == nil && was.entries[as(key)] != nil || st != nil && !same(key, st) {
			return false
		}
	}
	if among == nil {
		for key, st := range now.entries {
			if !same(key, st) {
				return false
			}
		}
	}
	counted := map[string]int{}
	for trip, failures := range now.ruleFailures {
		counted[read(trip)] += failures
	}
	return reflect.DeepEqual(counted, was.ruleFailures)
}

// A scrape that finds its rules' failures without making their keys, and
// tells the failure log the trip of each rule that starts to fail, logs what
// it logged while it made the key of every rule and the log read the trip
// out of the key, and leaves the failure log remembering what it remembered
// — but for the one trip the log read wrongly, a directory's file named
// rule, whose key ends as the marker of a rule's key begins: its rules are
// logged as those of a file of any other name were, where its rules'
// recoveries were never logged. Over 80 generated collectors whose
// rules share metric names, differ in expression and items, have twins, and
// have expressions of 2 KB that differ in their last byte or hold a NUL,
// under every error mode, and 60 generated scrapes each of two targets, of
// a directory's files, one of them named rule, and of the files of a target
// named rule, whose keys hold the marker, in turn: on
// which some rules fail with one text or another, the transform is at times
// not complete, minutes or hours pass, the trip is at times one of a
// collector a reload retired or a debug probe, a reload at times forgets the
// collector. Every eighth collector has 180 scrapes, and from the fifteenth
// to the hundred and fiftieth the log is full of the failures of other
// targets: filled before every scrape up to the forty-fifth, so that no
// failure that starts is remembered beside those that are, and before every
// fourth from there on, so that between two fillings a recovery, a reload
// or the sweep of an hour makes room, and a failure the full log did not
// remember is remembered at a later scrape, and repeats and recovers from
// there. After every scrape the log reads line for line as
// the former logging, kept as an oracle, wrote it, a debug probe's report
// has the lines it had, and the failure log holds the entries it held, each
// with the times and counts it had (while it is full those of the trips'
// rules after every scrape, with how many it holds, and every one after
// each twentieth), and counts the rules' failures it
// counted, each trip's where the log counted them then — the oracle being
// given each trip's key as it was made then (failureKeyWas), for the file
// named rule the key of a file named elur, and the lines' attributes as they
// are, and the entries and counts being compared under the keys they had
// then (ruleFailureKeyWas). The oracle given the key of the file named rule
// itself, on a log of its own, shows what is no longer so: on the scrapes of
// that file it logs no recovery, and the scrapes here log them. Under the
// race detector it is every tenth of the collectors.
func TestRulesAreLoggedAsTheyWereWhenEveryKeyWasMade(t *testing.T) {
	modes := []string{model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeLog, model.ErrorModeIgnore, model.ErrorModeFail}
	long := strings.Repeat("x", 2000)
	expressions := []string{".e0", ".e1", ".e2", ".e0 | " + long + "a", ".e0 | " + long + "b", ".e1 #\x00 " + long}
	kinds := map[string]int{`"level":"WARN"`: 0, `"repeat":true`: 0, `"repeated":`: 0, `"msg":"metric extraction recovered"`: 0, `"expression":`: 0, `"items":`: 0, `"file":"rule"`: 0, `"target":"rule"`: 0, `"superseded":true`: 0, "level=WARN": 0}
	quiet := slog.New(slog.DiscardHandler)
	var remembered, full, late, forgotten, compared, filler, recoveredOfRule int
	for seed := range uint64(80) {
		if alloctest.RaceDetector && seed%10 != 0 {
			continue
		}
		random := rand.New(rand.NewPCG(seed, 30))
		c := model.Collector{Name: "generated"}
		for i := range 1 + random.IntN(8) {
			rule := model.MetricRule{Name: fmt.Sprintf("m%d", random.IntN(3)), Expression: expressions[random.IntN(len(expressions))], ErrorMode: modes[random.IntN(len(modes))]}
			if random.IntN(3) == 0 {
				rule.Items = ".items[]"
			}
			// Each has a label of its own: rules alike in name, expression
			// and items, which are one rule to the log, load only when
			// their labels tell their series apart.
			rule.Labels = []model.LabelRule{{Name: "copy", Value: strconv.Itoa(i)}}
			c.Metrics = append(c.Metrics, rule)
		}
		now, logs, clock := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		was, wasLogs, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		was.failures.now = now.failures.now
		// stuck is the oracle's log of the file named rule under its own
		// key, which the log read a part short.
		stuck, stuckLogs, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		stuck.failures.now = now.failures.now
		// The oracle is given each trip under the key it had, and the file
		// named rule under that of a file of another name, which the log
		// read whole; as is a key of now's, a trip's or a rule's, as the
		// oracle's log holds it, and a key of neither, which the two logs
		// hold alike, as it is.
		ruleFile, ruleFileWas := failureKey(c.Name, "/var/metrics", "rule"), failureKeyWas(c.Name, "/var/metrics", "rule")
		wasKeys := map[string]string{}
		var trips []collectLog
		for _, at := range [][2]string{{"http://a.example", ""}, {"http://b.example", ""}, {"/var/metrics", "a.prom"}, {"/var/metrics", "rule"}, {"rule", "a.prom"}, {"rule", "b.prom"}} {
			attrs := []any{"collector", c.Name, "target", at[0]}
			if at[1] != "" {
				attrs = append(attrs, "file", at[1])
			}
			key, wasKey := failureKey(c.Name, at[0], at[1]), failureKeyWas(c.Name, at[0], at[1])
			if key == ruleFile {
				wasKey = failureKeyWas(c.Name, "/var/metrics", "elur")
			}
			wasKeys[key.bytes] = wasKey
			for _, rule := range c.Metrics {
				wasKeys[ruleFailureKeyJoined(key.bytes, rule.Name, rule.Expression, rule.Items)] = ruleFailureKeyWas(wasKey, rule.Name, rule.Expression, rule.Items)
			}
			trips = append(trips, collectLog{key: key, attrs: attrs})
		}
		as := func(key string) string {
			if wasKey, known := wasKeys[key]; known {
				return wasKey
			}
			return key
		}
		retired := retiredRead()
		// missed are the failures a full log did not remember, by key.
		steps, missed := 60, map[string]bool{}
		if seed%8 == 0 {
			steps = 180
		}
		for step := range steps {
			if seed%8 == 0 && step >= 15 && step < 150 && (step < 45 || step%4 == 0) {
				// The log is filled with the failures of other targets:
				// the rules' failures it remembers by now stay, and one
				// that starts is not remembered, and is logged in full
				// every time, until there is room.
				for len(now.failures.entries) < failureLogMaxEntries {
					filler++
					for _, server := range []*Server{now, was} {
						server.failures.failed(quiet, slog.LevelError, failureKey("filling", fmt.Sprintf("http://t%d", filler), ""), "failed", "http", model.Errorf("connection refused"))
					}
				}
			}
			switch random.IntN(24) {
			case 0, 1:
				clock.Add(int64(failureRepeatInterval + time.Minute))
			case 2:
				clock.Add(int64(failureLogForget + time.Minute))
			case 3:
				// A reload changed the collector.
				now.failures.forgetCollectors(map[string]bool{c.Name: true})
				was.failures.forgetCollectors(map[string]bool{c.Name: true})
				stuck.failures.forgetCollectors(map[string]bool{c.Name: true})
				forgotten++
			}
			l := trips[random.IntN(len(trips))]
			read := configRead{}
			if random.IntN(10) == 0 {
				read = retired
			}
			ctx, wasCtx := context.Background(), context.Background()
			var trace, wasTrace *probeTrace
			if random.IntN(10) == 0 {
				ctx, trace = newProbeTrace(ctx)
				wasCtx, wasTrace = newProbeTrace(wasCtx)
			}
			var failures []transform.RuleFailure
			complete := random.IntN(8) != 0
			for i, rule := range c.Metrics {
				// Rules alike in name, expression and items are one rule,
				// logged when either is under log.
				twin, logged := false, false
				for j, other := range c.Metrics {
					if other.Name == rule.Name && other.Expression == rule.Expression && other.Items == rule.Items {
						twin = twin || j < i
						logged = logged || other.ErrorMode == model.ErrorModeLog
					}
				}
				if twin || random.IntN(3) != 0 {
					continue
				}
				if rule.ErrorMode == model.ErrorModeFail {
					complete = false
					continue
				}
				failed := 1 + random.Uint64N(4)
				failures = append(failures, transform.RuleFailure{
					Metric: rule.Name, Expression: rule.Expression, Items: rule.Items, Failures: failed, Missing: random.Uint64N(failed + 1),
					First:  model.Errorf("metric %q value %d is missing for item %d", rule.Name, random.IntN(2), model.Position(random.IntN(9))),
					Logged: logged,
				})
			}
			now.logRuleFailures(ctx, read, &c, failures, l, complete)
			logRuleFailuresMakingEveryKey(wasCtx, was, read, &c, failures, collectLog{key: l.key.rule(as(l.key.bytes)), attrs: l.attrs}, complete)
			if l.key == ruleFile && trace == nil {
				logRuleFailuresMakingEveryKey(ctx, stuck, read, &c, failures, collectLog{key: l.key.rule(ruleFileWas), attrs: l.attrs}, complete)
				const recovery = `"msg":"metric extraction recovered"`
				if strings.Contains(stuckLogs.String(), recovery) {
					t.Fatalf("seed %d, scrape %d of %q: the oracle, given the key of the file named rule, logs a recovery of its rule:\n%s\nit does not read the key as the log read it", seed, step, l.key.bytes, stuckLogs)
				}
				recoveredOfRule += strings.Count(logs.String(), recovery)
				stuckLogs.Reset()
			}
			if logs.String() != wasLogs.String() {
				t.Fatalf("seed %d, scrape %d of %q, with the rules %+v failing %+v, complete %v: the log is\n%s\nwant as it was\n%s", seed, step, l.key.bytes, c.Metrics, failures, complete, logs, wasLogs)
			}
			if trace != nil {
				if trace.logs.String() != wasTrace.logs.String() {
					t.Fatalf("seed %d, scrape %d of %q: a debug probe's logs are\n%s\nwant as they were\n%s", seed, step, l.key.bytes, trace.logs.String(), wasTrace.logs.String())
				}
				kinds["level=WARN"] += strings.Count(trace.logs.String(), "level=WARN")
			}
			// While the log is full, what a scrape may have changed is
			// compared after it: the entries of the trips' rules, which
			// wasKeys has the keys of; and every entry after each
			// twentieth scrape and the last.
			among := wasKeys
			if len(now.failures.entries) < failureLogMaxEntries/2 || step%20 == 0 || step == steps-1 {
				among = nil
			}
			if !sameEntriesAs(now.failures, was.failures, as, among) {
				t.Fatalf("seed %d, scrape %d of %q, with the rules %+v failing %+v, complete %v: the failure log remembers %d entries and counts %v, and remembered %d and counted %v, or others", seed, step, l.key.bytes, c.Metrics, failures, complete, len(now.failures.entries), now.failures.ruleFailures, len(was.failures.entries), was.failures.ruleFailures)
			}
			for kind := range kinds {
				if kind != "level=WARN" {
					kinds[kind] += strings.Count(logs.String(), kind)
				}
			}
			for _, counted := range now.failures.ruleFailures {
				remembered += counted
			}
			// A failure that the full log does not remember is one of a
			// trip that is neither a debug probe nor of a retired
			// collector, and one it remembers at a later scrape is
			// remembered late.
			for _, f := range failures {
				if !f.Logged || trace != nil || read.followed != nil {
					continue
				}
				key := ruleFailureKeyJoined(l.key.bytes, f.Metric, f.Expression, f.Items)
				switch {
				case !now.failures.remembers(key):
					full++
					missed[key] = true
				case missed[key]:
					late++
					delete(missed, key)
				}
			}
			compared++
			logs.Reset()
			wasLogs.Reset()
		}
	}
	for kind, lines := range kinds {
		if lines < compared/60 {
			t.Errorf("only %d lines with %s were compared in %d scrapes", lines, kind, compared)
		}
	}
	if remembered < compared || full < compared/60 || late < compared/200 || forgotten < compared/60 || recoveredOfRule < compared/100 {
		t.Errorf("in %d scrapes %d remembered failures of rules were compared, %d failures were not remembered by a full log and %d of those were at a later scrape, a reload forgot the collector %d times and %d recoveries of rules of the file named rule were logged: the generator shows too little", compared, remembered, full, late, forgotten, recoveredOfRule)
	}
}

// A scrape of a trip that has a rule's failure remembered makes no rule's
// key either: what it allocates for the log does not grow with the rules'
// expressions. For thirty rules under log it allocates no more times, and
// within a few hundred bytes the same, with expressions of 20 KB as with
// expressions of 200 bytes — where it made a key of the expression's size
// for each of the thirty — when one rule fails on every scrape, as that of
// a target that lacks its value does for as long as it does; when every
// rule fails on every scrape; and when the scrape goes on to ask about each
// of its rules, which one of a collector a reload retired does, whose
// success is no recovery, so the failure stays remembered. A failure that
// starts and then ends has its key made once, where the two scrapes made
// sixty-one: the pair allocates less than three keys more with the long
// expressions. The former logging, kept as an oracle, allocates for the one
// rule that fails on every scrape more than thirty times the difference of
// the expressions.
//
// The bounds are those of a build without the race detector, under which
// the test is skipped: there what a scrape allocates is not the same from
// one measurement to the next.
func TestAScrapeWithARuleRememberedAllocatesNothingOfItsExpressionsSize(t *testing.T) {
	if alloctest.RaceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	const short, long = 200, 20000
	type cost struct {
		allocs float64
		bytes  uint64
	}
	measure := func(length int) map[string]cost {
		c := model.Collector{Name: "generated"}
		var all []transform.RuleFailure
		for i := range 30 {
			rule := model.MetricRule{Name: fmt.Sprintf("m%d", i), Items: ".items[]", Expression: fmt.Sprintf(".values.e%d | %s", i, strings.Repeat("x", length)), ErrorMode: model.ErrorModeLog}
			c.Metrics = append(c.Metrics, rule)
			all = append(all, transform.RuleFailure{Metric: rule.Name, Expression: rule.Expression, Items: rule.Items, Failures: 1, First: model.Errorf("metric %q value is missing for item %d", rule.Name, model.Position(3)), Logged: true})
		}
		one := all[7:8]
		server, _, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		server.logger = slog.New(slog.DiscardHandler)
		trip := func(target string) collectLog {
			return collectLog{key: failureKey(c.Name, target, ""), attrs: []any{"collector", c.Name, "target", target, "url", target}}
		}
		ctx := context.Background()
		remembers := func(l collectLog, want int) {
			t.Helper()
			if got := server.failures.ruleFailures[l.key.bytes]; got != want {
				t.Fatalf("with expressions of %d bytes the log remembers %d failures of the rules of %q, want %d", length, got, l.key.bytes, want)
			}
		}
		costs := map[string]cost{}
		scrapes := func(what string, l collectLog, want int, scrape func()) {
			t.Helper()
			scrape()
			allocs, bytes := alloctest.Allocations(50, scrape)
			costs[what] = cost{allocs, bytes}
			remembers(l, want)
		}

		l := trip("http://one.example")
		scrapes("one rule fails on every scrape", l, 1, func() { server.logRuleFailures(ctx, configRead{}, &c, one, l, true) })
		every := trip("http://every.example")
		scrapes("every rule fails on every scrape", every, 30, func() { server.logRuleFailures(ctx, configRead{}, &c, all, every, true) })
		asked := trip("http://asked.example")
		server.logRuleFailures(ctx, configRead{}, &c, one, asked, true)
		retired := retiredRead()
		scrapes("the scrape asks about each of its rules", asked, 1, func() { server.logRuleFailures(ctx, retired, &c, nil, asked, true) })
		ends := trip("http://ends.example")
		scrapes("a failure starts and ends", ends, 0, func() {
			server.logRuleFailures(ctx, configRead{}, &c, one, ends, true)
			server.logRuleFailures(ctx, configRead{}, &c, nil, ends, true)
		})
		was := trip("http://was.example")
		scrapes("every key is made", was, 1, func() { logRuleFailuresMakingEveryKey(ctx, server, configRead{}, &c, one, was, true) })
		return costs
	}
	brief, lengthy := measure(short), measure(long)
	for _, what := range []string{"one rule fails on every scrape", "every rule fails on every scrape", "the scrape asks about each of its rules"} {
		if lengthy[what].allocs > brief[what].allocs || lengthy[what].bytes > brief[what].bytes+512 {
			t.Errorf("%s: with expressions of %d bytes the scrape allocates %v times and %d bytes for the log, and with expressions of %d bytes %v times and %d bytes; want no more, whatever the length", what, long, lengthy[what].allocs, lengthy[what].bytes, short, brief[what].allocs, brief[what].bytes)
		}
	}
	const pair = "a failure starts and ends"
	if grown := int64(lengthy[pair].bytes) - int64(brief[pair].bytes); lengthy[pair].allocs > brief[pair].allocs || grown > 3*(long-short) {
		t.Errorf("%s: with expressions of %d bytes the two scrapes allocate %v times and %d bytes for the log, and with expressions of %d bytes %v times and %d bytes: %d bytes more, want less than three keys", pair, long, lengthy[pair].allocs, lengthy[pair].bytes, short, brief[pair].allocs, brief[pair].bytes, grown)
	}
	const was = "every key is made"
	if lengthy[was].bytes < brief[was].bytes+30*(long-short) {
		t.Errorf("the logging that made every key allocates %d bytes with expressions of %d bytes and %d with expressions of %d: the scrape measured does not ask about thirty rules", lengthy[was].bytes, long, brief[was].bytes, short)
	}
}

// fillFailureLog fills the server's failure log to its 10,000 entries with
// the failures of targets of their own, of a collector no test scrapes, and
// returns the keys they are under, for a test to make room with.
func fillFailureLog(tb testing.TB, server *Server) []subjectKey {
	tb.Helper()
	quiet := slog.New(slog.DiscardHandler)
	var filled []subjectKey
	for i := 0; len(server.failures.entries) < failureLogMaxEntries; i++ {
		key := failureKey("filling", fmt.Sprintf("http://t%d", i), "")
		server.failures.failed(quiet, slog.LevelError, key, "failed", "http", model.Errorf("connection refused"))
		filled = append(filled, key)
	}
	if len(server.failures.entries) != failureLogMaxEntries {
		tb.Fatalf("the failure log remembers %d failures, want it full at %d", len(server.failures.entries), failureLogMaxEntries)
	}
	return filled
}

// A scrape whose rules' failures the failure log does not remember makes no
// rule's key: what it allocates for the log does not grow with the rules'
// expressions. While the log is full of the failures of other targets, a
// rule that fails is logged in full on every scrape and remembered nowhere,
// for as long as the log stays full; for thirty rules under log the scrape
// allocates no more times, and within a few hundred bytes the same, with
// expressions of 20 KB as with expressions of 200 bytes — where it made a
// key of the expression's size for each rule that failed, on every scrape —
// when one rule fails and when all thirty do. So does a debug probe's
// scrape, which is remembered nowhere whether the log is full or not, and
// made a key for each failing rule that no scrape's failure was remembered
// of. The log is as full afterwards and remembers none of the rules. The
// former logging, kept as an oracle, allocates for thirty failing rules more
// than thirty times the difference of the expressions.
//
// The bounds are those of a build without the race detector, under which
// the test is skipped: there what a scrape allocates is not the same from
// one measurement to the next.
func TestAScrapeWhoseRulesAFullLogDoesNotRememberAllocatesNothingOfItsExpressionsSize(t *testing.T) {
	if alloctest.RaceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	const short, long = 200, 20000
	type cost struct {
		allocs float64
		bytes  uint64
	}
	measure := func(length int) map[string]cost {
		c := model.Collector{Name: "generated"}
		var all []transform.RuleFailure
		for i := range 30 {
			rule := model.MetricRule{Name: fmt.Sprintf("m%d", i), Items: ".items[]", Expression: fmt.Sprintf(".values.e%d | %s", i, strings.Repeat("x", length)), ErrorMode: model.ErrorModeLog}
			c.Metrics = append(c.Metrics, rule)
			all = append(all, transform.RuleFailure{Metric: rule.Name, Expression: rule.Expression, Items: rule.Items, Failures: 1, First: model.Errorf("metric %q value is missing for item %d", rule.Name, model.Position(3)), Logged: true})
		}
		one := all[7:8]
		server, _, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		server.logger = slog.New(slog.DiscardHandler)
		trip := func(target string) collectLog {
			return collectLog{key: failureKey(c.Name, target, ""), attrs: []any{"collector", c.Name, "target", target, "url", target}}
		}
		ctx := context.Background()
		costs := map[string]cost{}
		scrapes := func(what string, l collectLog, scrape func()) {
			t.Helper()
			scrape()
			allocs, bytes := alloctest.Allocations(50, scrape)
			costs[what] = cost{allocs, bytes}
			if got := server.failures.ruleFailures[l.key.bytes]; got != 0 {
				t.Fatalf("with expressions of %d bytes the log remembers %d failures of the rules of %q, want none", length, got, l.key.bytes)
			}
		}

		// A debug probe's scrape, of a log with room.
		probed := trip("http://probed.example")
		scrapes("a debug probe's rules fail", probed, func() {
			traced, _ := newProbeTrace(ctx)
			server.logRuleFailures(traced, configRead{}, &c, all, probed, true)
		})
		fillFailureLog(t, server)
		l := trip("http://one.example")
		scrapes("one rule fails and the log is full", l, func() { server.logRuleFailures(ctx, configRead{}, &c, one, l, true) })
		every := trip("http://every.example")
		scrapes("every rule fails and the log is full", every, func() { server.logRuleFailures(ctx, configRead{}, &c, all, every, true) })
		was := trip("http://was.example")
		scrapes("every key is made", was, func() { logRuleFailuresMakingEveryKey(ctx, server, configRead{}, &c, all, was, true) })
		if len(server.failures.entries) != failureLogMaxEntries {
			t.Fatalf("with expressions of %d bytes the log remembers %d failures after the scrapes, want it full at %d", length, len(server.failures.entries), failureLogMaxEntries)
		}
		return costs
	}
	brief, lengthy := measure(short), measure(long)
	for _, what := range []string{"a debug probe's rules fail", "one rule fails and the log is full", "every rule fails and the log is full"} {
		if lengthy[what].allocs > brief[what].allocs || lengthy[what].bytes > brief[what].bytes+512 {
			t.Errorf("%s: with expressions of %d bytes the scrape allocates %v times and %d bytes for the log, and with expressions of %d bytes %v times and %d bytes; want no more, whatever the length", what, long, lengthy[what].allocs, lengthy[what].bytes, short, brief[what].allocs, brief[what].bytes)
		}
	}
	const was = "every key is made"
	if lengthy[was].bytes < brief[was].bytes+30*(long-short) {
		t.Errorf("the logging that made every key allocates %d bytes with expressions of %d bytes and %d with expressions of %d: the scrape measured does not report thirty rules to a full log", lengthy[was].bytes, long, brief[was].bytes, short)
	}
}

// A rule that failed on a scrape is not logged as recovered by that scrape,
// though its failure was not remembered when the scrape reported it and is
// when the scrape looks for the rules that recovered: the log was full, and
// another scrape of the trip, at the same time, reported the rule's failure
// after room was made. The scrape holds no key of a failure that was not
// remembered, and tells such a rule by what tells a rule apart.
//
// The other scrape is made where the first is between its two failures:
// the log's clock, which a failure is reported by and which is read outside
// the log's lock, makes it, once. The first scrape's two rules both failed;
// afterwards both failures are remembered, no recovery is logged, and the
// log reads line for line as that of the former logging, kept as an oracle
// (logRuleFailuresMakingEveryKey), which held every failing rule's key and
// is given the same scrapes. A scrape of the trip on which the first rule
// works then logs its recovery, once.
func TestARuleRememberedByAnotherScrapeMeanwhileIsNotLoggedAsRecovered(t *testing.T) {
	c := model.Collector{Name: "generated"}
	var failures []transform.RuleFailure
	for i := range 2 {
		rule := model.MetricRule{Name: fmt.Sprintf("m%d", i), Expression: fmt.Sprintf(".e%d", i), ErrorMode: model.ErrorModeLog}
		c.Metrics = append(c.Metrics, rule)
		failures = append(failures, transform.RuleFailure{Metric: rule.Name, Expression: rule.Expression, Failures: 1, First: model.Errorf("metric %q value is missing", rule.Name), Logged: true})
	}
	const target = "http://a.example"
	attrs := []any{"collector", c.Name, "target", target}
	ctx := context.Background()
	type logging func(s *Server, failures []transform.RuleFailure, l collectLog, complete bool)
	// scraped makes the scrapes on a server of its own, logging as log
	// does, and returns what was logged by the two scrapes at once and by
	// the one after, and the rules' failures remembered between them.
	scraped := func(log logging, l collectLog, ruleKey func(metric, expression string) string) (atOnce, after string, remembered []bool) {
		t.Helper()
		server, logs, _ := ruleLogServer(t, testutil.Collector(c.Name, "text"))
		filled := fillFailureLog(t, server)
		clock, reports, other := server.failures.now, 0, false
		server.failures.now = func() time.Time {
			if reports++; reports == 2 && !other {
				// The first scrape has reported its first failure, which
				// the full log did not remember. Room is made, and
				// another scrape of the trip reports the same failure.
				other = true
				server.failures.forget(filled[0])
				server.failures.forget(filled[1])
				log(server, failures[:1], l, false)
			}
			return clock()
		}
		log(server, failures, l, true)
		if !other {
			t.Fatal("the other scrape was not made between the first one's failures")
		}
		atOnce = logs.String()
		logs.Reset()
		for _, f := range failures {
			remembered = append(remembered, server.failures.remembers(ruleKey(f.Metric, f.Expression)))
		}
		log(server, failures[1:], l, true)
		return atOnce, logs.String(), remembered
	}
	key := failureKey(c.Name, target, "")
	atOnce, after, remembered := scraped(func(s *Server, failures []transform.RuleFailure, l collectLog, complete bool) {
		s.logRuleFailures(ctx, configRead{}, &c, failures, l, complete)
	}, collectLog{key: key, attrs: attrs}, func(metric, expression string) string {
		return ruleFailureKeyJoined(key.bytes, metric, expression, "")
	})
	wasKey := failureKeyWas(c.Name, target, "")
	wasAtOnce, wasAfter, wasRemembered := scraped(func(s *Server, failures []transform.RuleFailure, l collectLog, complete bool) {
		logRuleFailuresMakingEveryKey(ctx, s, configRead{}, &c, failures, l, complete)
	}, collectLog{key: key.rule(wasKey), attrs: attrs}, func(metric, expression string) string {
		return ruleFailureKeyWas(wasKey, metric, expression, "")
	})
	if atOnce != wasAtOnce || after != wasAfter {
		t.Errorf("the two scrapes at once log\n%s\nand the one after\n%s\nwant as the logging that held every failing rule's key\n%s\nand\n%s", atOnce, after, wasAtOnce, wasAfter)
	}
	if !reflect.DeepEqual(remembered, []bool{true, true}) || !reflect.DeepEqual(wasRemembered, remembered) {
		t.Errorf("after the two scrapes at once the failures of the two rules are remembered: %v, and by the former logging %v; want both by both", remembered, wasRemembered)
	}
	const recovery = `"msg":"metric extraction recovered"`
	if strings.Contains(atOnce, recovery) || strings.Count(atOnce, `"level":"WARN","msg":"metric extraction failed"`) != 3 {
		t.Errorf("the two scrapes at once log\n%s\nwant the first rule's failure twice and the second's once, each in full, and no recovery", atOnce)
	}
	if strings.Count(after, recovery) != 1 || !strings.Contains(after, `"metric":"m0","stage":"metric"`) {
		t.Errorf("the scrape on which the first rule works logs\n%s\nwant its recovery, once", after)
	}
}
