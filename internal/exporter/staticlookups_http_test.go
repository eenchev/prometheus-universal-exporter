//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A scrape of a static target that ends asks whether its target is in force
// where the names of the file followed are noted, and a read of the verbose
// self-metrics finds the collector of every static target where the
// configuration followed keeps its place (statictargetsendpoint.go,
// requeststats.go). The tests here hold that both are told what going
// through the targets and the collectors told them, through reloads too,
// and, by counting the times either is gone through and never by time, that
// neither is gone through for a scrape or for a read.

// staticRequestKeysAsItWas is Server.staticRequestKeys as it was, part of
// seedStaticRequests, while it went through the collectors of the
// configuration for every static target.
func staticRequestKeysAsItWas(s *Server) map[requestKey]bool {
	cfg, file := s.manager.InForce()
	keys := map[requestKey]bool{}
	for _, target := range staticTargetsOf(file) {
		c := model.CollectorByName(cfg, target.Collector)
		if c == nil {
			continue
		}
		overrides := fetch.TargetOverrides(&target)
		label, err := fetch.RequestLabelFor(target.Target, c, overrides)
		if err != nil {
			continue
		}
		keys[requestKey{Collector: c.Name, URL: label, Method: fetch.RequestMethodFor(c, overrides)}] = true
	}
	return keys
}

// seedStaticRequestsAsItWas is Server.seedStaticRequests as it was.
func seedStaticRequestsAsItWas(s *Server) { s.requests.setStatic(staticRequestKeysAsItWas(s)) }

// targetsOf is a static target file of the named targets, all of collector
// and at address, scraped every hour.
func targetsOf(collector, address string, names ...string) string {
	var b strings.Builder
	b.WriteString("interval: 1h\ntargets:\n")
	for _, name := range names {
		fmt.Fprintf(&b, "  - name: %s\n    collector: %s\n    target: %s\n", name, collector, address)
	}
	return b.String()
}

// A static target is told to be in force through reloads as it was: over a
// run of reloads of both files, of the static target file alone, of the
// configuration alone, of the same files read again and of files that are
// refused, every name a file of the run has, and one none has, is told in
// force exactly when going through the targets in force finds it — when
// the reload has read and checked its files and they are not yet in force
// (config.Manager's OnPrepare), when they are in force and the reload has
// not followed them yet (followPlannedHook), where the targets are gone
// through as they were when the file is another, and when the reload is
// done, where they are not. Every scrape that read its target before one of
// the reloads, however many reloads ago, and ends at one of the last two
// moments publishes exactly when the former lookup and the target's stay
// (targetStands) say it does: the run has scrapes that publish, scrapes of
// a target no longer in force, and scrapes of a target still in force under
// its name that a reload changed.
func TestAStaticTargetIsToldInForceThroughReloadsAsItWas(t *testing.T) {
	testutil.CaptureLogs(t)
	scans := countTargetScans(t)
	const address, moved = "http://127.0.0.1:9/a", "http://127.0.0.1:9/b"
	asked := []string{"t1", "t2", "t3", "t4", "t5", "never_configured", ""}
	r := newReloadable(t, cachedDocument("x", "y"), targetsOf("x", address, "t1", "t2", "t3"))
	server := r.server
	// check asks about every name, and says how many times the targets are
	// gone through for each ask.
	check := func(what string, perAsk int64) {
		t.Helper()
		for _, name := range asked {
			before := scans.Load()
			got, want := server.staticTargetInForce(name), staticTargetInForceAsItWas(server, name)
			if got != want {
				t.Errorf("%s: a target named %q is in force %v, want %v", what, name, got, want)
			}
			if gone := scans.Load() - before; gone != perAsk {
				t.Errorf("%s: the targets were gone through %d times to tell whether one is named %q, want %d", what, gone, name, perAsk)
			}
		}
	}
	// held are the scrapes that read their target before a reload, each
	// ended at every moment after it.
	type heldScrape struct {
		read   configRead
		target model.StaticTarget
	}
	var held []heldScrape
	hold := func() {
		followed := server.followedInForce()
		for _, target := range staticTargetsOf(followed.targets) {
			held = append(held, heldScrape{read: server.readTargetAt(followed.generation, target.Name), target: target})
		}
	}
	attempt, published, notInForce, changed := 0.0, 0, 0, 0
	end := func(what string) {
		t.Helper()
		for _, scrape := range held {
			attempt++
			inForce, stands := staticTargetInForceAsItWas(server, scrape.target.Name), scrape.read.targetStands(scrape.target.Collector, scrape.target.Name)
			server.publishStaticResult(scrape.read, scrape.target, otlpResourceIdentity{}, model.MetricSet{Metrics: []model.Metric{{Name: "attempt", Type: model.GaugeMetricType, Value: attempt}}}, time.Time{}, scrapeTime{})
			server.staticMu.Lock()
			got := slices.ContainsFunc(server.staticResults[scrape.target.Name].Metrics, func(m model.Metric) bool { return m.Name == "attempt" && m.Value == attempt })
			server.staticMu.Unlock()
			if got != (inForce && stands) {
				t.Errorf("%s: a scrape that read the target %q at generation %d published %v; a target of the name is in force %v and the target stands as read %v", what, scrape.target.Name, scrape.read.generation, got, inForce, stands)
			}
			switch {
			case got:
				published++
			case !inForce:
				notInForce++
			case !stands:
				changed++
			}
		}
	}
	// step is the reload under way, and fileBefore the static target file
	// in force before it.
	var step string
	var fileBefore *model.StaticTargetFile
	r.manager.OnPrepare(func(*model.Config, *model.StaticTargetFile) {
		check(step+", read and not yet in force", 0)
	})
	planned := func() {
		// In force and not yet followed: another file is not the one whose
		// names are noted, and its targets are gone through as they were.
		perAsk := int64(0)
		if r.manager.StaticTargetFile() != fileBefore {
			perAsk = 1
		}
		if followed := server.followed.Load(); followed.targets != fileBefore {
			t.Errorf("%s: the reload's files are followed before the following was made", step)
		}
		check(step+", in force and not yet followed", perAsk)
		end(step + ", in force and not yet followed")
	}
	followPlannedHook.Store(&planned)
	t.Cleanup(func() { followPlannedHook.Store(nil) })
	const broken = "collectors: {\n"
	kinds := map[string]int{}
	for _, reload := range []struct {
		what, document, targets string
		refused                 bool
	}{
		{"both files reloaded", cachedDocument("x", "y"), targetsOf("x", address, "t2", "t3", "t4"), false},
		{"the static target file reloaded alone", broken, targetsOf("x", address, "t1", "t4", "t5"), true},
		{"the configuration reloaded alone", cachedDocument("x", "y", "z"), "targets: []\n", true},
		{"a reload whose files are both refused", broken, "targets: []\n", true},
		{"a reload whose files disagree with those in force", cachedDocument("y", "z"), targetsOf("gone", address, "t1"), true},
		{"both files reloaded for another collector", cachedDocument("y", "z"), targetsOf("y", address, "t3", "t5"), false},
		{"the static target file reloaded alone with its targets changed", broken, targetsOf("y", moved, "t3", "t5"), true},
		{"the same files read again", cachedDocument("y", "z"), targetsOf("y", moved, "t3", "t5"), false},
	} {
		step = reload.what
		hold()
		cfgBefore, before := r.manager.InForce()
		fileBefore = before
		r.write(r.path, reload.document)
		r.write(r.targets, reload.targets)
		if err := r.manager.Reload(config.ReloadTriggerSignal); (err != nil) != reload.refused {
			t.Fatalf("%s: the reload returned %v, refused wanted %v", step, err, reload.refused)
		}
		cfg, file := r.manager.InForce()
		kinds[fmt.Sprintf("configuration %v, static target file %v", cfg != cfgBefore, file != fileBefore)]++
		if followed := server.followed.Load(); followed.config != cfg || followed.targets != file {
			t.Fatalf("%s: what the reload left in force is not followed", step)
		}
		check(step+", done", 0)
		end(step + ", done")
	}
	if len(kinds) != 4 {
		t.Errorf("the reloads replaced %v; want reloads that replace both files, either alone and neither", kinds)
	}
	if published == 0 || notInForce == 0 || changed == 0 {
		t.Errorf("of the scrapes that ended, %d published, %d were of a target no longer in force and %d of one in force under its name that a reload changed; none of one of them shows nothing", published, notInForce, changed)
	}
}

// staticRequestsCase is a generated configuration with verbose self-metrics
// and a static target file, neither read from a file. The collectors are
// named at random, and without unique two may share a name, which no loaded
// configuration has: each has a path and a method of its own, so the
// request of a target says which collector of a name it was found with.
// The targets name the collectors, one never configured and none, at an
// address of their own, one two targets share, or one no request can be
// named for, some with a method of their own.
func staticRequestsCase(random *rand.Rand, unique bool) (*model.Config, *model.StaticTargetFile) {
	cfg := &model.Config{Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}}}
	names := []string{"a", "b", "c", "d", "e", "f"}
	order := random.Perm(len(names))
	for i := range 1 + random.IntN(len(names)) {
		name := names[random.IntN(3)]
		if unique {
			name = names[order[i]]
		}
		c := testutil.Collector(name, "text")
		c.Request.Path = "/collector" + strconv.Itoa(i)
		c.Request.Method = []string{http.MethodGet, http.MethodPost}[i%2]
		cfg.Collectors = append(cfg.Collectors, c)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Hour)}
	addresses := []string{"http://127.0.0.1:9", "http://shared.example:8080/base", "http://bad host/", ""}
	for i := range random.IntN(11) {
		// Three targets in four are of a collector the configuration has.
		collector := []string{"never_configured", "", names[random.IntN(len(names))]}[random.IntN(3)]
		if random.IntN(4) > 0 {
			collector = cfg.Collectors[random.IntN(len(cfg.Collectors))].Name
		}
		target := model.StaticTarget{Name: "t" + strconv.Itoa(i), Collector: collector, Target: "http://target" + strconv.Itoa(i) + ".example", Interval: model.Duration(time.Hour)}
		if random.IntN(2) == 0 {
			target.Target = addresses[random.IntN(len(addresses))]
		}
		if random.IntN(4) == 0 {
			target.Request.Method = http.MethodPut
		}
		file.Targets = append(file.Targets, target)
	}
	return cfg, file
}

// withoutRuntime is the lines of a self-metrics exposition but those of the
// families of the Go runtime and of the process, which no two reads have
// alike.
func withoutRuntime(exposition string) string {
	var kept []string
	for _, line := range strings.Split(exposition, "\n") {
		name := strings.TrimPrefix(strings.TrimPrefix(line, "# HELP "), "# TYPE ")
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// The requests of the static targets a read of the verbose self-metrics
// tracks are the ones going through the collectors found. Over 60
// generated configurations (20 under the race detector) of one collector
// to six, half of them named so that collectors share a name, with no
// static target to ten, of those collectors, of one never configured and
// of none, at addresses two targets share and addresses no request can be
// named for: the requests found are the ones the former search, kept
// above, finds, each of the first collector of its name — with the
// configuration in force followed, with another configuration and file put
// in force that nothing has followed yet, where the collectors are gone
// through once for every target as they were, with those followed, and
// with another file put in force with that configuration. With the
// configuration followed its collectors are gone through once at most, by
// the first read, and not at all by the next. And the exposition of a
// verbose read is what it was: a server that tracks the requests the
// former search finds, and one that only reads, track the same requests
// with the same values, the read leaves the former's as they were, and
// both answer the read with the same bytes, but for the families of the
// Go runtime and of the process — at the start and after each of the two
// changes of what is in force.
func TestTheRequestsOfTheStaticTargetsAreTheOnesGoingThroughTheCollectorsFound(t *testing.T) {
	testutil.CaptureLogs(t)
	scans := countCollectorScans(t)
	clock := time.Unix(1_700_000_000, 0)
	cases := alloctest.UnlessRaced(60, 20)
	found, skipped, shared, tracked := 0, 0, 0, 0
	for seed := range cases {
		random := rand.New(rand.NewPCG(uint64(seed), 41))
		// managers are what is in force in turn: a configuration and a file,
		// another of each, and that configuration with another file.
		var managers [3]*config.Manager
		for i := range managers {
			cfg, file := staticRequestsCase(random, seed%2 == 0)
			if i == 2 {
				cfg = managers[1].Get()
			}
			managers[i] = config.NewManager(cfg, "", slog.Default())
			managers[i].SetTargets("", file)
		}
		// was tracks what the former search finds before it reads; now only
		// reads.
		was, now := NewServer(managers[0], "python3", slog.Default()), NewServer(managers[0], "python3", slog.Default())
		for _, server := range []*Server{was, now} {
			server.requests.now = func() time.Time { return clock }
		}
		// same holds that the requests found are the former's, and says how
		// many times the collectors were gone through for them.
		same := func(what string) int64 {
			t.Helper()
			scans.Store(0)
			got := now.staticRequestKeys()
			gone := scans.Load()
			want := staticRequestKeysAsItWas(now)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("configuration %d, %s: the requests of the static targets are %v, want %v", seed, what, got, want)
			}
			targets := staticTargetsOf(now.manager.StaticTargetFile())
			found += len(got)
			skipped += len(targets) - len(got)
			return gone
		}
		// read holds that a verbose read is answered as it was.
		read := func(what string) {
			t.Helper()
			seedStaticRequestsAsItWas(was)
			former, formerCapped := was.requests.Snapshot()
			answer, formerAnswer := withoutRuntime(selfMetrics(t, now)), withoutRuntime(selfMetrics(t, was))
			for _, server := range []*Server{was, now} {
				if samples, capped := server.requests.Snapshot(); !reflect.DeepEqual(samples, former) || capped != formerCapped {
					t.Fatalf("configuration %d, %s: after a verbose read the requests tracked are %v, capped %v; want %v, capped %v, those of the former search", seed, what, samples, capped, former, formerCapped)
				}
			}
			if answer != formerAnswer {
				t.Fatalf("configuration %d, %s: the verbose read is answered\n%s\nwant\n%s", seed, what, answer, formerAnswer)
			}
			if want := fmt.Sprintf("http_exporter_request_series_tracked %d\n", len(former)); !strings.Contains(answer, want) {
				t.Fatalf("configuration %d, %s: the verbose read does not say %q", seed, what, want)
			}
			tracked += len(former)
		}
		cfg := managers[0].Get()
		for i := range cfg.Collectors {
			if model.CollectorByName(cfg, cfg.Collectors[i].Name) != &cfg.Collectors[i] {
				shared++
			}
		}
		if gone := same("with the configuration in force followed"); gone > 1 {
			t.Fatalf("configuration %d: the first search for the requests went through the collectors %d times, want once at most, to note where each is", seed, gone)
		}
		if gone := same("with the configuration in force followed, again"); gone != 0 {
			t.Fatalf("configuration %d: the next search for the requests went through the collectors %d times, want not at all", seed, gone)
		}
		read("at the start")
		for i, what := range []string{"another configuration and file", "that configuration with another file"} {
			was.manager, now.manager = managers[i+1], managers[i+1]
			if followed, inForce := now.followed.Load(), managers[i+1].StaticTargetFile(); followed.targets == inForce {
				t.Fatalf("configuration %d: %s is followed before anything asked", seed, what)
			}
			// In force and not followed: the collectors of a configuration
			// that is not the one followed are gone through for every
			// target, and those of the one followed, with a file that is
			// not, once at most, when no target had asked for one yet.
			gone, targets := same("with "+what+" in force and not followed"), int64(len(staticTargetsOf(managers[i+1].StaticTargetFile())))
			if i == 0 && gone != targets || i == 1 && gone > 1 {
				t.Fatalf("configuration %d: with %s in force and not followed the collectors were gone through %d times for %d targets; want once for each target of a configuration that is not followed, and once at most for the one that is", seed, what, gone, targets)
			}
			read("with " + what)
			if gone := same("with " + what + " followed"); gone != 0 {
				t.Fatalf("configuration %d: with %s followed the collectors were gone through %d times, want not at all", seed, what, gone)
			}
		}
	}
	// What the configurations must have had to show anything.
	if floor := cases; found < 10*floor || skipped < 5*floor || shared < floor/2 || tracked < 5*floor {
		t.Errorf("the %d configurations had %d requests found for a static target and %d targets without one, %d collectors that share an earlier one's name and %d requests tracked at a read; too few of one of them to show anything", cases, found, skipped, shared, tracked)
	}
}

// A scrape of a static target and a read of the verbose self-metrics look
// nothing up by going through the targets or the collectors. Of 60
// collectors with a static target each and verbose self-metrics, of 240,
// and of 60 with five targets each (12, 48 and 12 under the race
// detector): the first read of the self-metrics goes through the
// collectors of the configuration the exporter started with once, to note
// where each is by its name, and the next not at all, where each read went
// through them once for every static target; the scrape of the last static
// target goes through the targets not at all when it ends, where it went
// through them once, and neither does a read of the static targets
// endpoint that names that target, which did so once too. A reload that
// removes half the collectors and adds as many, their targets with them,
// goes through the collectors of its configuration once and through the
// targets of its file once, to note their names, and the read, the scrape
// and the named read after it go through neither. The requests the read
// finds for the static targets are, at these sizes too, the ones the
// former search finds.
func TestAScrapeAndAVerboseReadGoThroughNeitherTheTargetsNorTheCollectors(t *testing.T) {
	testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	collectors, targets := countCollectorScans(t), countTargetScans(t)
	// counting runs do and says how many times the collectors and the
	// targets were gone through meanwhile.
	counting := func(do func()) (int64, int64) {
		collectors.Store(0)
		targets.Store(0)
		do()
		return collectors.Load(), targets.Load()
	}
	n := alloctest.UnlessRaced(60, 12)
	for _, size := range []struct{ collectors, each int }{{n, 1}, {4 * n, 1}, {n, 5}} {
		what := fmt.Sprintf("%d collectors with %d static targets each", size.collectors, size.each)
		names := [2][]string{followBenchNames(0, size.collectors), followBenchNames(size.collectors/2, size.collectors)}
		const verbose = "web:\n  self_metrics:\n    verbose: true\n"
		documents := [2]string{verbose + cachedDocument(names[0]...), verbose + cachedDocument(names[1]...)}
		files := [2]string{targetsOfEach(names[0], size.each, target.URL), targetsOfEach(names[1], size.each, target.URL)}
		r := newReloadable(t, documents[0], files[0])
		// look is a read of the verbose self-metrics, the scrape of the
		// last static target and a read of the static targets endpoint that
		// names it, each with the times it went through the collectors and
		// the targets.
		look := func(when string, readCollectors int64) {
			t.Helper()
			byCollectors, byTargets := counting(func() { selfMetrics(t, r.server) })
			if byCollectors != readCollectors || byTargets != 0 {
				t.Errorf("%s, %s: a read of the verbose self-metrics went through the collectors %d times and the targets %d times, want %d and not at all", what, when, byCollectors, byTargets, readCollectors)
			}
			// The targets of a collector, all at one address, share a request.
			if got, want := r.server.staticRequestKeys(), staticRequestKeysAsItWas(r.server); len(got) != size.collectors || !reflect.DeepEqual(got, want) {
				t.Errorf("%s, %s: the static targets have %d requests, want the %d the former search finds, one for each collector", what, when, len(got), len(want))
			}
			followed := r.server.followedInForce()
			static := staticTargetsOf(followed.targets)
			last := static[len(static)-1]
			byCollectors, byTargets = counting(func() {
				r.server.scrapeTargetSince(context.Background(), followed.config, followed.generation, last)
			})
			if len(publishedOf(r.server, last.Name)) == 0 {
				t.Fatalf("%s, %s: the scrape of the last static target published nothing", what, when)
			}
			if byCollectors != 0 || byTargets != 0 {
				t.Errorf("%s, %s: the scrape of the last static target went through the collectors %d times and the targets %d times, want neither at all", what, when, byCollectors, byTargets)
			}
			byCollectors, byTargets = counting(func() {
				if named := probeOnce(t, r.server, r.server.staticTargetsEndpoint()+"?"+staticTargetsParam+"="+last.Name, nil); named.Code != http.StatusOK || !strings.Contains(named.Body.String(), `static_target="`+last.Name+`"`) {
					t.Fatalf("%s, %s: the read of the static targets endpoint that names the last target was answered %d: %s", what, when, named.Code, named.Body)
				}
			})
			if byCollectors != 0 || byTargets != 0 {
				t.Errorf("%s, %s: a read of the static targets endpoint that names a target went through the collectors %d times and the targets %d times, want neither at all", what, when, byCollectors, byTargets)
			}
		}
		look("at the start", 1)
		look("at the start, again", 0)
		for reload := 1; reload <= 2; reload++ {
			byCollectors, byTargets := counting(func() { r.reloadBoth(documents[reload%2], files[reload%2]) })
			if byCollectors != 1 || byTargets != 1 {
				t.Errorf("%s: reload %d went through the collectors %d times and the targets %d times, want each once", what, reload, byCollectors, byTargets)
			}
			look(fmt.Sprintf("after reload %d", reload), 0)
		}
	}
}

// A lookup by name made while reloads come is of what was in force when it
// was made. Two readers ask, over and over, whether targets of some names
// are in force, for the targets three reads of the static targets endpoint
// name, one of them of a target only one of the files has and one of a
// target none has, and for the requests of the static targets, while
// reloads replace both files, the static target file alone and the
// configuration alone, in turn: whenever no reload came during one round of
// asking, every answer is what going through the targets and the collectors
// of the files in force gives, as the former lookups, kept beside the tests,
// give it. Each reload waits for both readers to have checked two rounds
// more, so they ask before, while and after every reload however fast the
// machine. Run with the race detector, it also shows that the names and the
// places noted for what is followed are read while a reload notes those of
// what it puts in force.
func TestALookupByNameMadeWhileReloadsComeIsOfWhatWasInForce(t *testing.T) {
	testutil.CaptureLogs(t)
	const address = "http://127.0.0.1:9/a"
	const verbose = "web:\n  self_metrics:\n    verbose: true\n"
	documents := [2]string{verbose + cachedDocument("x", "y"), verbose + cachedDocument("z", "y", "x")}
	files := [2]string{targetsOf("x", address, "t1", "t2", "t3"), targetsOf("y", address, "t4", "t3", "t1")}
	r := newReloadable(t, documents[0], files[0])
	server := r.server
	asked := []string{"t1", "t2", "t3", "t4", "never_configured"}
	queries := []url.Values{{staticTargetsParam: {"t1"}}, {staticTargetsParam: {"t3, t2", "t1"}}, {staticTargetsParam: {"never_configured"}}}
	const readers = 2
	var checked [readers]atomic.Int64
	var differed atomic.Bool
	// differs reports the first answer that is not the former lookup's, and
	// no other: a reader goes on asking.
	differs := func(format string, args ...any) {
		if differed.CompareAndSwap(false, true) {
			t.Errorf(format, args...)
		}
	}
	quit := make(chan struct{})
	var wg sync.WaitGroup
	for i := range readers {
		wg.Go(func() {
			for {
				select {
				case <-quit:
					return
				default:
				}
				cfg, file := server.manager.InForce()
				inForce, inForceWas := make([]bool, len(asked)), make([]bool, len(asked))
				for k, name := range asked {
					inForce[k], inForceWas[k] = server.staticTargetInForce(name), staticTargetInForceAsItWas(server, name)
				}
				type named struct {
					names   map[string]bool
					refused string
				}
				reads, readsWere := make([]named, len(queries)), make([]named, len(queries))
				for k, query := range queries {
					for _, read := range []struct {
						ask func(*Server, url.Values) (map[string]bool, error)
						to  *named
					}{{(*Server).requestedStaticTargets, &reads[k]}, {requestedStaticTargetsAsItWas, &readsWere[k]}} {
						names, err := read.ask(server, query)
						*read.to = named{names: names}
						if err != nil {
							read.to.refused = err.Error()
						}
					}
				}
				requests, requestsWere := server.staticRequestKeys(), staticRequestKeysAsItWas(server)
				// A reload that came meanwhile leaves the answers of the
				// files before it, of those after it, or of each in part.
				if now, fileNow := server.manager.InForce(); now != cfg || fileNow != file {
					continue
				}
				if !slices.Equal(inForce, inForceWas) {
					differs("the targets named %q are told in force %v while reloads come, want %v", asked, inForce, inForceWas)
				}
				if !reflect.DeepEqual(reads, readsWere) {
					differs("the reads %v are given %v while reloads come, want %v", queries, reads, readsWere)
				}
				if len(requests) == 0 || !reflect.DeepEqual(requests, requestsWere) {
					differs("the requests of the static targets are %v while reloads come, want %v, and one at least", requests, requestsWere)
				}
				checked[i].Add(1)
			}
		})
	}
	// wait waits until every reader has checked two rounds more.
	wait := func() {
		t.Helper()
		var after [readers]int64
		for i := range checked {
			after[i] = checked[i].Load() + 2
		}
		testutil.WaitFor(t, "the readers to check two rounds more", func() bool {
			for i := range checked {
				if checked[i].Load() < after[i] {
					return false
				}
			}
			return true
		})
	}
	kinds := map[string]int{}
	// reload reloads both files, and one alone where the other is refused.
	reload := func(document, targets string, refused bool) {
		t.Helper()
		cfgBefore, fileBefore := r.manager.InForce()
		r.write(r.path, document)
		r.write(r.targets, targets)
		if err := r.manager.Reload(config.ReloadTriggerSignal); (err != nil) != refused {
			t.Fatalf("the reload returned %v, refused wanted %v", err, refused)
		}
		cfg, file := r.manager.InForce()
		kinds[fmt.Sprintf("configuration %v, static target file %v", cfg != cfgBefore, file != fileBefore)]++
		wait()
	}
	wait()
	for range 3 {
		reload(documents[1], files[1], false)
		reload("collectors: {\n", files[0], true)
		reload(documents[0], "targets: []\n", true)
	}
	close(quit)
	wg.Wait()
	if len(kinds) != 3 || kinds["configuration false, static target file false"] != 0 {
		t.Errorf("the reloads replaced %v; want reloads that replace both files and either alone", kinds)
	}
}
