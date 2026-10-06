//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The schedule of the static targets tells whether a target's collector
// changed by the fingerprints the reload made, which the configuration
// followed keeps (followedConfig.fingerprintOf, statictargetschedule.go). The
// tests here hold that it plans what it planned while it encoded every
// definition itself, and, by counting where a definition is encoded and
// never by time, that it encodes none after a reload.

// planFollowedAsItWas is targetSchedule.planFollowed as it was while the
// schedule encoded and hashed the definition of every collector it asked
// about itself, after every reload.
func planFollowedAsItWas(s *targetSchedule, cfg *model.Config, targets []model.StaticTarget, followed *followedConfig, now time.Time) (due, skipped []dueTarget, next time.Time) {
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
	for i := range targets {
		target := targets[i]
		interval := time.Duration(target.Interval)
		if interval <= 0 {
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

// scheduleReloads generates the reloads of the test below: the collectors
// configured, each in one of three definitions, and the static targets in
// force, each of one of the collectors or of one that was never configured.
type scheduleReloads struct {
	t          *testing.T
	random     *rand.Rand
	collectors map[string]int
	targets    map[string]model.StaticTarget
}

var (
	scheduleReloadCollectors = []string{"a", "b", "c", "d", "e"}
	scheduleReloadTargets    = []string{"t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7"}
	scheduleReloadIntervals  = []time.Duration{0, time.Second, 7 * time.Second, time.Minute, time.Hour}
)

// changeCollectors keeps most collectors as they are, removes some, and
// adds, brings back or changes others; one is always left.
func (g *scheduleReloads) changeCollectors() {
	for _, name := range scheduleReloadCollectors {
		switch g.random.IntN(6) {
		case 0:
			delete(g.collectors, name)
		case 1, 2:
			g.collectors[name] = g.random.IntN(3)
		}
	}
	if len(g.collectors) == 0 {
		g.collectors[scheduleReloadCollectors[0]] = 0
	}
}

// changeTargets keeps most static targets as they are, removes some, adds or
// brings back others, moves some to another collector, and changes the
// labels or the interval of others.
func (g *scheduleReloads) changeTargets() {
	collector := func() string {
		if g.random.IntN(8) == 0 {
			return "never_configured"
		}
		return scheduleReloadCollectors[g.random.IntN(len(scheduleReloadCollectors))]
	}
	for _, name := range scheduleReloadTargets {
		target, present := g.targets[name]
		switch choice := g.random.IntN(10); {
		case choice == 0:
			delete(g.targets, name)
			continue
		case choice == 1 || choice == 2 && !present:
			target = model.StaticTarget{Name: name, Collector: collector(), Target: "http://" + name + ".invalid", Interval: model.Duration(scheduleReloadIntervals[g.random.IntN(len(scheduleReloadIntervals))])}
		case !present:
			continue
		case choice == 2:
			target.Collector = collector()
		case choice == 3:
			target.Labels = map[string]string{"site": string(rune('a' + g.random.IntN(3)))}
		case choice == 4:
			target.Interval = model.Duration(scheduleReloadIntervals[g.random.IntN(len(scheduleReloadIntervals))])
		default:
			continue
		}
		g.targets[name] = target
	}
}

// config is a configuration of the collectors, in an order of its own: a
// collector is seldom where it was in the configuration before.
func (g *scheduleReloads) config() *model.Config {
	g.t.Helper()
	names := model.SortedKeys(g.collectors)
	g.random.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	cfg := &model.Config{}
	for _, name := range names {
		c := testutil.Collector(name, "text")
		c.Metrics[0].Name = fmt.Sprintf("%s_v%d", name, g.collectors[name])
		cfg.Collectors = append(cfg.Collectors, c)
	}
	if err := config.Validate(cfg); err != nil {
		g.t.Fatal(err)
	}
	return cfg
}

// file is a static target file of the targets, in an order of its own.
func (g *scheduleReloads) file() *model.StaticTargetFile {
	names := model.SortedKeys(g.targets)
	g.random.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	file := &model.StaticTargetFile{}
	for _, name := range names {
		file.Targets = append(file.Targets, g.targets[name])
	}
	return file
}

// reload puts something in force on server: a configuration as a reload
// does, its fingerprints prepared, or as a test does, without; a static
// target file alone; or both. Most are followed at once, as the manager has
// the server follow its reloads, and the others when the schedule next
// looks, together with what came after them.
func (g *scheduleReloads) reload(server *Server) {
	switch g.random.IntN(4) {
	case 0:
		g.changeCollectors()
		cfg := g.config()
		server.prepareReload(cfg, server.manager.StaticTargetFile())
		installConfig(server, cfg)
	case 1:
		g.changeCollectors()
		installConfig(server, g.config())
	case 2:
		g.changeTargets()
		server.manager.SetTargets("", g.file())
	default:
		g.changeCollectors()
		g.changeTargets()
		cfg, file := g.config(), g.file()
		server.prepareReload(cfg, file)
		server.manager.SetTargets("", file)
		installConfig(server, cfg)
	}
	if g.random.IntN(4) != 0 {
		server.followReload()
	}
}

// describeLook is all that a look of the schedule s at now left: the scrapes
// due, with their deadlines and what each was given, the turns skipped, when
// to look next, and every target's place in the schedule, with whether it is
// the place the target had before the look, was.
func describeLook(s *targetSchedule, was map[string]*staticTargetState, due, skipped []dueTarget, next, now time.Time) string {
	var b strings.Builder
	when := func(at time.Time) string {
		if at.IsZero() {
			return "never"
		}
		return at.Sub(now).String()
	}
	for i := range due {
		d := &due[i]
		fmt.Fprintf(&b, "due %s of %s by %s, with the configuration planned with %v and its place in the schedule %v\n", d.target.Name, d.target.Collector, when(d.deadline), d.config == s.config, d.state == s.states[d.target.Name])
	}
	for i := range skipped {
		d := &skipped[i]
		fmt.Fprintf(&b, "skipped %s, with its place in the schedule %v\n", d.target.Name, d.state == s.states[d.target.Name])
	}
	fmt.Fprintf(&b, "next %s\n", when(next))
	for _, name := range model.SortedKeys(s.states) {
		st := s.states[name]
		fmt.Fprintf(&b, "%s: kept %v, target %+v, collector %q, since %d, interval %s, next %s, cadence %s, put off %v, running %v, awaited %v, waiting %v until %s\n",
			name, was[name] == st, st.target, st.collector, st.since, st.interval, when(st.next), when(st.cadence), st.putOff, st.running.Load(), st.awaited.Load(), st.waiting, when(st.until))
	}
	return b.String()
}

// The schedule that reads the fingerprints a reload made plans what the one
// that encoded every definition itself planned. Over generated runs of
// reloads — of the configuration, prepared as a reload prepares it or put in
// force without, of the static target file alone, and of both; collectors
// and targets kept, changed, removed, added and brought back, a target moved
// to another collector or to one never configured, the collectors and the
// targets in another order each time; none, one or two reloads between two
// looks, followed at once or only at the look; a probe asking for a
// fingerprint now and then — with looks at uneven times and scrapes that end
// some looks after they began, every look leaves what the former schedule,
// kept here, leaves beside it: the scrapes due with their deadlines and
// their configuration, the turns skipped, the time of the next look, which
// targets kept their place and which started anew, and every place with its
// target, its collector's fingerprint, its generation and its times. Half
// the runs plan as the scrape loop does, with the configuration followed;
// a quarter as a caller that follows none, which encodes as it did; and a
// quarter with the configuration followed at the look before, another than
// the one planned with whenever a reload came between, whose fingerprints
// must not be read for it. It is 12 runs of 60 looks, and 4 runs of 40 under
// the race detector. The collectors are found by their names
// (fingerprintGeneration.place) and no longer by going through them: more
// often than once a look a target's collector stands in the configuration
// after one that has no static target, and as often a target names a
// collector the configuration does not have, one never configured or one a
// reload removed, which has no fingerprint, the empty one, before and after.
func TestTheScheduleThatReadsTheReloadsFingerprintsPlansAsItDid(t *testing.T) {
	testutil.CaptureLogs(t)
	steps := []time.Duration{0, 100 * time.Millisecond, time.Second, 3 * time.Second, firstScrapeWindow, time.Minute}
	type inFlight struct {
		now, was *staticTargetState
		ends     int
	}
	runs, looks := alloctest.UnlessRaced(12, 4), alloctest.UnlessRaced(60, 40)
	// encodes counts the definitions encoded by whichever schedule plans.
	var encodes *int
	hook := func() {
		if encodes != nil {
			*encodes++
		}
	}
	fingerprintedHook.Store(&hook)
	t.Cleanup(func() { fingerprintedHook.Store(nil) })
	// By how the run plans: the definitions the schedule encoded, and those
	// the former one did.
	var encodedNow, encodedWas [3]int
	started, anew, kept, compared := 0, 0, 0, 0
	// The targets planned whose collector is not where a count of the
	// collectors with a static target would put it, one without a target
	// standing before it in the configuration, and those whose collector the
	// configuration does not have, never configured or removed by a reload.
	displaced, orphaned := 0, 0
	for run := range runs {
		how := []int{0, 0, 1, 2}[run%4]
		random := rand.New(rand.NewPCG(uint64(run), 33))
		g := &scheduleReloads{t: t, random: random, collectors: map[string]int{"a": 0, "b": 0, "c": 0}, targets: map[string]model.StaticTarget{}}
		for i, name := range scheduleReloadTargets[:5] {
			g.targets[name] = model.StaticTarget{Name: name, Collector: scheduleReloadCollectors[i%3], Target: "http://" + name + ".invalid", Interval: model.Duration(scheduleReloadIntervals[1+i%4])}
		}
		manager := config.NewManager(g.config(), "", testutil.QuietLogger(t))
		manager.SetTargets("", g.file())
		server := NewServer(manager, "python3", testutil.QuietLogger(t))
		schedule, former := newTargetSchedule(), newTargetSchedule()
		now := time.Unix(1_000_000, 0)
		var running []inFlight
		var lookedAt *followedConfig
		for look := range looks {
			now = now.Add(steps[random.IntN(len(steps))])
			for range random.IntN(3) {
				g.reload(server)
			}
			if cfg := server.manager.Get(); random.IntN(3) == 0 {
				// A probe of a caching collector asks for its fingerprint.
				server.fingerprints.fingerprint(cfg, &cfg.Collectors[random.IntN(len(cfg.Collectors))])
			}
			running = slices.DeleteFunc(running, func(scrape inFlight) bool {
				if scrape.ends > look {
					return false
				}
				scrape.now.scrapeEnded()
				scrape.was.scrapeEnded()
				return true
			})
			followed := server.followedInForce()
			targets := staticTargetsOf(followed.targets)
			given := followed
			switch how {
			case 1:
				given = nil
			case 2:
				given = lookedAt
			}
			lookedAt = followed
			targeted, configured := map[string]bool{}, testutil.CollectorNames(followed.config)
			for i := range targets {
				targeted[targets[i].Collector] = true
			}
			for i := range targets {
				switch at := slices.Index(configured, targets[i].Collector); {
				case at < 0:
					orphaned++
				case slices.ContainsFunc(configured[:at], func(before string) bool { return !targeted[before] }):
					displaced++
				}
			}
			places, formerPlaces := maps.Clone(schedule.states), maps.Clone(former.states)
			encodes = &encodedNow[how]
			due, skipped, next := schedule.planFollowed(followed.config, targets, given, now)
			encodes = &encodedWas[how]
			wasDue, wasSkipped, wasNext := planFollowedAsItWas(former, followed.config, targets, given, now)
			encodes = nil
			got, want := describeLook(schedule, places, due, skipped, next, now), describeLook(former, formerPlaces, wasDue, wasSkipped, wasNext, now)
			if got != want || len(due) != len(wasDue) || schedule.config != former.config || len(schedule.targets) != len(former.targets) {
				t.Fatalf("run %d, look %d: the schedule is left, where it first differs,\n%s\nwant, as it was left,\n%s", run, look, firstDifference(got, want), firstDifference(want, got))
			}
			for i := range due {
				due[i].state.running.Store(true)
				wasDue[i].state.running.Store(true)
				running = append(running, inFlight{now: due[i].state, was: wasDue[i].state, ends: look + 1 + random.IntN(4)})
			}
			started += len(due)
			for name, state := range schedule.states {
				switch place := places[name]; {
				case place == state:
					kept++
				case place != nil:
					anew++
				}
				if state.collector != "" {
					compared++
				}
			}
		}
	}
	// What the runs must have had to show anything: a scrape started and a
	// target started anew at every third look, and more kept in their place.
	if floor := runs * looks / 3; started < floor || anew < floor || kept < 5*floor || compared < 5*floor {
		t.Errorf("the %d runs started %d scrapes, started %d targets anew and kept %d in their place, %d of them with a collector's fingerprint; too few of one of them to show anything", runs, started, anew, kept, compared)
	}
	// And for the collectors to have been found by their names where it
	// matters: a target at every look whose collector stands after one without
	// a target, and one whose collector the configuration does not have.
	if floor := runs * looks; displaced < floor || orphaned < floor {
		t.Errorf("the %d runs planned %d targets whose collector stands after one without a static target and %d whose collector is not configured; too few of one of them to show anything", runs, displaced, orphaned)
	}
	// The schedule that plans as the loop does reads what the reloads made,
	// and one that follows no configuration encodes what it did.
	if encodedWas[0] < runs*looks/3 || encodedNow[0]*4 > encodedWas[0] {
		t.Errorf("planning with the configuration followed, the schedule encoded %d collectors' definitions where it encoded %d; want a quarter of them at most, those of the configurations put in force without a reload", encodedNow[0], encodedWas[0])
	}
	if encodedWas[1] == 0 || encodedNow[1] != encodedWas[1] {
		t.Errorf("planning with no configuration followed, the schedule encoded %d collectors' definitions where it encoded %d; want as many", encodedNow[1], encodedWas[1])
	}
	if encodedWas[2] == 0 || encodedNow[2] > encodedWas[2] {
		t.Errorf("planning with the configuration followed at the look before, the schedule encoded %d collectors' definitions where it encoded %d; want no more", encodedNow[2], encodedWas[2])
	}
}

// collectorsOfDocument names the collectors of a document of
// testutil.CollectorsDocument's, in its order.
func collectorsOfDocument(document string) []string {
	var names []string
	for line := range strings.Lines(document) {
		if name, is := strings.CutPrefix(line, "  - name: "); is {
			names = append(names, strings.TrimSpace(name))
		}
	}
	return names
}

// aTargetOfEach is a static target file with one target of each of the
// named collectors, scraped every hour, all at address.
func aTargetOfEach(collectors []string, address string) string {
	var b strings.Builder
	b.WriteString("interval: 1h\ntargets:\n")
	for _, name := range collectors {
		fmt.Fprintf(&b, "  - name: t_%s\n    collector: %s\n    target: %s\n", name, name, address)
	}
	return b.String()
}

// The schedule encodes no collector's definition after a reload. Every one
// of 100 collectors (10 under the race detector) has a static target; a
// reload that leaves the collectors as they were, changes every one, or
// removes half and adds as many, their targets with them, encodes each
// collector of its configuration once, before it is in force, and the look
// the schedule then takes, as the scrape loop takes it, encodes none, where
// it encoded every one of them again: each collector is encoded once for a
// reload in all. So it is for the first reload after the start: the
// fingerprints the schedule asked for at its first look, one for each
// collector of the configuration the exporter started with, are kept with
// that configuration, and the reload does not make them again. The look
// keeps the targets of the collectors the reload left as they were in their
// place, and starts the others anew.
func TestTheScheduleEncodesNoCollectorAfterAReload(t *testing.T) {
	n := alloctest.UnlessRaced(100, 10)
	for _, shape := range followBenchShapes {
		documents := [2]string{shape.first(n), shape.second(n)}
		targets := [2]string{aTargetOfEach(collectorsOfDocument(documents[0]), "http://127.0.0.1:9"), aTargetOfEach(collectorsOfDocument(documents[1]), "http://127.0.0.1:9")}
		r := newReloadable(t, documents[0], targets[0])
		encoded, locked := countFingerprints(t, r.server)
		schedule := newTargetSchedule()
		now := time.Unix(1_000_000, 0)
		// look is the scrape loop's look at what is in force, and says how
		// many definitions it encoded and how many targets kept their place.
		look := func() (made int64, kept int) {
			before, places := encoded.Load(), maps.Clone(schedule.states)
			followed := r.server.followedInForce()
			schedule.planFollowed(followed.config, staticTargetsOf(followed.targets), followed, now)
			for name, state := range schedule.states {
				if places[name] == state {
					kept++
				}
			}
			return encoded.Load() - before, kept
		}
		if made, _ := look(); made != int64(n) || len(schedule.states) != n {
			t.Fatalf("%s: the schedule's first look encoded %d collectors' definitions for %d targets, want %d for as many: each collector of the configuration the exporter started with once", shape.name, made, len(schedule.states), n)
		}
		wantKept := map[string]int{"unchanged": n, "changed": 0, "half": n / 2}[shape.name]
		for reload := 1; reload <= 3; reload++ {
			encoded.Store(0)
			r.reloadBoth(documents[reload%2], targets[reload%2])
			byReload := encoded.Load()
			made, kept := look()
			if followed := r.server.followed.Load(); followed.config != r.manager.Get() || len(schedule.states) != n || kept != wantKept {
				t.Fatalf("%s: after reload %d its configuration is followed %v, and the schedule has %d targets, %d of them in the place they had; want %d, %d in their place", shape.name, reload, followed.config == r.manager.Get(), len(schedule.states), kept, n, wantKept)
			}
			if made != 0 {
				t.Errorf("%s: the schedule's look after reload %d encoded %d collectors' definitions, want none: the reload made their fingerprints", shape.name, reload, made)
			}
			if byReload != int64(n) {
				t.Errorf("%s: reload %d encoded %d collectors' definitions, want %d, each collector of its configuration once and none of the one before it, which the schedule had asked for", shape.name, reload, byReload, n)
			}
			if locked.Load() != 0 {
				t.Errorf("%s: reload %d and the look after it encoded %d collectors' definitions while the statistics lock was held, want none", shape.name, reload, locked.Load())
			}
		}
	}
}

// The scrape loop does so. With the loop running over four collectors, each
// with a static target scraped hourly, the start encodes each collector
// once, and a reload that changes every collector encodes each once more,
// before its configuration is in force: the loop, which then starts every
// target anew and scrapes it with its collector's new definition, encodes
// none, where it encoded every one again, and the reload none of the
// configuration the exporter started with, which the loop had asked for.
func TestTheScrapeLoopEncodesNoCollectorAfterAReload(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	collectors := followBenchNames(0, 4)
	names := soonScraped(t, time.Hour, len(collectors))
	var targets strings.Builder
	targets.WriteString("interval: 1h\ntargets:\n")
	for i, name := range names {
		fmt.Fprintf(&targets, "  - name: %s\n    collector: %s\n    target: %s\n", name, collectors[i], target.URL)
	}
	conf := testutil.CollectorsDocument(collectors...)
	r := newReloadable(t, conf, targets.String())
	encoded, _ := countFingerprints(t, r.server)
	// scraped says whether every target has the result of a scrape by its
	// collector's definition with the metric so ended.
	scraped := func(ending string) func() bool {
		return func() bool {
			for i, name := range names {
				want := []string{collectors[i] + ending + " 42", "http_exporter_target_up 1"}
				if !slices.Equal(publishedOf(r.server, name), want) {
					return false
				}
			}
			return true
		}
	}
	stop := runLoop(t, r.server)
	testutil.WaitFor(t, "the first scrape of every target", scraped("_value"))
	if got := encoded.Load(); got != int64(len(collectors)) {
		t.Errorf("the start of the scrape loop encoded %d collectors' definitions, want %d, each collector once", got, len(collectors))
	}
	encoded.Store(0)
	r.reloadTo(strings.ReplaceAll(conf, "_value\n", "_renamed\n"))
	testutil.WaitFor(t, "a scrape of every target by its collector's new definition", scraped("_renamed"))
	stop()
	if got := encoded.Load(); got != int64(len(collectors)) {
		t.Errorf("the reload and the scrape loop after it encoded %d collectors' definitions, want %d, each collector of the reloaded configuration once", got, len(collectors))
	}
}

// The fingerprints the schedule reads are the ones the probes use, kept with
// the configuration, and it makes none of them twice. Of three caching
// collectors one has a static target: the schedule's first look encodes
// that one, and a probe of it then encodes none, where it encoded it again;
// a probe of the second encodes its own. The static target file is then
// reloaded alone, moving the target to the second collector and adding one
// of the first and one of the third: the look starts the moved target anew
// and the two new ones, and encodes the third collector alone, which
// nothing had asked about in the configuration the exporter started with,
// where it encoded all three; a probe of the third then encodes none, and
// neither does another look.
func TestTheScheduleSharesItsFingerprintsWithTheProbes(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	r := newReloadable(t, cachedDocument("x", "y", "z"), staticDocument("one", "x", target.URL))
	encoded, _ := countFingerprints(t, r.server)
	schedule := newTargetSchedule()
	now := time.Unix(1_000_000, 0)
	look := func() {
		followed := r.server.followedInForce()
		schedule.planFollowed(followed.config, staticTargetsOf(followed.targets), followed, now)
	}
	encodes := func(what string, want int64, do func()) {
		t.Helper()
		encoded.Store(0)
		do()
		if got := encoded.Load(); got != want {
			t.Errorf("%s encoded %d collectors' definitions, want %d", what, got, want)
		}
	}
	probe := func(collector string) func() {
		return func() {
			t.Helper()
			if outcome := probeOnce(t, r.server, probePath(collector, target.URL, ""), nil); outcome.Code != http.StatusOK {
				t.Fatalf("the probe of %s was answered %d: %s", collector, outcome.Code, outcome.Body)
			}
		}
	}
	encodes("the schedule's first look, at one target", 1, look)
	cfg := r.manager.Get()
	if got, want := schedule.states["one"].collector, collectorFingerprint(model.CollectorByName(cfg, "x")); got == "" || got != want {
		t.Fatalf("the schedule has the fingerprint %q of the target's collector, want %q", got, want)
	}
	encodes("a probe of the collector the schedule asked about", 0, probe("x"))
	encodes("a probe of a collector nothing had asked about", 1, probe("y"))
	// The static target file alone: the configuration is refused, and the
	// one in force stays.
	before := r.server.followed.Load()
	one := schedule.states["one"]
	r.write(r.targets, staticDocument("one", "y", target.URL)+"  - name: two\n    collector: x\n    target: "+target.URL+"\n  - name: three\n    collector: z\n    target: "+target.URL+"\n")
	r.write(r.path, "collectors: [\n")
	if err := r.manager.Reload(config.ReloadTriggerSignal); err == nil {
		t.Fatal("a broken configuration was put in force")
	}
	if followed := r.server.followed.Load(); followed.config != before.config || followed.generation != before.generation+1 || len(staticTargetsOf(followed.targets)) != 3 {
		t.Fatalf("the reload of the static target file alone was not followed as one: generation %d after %d, the configuration as it was %v", followed.generation, before.generation, followed.config == before.config)
	}
	encodes("the schedule's look after the static target file was reloaded alone", 1, look)
	if len(schedule.states) != 3 || schedule.states["one"] == one {
		t.Fatalf("the look left %d targets in the schedule, the moved one in its place %v; want three, the moved one started anew", len(schedule.states), schedule.states["one"] == one)
	}
	for name, collector := range map[string]string{"one": "y", "two": "x", "three": "z"} {
		if got, want := schedule.states[name].collector, collectorFingerprint(model.CollectorByName(cfg, collector)); got != want {
			t.Errorf("the schedule has the fingerprint %q of the collector of %s, want %q, that of %s", got, name, want, collector)
		}
	}
	encodes("a probe of the collector the schedule was the first to ask about", 0, probe("z"))
	encodes("another look of the schedule", 0, look)
}

// The fingerprint the schedule is given for a collector is that of the
// collector in the configuration it plans with, whatever is followed: read
// where it is kept when that configuration is the one followed, each made
// once and the same the probes are given, and encoded afresh, every time,
// for another configuration, for a caller that follows none, and when the
// one followed keeps none. A configuration that has the collector at
// another place, with another definition, is never answered with what is
// kept for the one followed; a name the configuration does not have has no
// fingerprint, and encodes nothing. The schedule given a configuration and
// another one's following plans by the first.
func TestTheFingerprintOfANamedCollectorIsTheOneOfTheConfigurationPlannedWith(t *testing.T) {
	server, _ := newCacheTestServer(t, testutil.Collector("x", "text"), testutil.Collector("y", "text"))
	followed := server.followedInForce()
	changed := testutil.Collector("x", "text")
	changed.Metrics[0].Name = "renamed_value"
	other := &model.Config{Collectors: []model.Collector{testutil.Collector("y", "text"), changed}}
	if err := config.Validate(other); err != nil {
		t.Fatal(err)
	}
	var encoded atomic.Int64
	hook := func() { encoded.Add(1) }
	want := map[*model.Config]map[string]string{}
	for _, cfg := range []*model.Config{followed.config, other} {
		want[cfg] = map[string]string{"never_configured": ""}
		for i := range cfg.Collectors {
			want[cfg][cfg.Collectors[i].Name] = collectorFingerprint(&cfg.Collectors[i])
		}
	}
	if want[other]["x"] == want[followed.config]["x"] || want[other]["x"] == want[other]["y"] || want[other]["y"] != want[followed.config]["y"] {
		t.Fatal("the two configurations do not differ in the one collector, at another place: the test shows nothing")
	}
	fingerprintedHook.Store(&hook)
	t.Cleanup(func() { fingerprintedHook.Store(nil) })
	for _, c := range []struct {
		what    string
		given   *followedConfig
		cfg     *model.Config
		encodes [2]int64
	}{
		{"the configuration followed", followed, followed.config, [2]int64{1, 0}},
		{"another configuration than the one followed", followed, other, [2]int64{1, 1}},
		{"a caller that follows no configuration", nil, followed.config, [2]int64{1, 1}},
		{"a configuration followed that keeps no fingerprints", &followedConfig{config: other}, other, [2]int64{1, 1}},
	} {
		for _, name := range []string{"x", "y", "never_configured"} {
			for ask, encodes := range c.encodes {
				if name == "never_configured" {
					encodes = 0
				}
				encoded.Store(0)
				if got := c.given.fingerprintOf(c.cfg, name); got != want[c.cfg][name] || encoded.Load() != encodes {
					t.Errorf("%s, ask %d for %s: the fingerprint is %q after %d definitions encoded, want %q after %d", c.what, ask+1, name, got, encoded.Load(), want[c.cfg][name], encodes)
				}
			}
		}
	}
	encoded.Store(0)
	for i := range followed.config.Collectors {
		c := &followed.config.Collectors[i]
		if got := server.fingerprints.fingerprint(followed.config, c); got != want[followed.config][c.Name] || encoded.Load() != 0 {
			t.Errorf("a probe of %s is given the fingerprint %q after %d definitions encoded, want %q, the one made already", c.Name, got, encoded.Load(), want[followed.config][c.Name])
		}
	}
	// The schedule plans with the configuration it is given.
	targets := []model.StaticTarget{{Name: "one", Collector: "x", Target: "http://one.invalid", Interval: model.Duration(time.Minute)}}
	schedule := newTargetSchedule()
	schedule.planFollowed(other, targets, followed, time.Unix(1_000_000, 0))
	if got := schedule.states["one"].collector; got != want[other]["x"] {
		t.Errorf("planning with a configuration that is not the one followed, the schedule has the fingerprint %q of the target's collector, want %q, that of the configuration planned with", got, want[other]["x"])
	}
}

// targetsOfEach is a static target file with each static targets of every
// one of the named collectors, scraped every hour, all at address.
func targetsOfEach(collectors []string, each int, address string) string {
	var b strings.Builder
	b.WriteString("interval: 1h\ntargets:\n")
	for _, name := range collectors {
		for k := range each {
			fmt.Fprintf(&b, "  - name: t_%s_%d\n    collector: %s\n    target: %s\n", name, k, name, address)
		}
	}
	return b.String()
}

// countCollectorScans counts, until the test ends, the times the collectors
// of a configuration are gone through, to find one or to note where each is
// (collectorsScannedHook).
func countCollectorScans(t *testing.T) *atomic.Int64 {
	t.Helper()
	scans := &atomic.Int64{}
	hook := func() { scans.Add(1) }
	collectorsScannedHook.Store(&hook)
	t.Cleanup(func() { collectorsScannedHook.Store(nil) })
	return scans
}

// The collectors of a configuration are gone through once for it, and not
// for its static targets, its probes or its scrapes. Of 60 caching
// collectors with a static target each, of 240, and of 60 with five targets
// each (12, 48 and 12 under the race detector), the schedule's first look
// goes through the collectors of the configuration the exporter started
// with once, to note where each is by its name, and a reload that removes
// half of them and adds as many, their targets with them, goes through
// those of its configuration once, before it is in force. The look the
// schedule then takes, as the scrape loop takes it, goes through them not at
// all, where it went through them once for every collector with a static
// target, so as many times as there are collectors: four times the
// collectors made sixteen times the names compared. Neither does a probe of
// the last collector, nor the scrape of the last static target, each of
// which went through them twice, for the collector and for its fingerprint.
// A caller that follows no configuration goes through them once for each
// collector it asks about, as before, however many targets share it.
func TestTheCollectorsOfAConfigurationAreGoneThroughOnce(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	scans := countCollectorScans(t)
	n := alloctest.UnlessRaced(60, 12)
	for _, size := range []struct{ collectors, each int }{{n, 1}, {4 * n, 1}, {n, 5}} {
		what := fmt.Sprintf("%d collectors with %d static targets each", size.collectors, size.each)
		names := [2][]string{followBenchNames(0, size.collectors), followBenchNames(size.collectors/2, size.collectors)}
		documents := [2]string{cachedDocument(names[0]...), cachedDocument(names[1]...)}
		targets := [2]string{targetsOfEach(names[0], size.each, target.URL), targetsOfEach(names[1], size.each, target.URL)}
		r := newReloadable(t, documents[0], targets[0])
		schedule := newTargetSchedule()
		now := time.Unix(1_000_000, 0)
		// look is the scrape loop's look at what is in force, and says how
		// many times it went through the collectors.
		look := func() int64 {
			before := scans.Load()
			followed := r.server.followedInForce()
			schedule.planFollowed(followed.config, staticTargetsOf(followed.targets), followed, now)
			return scans.Load() - before
		}
		scans.Store(0)
		byLook := look()
		if len(schedule.states) != size.collectors*size.each {
			t.Fatalf("%s: the schedule's first look left %d targets in the schedule, want %d", what, len(schedule.states), size.collectors*size.each)
		}
		if byLook != 1 {
			t.Errorf("%s: the schedule's first look went through the collectors %d times, want once", what, byLook)
		}
		for reload := 1; reload <= 3; reload++ {
			scans.Store(0)
			r.reloadBoth(documents[reload%2], targets[reload%2])
			byReload := scans.Load()
			byLook = look()
			followed := r.server.followedInForce()
			if followed.config != r.manager.Get() || len(schedule.states) != size.collectors*size.each {
				t.Fatalf("%s: after reload %d its configuration is followed %v and the schedule has %d targets, want %d", what, reload, followed.config == r.manager.Get(), len(schedule.states), size.collectors*size.each)
			}
			if byReload != 1 || byLook != 0 {
				t.Errorf("%s: reload %d went through the collectors %d times and the schedule's look after it %d times; want once and not at all", what, reload, byReload, byLook)
			}
			// The last collector, and the last static target, which is one of
			// its: the farthest to go through the collectors for.
			last := names[reload%2][size.collectors-1]
			static := staticTargetsOf(followed.targets)
			scans.Store(0)
			if outcome := probeOnce(t, r.server, probePath(last, target.URL, ""), nil); outcome.Code != http.StatusOK {
				t.Fatalf("%s: the probe of %s after reload %d was answered %d: %s", what, last, reload, outcome.Code, outcome.Body)
			}
			if got := scans.Load(); got != 0 {
				t.Errorf("%s: a probe of the last collector after reload %d went through the collectors %d times, want not at all", what, reload, got)
			}
			scans.Store(0)
			r.server.scrapeTargetSince(context.Background(), followed.config, followed.generation, static[len(static)-1])
			if published := publishedOf(r.server, static[len(static)-1].Name); static[len(static)-1].Collector != last || len(published) == 0 {
				t.Fatalf("%s: the scrape of the last static target, of %s, after reload %d published %v; want the result of a scrape of %s", what, static[len(static)-1].Collector, reload, published, last)
			}
			if got := scans.Load(); got != 0 {
				t.Errorf("%s: the scrape of the last static target after reload %d went through the collectors %d times, want not at all", what, reload, got)
			}
		}
		followed := r.server.followedInForce()
		scans.Store(0)
		newTargetSchedule().plan(followed.config, staticTargetsOf(followed.targets), now)
		if got := scans.Load(); got != int64(size.collectors) {
			t.Errorf("%s: a schedule that follows no configuration went through the collectors %d times at its first look, want %d, once for each collector", what, got, size.collectors)
		}
	}
}
