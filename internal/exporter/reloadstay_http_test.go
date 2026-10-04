//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Every stay of a static target has a first scrape of its own, and what a
// scrape tells the failure log is held to the stay it read as the result it
// publishes is (reconcile.go, statictargetschedule.go).

// hourlyDocument is a static target file with the target one of the
// collector x, scraped every hour, labelled site when it is not empty.
func hourlyDocument(name, address, site string) string {
	document := "interval: 1h\ntargets:\n  - name: " + name + "\n    collector: x\n    target: " + address + "\n"
	if site != "" {
		document += "    labels:\n      site: " + site + "\n"
	}
	return document
}

// A target whose scrape is in flight while reloads change it and change it
// back, change its collector and change it back, or remove it and bring it
// back, all before the schedule looks again, is defined as the schedule has
// it and is started again all the same: its first scrape comes within
// firstScrapeWindow of that look, waits for the scrape in flight, which
// publishes nothing, and is the only one until the cadence's turn. Compared
// by its definition alone the target was left to its next turn, an interval
// away, with nothing on the endpoint until then, its
// http_exporter_target_up neither.
func TestATargetChangedAndChangedBackBetweenTwoLooksIsScrapedAnew(t *testing.T) {
	for _, reloads := range []string{"change the collector and change it back", "change the target and change it back", "remove the target and bring it back"} {
		t.Run(reloads, func(t *testing.T) {
			testutil.CaptureLogs(t)
			target := textTarget(t, "value=42\n")
			conf, targets := testutil.CollectorsDocument("x"), hourlyDocument("one", target.URL, "")
			r := newReloadable(t, conf, targets)
			schedule := newTargetSchedule()
			look := func(now time.Time) (due []dueTarget, generation uint64, next time.Time) {
				followed := r.server.followedInForce()
				due, _, next = schedule.planFollowed(followed.config, staticTargetsOf(followed.targets), followed, now)
				return due, followed.generation, next
			}
			start := time.Unix(1_000_000, 0)
			_, _, first := look(start)
			due, read, _ := look(first)
			if len(due) != 1 {
				t.Fatalf("at its first scrape's time %d scrapes are due, want that of the target", len(due))
			}
			inFlight := due[0]
			inFlight.state.running.Store(true)
			switch reloads {
			case "change the collector and change it back":
				r.reloadBoth(strings.Replace(conf, "x_value", "renamed_value", 1), targets)
			case "change the target and change it back":
				r.reloadBoth(conf, hourlyDocument("one", target.URL, "b"))
			default:
				r.reloadBoth(conf, hourlyDocument("another", target.URL, ""))
				// A read of the endpoint forgets the results of the targets
				// that are gone.
				getPath(r.server, http.MethodGet, DefaultStaticTargetsPath)
			}
			r.reloadBoth(conf, targets)

			now := first.Add(scheduleCheckInterval)
			due, _, next := look(now)
			if len(due) != 0 || next.After(now.Add(firstScrapeWindow)) {
				t.Fatalf("at the look after the reloads %d scrapes are due and the next is due %s on; want none while the scrape in flight runs, and the target's own first scrape within %s", len(due), next.Sub(now), firstScrapeWindow)
			}
			// That scrape comes due, and waits for the one in flight.
			now = now.Add(firstScrapeWindow)
			if due, _, _ = look(now); len(due) != 0 {
				t.Fatalf("%d scrapes are due while the scrape begun before the reloads still runs, want none", len(due))
			}
			r.server.scrapeTargetSince(context.Background(), inFlight.config, read, inFlight.target)
			inFlight.state.scrapeEnded()
			if got := publishedOf(r.server, "one"); len(got) != 0 {
				t.Errorf("the scrape that read the target before the reloads published %v, want nothing", got)
			}
			now = now.Add(time.Millisecond)
			due, generation, _ := look(now)
			if len(due) != 1 || due[0].target.Name != "one" {
				t.Fatalf("once the scrape in flight has ended %d scrapes are due, want the target's own first", len(due))
			}
			due[0].state.running.Store(true)
			r.server.scrapeTargetSince(context.Background(), due[0].config, generation, due[0].target)
			due[0].state.scrapeEnded()
			if got, want := publishedOf(r.server, "one"), []string{"http_exporter_target_up 1", "x_value 42"}; !slices.Equal(got, want) {
				t.Errorf("the target's own first scrape published %v, want %v", got, want)
			}
			if body := getPath(r.server, http.MethodGet, DefaultStaticTargetsPath).Body.String(); !strings.Contains(body, `static_target="one"`) {
				t.Errorf("the endpoint lacks the target:\n%s", body)
			}
			// And it is the only one: the cadence takes over, a whole
			// interval on or later.
			now = now.Add(scheduleCheckInterval)
			if due, _, next = look(now); len(due) != 0 || next.Sub(now) < time.Hour-firstScrapeWindow {
				t.Errorf("after the target's first scrape %d more are due, the next %s on; want none before the cadence's turn", len(due), next.Sub(now))
			}
		})
	}
}

// The scrape loop does so. The first scrape of a target scraped hourly waits
// for its slot while reloads change the target and change it back, just after
// the loop has looked, and so within its second: that scrape goes to the
// target and publishes nothing, and the target is scraped anew at once, and
// once, so that the endpoint has what that scrape made. On a machine so slow
// that the loop looks between the two reloads it sees the target changed, and
// starts it again for that: the test then shows less, and passes all the
// same.
func TestTheScrapeLoopScrapesAnewATargetChangedBackWhileItsScrapeRan(t *testing.T) {
	testutil.CaptureLogs(t)
	name := soonScraped(t, time.Hour, 1)[0]
	// Each request is answered with its number.
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=" + strconv.FormatInt(hits.Add(1), 10) + "\n"))
	}))
	defer target.Close()
	reached, resume := holdFirstScrapeOf(t, name)
	conf := testutil.CollectorsDocument("x")
	r := newReloadable(t, conf, hourlyDocument(name, target.URL, ""))
	stop := runLoop(t, r.server)
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatal("the target's scrape never came due")
	}
	r.reloadBoth(conf, hourlyDocument(name, target.URL, "b"))
	r.reloadBoth(conf, hourlyDocument(name, target.URL, ""))
	resume()
	want := []string{"http_exporter_target_up 1", "x_value 2"}
	testutil.WaitFor(t, "the target's own first scrape after the reloads to be published", func() bool { return slices.Equal(publishedOf(r.server, name), want) })
	stop()
	if got := publishedOf(r.server, name); !slices.Equal(got, want) || hits.Load() != 2 {
		t.Errorf("the target has %v published after %d requests; want %v after two, that of the scrape begun before the reloads and that of the target's own first", got, hits.Load(), want)
	}
}

// planAsItWas is targetSchedule.plan as it was while a target was started
// again only for a definition other than its state's.
func planAsItWas(s *targetSchedule, cfg *model.Config, targets []model.StaticTarget, now time.Time) (due, skipped []dueTarget, next time.Time) {
	reloaded := cfg != s.config
	changed := reloaded || len(targets) != len(s.targets) || len(targets) > 0 && &targets[0] != &s.targets[0]
	s.config, s.targets = cfg, targets
	fingerprints := map[string]string{}
	fingerprint := func(collector string) string {
		value, known := fingerprints[collector]
		if !known {
			if cfg != nil {
				if c := model.CollectorByName(cfg, collector); c != nil {
					value = collectorFingerprint(c)
				}
			}
			fingerprints[collector] = value
		}
		return value
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		interval := time.Duration(target.Interval)
		if interval <= 0 {
			interval = time.Minute
		}
		seen[target.Name] = true
		state := s.states[target.Name]
		if state == nil || changed && (!reflect.DeepEqual(state.target, target) || reloaded && state.collector != fingerprint(target.Collector)) {
			running, awaited := &atomic.Bool{}, &atomic.Bool{}
			if state != nil {
				running, awaited = state.running, state.awaited
			}
			state = &staticTargetState{
				target:    target,
				collector: fingerprint(target.Collector),
				interval:  interval,
				next:      now.Add(scheduleOffset(target.Name, min(interval, firstScrapeWindow))),
				cadence:   now.Add(scheduleOffset(target.Name, interval)),
				running:   running,
				awaited:   awaited,
			}
			s.states[target.Name] = state
		}
		look, came := state.next, false
		switch {
		case now.Before(state.next):
		case !state.cadence.IsZero() && state.awaitScrape():
			look, state.putOff = now.Add(scheduleCheckInterval), true
		case !state.cadence.IsZero():
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

// stayVariants are the collectors and the static target files the reloads
// of the tests below choose among: the collector x as it is and with
// another rule, beside one no reload changes, and files that add, remove and
// change targets of either.
func stayVariants() (collectors [][]model.Collector, files [][]model.StaticTarget) {
	x, other, y := testutil.Collector("x", "text"), testutil.Collector("x", "text"), testutil.Collector("y", "text")
	other.Metrics[0].Name = "renamed_value"
	one := model.StaticTarget{Name: "one", Collector: "x", Target: "http://one.invalid", Interval: model.Duration(time.Minute)}
	two := model.StaticTarget{Name: "two", Collector: "y", Target: "http://two.invalid", Interval: model.Duration(7 * time.Second)}
	three := model.StaticTarget{Name: "three", Collector: "x", Target: "http://three.invalid", Interval: model.Duration(time.Second)}
	labelled, slower, moved := one, one, one
	labelled.Labels = map[string]string{"site": "b"}
	slower.Interval = model.Duration(time.Hour)
	moved.Collector = "y"
	return [][]model.Collector{{x, y}, {other, y}}, [][]model.StaticTarget{{one, two}, {one, two, three}, {labelled, two}, {slower, three}, {moved}, {two}, nil}
}

// While the schedule looks after every reload, it gives what it gave before
// it knew of generations: over generated runs of reloads of the
// configuration and of the static target file, among files that add, remove
// and change targets and collectors, with looks at uneven times and scrapes
// that end some looks after they began, the scrapes due, with their
// deadlines, the turns skipped and the time of the next look are those of
// the schedule as it was. So a target that a reload changed once is started
// once, one that no reload changed is not started again, and the first
// scrape after the start comes as it did.
func TestTheScheduleThatLooksAfterEveryReloadPlansAsItDid(t *testing.T) {
	testutil.CaptureLogs(t)
	collectors, files := stayVariants()
	steps := []time.Duration{0, 100 * time.Millisecond, time.Second, 3 * time.Second, firstScrapeWindow, time.Minute}
	type inFlight struct {
		now, was *staticTargetState
		ends     int
	}
	started := 0
	for run := range 25 {
		random := rand.New(rand.NewPCG(uint64(run), 12))
		server, manager := newCacheTestServer(t, collectors[0]...)
		manager.SetTargets("", &model.StaticTargetFile{Targets: slices.Clone(files[0])})
		schedule, former := newTargetSchedule(), newTargetSchedule()
		now := time.Unix(1_000_000, 0)
		var running []inFlight
		for step := range 80 {
			now = now.Add(steps[random.IntN(len(steps))])
			switch random.IntN(4) {
			case 0:
				reloadTo(t, server, collectors[random.IntN(len(collectors))]...)
			case 1:
				server.manager.SetTargets("", &model.StaticTargetFile{Targets: slices.Clone(files[random.IntN(len(files))])})
			}
			running = slices.DeleteFunc(running, func(scrape inFlight) bool {
				if scrape.ends > step {
					return false
				}
				scrape.now.scrapeEnded()
				scrape.was.scrapeEnded()
				return true
			})
			followed := server.followedInForce()
			targets := staticTargetsOf(followed.targets)
			due, skipped, next := schedule.planFollowed(followed.config, targets, followed, now)
			wasDue, wasSkipped, wasNext := planAsItWas(former, followed.config, targets, now)
			describe := func(due, skipped []dueTarget, next time.Time) string {
				var b strings.Builder
				for _, d := range due {
					fmt.Fprintf(&b, "due %s by %s; ", d.target.Name, d.deadline.Sub(now))
				}
				for _, d := range skipped {
					fmt.Fprintf(&b, "skipped %s; ", d.target.Name)
				}
				return b.String() + "next " + next.Sub(now).String()
			}
			if got, want := describe(due, skipped, next), describe(wasDue, wasSkipped, wasNext); got != want || len(due) != len(wasDue) {
				t.Fatalf("run %d, look %d: the schedule plans\n%s\nwant, as it did,\n%s", run, step, got, want)
			}
			for i := range due {
				due[i].state.running.Store(true)
				wasDue[i].state.running.Store(true)
				running = append(running, inFlight{now: due[i].state, was: wasDue[i].state, ends: step + 1 + random.IntN(4)})
			}
			started += len(due)
		}
	}
	if started < 500 {
		t.Errorf("the runs started %d scrapes, too few to show anything", started)
	}
}

// What a trip tells the failure log is held to what it was held to, but for
// a static target's scrape whose target left its stay: over every run of
// two reloads among the configurations and files above, each followed, a
// probe that read any earlier generation is held to its collector alone, as
// every trip was; a scrape of a target that has been in every file since as
// it was is held to the same; and a scrape of a target that has not is held
// to nothing, whatever its collector.
func TestWhatATripTellsTheFailureLogIsHeldToAsItWasButForATargetThatMoved(t *testing.T) {
	testutil.CaptureLogs(t)
	collectors, files := stayVariants()
	type choice struct{ collectors, file int }
	var choices []choice
	for c := range collectors {
		for f := range files {
			choices = append(choices, choice{c, f})
		}
	}
	const reloads = 2
	runs := 1
	for range reloads {
		runs *= len(choices)
	}
	for run := range runs {
		server, manager := newCacheTestServer(t, collectors[0]...)
		manager.SetTargets("", &model.StaticTargetFile{Targets: slices.Clone(files[0])})
		history := [][]model.StaticTarget{files[0]}
		generations := []uint64{server.reconcile().generation}
		for step, rest := 0, run; step < reloads; step, rest = step+1, rest/len(choices) {
			chosen := choices[rest%len(choices)]
			server.manager.SetTargets("", &model.StaticTargetFile{Targets: slices.Clone(files[chosen.file])})
			reloadTo(t, server, collectors[chosen.collectors]...)
			history = append(history, files[chosen.file])
			generations = append(generations, server.reconcile().generation)
			for age, generation := range generations {
				for _, collector := range []string{"x", "y", "never_configured"} {
					// formerly is what every trip was held to: its collector.
					formerly := server.readAt(generation).stands(collector)
					if got := server.readAt(generation).logStands(collector); got != formerly {
						t.Fatalf("run %d, reload %d: a probe of %s that read generation %d is held to %v, want %v as it was", run, step, collector, generation, got, formerly)
					}
					for _, name := range []string{"one", "two", "three", "never_configured"} {
						was := targetNamed(history[age], name)
						stayed := was != nil
						for _, later := range history[age:] {
							if in := targetNamed(later, name); in == nil || !reflect.DeepEqual(in, was) {
								stayed = false
							}
						}
						if got, want := server.readTargetAt(generation, name).logStands(collector), stayed && formerly; got != want {
							t.Fatalf("run %d, reload %d: a scrape of the target %s of %s that read generation %d, the target as it was since %v, is held to %v, want %v", run, step, name, collector, generation, stayed, got, want)
						}
					}
				}
			}
		}
	}
}

// A static target that a reload of the static target file changed, its
// collector as it was, is another target to the failure log, as one whose
// collector changed is. The first failure of the new definition, failing as
// the old one did, is logged in full and remembered as a first, where it was
// a repeat of the old definition's, logged at debug level only. A scrape
// that read the old definition and fails when the new one has recovered is
// not remembered, and is logged at debug level only, as superseded, so the
// next good scrape of the target logs no second recovery; and one that
// succeeds while the new definition fails is no recovery of it.
func TestAStaticTargetChangedInItsFileIsAnotherTargetToTheFailureLog(t *testing.T) {
	testutil.CaptureLogs(t)
	down, _, downFails := failableTarget(t)
	moved, _, movedFails := failableTarget(t)
	downFails.Store(true)
	movedFails.Store(true)
	conf := testutil.CollectorsDocument("x")
	r := newReloadable(t, conf, staticDocument("one", "x", down.URL))
	logs := loopLog(r.server)
	logged := func(level, msg string) int {
		return strings.Count(logs.String(), `"level":"`+level+`","msg":"`+msg+`"`)
	}
	oldCfg, oldFile, oldGeneration := r.server.inForce()
	late := func() { r.server.scrapeTargetSince(context.Background(), oldCfg, oldGeneration, oldFile.Targets[0]) }
	late()
	if remembered := rememberedOf(r.server, "x"); !slices.Equal(remembered, []string{"http_status x1"}) || logged("ERROR", "static target scrape failed") != 1 {
		t.Fatalf("the target's failure is remembered as %v, want once, and logged once:\n%s", remembered, logs)
	}
	// The target is pointed elsewhere, which fails as well.
	r.reloadBoth(conf, staticDocument("one", "x", moved.URL))
	if remembered := rememberedOf(r.server, "x"); len(remembered) != 0 {
		t.Errorf("once the reload changed the target its failures %v are still remembered", remembered)
	}
	cfg, file, generation := r.server.inForce()
	inForce := func() { r.server.scrapeTargetSince(context.Background(), cfg, generation, file.Targets[0]) }
	inForce()
	if remembered := rememberedOf(r.server, "x"); !slices.Equal(remembered, []string{"http_status x1"}) || logged("ERROR", "static target scrape failed") != 2 {
		t.Errorf("the first failure of the changed target is remembered as %v, want as a first, and each definition's failure logged in full:\n%s", remembered, logs)
	}
	// A scrape of the old definition succeeds: no recovery of the new one.
	downFails.Store(false)
	late()
	if remembered := rememberedOf(r.server, "x"); !slices.Equal(remembered, []string{"http_status x1"}) || logged("INFO", "static target recovered") != 0 {
		t.Errorf("after the success of a scrape of the old definition the target has %v remembered, want its failure, and no recovery logged:\n%s", remembered, logs)
	}
	// The new definition recovers, and a scrape of the old one fails.
	movedFails.Store(false)
	inForce()
	if remembered := rememberedOf(r.server, "x"); len(remembered) != 0 || logged("INFO", "static target recovered") != 1 {
		t.Fatalf("after its own success the target has %v remembered, want nothing, and its recovery logged once:\n%s", remembered, logs)
	}
	downFails.Store(true)
	late()
	if remembered := rememberedOf(r.server, "x"); len(remembered) != 0 || logged("ERROR", "static target scrape failed") != 2 || !strings.Contains(logs.String(), `"superseded":true`) {
		t.Errorf("the failure of a scrape of the old definition is remembered as %v, or logged above debug level, or not as superseded:\n%s", remembered, logs)
	}
	inForce()
	if logged("INFO", "static target recovered") != 1 {
		t.Errorf("the next good scrape of the target logged a recovery from the old definition's failure:\n%s", logs)
	}
}

// Following a reload forgets what the failure log remembers under the name
// of a static target the reload removed or changed, whichever way the
// reload is followed: the static target file alone reloaded, the
// configuration reloaded with it and no collector changed, and a collector
// changed with it. What it remembers of a target the reload left as it was
// stays, and so does what the static targets endpoint remembers of a metric
// it left out, which is the endpoint's to settle, and a failure remembered
// under an address, which a probe shares.
func TestFollowingAReloadForgetsTheFailuresOfTheStaticTargetsItRetired(t *testing.T) {
	const address = "http://target.invalid"
	document := func(changed, removed bool) string {
		out := "interval: 1m\ntargets:\n  - name: kept\n    collector: x\n    target: " + address + "\n"
		if !removed {
			out += "  - name: gone\n    collector: x\n    target: " + address + "\n"
		}
		out += "  - name: changed\n    collector: x\n    target: " + address + "\n"
		if changed {
			out += "    labels:\n      site: b\n"
		}
		return out
	}
	for _, how := range []string{"the static target file alone", "the configuration with it, no collector changed", "a collector changed with it"} {
		logger := testutil.QuietLogger(t)
		conf := cachedDocument("x", "y")
		r := newReloadable(t, conf, document(false, false))
		remember := func(collector, target, file string) {
			r.server.failures.failed(logger, slog.LevelError, failureKey(collector, target, file), "static target scrape failed", "fetch", context.DeadlineExceeded)
		}
		for _, name := range []string{"kept", "gone", "changed"} {
			remember("x", staticTargetKey(name), "")
			remember("x", staticTargetKey(name), "schedule")
			remember("", staticTargetKey(name), "family demo")
		}
		remember("x", address, "\x00utf8")
		r.write(r.targets, document(true, true))
		switch how {
		case "the static target file alone":
			// The configuration is refused, and the one in force stays.
			r.write(r.path, "collectors: [\n")
			if err := r.manager.Reload(config.ReloadTriggerSignal); err == nil {
				t.Fatal("a broken configuration was put in force")
			}
			if followed := r.server.followed.Load(); followed.config != r.manager.Get() || followed.targets != r.manager.StaticTargetFile() || len(followed.targets.Targets) != 2 {
				t.Fatalf("%s: the reload of the static target file was not followed", how)
			}
		case "the configuration with it, no collector changed":
			r.reloadBoth(conf, document(true, true))
		default:
			r.reloadBoth(strings.Replace(conf, "y_value", "renamed_value", 1), document(true, true))
		}
		var remembered []string
		r.server.failures.mu.Lock()
		for key := range r.server.failures.entries {
			remembered = append(remembered, strings.ReplaceAll(key, "\x00", "|"))
		}
		r.server.failures.mu.Unlock()
		slices.Sort(remembered)
		want := []string{"x|" + address + "||utf8", "x|static target kept|", "x|static target kept|schedule", "|static target changed|family demo", "|static target gone|family demo", "|static target kept|family demo"}
		slices.Sort(want)
		if !slices.Equal(remembered, want) {
			t.Errorf("%s reloaded: the failure log remembers\n%q\nwant\n%q", how, remembered, want)
		}
	}
}

// A turn the schedule gave up is held to the target and the collector the
// loop read with it: read before a reload removed the target, changed it,
// or changed its collector, it is not remembered, and is logged at debug
// level only, as superseded; read as the target is in force, it is
// remembered and logged as skipped.
func TestASkippedTurnOfATargetAReloadRetiredIsNotRemembered(t *testing.T) {
	for _, reload := range []string{"removes the target", "changes the target", "changes the collector", "leaves both as they were"} {
		testutil.CaptureLogs(t)
		conf := testutil.CollectorsDocument("x")
		r := newReloadable(t, conf, staticDocument("one", "x", "http://target.invalid"))
		logs := loopLog(r.server)
		_, file, generation := r.server.inForce()
		turn := dueTarget{target: file.Targets[0], state: &staticTargetState{interval: time.Minute}}
		switch reload {
		case "removes the target":
			r.reloadBoth(conf, staticDocument("another", "x", "http://target.invalid"))
		case "changes the target":
			r.reloadBoth(conf, staticDocument("one", "x", "http://elsewhere.invalid"))
		case "changes the collector":
			r.reloadBoth(strings.Replace(conf, "x_value", "renamed_value", 1), staticDocument("one", "x", "http://target.invalid"))
		default:
			r.reloadBoth(conf, staticDocument("one", "x", "http://target.invalid"))
		}
		r.server.turnSkipped(generation, turn)
		remembered, text := rememberedOf(r.server, "x"), logs.String()
		if reload == "leaves both as they were" {
			if !slices.Equal(remembered, []string{"schedule x1"}) || !strings.Contains(text, `"level":"WARN","msg":"static target scrape skipped"`) {
				t.Errorf("a reload that %s: the skipped turn is remembered as %v, want once, and logged as skipped:\n%s", reload, remembered, text)
			}
			continue
		}
		if len(remembered) != 0 || !strings.Contains(text, `"level":"DEBUG","msg":"static target scrape skipped"`) || !strings.Contains(text, `"superseded":true`) {
			t.Errorf("a reload that %s: the skipped turn read before it is remembered as %v, want nothing, and logged at debug level as superseded:\n%s", reload, remembered, text)
		}
	}
}
